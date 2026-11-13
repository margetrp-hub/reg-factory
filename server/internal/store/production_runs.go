package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

func (s *Store) EnsureProductionRuns() error {
	err := s.ex.ExecEach(`CREATE TABLE IF NOT EXISTS production_materials (
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
	var idxDef string
	idxDef, err = s.ex.IndexSQL("production_run_email")
	indexSQL = idxDef
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
	// 拆成两次独立的 Exec：PG 下多语句兼容性差，
	// 分开执行能精确定位问题，也不依赖驱动的多语句支持。
	if _, err = s.ex.ExecTxArgs(tx, `DROP INDEX IF EXISTS production_run_email`); err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `CREATE UNIQUE INDEX production_run_email ON production_runs(email)
 WHERE email<>'' AND NOT (status='cancelled' AND stop_effect='none' AND stop_requested=1)`); err != nil {
		return err
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProductionMaterial(scope string) (string, error) {
	var raw string
	err := s.ex.QueryRow(`SELECT envelope FROM production_materials WHERE context_sha=?`, scope).Scan(&raw)
	return raw, err
}

func (s *Store) SaveProductionMaterial(scope, raw string, expiry int64) error {
	_, err := s.ex.Exec(`INSERT INTO production_materials(context_sha,envelope,expires_at) VALUES(?,?,?)
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

// ProductionDatabaseDSN is private child-process input, never a diagnostic label.
func (s *Store) ProductionDatabaseDSN() (string, error) {
	if s.workerDSN == "" {
		return "", errors.New("PRODUCTION_STORAGE_UNAVAILABLE")
	}
	return s.workerDSN, nil
}

func (s *Store) PendingProductions(stamp int64) ([]string, error) {
	rows, err := s.ex.Query(`SELECT operation_id FROM production_runs WHERE (status IN ('queued','running','unknown') OR (stop_requested=1 AND stop_effect=''))
 AND status<>'delivered' AND next_at<=? ORDER BY updated_at LIMIT 20`, stamp)
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
	_, err := s.ex.Exec(`UPDATE production_runs SET status='delivered' WHERE operation_id=?`, id)
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
	lock := ""
	if s.dialect == DialectPG {
		lock = " FOR UPDATE"
	}
	err = s.ex.QueryRowTx(tx, `SELECT stage,status FROM production_runs WHERE operation_id=?`+lock, id).Scan(&stage, &status)
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
		_, err = s.ex.ExecTxArgs(tx, `UPDATE production_runs SET status='queued',error_code='',next_at=0,retry_attempts=0,oauth_retry_base=oauth_attempts,invite_attempts=0 WHERE operation_id=?`, id)
		if err == nil {
			_, err = s.ex.ExecTxArgs(tx, `UPDATE production_admissions SET state='queued',error_code='',finished_at='' WHERE operation_id=?`, id)
		}
		if err == nil {
			_, err = s.ex.ExecTxArgs(tx, `UPDATE tasks SET status='queued',error='',finished_at='' WHERE id=?`, "work-"+id)
		}
	} else {
		return errors.New("PRODUCTION_CONTROL_INVALID")
	}
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)`, "work-"+id, now(), stageLine(StageEvent{Stage: "manual_" + action, Status: "queued", Action: "节点管理员处理原账号；未创建新生产单"}))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProductionRunSideEffectFree(id string) bool {
	var started int
	err := s.ex.QueryRow(`SELECT external_started FROM production_runs WHERE operation_id=?`, id).Scan(&started)
	return err == nil && started == 0
}

func (s *Store) HasProductionRun(id string) bool {
	var n int
	return s.ex.QueryRow(`SELECT 1 FROM production_runs WHERE operation_id=?`, id).Scan(&n) == nil
}

func (s *Store) ProductionStopped(id string) bool {
	var n int
	return s.ex.QueryRow(`SELECT 1 FROM production_stops WHERE operation_id=?`, id).Scan(&n) == nil
}

func (s *Store) StopProduction(id, scope string, stamp int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var old string
	lock := ""
	if s.dialect == DialectPG {
		lock = " FOR UPDATE"
	}
	if err = s.ex.QueryRowTx(tx, `SELECT request_sha FROM production_admissions WHERE operation_id=?`+lock, id).Scan(&old); err != nil {
		return err
	}
	if old != scope {
		return errors.New("PRODUCTION_REQUEST_CHANGED")
	}
	var previous string
	if err = s.ex.QueryRowTx(tx, `SELECT request_sha FROM production_stops WHERE operation_id=?`, id).Scan(&previous); err == nil && previous != scope {
		return errors.New("PRODUCTION_REQUEST_CHANGED")
	} else if err != nil && err != sql.ErrNoRows {
		return err
	}
	if previous == scope {
		return tx.Commit()
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO production_stops(operation_id,request_sha,stopped_at) VALUES(?,?,?) ON CONFLICT(operation_id) DO NOTHING`, id, scope, stamp)
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE production_runs SET stop_requested=1,
 status=CASE WHEN status='delivered' THEN status ELSE 'running' END,
 error_code=CASE WHEN status='delivered' THEN error_code ELSE 'PRODUCTION_STOP_REQUESTED' END,next_at=0 WHERE operation_id=?`, id)
	if err != nil {
		return err
	}
	state, code, finished := "running", "PRODUCTION_STOP_REQUESTED", ""
	var runExists int
	if err = s.ex.QueryRowTx(tx, `SELECT count(*) FROM production_runs WHERE operation_id=?`, id).Scan(&runExists); err != nil {
		return err
	}
	if runExists == 0 {
		state, code, finished = "cancelled", "PRODUCTION_ADMIN_STOPPED", now()
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE production_admissions SET state=?,error_code=?,finished_at=? WHERE operation_id=? AND state<>'completed'`, state, code, finished, id)
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE tasks SET status=?,error=?,finished_at=? WHERE id=? AND status<>'completed'`, state, code, finished, "work-"+id)
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)`, "work-"+id, now(),
		stageLine(StageEvent{Stage: "stop_requested", Status: state, Code: code, Action: "已收到终止请求；等待当前步骤结束，保留产物并核对最终结果"}))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProductionStopEffect(id string) (string, string) {
	var effect, email string
	err := s.ex.QueryRow(`SELECT stop_effect,email FROM production_runs WHERE operation_id=?`, id).Scan(&effect, &email)
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
	result, err := s.ex.ExecTxArgs(tx, `UPDATE production_runs SET status='unknown',error_code=?,next_at=? WHERE operation_id=? AND status<>'delivered'`, code, time.Now().Unix()+30, id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return tx.Commit()
	}
	if _, err = s.ex.ExecTxArgs(tx, `UPDATE production_admissions SET state='unknown',error_code=?,finished_at=? WHERE operation_id=?`, code, now(), id); err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `UPDATE tasks SET status='unknown',error=?,finished_at=? WHERE id=?`, code, now(), "work-"+id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.Stage("work-"+id, StageEvent{Stage: "result_export", Status: "unknown", Code: code, Action: "原始凭据已保留，自动重试导出；管理员可终止本项"})
}
