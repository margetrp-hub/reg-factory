package store

import "strings"

const accountNotTransferredSQL = `NOT EXISTS(SELECT 1 FROM asset_transferred t WHERE t.ref=accounts.ref OR
 (t.email<>'' AND t.platform=accounts.platform AND t.email=lower(trim(accounts.identity))))`

func (s *Store) initSharing() error {
	_, err := s.ex.Exec(`CREATE TABLE IF NOT EXISTS asset_sharing(source_ref TEXT PRIMARY KEY,record_ciphertext TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS central_received_assets(task_id TEXT PRIMARY KEY,account_id TEXT NOT NULL,revision INTEGER NOT NULL,record_ciphertext TEXT NOT NULL,received_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS asset_transferred(ref TEXT PRIMARY KEY,platform TEXT NOT NULL,email TEXT NOT NULL);`)
	return err
}

// Withdrawal records are idempotency tombstones, never usage/eligibility facts.
func (s *Store) WithdrawAccount(ref string) error {
	_, err := s.ex.Exec(`INSERT INTO asset_transferred(ref,platform,email)
      SELECT ref,platform,lower(trim(identity)) FROM accounts WHERE ref=?
      ON CONFLICT(ref) DO NOTHING`, ref)
	return err
}
func (s *Store) TransferredAccount(platform, email string) (bool, error) {
	var n int
	err := s.exRO.QueryRow(`SELECT count(*) FROM asset_transferred WHERE platform=? AND email=?`, platform, strings.ToLower(strings.TrimSpace(email))).Scan(&n)
	return n > 0, err
}
func (s *Store) TransferredRef(ref string) (bool, error) {
	var n int
	err := s.exRO.QueryRow(`SELECT count(*) FROM asset_transferred t WHERE t.ref=? OR EXISTS
      (SELECT 1 FROM accounts a WHERE a.ref=? AND t.email<>'' AND t.email=lower(trim(a.identity)) AND t.platform=a.platform)`, ref, ref).Scan(&n)
	return n > 0, err
}
func (s *Store) TransferredIdentity(ref string) (platform, email string, err error) {
	err = s.exRO.QueryRow(`SELECT platform,email FROM asset_transferred WHERE ref=?`, ref).Scan(&platform, &email)
	return
}
