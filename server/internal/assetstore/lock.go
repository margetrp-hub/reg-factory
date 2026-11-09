package assetstore

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *Store) lock(a Asset) (func(), error) {
	identity := strings.ToLower(strings.TrimSpace(a.Email))
	if identity == "" {
		identity = strings.ToLower(documentEmail(a.Payload))
	}
	if identity == "" {
		identity = a.Ref
	}
	dir := filepath.Join(filepath.Dir(s.root), "credential-locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	name := fmt.Sprintf("%x.lock", sha256.Sum256([]byte(a.Platform+"\n"+identity)))
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("CREDENTIAL_BUSY: %w", err)
	}
	return func() { unlockFile(f); f.Close() }, nil
}
