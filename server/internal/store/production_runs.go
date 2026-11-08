package store

import (
	"database/sql"
	"errors"
	"strings"
)

func (s *Store) EnsureProductionRuns() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS production_materials (
 context_sha TEXT PRIMARY KEY, envelope TEXT NOT NULL, expires_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS production_runs (
 operation_id TEXT PRIMARY KEY, material TEXT NOT NULL, email TEXT NOT NULL DEFAULT '',
 input_artifact TEXT NOT NULL DEFAULT '', output_artifact TEXT NOT NULL DEFAULT '',
 source_ref TEXT NOT NULL DEFAULT '', source_sha TEXT NOT NULL DEFAULT '',
 stage TEXT NOT NULL, status TEXT NOT NULL, error_code TEXT NOT NULL DEFAULT '',
 external_started INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL,
 next_at INTEGER NOT NULL DEFAULT 0, oauth_attempts INTEGER NOT NULL DEFAULT 0, stop_requested INTEGER NOT NULL DEFAULT 0,
 stop_effect TEXT NOT NULL DEFAULT '', stop_observed_at INTEGER NOT NULL DEFAULT 0,
 retry_attempts INTEGER NOT NULL DEFAULT 0, oauth_retry_base INTEGER NOT NULL DEFAULT 0,
 invite_attempts INTEGER NOT NULL DEFAULT 0, invite_observed_at INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS production_stops(operation_id TEXT PRIMARY KEY, request_sha TEXT NOT NULL, stopped_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS production_outputs(operation_id TEXT NOT NULL, attempt INTEGER NOT NULL, artifact TEXT NOT NULL,
 created_at INTEGER NOT NULL, PRIMARY KEY(operation_id,attempt));`)
	if err != nil {
		return err
	}
	// Confirmed stop absence releases an execution claim, never the retained
	// result. Upgrade the old index atomically so concurrent claims stay unique.
	var indexSQL string
	err = s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='production_run_email'`).Scan(&indexSQL)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if strings.Contains(indexSQL, "stop_requested=1") {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`DROP INDEX IF EXISTS production_run_email;
 CREATE UNIQUE INDEX production_run_email ON production_runs(email)
 WHERE email<>'' AND NOT (status='cancelled' AND stop_effect='none' AND stop_requested=1);`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProductionMaterial(scope string) (string, error) {
	var raw string
	err := s.db.QueryRow(`SELECT envelope FROM production_materials WHERE context_sha=?`, scope).Scan(&raw)
	return raw, err
}

func (s *Store) SaveProductionMaterial(scope, raw string, expiry int64) error {
	_, err := s.db.Exec(`INSERT INTO production_materials(context_sha,envelope,expires_at) VALUES(?,?,?)
 ON CONFLICT(context_sha) DO NOTHING`, scope, raw, expiry)
	if err != nil {
		return err
	}
	saved, err := s.ProductionMaterial(scope)
	if err == nil && saved != raw {
		return errors.New("PRODUCTION_MATERIAL_CHANGED")
	}
	return err
}

func (s *Store) ProductionDBPath() (string, error) {
	var seq int
	var name, path string
	err := s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path)
	return path, err
}

func (s *Store) PendingProductions(stamp int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT operation_id FROM production_runs WHERE (status IN ('queued','running','unknown') OR (stop_requested=1 AND stop_effect=''))
 AND next_at<=? ORDER BY updated_at LIMIT 20`, stamp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) CompleteProductionRun(id string) error {
	_, err := s.db.Exec(`UPDATE production_runs SET status='delivered' WHERE operation_id=? AND stop_requested=0`, id)
	return err
}

// Panel recovery never replaces the account or discards an issued result.
func (s *Store) ControlProduction(id, action string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stage, status string
	err = tx.QueryRow(`SELECT stage,status FROM production_runs WHERE operation_id=?`, id).Scan(&stage, &status)
	if err != nil {
		return err
	}
	if status == "delivered" || status == "cancelled" || status == "failed" {
		return errors.New("PRODUCTION_ALREADY_FINISHED")
	}
	if action == "continue" {
		if status != "waiting_user" && status != "unknown" {
			return errors.New("PRODUCTION_NOT_WAITING")
		}
		// Preserve phase and lifetime OAuth attempt numbers (output archive keys).
		// Reset only the bounded retry window, not the original account or results.
		_, err = tx.Exec(`UPDATE production_runs SET status='queued',error_code='',next_at=0,retry_attempts=0,oauth_retry_base=oauth_attempts,invite_attempts=0 WHERE operation_id=?`, id)
		if err == nil {
			_, err = tx.Exec(`UPDATE production_admissions SET state='queued',error_code='',finished_at='' WHERE operation_id=?`, id)
		}
		if err == nil {
			_, err = tx.Exec(`UPDATE tasks SET status='queued',error='',finished_at='' WHERE id=?`, "work-"+id)
		}
	} else {
		return errors.New("PRODUCTION_CONTROL_INVALID")
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)`, "work-"+id, now(), stageLine(StageEvent{Stage: "manual_" + action, Status: "queued", Action: "节点管理员处理原账号；未创建新生产单"}))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProductionRunSideEffectFree(id string) bool {
	var started int
	err := s.db.QueryRow(`SELECT external_started FROM production_runs WHERE operation_id=?`, id).Scan(&started)
	return err == nil && started == 0
}

func (s *Store) HasProductionRun(id string) bool {
	var n int
	return s.db.QueryRow(`SELECT 1 FROM production_runs WHERE operation_id=?`, id).Scan(&n) == nil
}

func (s *Store) ProductionStopped(id string) bool {
	var n int
	return s.db.QueryRow(`SELECT 1 FROM production_stops WHERE operation_id=?`, id).Scan(&n) == nil
}

func (s *Store) StopProduction(id, scope string, stamp int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	if err = tx.QueryRow(`SELECT request_sha FROM production_admissions WHERE operation_id=?`, id).Scan(&old); err != nil {
		return err
	}
	if old != scope {
		return errors.New("PRODUCTION_REQUEST_CHANGED")
	}
	var previous string
	if err = tx.QueryRow(`SELECT request_sha FROM production_stops WHERE operation_id=?`, id).Scan(&previous); err == nil && previous != scope {
		return errors.New("PRODUCTION_REQUEST_CHANGED")
	} else if err != nil && err != sql.ErrNoRows {
		return err
	}
	if previous == scope {
		return tx.Commit()
	}
	_, err = tx.Exec(`INSERT INTO production_stops(operation_id,request_sha,stopped_at) VALUES(?,?,?) ON CONFLICT(operation_id) DO NOTHING`, id, scope, stamp)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE production_runs SET stop_requested=1,status='cancelled',error_code='PRODUCTION_ADMIN_STOPPED',next_at=0 WHERE operation_id=?`, id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE production_admissions SET state='cancelled',error_code='PRODUCTION_ADMIN_STOPPED',finished_at=? WHERE operation_id=?`, now(), id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE tasks SET status='cancelled',error='PRODUCTION_ADMIN_STOPPED',finished_at=? WHERE id=?`, now(), "work-"+id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProductionStopEffect(id string) (string, string) {
	var effect, email string
	err := s.db.QueryRow(`SELECT stop_effect,email FROM production_runs WHERE operation_id=?`, id).Scan(&effect, &email)
	if err != nil && s.ProductionStopped(id) {
		return "none", ""
	}
	return effect, email
}

func (s *Store) DeferProductionRun(id, code string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE production_runs SET status='unknown',error_code=?,next_at=strftime('%s','now')+30 WHERE operation_id=? AND stop_requested=0 AND status<>'delivered'`, code, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return tx.Commit()
	}
	if _, err = tx.Exec(`UPDATE production_admissions SET state='unknown',error_code=?,finished_at=? WHERE operation_id=?`, code, now(), id); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE tasks SET status='unknown',error=?,finished_at=? WHERE id=?`, code, now(), "work-"+id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.Stage("work-"+id, StageEvent{Stage: "result_export", Status: "unknown", Code: code, Action: "原始凭据已保留，自动重试导出；管理员可终止本项"})
}
