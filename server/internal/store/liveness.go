package store

import (
	"encoding/json"
	"fmt"
	"time"
)

// ScheduleLivenessCheck snapshots current identities and accepts their AT work
// in one transaction. It never walks credential files or creates a zero-item job.
func (s *Store) ScheduleLivenessCheck(at time.Time, interval time.Duration) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err = s.ex.LockKey(tx, "liveness:chatgpt"); err != nil {
		return "", err
	}
	var active int
	var latest string
	err = s.ex.QueryRowTx(tx, `SELECT COALESCE(MAX(created_at),''),COALESCE(SUM(CASE WHEN status IN ('queued','running','stopping') THEN 1 ELSE 0 END),0)
 FROM tasks WHERE actor='scheduler' AND script=?
 AND `+s.ex.JSONText("args_json", "platform")+`='chatgpt'`, CredentialCheckScript).Scan(&latest, &active)
	if err != nil {
		return "", err
	}
	if active > 0 {
		return "", nil
	}
	if latest != "" {
		last, parseErr := time.Parse(time.RFC3339, latest)
		if parseErr != nil {
			return "", parseErr
		}
		if at.Before(last.Add(interval)) {
			return "", nil
		}
	}
	rows, err := s.ex.QueryTx(tx, accountProjectionSQL+`SELECT ref,identity,platform FROM account_projection p
 WHERE platform='chatgpt' AND identity_leased=0
 AND NOT EXISTS (SELECT 1 FROM account_refreshes r JOIN tasks t ON t.id=r.task_id
 JOIN accounts a ON a.ref=r.ref WHERE t.status IN ('queued','running','stopping')
 AND a.platform=p.platform AND (a.ref=p.ref OR (trim(p.identity)<>'' AND lower(trim(a.identity))=lower(trim(p.identity)))))
 AND NOT EXISTS (SELECT 1 FROM credential_check_items i JOIN tasks t ON t.id=i.task_id
 JOIN accounts a ON a.ref=i.ref WHERE t.status IN ('queued','running','stopping')
 AND a.platform=p.platform AND (a.ref=p.ref OR (trim(p.identity)<>'' AND lower(trim(a.identity))=lower(trim(p.identity)))))
 ORDER BY ref`)
	if err != nil {
		return "", err
	}
	accounts := []Account{}
	refs := []string{}
	for rows.Next() {
		var account Account
		if err = rows.Scan(&account.Ref, &account.Identity, &account.Platform); err != nil {
			rows.Close()
			return "", err
		}
		accounts = append(accounts, account)
		refs = append(refs, account.Ref)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(accounts) == 0 {
		return "", err
	}
	args, err := json.Marshal(map[string]any{"platform": "chatgpt", "refs": refs})
	if err != nil {
		return "", err
	}
	task := Task{ID: fmt.Sprintf("liveness-chatgpt-%d", at.UnixNano()), Script: CredentialCheckScript,
		Title: fmt.Sprintf("周期 AT 巡检 · %d 个账号", len(accounts)), ArgsJSON: string(args),
		Actor: "scheduler", CreatedAt: at.UTC().Format(time.RFC3339)}
	if err = createCredentialCheck(s, tx, task, accounts); err != nil {
		return "", err
	}
	return task.ID, tx.Commit()
}

// Other platforms keep their scanner, without overlapping an accepted scan.
func (s *Store) HasActiveLivenessScan(platform string) (bool, error) {
	var count int
	err := s.exRO.QueryRow(`SELECT COUNT(*) FROM tasks WHERE actor='scheduler' AND script='asset_ops'
 AND status IN ('queued','running','stopping')
 AND `+s.ex.JSONText("args_json", "0")+`='scan' AND `+s.ex.JSONText("args_json", "2")+`=?`, platform).Scan(&count)
	return count > 0, err
}
