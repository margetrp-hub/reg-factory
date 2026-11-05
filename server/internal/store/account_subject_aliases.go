package store

import (
	"fmt"
	"strings"
)

// BindAccountSubject records a proven historical Core subject without changing
// the source account or its version. Existing aliases cannot be repointed.
func (s *Store) BindAccountSubject(subject, ref, evidence string) error {
	if strings.TrimSpace(subject) == "" || len(evidence) != 64 {
		return fmt.Errorf("invalid account subject evidence")
	}
	if _, err := s.GetAccount(ref); err != nil {
		return err
	}
	if account, err := s.GetAccount(subject); err == nil && account.Ref != ref {
		return fmt.Errorf("account subject conflicts with canonical ref")
	}
	_, err := s.db.Exec(`INSERT INTO account_subject_aliases(subject_id, account_ref, evidence_sha256, created_at)
		VALUES(?,?,?,?) ON CONFLICT(subject_id) DO NOTHING`, subject, ref, evidence, now())
	if err != nil {
		return err
	}
	bound, err := s.AccountSubjectRef(subject)
	if err != nil {
		return err
	}
	if bound != ref {
		return fmt.Errorf("account subject alias conflict")
	}
	return nil
}

func (s *Store) AccountSubjectRef(subject string) (string, error) {
	var ref string
	err := s.db.QueryRow(`SELECT a.ref FROM account_subject_aliases x JOIN accounts a ON a.ref=x.account_ref WHERE x.subject_id=?`, subject).Scan(&ref)
	return ref, err
}
