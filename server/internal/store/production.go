package store

import (
	"database/sql"
	"errors"
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
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS production_results (
 operation_id TEXT PRIMARY KEY, request_sha TEXT NOT NULL, parameters TEXT NOT NULL,
 source_key TEXT NOT NULL, source_ref TEXT NOT NULL, source_sha TEXT NOT NULL,
 account_ref TEXT NOT NULL UNIQUE, email TEXT NOT NULL, artifact TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'queued', result TEXT NOT NULL DEFAULT '{}',
 checked_at INTEGER NOT NULL DEFAULT 0, finished_at TEXT NOT NULL DEFAULT '',
 UNIQUE(source_key,source_sha));
 CREATE UNIQUE INDEX IF NOT EXISTS production_source_active ON production_results(source_key)
 WHERE state NOT IN ('failed','cancelled')`)
	return err
}

func (s *Store) Production(id string) (p Production, err error) {
	err = s.db.QueryRow(`SELECT operation_id,request_sha,parameters,source_key,source_ref,source_sha,
 account_ref,email,artifact,state,result,checked_at,finished_at FROM production_results WHERE operation_id=?`, id).Scan(
		&p.OperationID, &p.RequestSHA, &p.Parameters, &p.SourceKey, &p.SourceRef, &p.SourceSHA,
		&p.AccountRef, &p.Email, &p.Artifact, &p.State, &p.Result, &p.CheckedAt, &p.FinishedAt)
	return
}

func (s *Store) ProductionArtifact(ref string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT artifact FROM production_results WHERE account_ref=? AND state='completed'`, ref).Scan(&value)
	return value, err
}

func (s *Store) ProductionClaimed(sourceKey, sourceSHA string) (bool, error) {
	var id string
	err := s.db.QueryRow(`SELECT operation_id FROM production_results WHERE source_key=?
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
	var old string
	err = tx.QueryRow(`SELECT request_sha FROM production_results WHERE operation_id=?`, p.OperationID).Scan(&old)
	if err == nil {
		if old != p.RequestSHA {
			return errors.New("PRODUCTION_REQUEST_CHANGED")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.Exec(`INSERT INTO production_results(operation_id,request_sha,parameters,source_key,source_ref,source_sha,
 account_ref,email,artifact) VALUES(?,?,?,?,?,?,?,?,?)`, p.OperationID, p.RequestSHA, p.Parameters, p.SourceKey, p.SourceRef, p.SourceSHA,
		p.AccountRef, p.Email, p.Artifact)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO work_operations(operation_id,task_id,action,input_resource_version,created_at)
 VALUES(?,?,'production.team_json',1,?)`, p.OperationID, "work-"+p.OperationID, now())
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO tasks(id,script,title,args_json,status,created_at,actor)
 VALUES(?,'production.team_json','TEAM JSON',?,'queued',?,'work')`, "work-"+p.OperationID, p.Parameters, now())
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)`, "work-"+p.OperationID, now(),
		stageLine(StageEvent{Stage: "result_saved", Status: "queued", ResultSHA: p.SourceSHA, Action: "已保留原始成品，等待只读核验"}))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// A poll only rechecks the retained output. Its short lease prevents parallel
// polls from amplifying upstream traffic and survives process restarts.
func (s *Store) ClaimProductionCheck(id string, stamp int64) (bool, error) {
	result, err := s.db.Exec(`UPDATE production_results SET checked_at=? WHERE operation_id=?
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
	err = tx.QueryRow(`SELECT state,account_ref,email FROM production_results WHERE operation_id=? AND checked_at=?`, id, stamp).Scan(&current, &ref, &email)
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
		_, err = tx.Exec(`INSERT INTO accounts(ref,platform,kind,identity,secret_ref,status,created_at,updated_at)
  VALUES(?,'chatgpt','oauth',?,'production-result','normal',?,?)`, ref, email, now(), now())
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE production_results SET state=?,result=?,finished_at=? WHERE operation_id=?`, state, result, finished, id)
	if err != nil {
		return err
	}
	code := 0
	if state == "failed" {
		code = 1
	}
	_, err = tx.Exec(`UPDATE tasks SET status=?,finished_at=?,exit_code=?,error=? WHERE id=?`, state, finished, code, safeProductionCode(result), "work-"+id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'stage',?)`, "work-"+id, now(),
		stageLine(StageEvent{Stage: "verification", Status: state, Code: safeProductionCode(result), Action: productionActionFor(state)}))
	if err != nil {
		return err
	}
	return tx.Commit()
}
