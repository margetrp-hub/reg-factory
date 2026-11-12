package store

import "errors"

// The binding is saved from an authenticated assignment before execution. A
// read request must never establish its own authority to another task's logs.
func (s *Store) BindWorkEventScope(operationID, taskID, scope string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO work_event_scopes(operation_id,task_id,scope_sha) VALUES(?,?,?) ON CONFLICT DO NOTHING`, operationID, taskID, scope); err != nil {
		return err
	}
	var savedTask, savedScope string
	if err = s.ex.QueryRowTx(tx, `SELECT task_id,scope_sha FROM work_event_scopes WHERE operation_id=?`, operationID).Scan(&savedTask, &savedScope); err != nil {
		return err
	}
	if savedTask != taskID || savedScope != scope {
		return errors.New("WORK_EVENTS_SCOPE_MISMATCH")
	}
	return tx.Commit()
}

func (s *Store) WorkEventScope(operationID string) (string, string, error) {
	var task, scope string
	err := s.ex.QueryRow(`SELECT task_id,scope_sha FROM work_event_scopes WHERE operation_id=?`, operationID).Scan(&task, &scope)
	return task, scope, err
}

// ConsumeWorkEventRead survives process restarts and stays bounded by the
// signed request lifetime. Retries use a fresh request ID, never a new task.
func (s *Store) ConsumeWorkEventRead(issuer, requestID string, expires, now int64) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = s.ex.ExecTxArgs(tx, `DELETE FROM work_event_reads WHERE expires_at<=?`, now); err != nil {
		return false, err
	}
	result, err := s.ex.ExecTxArgs(tx, `INSERT INTO work_event_reads(issuer,request_id,expires_at) VALUES(?,?,?) ON CONFLICT DO NOTHING`, issuer, requestID, expires)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, tx.Commit()
}

// WorkEventsPage reads only this persisted task; no runner or remote work starts.
func (s *Store) WorkEventsPage(taskID string, page, size int) ([]Event, int64, error) {
	var total int64
	if err := s.ex.QueryRow(`SELECT count(*) FROM events WHERE task_id=?`, taskID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.ex.Query(`SELECT id,task_id,ts,stream,substr(line,1,16384) FROM events WHERE task_id=? ORDER BY id LIMIT CAST(? AS BIGINT) OFFSET CAST(? AS BIGINT)`, taskID, size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Event{}
	for rows.Next() {
		var item Event
		if err := rows.Scan(&item.ID, &item.TaskID, &item.TS, &item.Stream, &item.Line); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}
