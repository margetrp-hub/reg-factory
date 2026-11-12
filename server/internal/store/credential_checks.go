package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const CredentialCheckScript = "credential.check"

type CredentialCheckItem struct {
	Ordinal      int    `json:"ordinal"`
	Ref          string `json:"ref"`
	Identity     string `json:"identity"`
	Status       string `json:"status"`
	Message      string `json:"message"`
	CheckedAt    string `json:"checked_at"`
	ChildTaskID  string `json:"child_task_id"`
	Mode         string `json:"mode"`
	CanRefresh   bool   `json:"can_refresh"`
	CheckVersion int    `json:"-"`
}

type CredentialCheckCounts struct {
	Total        int `json:"total"`
	Checked      int `json:"checked"`
	OK           int `json:"ok"`
	NeedsRefresh int `json:"needs_refresh"`
	Unknown      int `json:"unknown"`
	Abnormal     int `json:"abnormal"`
}

// CreateCredentialCheck commits acceptance and the full ordered work list together.
// Only execution evidence is stored here; credentials stay in the asset store.
func (s *Store) CreateCredentialCheck(task Task, accounts []Account) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = createCredentialCheck(s, tx, task, accounts); err != nil {
		return err
	}
	return tx.Commit()
}

func createCredentialCheck(s *Store, tx *sql.Tx, task Task, accounts []Account) error {
	if err := s.ex.LockKey(tx, "credential-check-admission"); err != nil {
		return err
	}
	var queued int
	if err := s.ex.QueryRowTx(tx, `SELECT COUNT(*) FROM tasks WHERE script=? AND status IN ('queued','running','stopping')`, CredentialCheckScript).Scan(&queued); err != nil {
		return err
	}
	if queued >= 20 {
		return errors.New("check_queue_full")
	}
	createdAt := task.CreatedAt
	if createdAt == "" {
		createdAt = now()
	}
	_, err := s.ex.ExecTxArgs(tx, `INSERT INTO tasks(id,script,title,args_json,status,created_at,actor) VALUES(?,?,?,?,'queued',?,?)`, task.ID, CredentialCheckScript, task.Title, task.ArgsJSON, createdAt, task.Actor)
	if err != nil {
		return err
	}
	for i, account := range accounts {
		mode := "report"
		if account.Platform == "chatgpt" {
			mode = "at"
		}
		if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO credential_check_items(task_id,ordinal,ref,identity,mode) VALUES(?,?,?,?,?)`, task.ID, i, account.Ref, account.Identity, mode); err != nil {
			return err
		}
	}
	return nil
}

// ClaimCredentialCheck is used by the single bounded API worker, never Python.
func (s *Store) ClaimCredentialCheck() (Task, error) {
	var id string
	err := s.ex.QueryRow(`UPDATE tasks SET status='running',started_at=?
 WHERE id=(SELECT id FROM tasks WHERE script=? AND status='queued' ORDER BY created_at,id LIMIT 1)
 AND status='queued' RETURNING id`, now(), CredentialCheckScript).Scan(&id)
	if err != nil {
		return Task{}, err
	}
	return s.GetTask(id)
}

func (s *Store) CredentialCheckItems(taskID string) ([]CredentialCheckItem, error) {
	rows, err := s.ex.Query(`SELECT i.ordinal,i.ref,i.identity,i.status,i.message,i.checked_at,i.child_task_id,i.mode,
 CASE WHEN a.ref IS NOT NULL AND a.claimed=0 THEN 1 ELSE 0 END
 FROM credential_check_items i LEFT JOIN accounts a ON a.ref=i.ref WHERE i.task_id=? ORDER BY i.ordinal`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []CredentialCheckItem{}
	for rows.Next() {
		var item CredentialCheckItem
		if err = rows.Scan(&item.Ordinal, &item.Ref, &item.Identity, &item.Status, &item.Message, &item.CheckedAt, &item.ChildTaskID, &item.Mode, &item.CanRefresh); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) CredentialCheckCounts(ids []string) (map[string]CredentialCheckCounts, error) {
	counts := map[string]CredentialCheckCounts{}
	if len(ids) == 0 {
		return counts, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.ex.Query(`SELECT task_id,COUNT(*),SUM(CASE WHEN status NOT IN ('pending','running') THEN 1 ELSE 0 END),SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),SUM(CASE WHEN status='needs_refresh' THEN 1 ELSE 0 END),SUM(CASE WHEN status='unknown' THEN 1 ELSE 0 END),SUM(CASE WHEN status NOT IN ('pending','running','ok','needs_refresh','unknown') THEN 1 ELSE 0 END)
 FROM credential_check_items WHERE task_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`) GROUP BY task_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c CredentialCheckCounts
		if err = rows.Scan(&id, &c.Total, &c.Checked, &c.OK, &c.NeedsRefresh, &c.Unknown, &c.Abnormal); err != nil {
			return nil, err
		}
		counts[id] = c
	}
	return counts, rows.Err()
}

func (s *Store) StartCredentialCheckItem(taskID string, ordinal int) (bool, error) {
	result, err := s.ex.Exec(`UPDATE credential_check_items SET status='running' WHERE task_id=? AND ordinal=? AND status='pending' AND EXISTS(SELECT 1 FROM tasks WHERE id=? AND status='running')`, taskID, ordinal, taskID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// CompleteCredentialCheckItem keeps the latest health projection and its receipt
// atomic. Historical report reads never pretend to be a fresh health observation.
func (s *Store) CompleteCredentialCheckItem(taskID string, item CredentialCheckItem) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if item.CheckedAt == "" && item.Mode == "at" {
		item.CheckedAt = now()
	}
	item.Message = RedactLog(item.Message)
	if item.Mode == "at" {
		written, err := s.recordATHealth(tx, item.Ref, item.Status, item.Message, item.CheckedAt, item.CheckVersion)
		if err != nil {
			return err
		}
		if !written {
			item.Status, item.Message = "unknown", "凭据版本已变化或原刷新尚未结束；请稍后重测 AT"
		}
	}
	res, err := s.ex.ExecTxArgs(tx, `UPDATE credential_check_items SET status=?,message=?,checked_at=?,child_task_id=? WHERE task_id=? AND ordinal=? AND status='running'`, item.Status, item.Message, item.CheckedAt, item.ChildTaskID, taskID, item.Ordinal)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("check_item_not_running")
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'check',?)`, taskID, now(), fmt.Sprintf("第 %d 项 · %s · %s", item.Ordinal+1, item.Status, item.Message)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FinishCredentialCheck(taskID, status, message string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = s.ex.ExecTxArgs(tx, `UPDATE credential_check_items SET status='unknown',message='检查未取得最终结果；可重新测试'
 WHERE task_id=? AND status='running'`, taskID); err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE tasks SET status=CASE WHEN status='stopping' THEN 'stopped' ELSE ? END,
 error=?,exit_code=CASE WHEN ?='succeeded' THEN 0 ELSE -1 END,finished_at=?,pid=0
 WHERE id=? AND script=? AND status IN ('running','stopping')`, status, RedactLog(message), status, now(), taskID, CredentialCheckScript)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) StopCredentialCheck(taskID string) error {
	_, err := s.ex.Exec(`UPDATE tasks SET status=CASE WHEN status='queued' THEN 'stopped' ELSE 'stopping' END,
 finished_at=CASE WHEN status='queued' THEN ? ELSE finished_at END,error='操作员停止；保留已返回结果'
 WHERE id=? AND script=? AND status IN ('queued','running')`, now(), taskID, CredentialCheckScript)
	return err
}

func (s *Store) StopCredentialChecks() error {
	_, err := s.ex.Exec(`UPDATE tasks SET status=CASE WHEN status='queued' THEN 'stopped' ELSE 'stopping' END,
 finished_at=CASE WHEN status='queued' THEN ? ELSE finished_at END,error='操作员停止；保留已返回结果'
 WHERE script=? AND status IN ('queued','running')`, now(), CredentialCheckScript)
	return err
}
