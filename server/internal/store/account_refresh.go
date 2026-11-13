package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

var ErrAccountOwned = errors.New("account_owned_by_core")
var ErrRefreshState = errors.New("refresh_not_awaiting_resolution")

func AccountRefreshRef(task Task) string {
	// Work owns delivered-account authorization and its signed recovery protocol.
	// Panel locks must not take over or reject those already-authorized tasks.
	if strings.HasPrefix(task.ID, "work-") {
		return ""
	}
	return CredentialTaskRef(task)
}

func CredentialTaskRef(task Task) string {
	var args []string
	if task.Script != "asset_ops" || json.Unmarshal([]byte(task.ArgsJSON), &args) != nil || len(args) < 3 || args[0] != "refresh" {
		return ""
	}
	for i := 1; i+1 < len(args); i++ {
		if args[i] == "--ref" {
			return args[i+1]
		}
	}
	return ""
}

type RefreshEvidence struct {
	Ref        string `json:"ref"`
	SourceSHA  string `json:"source_sha"`
	ResultSHA  string `json:"result_sha"`
	Resolution string `json:"resolution"`
}

// QueueAccountRefresh serializes both request replays and distinct requests for
// the same account. Ended failures release ownership for a fresh repair request.
func (s *Store) QueueAccountRefresh(task Task, ref string) (string, bool, error) {
	if AccountRefreshRef(task) != ref || ref == "" {
		return "", false, errors.New("invalid_refresh_task")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var old, args, actor string
	if err = s.ex.LockKey(tx, "refresh-request:"+task.ID); err != nil {
		return "", false, err
	}
	err = s.ex.QueryRowTx(tx, `SELECT task_id,args_json,actor FROM account_refresh_requests WHERE request_id=?`, task.ID).Scan(&old, &args, &actor)
	if err == nil {
		if args != task.ArgsJSON || actor != task.Actor {
			return "", false, errors.New("task_request_conflict")
		}
		return old, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	var claimed bool
	var platform, identity string
	if err = s.ex.QueryRowTx(tx, `SELECT claimed,platform,`+accountIdentitySQL+` FROM accounts WHERE ref=?`, ref).Scan(&claimed, &platform, &identity); err != nil {
		return "", false, err
	}
	if err = s.ex.LockKey(tx, "refresh-account:"+platform+":"+identity); err != nil {
		return "", false, err
	}
	if err = s.ex.QueryRowTx(tx, `SELECT MAX(claimed) FROM accounts WHERE platform=? AND (`+accountIdentitySQL+`)=?`, platform, identity).Scan(&claimed); err != nil {
		return "", false, err
	}
	if claimed {
		return "", false, ErrAccountOwned
	}
	err = s.ex.QueryRowTx(tx, `SELECT id FROM tasks WHERE script='asset_ops'
 AND `+s.ex.JSONText("args_json", "0")+`='refresh' AND `+s.ex.JSONText("args_json", "2")+` IN
 (SELECT ref FROM accounts WHERE platform=? AND (`+accountIdentitySQL+`)=?)
 AND status IN ('queued','running','stopping')
 ORDER BY created_at,id LIMIT 1`, platform, identity).Scan(&old)
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		return "", false, err
	}
	if !created && strings.HasPrefix(old, "work-") {
		return "", false, ErrAccountOwned
	}
	if created {
		old = task.ID
		if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO tasks(id,script,title,args_json,status,created_at,actor) VALUES(?,?,?,?,'queued',?,?)`, task.ID, task.Script, task.Title, task.ArgsJSON, now(), task.Actor); err != nil {
			return "", false, err
		}
		if _, err = s.ex.ExecTxArgs(tx, `UPDATE accounts SET refresh_state='queued',refresh_at=?,version=version+1 WHERE ref=?`, now(), ref); err != nil {
			return "", false, err
		}
	}
	if created {
		if _, err = s.ex.ExecTxArgs(tx, UpsertIgnore(s.dialect, `INSERT INTO account_refreshes(task_id,ref) VALUES(?,?)`), old, ref); err != nil {
			return "", false, err
		}
	}
	// Persist coalesced request IDs too, including after the original finishes.
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO account_refresh_requests(request_id,task_id,args_json,actor) VALUES(?,?,?,?)`, task.ID, old, task.ArgsJSON, task.Actor); err != nil {
		return "", false, err
	}
	return old, created, tx.Commit()
}

// BeginAccountRefresh records the current version immediately before spawning.
func (s *Store) BeginAccountRefresh(taskID, ref, sha string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	var claimed bool
	if err = s.ex.QueryRowTx(tx, `SELECT t.status,a.claimed FROM tasks t JOIN accounts a ON a.ref=? WHERE t.id=?`, ref, taskID).Scan(&status, &claimed); err != nil {
		return err
	}
	if status != "queued" {
		return errors.New("refresh_not_queued")
	}
	if claimed {
		return ErrAccountOwned
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO account_refreshes(task_id,ref,source_sha) VALUES(?,?,?) ON CONFLICT(task_id) DO UPDATE SET source_sha=excluded.source_sha`, taskID, ref, sha); err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `UPDATE tasks SET status='running',started_at=? WHERE id=?`, now(), taskID); err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `UPDATE accounts SET refresh_state='running',refresh_at=? WHERE ref=?`, now(), ref); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RefreshEvidence(taskID string) (RefreshEvidence, error) {
	var evidence RefreshEvidence
	err := s.ex.QueryRow(`SELECT ref,source_sha,result_sha,resolution FROM account_refreshes WHERE task_id=?`, taskID).Scan(&evidence.Ref, &evidence.SourceSHA, &evidence.ResultSHA, &evidence.Resolution)
	if errors.Is(err, sql.ErrNoRows) {
		task, readErr := s.GetTask(taskID)
		if readErr != nil {
			return evidence, readErr
		}
		evidence.Ref = AccountRefreshRef(task)
		if evidence.Ref != "" {
			return evidence, nil
		}
	}
	return evidence, err
}

// FinishAccountRefresh commits the task and account projections together.
func (s *Store) FinishAccountRefresh(taskID, ref, status string, code int, message, resultSHA string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := s.ex.ExecTxArgs(tx, `UPDATE tasks SET status=?,exit_code=?,error=?,finished_at=?,pid=0 WHERE id=? AND status IN ('queued','running','stopping')`, status, code, RedactLog(message), now(), taskID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		var existing, digest string
		if err := s.ex.QueryRowTx(tx, `SELECT t.status,COALESCE(r.result_sha,'') FROM tasks t LEFT JOIN account_refreshes r ON r.task_id=t.id WHERE t.id=?`, taskID).Scan(&existing, &digest); err == nil && existing == status && digest == resultSHA {
			return nil
		}
		return errors.New("refresh_already_finished")
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO account_refreshes(task_id,ref,result_sha) VALUES(?,?,?) ON CONFLICT(task_id) DO UPDATE SET result_sha=excluded.result_sha`, taskID, ref, resultSHA); err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `UPDATE accounts SET refresh_state=?,refresh_at=?,updated_at=? WHERE ref=?`, status, now(), now(), ref); err != nil {
		return err
	}
	if status == "succeeded" && resultSHA != "" {
		if err = prepareRefreshedAccount(s, tx, taskID, ref); err != nil {
			return err
		}
	}
	if status == "failed" && code == 1 {
		if err = recordRefreshFailureState(s, tx, taskID, ref); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Only explicit structured evidence can change account/workspace availability.
// Keep the original credential and terminal task in the same transaction.
func recordRefreshFailureState(s *Store, tx *sql.Tx, taskID, ref string) error {
	rows, err := s.ex.QueryTx(tx, `SELECT line FROM events WHERE task_id=? AND stream='stdout' ORDER BY id DESC LIMIT 100`, taskID)
	if err != nil {
		return err
	}
	state := ""
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			break
		}
		var value struct {
			Ref, Result string
			Schema      string `json:"error_schema"`
			Code        string `json:"error_code"`
		}
		if json.Unmarshal([]byte(line), &value) != nil || value.Ref != ref || value.Result != "failed" || value.Schema != "account.failure.v1" {
			continue
		}
		switch value.Code {
		case "account_deactivated", "account_banned":
			state = "banned"
		case "workspace_deactivated", "deactivated_workspace":
			state = "workspace_deactivated"
		}
		break
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil || state == "" {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE accounts SET status=?,liveness_status=?,health_checked_at=?,liveness_checked_at=?,health_message=?
 WHERE ref=? AND claimed=0 AND health_checked_at<=COALESCE((SELECT started_at FROM tasks WHERE id=?),'')`,
		state, state, now(), now(), "上游明确返回："+state, ref, taskID)
	return err
}

// UnfinishedCredentials is consumed before generic restart recovery or queue
// dispatch. It never reruns a worker or rewrites an already ended task.
func (s *Store) UnfinishedCredentials() ([]Task, error) {
	rows, err := s.ex.Query(`SELECT id FROM tasks WHERE script='asset_ops' AND status IN ('queued','running','stopping') AND ` + s.ex.JSONText("args_json", "0") + `='refresh' ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var tasks []Task
	for _, id := range ids {
		task, err := s.GetTask(id)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func (s *Store) ResolveAccountRefresh(taskID, actor, reason, outcome, resultSHA string) error {
	if outcome != "succeeded" && outcome != "failed" {
		return errors.New("invalid_resolution")
	}
	reason = RedactLog(strings.TrimSpace(reason))
	if outcome == "failed" && (len(reason) < 4 || len(reason) > 1000) {
		return errors.New("resolution_reason_required")
	}
	task, err := s.GetTask(taskID)
	if err != nil {
		return err
	}
	ref := AccountRefreshRef(task)
	if ref == "" {
		return ErrRefreshState
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status, resolution string
	var claimed bool
	if err = s.ex.QueryRowTx(tx, `SELECT t.status,a.claimed,COALESCE(r.resolution,'') FROM tasks t JOIN accounts a ON a.ref=? LEFT JOIN account_refreshes r ON r.task_id=t.id WHERE t.id=?`, ref, taskID).Scan(&status, &claimed, &resolution); err != nil {
		return err
	}
	if claimed {
		return ErrAccountOwned
	}
	if resolution != "" && status == outcome {
		return nil
	}
	if status != "unknown" && status != "interrupted" && status != "stopped" {
		return ErrRefreshState
	}
	resolution = "管理员确认未完成刷新：" + reason
	code := 1
	message := resolution
	if outcome == "succeeded" {
		resolution = "核对已保存的新凭据通过；未再次使用 RT"
		code = 0
		message = ""
	}
	if _, err = s.ex.ExecTxArgs(tx, `UPDATE tasks SET status=?,exit_code=?,error=?,finished_at=?,pid=0 WHERE id=?`, outcome, code, message, now(), taskID); err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO account_refreshes(task_id,ref,result_sha,resolution) VALUES(?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET result_sha=excluded.result_sha,resolution=excluded.resolution`, taskID, ref, resultSHA, resolution); err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `UPDATE accounts SET refresh_state=?,refresh_at=?,updated_at=? WHERE ref=?`, outcome, now(), now(), ref); err != nil {
		return err
	}
	if outcome == "succeeded" && resultSHA != "" {
		if err = prepareRefreshedAccount(s, tx, taskID, ref); err != nil {
			return err
		}
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'system',?)`, taskID, now(), RedactLog(actor+" · "+resolution)); err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO audit(ts,actor,action,detail) VALUES(?,?,'account.refresh.resolve',?)`, now(), actor, RedactLog(taskID+" · "+resolution)); err != nil {
		return err
	}
	return tx.Commit()
}
