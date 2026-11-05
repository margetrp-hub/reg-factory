package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// RecoverLegacyQuota closes only read-only reservations created by the old
// synchronous ACK path. No observation was saved, so never invent success.
func (s *Store) RecoverLegacyQuota() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now()
	_, err = tx.Exec(`INSERT INTO tasks(id,script,title,args_json,status,created_at,finished_at,exit_code,error,actor)
		SELECT o.task_id,'account.quota','Work quota recovery','{}','cancelled',o.created_at,?,-1,'WORK_QUOTA_LEGACY_CANCELLED','work:recovery'
		FROM work_operations o LEFT JOIN tasks t ON t.id=o.task_id
		WHERE o.action='account.quota' AND t.id IS NULL`, stamp)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE work_operations SET completed_at=(SELECT finished_at FROM tasks WHERE id=work_operations.task_id)
		WHERE action='account.quota' AND completed_at='' AND task_id IN (SELECT id FROM tasks WHERE script='account.quota' AND status='cancelled')`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// RecordQuotaObservation atomically saves the read-only snapshot and terminal
// task before acknowledging Work. A replay cannot reread inventory or change it.
func (s *Store) RecordQuotaObservation(operationID, taskID, platform string, inputVersion int) error {
	if operationID == "" || taskID == "" {
		return fmt.Errorf("quota scope missing")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var priorTask, priorAction string
	err = tx.QueryRow(`SELECT task_id,action FROM work_operations WHERE operation_id=?`, operationID).Scan(&priorTask, &priorAction)
	if err == nil {
		if priorTask != taskID || priorAction != "account.quota" {
			return fmt.Errorf("quota operation scope conflict")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	var available int
	if err = tx.QueryRow(`SELECT count(*) FROM accounts WHERE status='ok' AND (?='' OR platform=?)`, platform, platform).Scan(&available); err != nil {
		return err
	}
	snapshot, err := json.Marshal(map[string]any{"platform": platform, "available": available})
	if err != nil {
		return err
	}
	stamp := now()
	_, err = tx.Exec(`INSERT INTO work_operations(operation_id,task_id,action,input_resource_version,created_at,completed_at) VALUES(?,?,'account.quota',?,?,?)`, operationID, taskID, inputVersion, stamp, stamp)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO tasks(id,script,title,args_json,status,created_at,started_at,finished_at,exit_code,actor)
		VALUES(?,'account.quota','Work quota observation',?,'succeeded',?,?,?,0,'work:quota')`, taskID, string(snapshot), stamp, stamp, stamp)
	if err != nil {
		return err
	}
	return tx.Commit()
}
