package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// One projection for listing, pagination, panel feedback and signed references.
// Custody and production history outrank credential health, including revoked stock.
const activeAccountRefreshSQL = `EXISTS (SELECT 1 FROM account_refreshes r JOIN tasks t ON t.id=r.task_id
 JOIN accounts source ON source.ref=r.ref
 WHERE (r.ref=accounts.ref OR (source.platform=accounts.platform AND trim(accounts.identity)<>'' AND lower(trim(source.identity))=lower(trim(accounts.identity))))
 AND t.status IN ('queued','running','stopping'))`

const accountLibraryStatusSQL = `CASE
 WHEN claimed=1 OR join_status IN ('joined','switched')
 OR EXISTS (SELECT 1 FROM inventory_relay_bindings b WHERE b.account_ref=accounts.ref)
 OR EXISTS (SELECT 1 FROM production_results p WHERE p.account_ref=accounts.ref OR p.source_ref=accounts.ref) THEN 'used'
 WHEN ` + activeAccountRefreshSQL + ` THEN 'leased'
 WHEN oauth_status='failed' OR liveness_status IN ('dead','banned','workspace_deactivated','invalid') THEN 'invalid'
 WHEN oauth_status='completed' AND liveness_status='alive' AND join_status='none' THEN 'available'
 ELSE 'pending' END`

var ErrCheckStale = errors.New("credential_changed_during_check")

// recordATHealth writes only evidence for the tested version. An in-flight or
// uncertain RT exchange retains its lock even when the previous AT still works.
func (s *Store) recordATHealth(tx *sql.Tx, ref, status, message, checkedAt string, version int) (bool, error) {
	liveness := "unknown"
	switch status {
	case "ok":
		liveness = "alive"
	case "needs_refresh", "expired", "relogin_required":
		liveness = "needs_refresh"
	case "banned", "workspace_deactivated", "invalid":
		liveness = status
	}
	result, err := s.ex.ExecTxArgs(tx, `UPDATE accounts SET status=?,health_checked_at=?,health_message=?,updated_at=?,
 oauth_status=CASE WHEN ?='ok' AND kind='oauth' THEN 'completed' ELSE oauth_status END,
 join_status=CASE WHEN join_status='' AND kind='oauth' THEN 'none' ELSE join_status END,
 liveness_status=?,liveness_checked_at=?
 WHERE ref=? AND (?=0 OR version=?) AND NOT (`+activeAccountRefreshSQL+`)`,
		status, checkedAt, RedactLog(message), now(), status, liveness, checkedAt, ref, version, version)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *Store) MarkATHealth(ref, status, message string, version int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	written, err := s.recordATHealth(tx, ref, status, message, now(), version)
	if err != nil {
		return err
	}
	if !written {
		return ErrCheckStale
	}
	return tx.Commit()
}

func RefreshVerificationID(taskID string) string { return "check-" + taskID }

// The next read-only action is committed with the new credential projection;
// closing the browser or restarting the service cannot lose it.
func prepareRefreshedAccount(s *Store, tx *sql.Tx, taskID, ref string) error {
	result, err := s.ex.ExecTxArgs(tx, `UPDATE accounts SET kind='oauth',oauth_status='completed',
 status='unknown',health_checked_at='',health_message='凭据已更新，自动测试 AT 已排队',
 liveness_status='unchecked',liveness_checked_at='',
 join_status=CASE WHEN join_status='' THEN 'none' ELSE join_status END,version=version+1
 WHERE ref=? AND platform='chatgpt' AND claimed=0`, ref)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	id := RefreshVerificationID(taskID)
	args, err := json.Marshal(map[string]any{"platform": "chatgpt", "refs": []string{ref}, "parent_id": taskID})
	if err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO tasks(id,script,title,args_json,status,created_at,actor)
 VALUES(?,?,'凭据更新后 AT 测试',?,'queued',?,'system')`, id, CredentialCheckScript, string(args), now()); err != nil {
		return err
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO credential_check_items(task_id,ordinal,ref,identity,mode)
 SELECT ?,0,ref,identity,'at' FROM accounts WHERE ref=?`, id, ref); err != nil {
		return err
	}
	_, err = s.ex.ExecTxArgs(tx, `INSERT INTO events(task_id,ts,stream,line) VALUES(?,?,'system',?)`, taskID, now(), "凭据已保存；自动 AT 测试排队："+id)
	return err
}
