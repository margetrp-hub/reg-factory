package store

import (
	"database/sql"
	"errors"
	"strconv"
	"time"
)

// Production retains one frozen output for one Core slot. Credentials are
// sealed with the node artifact key; task views contain references only.
type Production struct {
	OperationID, RequestSHA, Parameters, SourceKey, SourceRef, SourceSHA string
	AccountRef, Email, Artifact, State, Result, FinishedAt               string
	CheckedAt                                                            int64
}

func (s *Store) EnsureProduction() error {
	err := s.ex.ExecEach(sqlEnsureProduction)
	if err != nil {
		return err
	}
	return s.EnsureProductionRuns()
}

func (s *Store) Production(id string) (p Production, err error) {
	err = s.ex.QueryRow(`SELECT operation_id,request_sha,parameters,source_key,source_ref,source_sha,
 account_ref,email,artifact,state,result,checked_at,finished_at FROM production_results WHERE operation_id=?`, id).Scan(
		&p.OperationID, &p.RequestSHA, &p.Parameters, &p.SourceKey, &p.SourceRef, &p.SourceSHA,
		&p.AccountRef, &p.Email, &p.Artifact, &p.State, &p.Result, &p.CheckedAt, &p.FinishedAt)
	return
}

// Admission is durable before opening or encrypting a source. A configuration
// failure still has an original identity, visible task and append-only events.
func (s *Store) ReserveProductionAdmission(id, scope, parameters string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO production_admissions(operation_id,request_sha,parameters)
 VALUES(?,?,?) ON CONFLICT(operation_id) DO NOTHING`, id, scope, parameters); err != nil {
		return err
	}
	var old string
	if err = s.ex.QueryRowTx(tx, `SELECT request_sha FROM production_admissions WHERE operation_id=?`, id).Scan(&old); err != nil {
		return err
	}
	if old != scope {
		return errors.New("PRODUCTION_REQUEST_CHANGED")
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO tasks(id,script,title,args_json,status,created_at,actor)
 VALUES(?,'production.team_json','TEAM JSON',?,'unknown',?,'work') ON CONFLICT(id) DO NOTHING`, "work-"+id, parameters, now())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProductionAdmission(id string) (p Production, err error) {
	p.OperationID = id
	var code string
	err = s.ex.QueryRow(`SELECT request_sha,parameters,state,error_code,finished_at
 FROM production_admissions WHERE operation_id=?`, id).Scan(&p.RequestSHA, &p.Parameters, &p.State, &code, &p.FinishedAt)
	p.Result = `{"status":` + strconv.Quote(p.State) + `,"error_code":` + strconv.Quote(code) + `}`
	return
}

