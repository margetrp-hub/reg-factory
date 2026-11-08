package assetstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyScanCannotReplaceCurrentResultAndHistoryExportsExactVersion(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	a := Asset{Ref: "import:child@example.test", Platform: "chatgpt", Kind: "oauth", Email: "child@example.test",
		Payload: map[string]any{"email": "child@example.test", "access_token": "old", "refresh_token": "old-rt", "workspace_account_id": "workspace"}}
	legacy := filepath.Join(root, "plus-import", "team-json")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(a.Payload)
	if err := os.WriteFile(filepath.Join(legacy, "child.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(root); err != nil {
		t.Fatal(err)
	}
	old, err := s.Get(a.Ref)
	if err != nil {
		t.Fatal(err)
	}
	a.Payload["access_token"], a.Payload["refresh_token"] = "new", "new-rt"
	if err := s.Put(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(root); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(a.Ref)
	if err != nil || current.Payload["access_token"] != "new" {
		t.Fatal("legacy overwrote new result", err)
	}
	retained, err := s.Result(a.Ref, old.ResultSHA)
	if err != nil || retained.Payload["access_token"] != "old" {
		t.Fatal("original result lost", err)
	}
	if _, err := s.Result("other", old.ResultSHA); err == nil {
		t.Fatal("cross-account result accepted")
	}
	items, err := s.History(a.Ref)
	if err != nil || len(items) < 2 {
		t.Fatal("missing history", err)
	}
	for _, item := range items {
		if item.Payload != nil {
			t.Fatal("history leaked credentials")
		}
	}
	projected, err := OAuthDocument(current.Payload, a.Email)
	if err != nil || projected["workspace_account_id"] != "workspace" {
		t.Fatal("export lost workspace", err)
	}
}

func TestExportNeverStitchesCredentialSets(t *testing.T) {
	_, err := OAuthDocument(map[string]any{"access_token": "web-session", "tokens": map[string]any{
		"access_token": "oauth-session", "refresh_token": "oauth-refresh"}}, "test@example.test")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatal("mixed sessions exported", err)
	}
	doc, err := OAuthDocument(map[string]any{"tokens": map[string]any{"access_token": "oauth", "refresh_token": "rt", "account_id": "workspace"}}, "test@example.test")
	if err != nil || doc["account_id"] != "workspace" || doc["client_id"] != nil {
		t.Fatal("lost metadata or fabricated client", err)
	}
}
