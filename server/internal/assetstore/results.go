package assetstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrAmbiguous = errors.New("同一引用有不同凭据结果，请选择明确的结果版本")

// Retain stores the exact bytes before changing any current projection. The
// digest names immutable content, not a mailbox or a mutable export filename.
func (s *Store) Retain(raw []byte) (string, error) {
	sum := sha256.Sum256(raw)
	id := hex.EncodeToString(sum[:])
	dir := filepath.Join(filepath.Dir(s.root), "asset-results")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, id+".json")
	if old, err := os.ReadFile(path); err == nil {
		if string(old) != string(raw) {
			return "", errors.New("RESULT_DIGEST_CONFLICT")
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := atomicWrite(path, raw); err != nil {
		return "", err
	}
	return id, nil
}

func atomicWrite(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".result-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (s *Store) Result(ref, digest string) (Asset, error) {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || digest != strings.ToLower(digest) {
		return Asset{}, os.ErrInvalid
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(s.root), "asset-results", digest+".json"))
	if err != nil {
		return Asset{}, err
	}
	sum := sha256.Sum256(raw)
	var a Asset
	if hex.EncodeToString(sum[:]) != digest || json.Unmarshal(raw, &a) != nil || a.Ref != ref {
		return Asset{}, errors.New("RESULT_IDENTITY_MISMATCH")
	}
	a.ResultSHA = digest
	return a, nil
}

// History exposes metadata only. Reading a list never exports credentials.
func (s *Store) History(ref string) ([]Asset, error) {
	current, err := s.List()
	if err != nil {
		return nil, err
	}
	currentIDs := map[string]bool{}
	for _, item := range current {
		currentIDs[item.ResultSHA] = true
	}
	files, err := os.ReadDir(filepath.Join(filepath.Dir(s.root), "asset-results"))
	if os.IsNotExist(err) {
		return []Asset{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Asset{}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		selectedRef := ref
		if selectedRef == "" {
			raw, readErr := os.ReadFile(filepath.Join(filepath.Dir(s.root), "asset-results", f.Name()))
			var meta Asset
			if readErr != nil || json.Unmarshal(raw, &meta) != nil || meta.Ref == "" {
				continue
			}
			selectedRef = meta.Ref
		}
		a, err := s.Result(selectedRef, strings.TrimSuffix(f.Name(), ".json"))
		if err != nil {
			continue
		}
		a.Payload = nil
		a.Current = currentIDs[a.ResultSHA]
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// OAuthDocument selects one whole credential set. It never fills a missing
// refresh token with one from another session or guesses a client ID.
func OAuthDocument(payload map[string]any, email string) (map[string]any, error) {
	var candidates []map[string]any
	var visit func(map[string]any)
	visit = func(value map[string]any) {
		if value == nil {
			return
		}
		copy := map[string]any{}
		for k, v := range value {
			copy[k] = v
		}
		for _, pair := range [][2]string{{"access_token", "accessToken"}, {"refresh_token", "refreshToken"}, {"id_token", "idToken"}} {
			if copy[pair[0]] == nil || copy[pair[0]] == "" {
				copy[pair[0]] = copy[pair[1]]
			}
		}
		if token, _ := copy["access_token"].(string); token != "" {
			candidates = append(candidates, copy)
		}
		for _, key := range []string{"payload", "credentials", "oauth", "token", "tokens"} {
			if nested, ok := value[key].(map[string]any); ok {
				visit(nested)
			}
		}
	}
	visit(payload)
	if len(candidates) == 0 {
		return nil, errors.New("CREDENTIAL_RESULT_INCOMPLETE")
	}
	chosen := candidates[0]
	for _, c := range candidates[1:] {
		for _, k := range []string{"access_token", "refresh_token", "id_token"} {
			if c[k] != chosen[k] {
				return nil, ErrAmbiguous
			}
		}
	}
	if value, _ := chosen["email"].(string); value == "" {
		chosen["email"] = email
	}
	return chosen, nil
}
