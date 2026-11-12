package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const centralAssetSchema = `CREATE TABLE IF NOT EXISTS central_asset_receipts (
 task_id TEXT PRIMARY KEY REFERENCES tasks(id), receipt_json TEXT NOT NULL,
 settled INTEGER NOT NULL DEFAULT 0, acknowledged INTEGER NOT NULL DEFAULT 0,
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt BIGINT NOT NULL DEFAULT 0);
 CREATE INDEX IF NOT EXISTS idx_central_asset_delivery ON central_asset_receipts(acknowledged,next_attempt);`

// Only public evidence is stored here. Credentials stay in the encrypted SDK
// journal until both the owner ACK and this local task transaction commit.
func (s *Store) CentralReceipt(taskID string) (map[string]any, error) {
	var raw string
	if err := s.exRO.QueryRow("SELECT receipt_json FROM central_asset_receipts WHERE task_id=?", taskID).Scan(&raw); err != nil {
		return nil, err
	}
	var value map[string]any
	err := json.Unmarshal([]byte(raw), &value)
	return value, err
}

func (s *Store) SaveCentralReceipt(taskID, state string, value map[string]any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 16384 {
		return errors.New("ASSET_RECEIPT_INVALID")
	}
	settled := state == "completed" || state == "failed" || state == "closed"
	status, code := "waiting_asset", 0
	if state == "completed" {
		status = "succeeded"
	} else if state == "failed" || state == "closed" {
		status = "failed"
		code = 1
	} else if state == "unknown" {
		status = "unknown"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lock := "SELECT id FROM tasks WHERE id=?"
	if s.dialect == DialectPG {
		lock += " FOR UPDATE"
	}
	var locked string
	if err = s.ex.QueryRowTx(tx, lock, taskID).Scan(&locked); err != nil {
		return err
	}
	var previous string
	var terminal bool
	e := s.ex.QueryRowTx(tx, "SELECT receipt_json,settled FROM central_asset_receipts WHERE task_id=?", taskID).Scan(&previous, &terminal)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if e == nil && terminal && previous != string(raw) {
		return errors.New("ASSET_LOCAL_RESULT_CONFLICT")
	}
	settledInt := 0
	if settled {
		settledInt = 1
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO central_asset_receipts(task_id,receipt_json,settled,next_attempt) VALUES(?,?,?,?)
	 ON CONFLICT(task_id) DO UPDATE SET receipt_json=excluded.receipt_json,settled=excluded.settled,
	 attempts=central_asset_receipts.attempts+1,next_attempt=excluded.next_attempt`, taskID, string(raw), settledInt, time.Now().Unix()+30); err != nil {
		return err
	}
	message, _ := value["error_code"].(string)
	_, err = s.ex.ExecTxArgs(tx, "UPDATE tasks SET status=?,exit_code=?,error=?,finished_at=CASE WHEN status IN ('succeeded','failed') AND finished_at<>'' THEN finished_at ELSE ? END,pid=0 WHERE id=?", status, code, message, now(), taskID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AcknowledgeCentral(taskID string) error {
	_, err := s.ex.Exec("UPDATE central_asset_receipts SET acknowledged=1 WHERE task_id=? AND settled=1", taskID)
	return err
}

func (s *Store) DeferCentral(taskID string) {
	_, _ = s.ex.Exec(`INSERT INTO central_asset_receipts(task_id,receipt_json,next_attempt,attempts)
	 VALUES(?,'{"state":"pending","error_code":"ASSET_RESULT_DELIVERY_PENDING"}',?,1)
	 ON CONFLICT(task_id) DO UPDATE SET next_attempt=?+CASE WHEN central_asset_receipts.attempts>=9 THEN 300 ELSE 30*(central_asset_receipts.attempts+1) END,attempts=central_asset_receipts.attempts+1`, taskID, time.Now().Unix()+30, time.Now().Unix())
}

func (s *Store) PendingCentral() ([]Task, error) {
	rows, err := s.exRO.Query(`SELECT t.id FROM tasks t LEFT JOIN central_asset_receipts c ON c.task_id=t.id
	 WHERE t.script='central_asset' AND t.status NOT IN ('queued','running','stopping')
	 AND COALESCE(c.acknowledged,0)=0 AND COALESCE(c.next_attempt,0)<=?
	 ORDER BY COALESCE(c.next_attempt,0),t.id LIMIT 10`, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	ids := []string{}
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
	result := []Task{}
	for _, id := range ids {
		t, e := s.GetTask(id)
		if e != nil {
			return nil, e
		}
		result = append(result, t)
	}
	return result, nil
}