func (s *Store) RejectProductionAdmission(id, state, code string) error {
	if s.ProductionStopped(id) {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A concurrent successful admission always outranks a failed preparation.
	var exists int
	if err = s.ex.QueryRowTx(tx, `SELECT count(*) FROM production_results WHERE operation_id=?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	finished := ""
	if state == "failed" {
		finished = now()
	}
	action := "节点管理员修复接收配置后，Work 沿原任务恢复；不刷新凭证"
	if state == "failed" {
		action = "接收被拒绝；在生产中心查看原文件和错误，不能自动更换源文件"
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE production_admissions SET state=?,error_code=?,finished_at=? WHERE operation_id=?`, state, code, finished, id)
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE tasks SET status=?,error=?,finished_at=? WHERE id=?`, state, code, finished, "work-"+id)
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)`, "work-"+id, now(),
		stageLine(StageEvent{Stage: "admission", Status: state, Code: code, Action: action}))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProductionArtifact(ref string) (string, error) {
	var value string
	err := s.ex.QueryRow(`SELECT artifact FROM production_results WHERE account_ref=? AND state='completed'`, ref).Scan(&value)
	return value, err
}

func (s *Store) ProductionClaimed(sourceKey, sourceSHA string) (bool, error) {
	var id string
	err := s.ex.QueryRow(`SELECT operation_id FROM production_results WHERE source_key=?
 AND (state NOT IN ('failed','cancelled') OR source_sha=?) LIMIT 1`, sourceKey, sourceSHA).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) AdmitProduction(p Production) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stopped int
	if err = s.ex.QueryRowTx(tx, `SELECT count(*) FROM production_stops WHERE operation_id=?`, p.OperationID).Scan(&stopped); err != nil {
		return err
	}
	if stopped != 0 {
		// Stop blocks new execution, not custody of an output already issued by
		// this exact original run. Arbitrary new/changed artifacts remain denied.
		var artifact string
		if err = s.ex.QueryRowTx(tx, `SELECT output_artifact FROM production_runs WHERE operation_id=?`, p.OperationID).Scan(&artifact); err != nil || artifact == "" || artifact != p.Artifact {
			return errors.New("PRODUCTION_ADMIN_STOPPED")
		}
	}
	var old string
	err = s.ex.QueryRowTx(tx, `SELECT request_sha FROM production_results WHERE operation_id=?`, p.OperationID).Scan(&old)
	if err == nil {
		if old != p.RequestSHA {
			return errors.New("PRODUCTION_REQUEST_CHANGED")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO production_results(operation_id,request_sha,parameters,source_key,source_ref,source_sha,
 account_ref,email,artifact) VALUES(?,?,?,?,?,?,?,?,?)`, p.OperationID, p.RequestSHA, p.Parameters, p.SourceKey, p.SourceRef, p.SourceSHA,
		p.AccountRef, p.Email, p.Artifact)
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO work_operations(operation_id,task_id,action,input_resource_version,created_at)
 VALUES(?,?,'production.team_json',1,?)`, p.OperationID, "work-"+p.OperationID, now())
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO tasks(id,script,title,args_json,status,created_at,actor)
 VALUES(?,'production.team_json','TEAM JSON',?,'queued',?,'work')
 ON CONFLICT(id) DO UPDATE SET status='queued',error='',finished_at=''`, "work-"+p.OperationID, p.Parameters, now())
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)`, "work-"+p.OperationID, now(),
		stageLine(StageEvent{Stage: "result_saved", Status: "queued", ResultSHA: p.SourceSHA, Action: "已保留原始成品，等待只读核验"}))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// A poll only rechecks the retained output. Its short lease prevents parallel
// polls from amplifying upstream traffic and survives process restarts.
func (s *Store) ClaimProductionCheck(id string, stamp int64) (bool, error) {
	result, err := s.ex.Exec(`UPDATE production_results SET checked_at=? WHERE operation_id=?
 AND state IN ('queued','unknown') AND checked_at<=?`, stamp, id, stamp-30)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) FinishProduction(id, state, result string, stamp int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, ref, email string
	err = s.ex.QueryRowTx(tx, `SELECT state,account_ref,email FROM production_results WHERE operation_id=? AND checked_at=?`, id, stamp).Scan(&current, &ref, &email)
	if err != nil {
		return err
	}
	if current == "completed" || current == "failed" || current == "cancelled" {
		return nil
	}
	finished := ""
	if state != "unknown" {
		finished = time.Unix(stamp, 0).UTC().Format(time.RFC3339)
	}
	if state == "completed" {
		// The asset scanner may already have projected this exact saved export.
		// Preserve its file reference and flags; a colliding identity is not ours.
		var projected sql.Result
		projected, err = s.ex.ExecTxArgs(tx, `INSERT INTO accounts(ref,platform,kind,identity,secret_ref,status,created_at,updated_at)
  VALUES(?,'chatgpt','oauth',?,'production-result','normal',?,?)
  ON CONFLICT(ref) DO UPDATE SET status=excluded.status,updated_at=excluded.updated_at
  WHERE accounts.platform=excluded.platform AND lower(accounts.identity)=lower(excluded.identity)
  AND accounts.kind IN ('oauth','team')`, ref, email, now(), now())
		if err != nil {
			return err
		}
		if count, err := projected.RowsAffected(); err != nil {
			return err
		} else if count != 1 {
			return errors.New("PRODUCTION_ACCOUNT_IDENTITY_CONFLICT")
		}
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE production_results SET state=?,result=?,finished_at=? WHERE operation_id=?`, state, result, finished, id)
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE production_admissions SET state=?,error_code=?,finished_at=? WHERE operation_id=?`, state, safeProductionCode(result), finished, id)
	if err != nil {
		return err
	}
	code := 0
	if state == "failed" {
		code = 1
	}
	_, err = s.ex.ExecTxArgs(tx, `UPDATE tasks SET status=?,finished_at=?,exit_code=?,error=? WHERE id=?`, state, finished, code, safeProductionCode(result), "work-"+id)
	if err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)`, "work-"+id, now(),
		stageLine(StageEvent{Stage: "verification", Status: state, Code: safeProductionCode(result), Action: productionActionFor(state)}))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// sqlEnsureProduction 是 production_results / admissions 的建表语句。
const sqlEnsureProduction = `CREATE TABLE IF NOT EXISTS production_results (
 operation_id TEXT PRIMARY KEY, request_sha TEXT NOT NULL, parameters TEXT NOT NULL,
 source_key TEXT NOT NULL, source_ref TEXT NOT NULL, source_sha TEXT NOT NULL,
 account_ref TEXT NOT NULL UNIQUE, email TEXT NOT NULL, artifact TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'queued', result TEXT NOT NULL DEFAULT '{}',
 checked_at INTEGER NOT NULL DEFAULT 0, finished_at TEXT NOT NULL DEFAULT '',
 UNIQUE(source_key,source_sha));
 CREATE UNIQUE INDEX IF NOT EXISTS production_source_active ON production_results(source_key)
 WHERE state NOT IN ('failed','cancelled');
 CREATE TABLE IF NOT EXISTS production_admissions (
 operation_id TEXT PRIMARY KEY, request_sha TEXT NOT NULL, parameters TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'unknown', error_code TEXT NOT NULL DEFAULT 'PRODUCTION_ADMISSION_INTERRUPTED',
 finished_at TEXT NOT NULL DEFAULT '')`
