package assetstore

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func credentialJWT(email, subject, workspace string, issued int) string {
	raw, _ := json.Marshal(map[string]any{"email": email, "sub": subject, "iat": issued, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": workspace}})
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".test"
}

func TestOldReferenceReadsLatestSuccessfulCredentials(t *testing.T) {
	s := New(t.TempDir())
	for _, ref := range []string{"old-source", "new-source"} {
		if err := s.Put(Asset{Ref: ref, Platform: "chatgpt", Kind: "oauth", Email: "account@example.test",
			Payload: map[string]any{"access_token": ref, "refresh_token": ref + "-rt", "workspace_account_id": "workspace"}}); err != nil {
			t.Fatal(err)
		}
	}
	current, err := s.Get("old-source")
	if err != nil || current.Payload["access_token"] != "new-source" {
		t.Fatal("old reference did not follow current credentials", err)
	}
	if current.Ref != "old-source" {
		t.Fatal("original authorization reference changed")
	}
	exports, err := s.Current()
	if err != nil || len(exports) != 1 {
		t.Fatal("bulk export duplicated aliases", len(exports), err)
	}
	files, _ := os.ReadDir(filepath.Join(filepath.Dir(s.Root()), "asset-results"))
	if len(files) != 0 {
		t.Fatal("reading current credentials regenerated historical token files")
	}
}

func TestAccountReuseMovesToLatestWorkspaceWithoutChangingOldTaskEvidence(t *testing.T) {
	s := New(t.TempDir())
	for i, workspace := range []string{"free-workspace", "team-workspace"} {
		token := credentialJWT("same@example.test", "same-user", workspace, i+1)
		s.Put(Asset{Ref: workspace, Platform: "chatgpt", Kind: "oauth", Email: "same@example.test", Payload: map[string]any{
			"access_token": token, "refresh_token": workspace, "id_token": token, "workspace_account_id": workspace}})
	}
	a, err := s.Get("free-workspace")
	if err != nil || a.Payload["workspace_account_id"] != "team-workspace" {
		t.Fatal("reuse read retired workspace", err)
	}
}

func TestLegacyEnvelopeRejectsUnrelatedNestedCredentials(t *testing.T) {
	newToken := credentialJWT("same@example.test", "same-user", "workspace", 2)
	oldToken := credentialJWT("wrong@example.test", "wrong-user", "other", 1)
	doc, err := OAuthDocument(map[string]any{"email": "same@example.test", "access_token": newToken, "refresh_token": "new",
		"id_token": newToken, "payload": map[string]any{"access_token": oldToken, "refresh_token": "wrong", "id_token": oldToken}}, "same@example.test")
	if err != nil || doc["refresh_token"] != "new" || doc["payload"] != nil {
		t.Fatal("legacy mixed identity was selected", err)
	}
}

func TestATOnlySourceFollowsSameOAuthSubjectAfterWorkspaceChange(t *testing.T) {
	s := New(t.TempDir())
	s.Put(Asset{Ref: "old-free", Platform: "chatgpt", Kind: "free", Email: "same@example.test", Payload: map[string]any{
		"access_token": credentialJWT("same@example.test", "oauth-subject", "free-workspace", 1),
		"user_id":      "application-user-id", "account_id": "free-workspace"}})
	token := credentialJWT("same@example.test", "oauth-subject", "team-workspace", 2)
	s.Put(Asset{Ref: "new-oauth", Platform: "chatgpt", Kind: "oauth", Email: "same@example.test", Payload: map[string]any{
		"access_token": token, "refresh_token": "current-rt", "id_token": token, "account_id": "team-workspace"}})
	a, err := s.Get("old-free")
	if err != nil || a.Payload["refresh_token"] != "current-rt" || a.Ref != "old-free" {
		t.Fatal("AT-only reference did not follow the same user's current grant", err)
	}
}
