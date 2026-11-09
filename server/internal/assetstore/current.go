package assetstore

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func credentialString(doc map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := doc[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func credentialClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err != nil || json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

// Claims only disambiguate already-owned local records; they never authorize a request.
func credentialScope(a Asset) [2]string {
	doc, _ := OAuthDocument(a.Payload, a.Email)
	if doc == nil {
		doc = a.Payload
	}
	at := credentialClaims(credentialString(doc, "access_token"))
	id := credentialClaims(credentialString(doc, "id_token"))
	auth, _ := at["https://api.openai.com/auth"].(map[string]any)
	workspace := credentialString(doc, "workspace_account_id", "chatgpt_account_id", "account_id")
	if workspace == "" {
		workspace = credentialString(auth, "chatgpt_account_id")
	}
	subject := credentialString(id, "sub")
	if subject == "" {
		subject = credentialString(at, "sub")
	}
	if subject == "" {
		subject = credentialString(doc, "credential_subject", "chatgpt_user_id", "user_id")
	}
	return [2]string{workspace, subject}
}

func credentialTime(a Asset) time.Time {
	if ts, err := time.Parse(time.RFC3339Nano, a.CredentialUpdatedAt); err == nil {
		return ts
	}
	doc, _ := OAuthDocument(a.Payload, a.Email)
	claims := credentialClaims(credentialString(doc, "access_token"))
	if iat, ok := claims["iat"].(float64); ok && iat > 0 {
		return time.Unix(int64(iat), 0).UTC()
	}
	return time.Time{}
}

func credentialComplete(a Asset) bool {
	doc, err := OAuthDocument(a.Payload, a.Email)
	return err == nil && credentialString(doc, "refresh_token") != ""
}

func credentialEqual(a, b Asset) bool {
	left, _ := OAuthDocument(a.Payload, a.Email)
	right, _ := OAuthDocument(b.Payload, b.Email)
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		if credentialString(left, key) != credentialString(right, key) {
			return false
		}
	}
	return true
}

func newestCredential(items []Asset) (Asset, error) {
	selected := items[0]
	for _, a := range items[1:] {
		if credentialTime(a).After(credentialTime(selected)) {
			selected = a
		}
	}
	for _, a := range items {
		if credentialTime(a).Equal(credentialTime(selected)) && !credentialEqual(a, selected) {
			return Asset{}, ErrAmbiguous
		}
	}
	return selected, nil
}

func (s *Store) current(ref string, all []Asset) (Asset, error) {
	var original []Asset
	for _, a := range all {
		if a.Ref == ref {
			original = append(original, a)
		}
	}
	if len(original) == 0 {
		return Asset{}, os.ErrNotExist
	}
	anchor := original[0]
	if anchor.Platform != "chatgpt" {
		for _, a := range original[1:] {
			if anchor.Kind == "oauth" && a.Kind != "oauth" {
				continue
			}
			if anchor.Kind == a.Kind {
				left, _ := json.Marshal(anchor.Payload)
				right, _ := json.Marshal(a.Payload)
				if string(left) != string(right) {
					return Asset{}, ErrAmbiguous
				}
			}
			if a.Kind == "oauth" {
				anchor = a
			}
		}
		return anchor, nil
	}
	var exact []Asset
	for _, a := range original {
		if credentialComplete(a) {
			exact = append(exact, a)
		}
	}
	if len(exact) > 0 {
		var err error
		anchor, err = newestCredential(exact)
		if err != nil {
			return Asset{}, err
		}
	}
	email := strings.ToLower(strings.TrimSpace(anchor.Email))
	if email == "" {
		email = strings.ToLower(documentEmail(anchor.Payload))
	}
	scope := credentialScope(anchor)
	var candidates []Asset
	for _, a := range all {
		if a.Platform != anchor.Platform || !credentialComplete(a) {
			continue
		}
		otherEmail := strings.ToLower(strings.TrimSpace(a.Email))
		if otherEmail == "" {
			otherEmail = strings.ToLower(documentEmail(a.Payload))
		}
		if a.Ref != ref && (email == "" || email != otherEmail) {
			continue
		}
		other := credentialScope(a)
		sameUser := scope[1] != "" && scope[1] == other[1]
		if (!sameUser && scope[0] != "" && other[0] != "" && scope[0] != other[0]) ||
			(scope[1] != "" && other[1] != "" && scope[1] != other[1]) {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return anchor, nil
	}
	// An unscoped login record cannot bridge two different workspaces or users.
	for axis := 0; axis < 2; axis++ {
		if axis == 0 && scope[1] != "" {
			same := true
			for _, a := range candidates {
				if credentialScope(a)[1] != scope[1] {
					same = false
				}
			}
			if same {
				continue
			}
		}
		known := scope[axis]
		for _, a := range candidates {
			value := credentialScope(a)[axis]
			if value != "" && known != "" && value != known {
				return Asset{}, ErrAmbiguous
			}
			if value != "" {
				known = value
			}
		}
	}
	selected, err := newestCredential(candidates)
	if err != nil {
		return Asset{}, err
	}
	if selected.Ref != ref {
		selected.CredentialRef = selected.Ref
	}
	selected.Ref = ref // stable Core subject/authorization; only the credential projection changes.
	selected.ResultSHA, selected.Current = "", false
	raw, err := json.MarshalIndent(selected, "", "  ")
	if err != nil {
		return Asset{}, err
	}
	selected.ResultSHA = fmt.Sprintf("%x", sha256.Sum256(raw))
	return selected, nil
}

// Snapshot pins only the input of an actual credential operation. List and
// download requests must not recreate obsolete credential files after cleanup.
func (s *Store) Snapshot(ref string, knownEmail ...string) (Asset, error) {
	selected, err := s.Get(ref, knownEmail...)
	if err != nil {
		return Asset{}, err
	}
	selected.ResultSHA, selected.Current = "", false
	raw, err := json.MarshalIndent(selected, "", "  ")
	if err != nil {
		return Asset{}, err
	}
	selected.ResultSHA, err = s.Retain(raw)
	return selected, err
}

// Current resolves exports through the same path used by execution and repair.
func (s *Store) Current() ([]Asset, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	out := []Asset{}
	seen := map[string]bool{}
	selectedRefs := map[string]bool{}
	for _, a := range all {
		if seen[a.Ref] {
			continue
		}
		seen[a.Ref] = true
		current, err := s.current(a.Ref, all)
		if err != nil {
			return nil, err
		}
		selectedRef := current.CredentialRef
		if selectedRef == "" {
			selectedRef = current.Ref
		}
		key := current.Platform + "\n" + selectedRef
		if selectedRefs[key] {
			continue
		}
		selectedRefs[key] = true
		out = append(out, current)
	}
	return out, nil
}
