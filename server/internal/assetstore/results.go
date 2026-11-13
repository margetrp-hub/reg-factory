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

// ReleaseSnapshot removes only a settled operation's temporary credential input.
// The task retains its digest, timestamps and outcome as ordinary metadata.
func (s *Store) ReleaseSnapshot(digest string) error {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 || digest != strings.ToLower(digest) {
		return os.ErrInvalid
	}
	err = os.Remove(filepath.Join(filepath.Dir(s.root), "asset-results", digest+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
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
	if accounts, present := payload["accounts"]; present {
		items, ok := accounts.([]any)
		if !ok || len(items) != 1 {
			return nil, ErrAmbiguous
		}
		account, ok := items[0].(map[string]any)
		if !ok {
			return nil, ErrAmbiguous
		}
		return OAuthDocument(account, email)
	}
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
	expected := strings.ToLower(strings.TrimSpace(email))
	if expected == "" {
		expected = strings.ToLower(documentEmail(payload))
	}
	if expected != "" {
		valid := []map[string]any{}
		for _, c := range candidates {
			belongs := true
			for _, key := range []string{"access_token", "id_token"} {
				claims := credentialClaims(credentialString(c, key))
				profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
				identity := credentialString(claims, "email")
				if identity == "" {
					identity = credentialString(profile, "email")
				}
				if identity != "" && strings.ToLower(identity) != expected {
					belongs = false
				}
			}
			if belongs {
				valid = append(valid, c)
			}
		}
		candidates = valid
	}
	if len(candidates) == 0 {
		return nil, errors.New("CREDENTIAL_RESULT_INCOMPLETE")
	}
	// An AT-only web session cannot replace a complete OAuth grant. Select the
	// complete bundle as a whole; never borrow its RT for a different AT.
	complete := []map[string]any{}
	for _, candidate := range candidates {
		if credentialString(candidate, "refresh_token") != "" {
			complete = append(complete, candidate)
		}
	}
	if len(complete) > 0 {
		candidates = complete
	}
	chosen := candidates[0]
	for _, c := range candidates[1:] {
		equal := true
		for _, k := range []string{"access_token", "refresh_token", "id_token"} {
			if c[k] != chosen[k] {
				equal = false
			}
		}
		if equal {
			continue
		}
		before, after := credentialClaims(credentialString(chosen, "access_token")), credentialClaims(credentialString(c, "access_token"))
		left, _ := before["iat"].(float64)
		right, _ := after["iat"].(float64)
		oldSub := credentialString(credentialClaims(credentialString(chosen, "id_token")), "sub")
		newSub := credentialString(credentialClaims(credentialString(c, "id_token")), "sub")
		if oldSub == "" {
			oldSub = credentialString(before, "sub")
		}
		if newSub == "" {
			newSub = credentialString(after, "sub")
		}
		if left == 0 || right == 0 || left == right || oldSub == "" || oldSub != newSub {
			return nil, ErrAmbiguous
		}
		if right > left {
			chosen = c
		}
	}
	for _, key := range []string{"payload", "credentials", "oauth", "token", "tokens"} {
		delete(chosen, key)
	}
	if value, _ := chosen["email"].(string); value == "" {
		chosen["email"] = email
	}
	// Workspace metadata scopes this whole bundle; never borrow another token.
	if credentialString(chosen, "account_id") == "" {
		for _, key := range []string{"workspace_account_id", "chatgpt_account_id", "account_id"} {
			value := credentialString(chosen, key)
			if value == "" {
				value = credentialString(payload, key)
			}
			if value != "" {
				chosen["account_id"] = value
				break
			}
		}
	}
	return chosen, nil
}
