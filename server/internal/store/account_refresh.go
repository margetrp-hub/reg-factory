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
	var args []string
	// Work owns delivered-account authorization and its signed recovery protocol.
	// Panel locks must not take over or reject those already-authorized tasks.
	if strings.HasPrefix(task.ID, "work-") || task.Script != "asset_ops" || json.Unmarshal([]byte(task.ArgsJSON), &args) != nil || len(args) < 3 || args[0] != "refresh" {
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
// the same account. Unknown or interrupted exchanges retain their ownership.
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
	err = tx.QueryRow(`SELECT task_id,args_json,actor FROM account_refresh_requests WHERE request_id=?`, task.ID).Scan(&old, &args, &actor)
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
	if err = tx.QueryRow(`SELECT claimed FROM accounts WHERE ref=?`, ref).Scan(&claimed); err != nil {
		return "", false, err
	}
	if claimed {
		return "", false, ErrAccountOwned
	}
	err = tx.QueryRow(`SELECT id FROM tasks WHERE script='asset_ops' AND json_valid(args_json)
 AND json_extract(args_json,'$[0]')='refresh' AND json_extract(args_json,'$[2]')=?
 AND (status IN ('queued','running','unknown','stopping') OR (status IN ('stopped','interrupted') AND started_at<>''))
 ORDER BY created_at,id LIMIT 1`, ref).Scan(&old)
	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		return "", false, err
	}
	if created {
		old = task.ID
		if _, err = tx.Exec(`INSERT INTO tasks(id,script,title,args_json,status,created_at,actor) VALUES(?,?,?,?,'queued',?,?)`, task.ID, task.Script, task.Title, task.ArgsJSON, now(), task.Actor); err != nil {
			return "", false, err
		}
		if _, err = tx.Exec(`UPDATE accounts SET refresh_state='queued',refresh_at=?,version=version+1 WHERE ref=?`, now(), ref); err != nil {
			return "", false, err
		}
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO account_refreshes(task_id,ref) VALUES(?,?)`, old, ref); err != nil {
		return "", false, err
	}
	// Persist coalesced request IDs too, including after the original finishes.
	if _, err = tx.Exec(`INSERT INTO account_refresh_requests(request_id,task_id,args_json,actor) VALUES(?,?,?,?)`, task.ID, old, task.ArgsJSON, task.Actor); err != nil {
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
	if err = tx.QueryRow(`SELECT t.status,a.claimed FROM tasks t JOIN accounts a ON a.ref=? WHERE t.id=?`, ref, taskID).Scan(&status, &claimed); err != nil {
		return err
	}
	if status != "queued" {
		return errors.New("refresh_not_queued")
	}
	if claimed {
		return ErrAccountOwned
	}
	if _, err = tx.Exec(`INSERT INTO account_refreshes(task_id,ref,source_sha) VALUES(?,?,?) ON CONFLICT(task_id) DO UPDATE SET source_sha=excluded.source_sha`, taskID, ref, sha); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE tasks SET status='running',started_at=? WHERE id=?`, now(), taskID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE accounts SET refresh_state='running',refresh_at=? WHERE ref=?`, now(), ref); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RefreshEvidence(taskID string) (RefreshEvidence, error) {
	var evidence RefreshEvidence
	err := s.db.QueryRow(`SELECT ref,source_sha,result_sha,resolution FROM account_refreshes WHERE task_id=?`, taskID).Scan(&evidence.Ref, &evidence.SourceSHA, &evidence.ResultSHA, &evidence.Resolution)
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
	result, err := tx.Exec(`UPDATE tasks SET status=?,exit_code=?,error=?,finished_at=?,pid=0 WHERE id=? AND status IN ('queued','running','stopping')`, status, code, RedactLog(message), now(), taskID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("refresh_already_finished")
	}
	if _, err = tx.Exec(`INSERT INTO account_refreshes(task_id,ref,result_sha) VALUES(?,?,?) ON CONFLICT(task_id) DO UPDATE SET result_sha=excluded.result_sha`, taskID, ref, resultSHA); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE accounts SET refresh_state=?,refresh_at=?,updated_at=? WHERE ref=?`, status, now(), now(), ref); err != nil {
		return err
	}
	if status == "succeeded" && resultSHA != "" {
		if err = prepareRefreshedAccount(tx, taskID, ref); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	if err = tx.QueryRow(`SELECT t.status,a.claimed,COALESCE(r.resolution,'') FROM tasks t JOIN accounts a ON a.ref=? LEFT JOIN account_refreshes r ON r.task_id=t.id WHERE t.id=?`, ref, taskID).Scan(&status, &claimed, &resolution); err != nil {
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
	if _, err = tx.Exec(`UPDATE tasks SET status=?,exit_code=?,error=?,finished_at=?,pid=0 WHERE id=?`, outcome, code, message, now(), taskID); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO account_refreshes(task_id,ref,result_sha,resolution) VALUES(?,?,?,?) ON CONFLICT(task_id) DO UPDATE SET result_sha=excluded.result_sha,resolution=excluded.resolution`, taskID, ref, resultSHA, resolution); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE accounts SET refresh_state=?,refresh_at=?,updated_at=? WHERE ref=?`, outcome, now(), now(), ref); err != nil {
		return err
	}
	if outcome == "succeeded" && resultSHA != "" {
		if err = prepareRefreshedAccount(tx, taskID, ref); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'system',?)`, taskID, now(), RedactLog(actor+" · "+resolution)); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO audit(ts,actor,action,detail) VALUES(?,?,'account.refresh.resolve',?)`, now(), actor, RedactLog(taskID+" · "+resolution)); err != nil {
		return err
	}
	return tx.Commit()
}
