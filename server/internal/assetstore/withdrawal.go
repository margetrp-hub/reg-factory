package assetstore

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Withdrawal only retires private availability. Retained files are audit
// evidence; the center's subsequent tasks carry their own authorized material.
func (s *Store) withdrawalPath(key string) string {
	return filepath.Join(filepath.Dir(s.root), "asset-withdrawals", fmt.Sprintf("%x", sha256.Sum256([]byte(key))))
}
func (s *Store) withdrawn(a Asset) bool {
	keys := []string{"ref:" + a.Ref}
	if a.Email != "" {
		keys = append(keys, a.Platform+":"+strings.ToLower(strings.TrimSpace(a.Email)))
	}
	for _, key := range keys {
		_, err := os.Stat(s.withdrawalPath(key))
		if !os.IsNotExist(err) {
			return true
		}
	}
	return false
}
func (s *Store) Withdraw(ref, platform, email string) error {
	keys := []string{"ref:" + ref}
	if email != "" {
		keys = append(keys, platform+":"+strings.ToLower(strings.TrimSpace(email)))
	}
	for _, key := range keys {
		path := s.withdrawalPath(key)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
