package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/apps/mail"
	"github.com/tiantianGPU/reg-factory/server/internal/auth"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/testdatabase"
)

func TestMailMountedAuthenticationAndPublicCode(t *testing.T) {
	root := t.TempDir()
	st := testdatabase.Open(t, root)
	dsn, err := st.ProductionDatabaseDSN()
	if err != nil {
		t.Fatal(err)
	}
	verify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("response") == "valid-fixture" {
			w.Write([]byte(`{"success":true,"hostname":"example.test","action":"otp_lookup"}`))
		} else {
			w.Write([]byte(`{"success":false}`))
		}
	}))
	defer verify.Close()
	app, err := mail.New(mail.Config{DBPath: dsn, AdminKey: "separate-key", TurnstileSecret: "fixture", ExpectedHost: "example.test", TurnstileVerifyURL: verify.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	source, err := app.DB.UpsertSource(mail.Source{Name: "fixture", Username: "owner@example.test", Host: "example.test", Password: "never-return-this", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.DB.StoreMessage(mail.MessageRecord{SourceID: source.ID, Mailbox: "INBOX", UID: "1", ToHeader: "user@example.test", Subject: "secret subject", BodyText: "verification code: 123456"}); err != nil {
		t.Fatal(err)
	}
	cfg := &settings.Settings{AppRoot: root, DataDir: root, PanelUser: "operator", PanelPass: "fixture-password", MaxParallel: 0}
	manager := runner.New(cfg, st)
	defer manager.Shutdown(context.Background())
	s := New(cfg, settings.NewEnv(filepath.Join(root, "config.env")), st, manager, auth.New(cfg), "fixture", app)
	r := gin.New()
	s.Register(r)
	invoke := func(method, path, body string, loggedIn bool) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if loggedIn {
			req.SetBasicAuth("operator", "fixture-password")
		}
		r.ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"/pool", "/sources", "/messages", "/sync", "/config", "/health", "/code?email=user@example.test"} {
		if w := invoke("GET", "/api/apps/mail"+path, "", false); w.Code != 401 {
			t.Fatal("anonymous management", path, w.Code)
		}
		if w := invoke("GET", "/api/apps/mail"+path, "", true); w.Code != 200 || strings.Contains(w.Body.String(), "never-return-this") {
			t.Fatal("management auth", path, w.Code, w.Body.String())
		}
	}
	if w := invoke("GET", "/api/apps/mail/workflow/mailbox?email=user@example.test", "", false); w.Code != 401 {
		t.Fatal("anonymous worker mailbox", w.Code)
	}
	if w := invoke("POST", "/api/apps/mail/public/code", `{"email":"user@example.test","turnstileToken":"bad"}`, false); w.Code != 401 {
		t.Fatal("Turnstile bypass", w.Code)
	}
	w := invoke("POST", "/api/apps/mail/public/code", `{"email":"user@example.test","turnstileToken":"valid-fixture"}`, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "123456") || strings.Contains(w.Body.String(), "secret subject") {
		t.Fatal("public projection", w.Code, w.Body.String())
	}
	if err = app.DB.SetMailbox(context.Background(), "user@example.test", source.ID, "paused"); err != nil {
		t.Fatal(err)
	}
	w = invoke("POST", "/api/apps/mail/public/code", `{"email":"user@example.test","turnstileToken":"valid-fixture"}`, false)
	if w.Code != 409 {
		t.Fatal("public pause", w.Code, w.Body.String())
	}
	mailbox := poolMailbox{app: app, email: "user@example.test", sourceID: source.ID}
	if _, err = mailbox.WaitForMagicLink(context.Background(), time.Second); err == nil || !strings.Contains(err.Error(), "暂停") {
		t.Fatal("protocol pause", err)
	}
	if w = invoke("GET", "/api/apps/mail/prune?days=1", "", true); w.Code == 200 {
		t.Fatal("destructive GET retained")
	}
	for _, days := range []string{"0", "-1", "3651", "invalid", "1.5"} {
		if w = invoke("POST", "/api/apps/mail/prune?days="+days, "", true); w.Code != 400 {
			t.Fatal("invalid retention accepted", days, w.Code)
		}
	}
	for _, path := range []string{"/api/apps/icmail/pool", "/api/apps/mail/aliases"} {
		if w = invoke("GET", path, "", true); w.Code != 404 {
			t.Fatal("retired route remains", path, w.Code)
		}
	}
}

func TestProtocolMailboxUsesCurrentAttemptAndSource(t *testing.T) {
	st := testdatabase.Open(t, t.TempDir())
	dsn, err := st.ProductionDatabaseDSN()
	if err != nil {
		t.Fatal(err)
	}
	app, err := mail.New(mail.Config{DBPath: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	source, err := app.DB.UpsertSource(mail.Source{Name: "protocol", Username: "owner@example.test", Host: "imap.example.test", Password: "fixture", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	email := "protocol@icloud.com"
	ctx := context.Background()
	if _, err = app.DB.ImportMailboxes(ctx, source.ID, []string{email}); err != nil {
		t.Fatal(err)
	}
	now := float64(time.Now().Unix())
	send := func(uid, recipient, link string, at float64) {
		t.Helper()
		if _, err := app.DB.StoreMessage(mail.MessageRecord{SourceID: source.ID, Mailbox: "INBOX", UID: uid, ToHeader: recipient,
			Subject: "Claude login", BodyText: link, SentAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	oldLink := "https://claude.ai/magic-link#old-nonce"
	newLink := "https://claude.ai/magic-link#new-nonce"
	send("old", email, oldLink, now-60)
	snapshot, err := app.DB.WorkflowMailbox(ctx, email, "claude", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	box := poolMailbox{app: app, email: email, sourceID: source.ID, cursor: snapshot.Cursor, startedAt: now}
	if link, err := box.WaitForMagicLink(ctx, 50*time.Millisecond); err == nil || link != "" {
		t.Fatal("reused old link", link, err)
	}
	send("delayed", email, oldLink, now-60)
	send("other-recipient", "other@example.test", oldLink, now)
	send("fresh", email, newLink, now)
	if link, err := box.WaitForMagicLink(ctx, time.Second); err != nil || link != newLink {
		t.Fatal("current message", link, err)
	}
	if err = app.DB.MarkPlatformBan(ctx, mail.PlatformBan{Email: email, Platform: "claude", State: mail.BanBlocked}); err != nil {
		t.Fatal(err)
	}
	if _, err = box.WaitForMagicLink(ctx, time.Second); err == nil || !strings.Contains(err.Error(), "所选平台") {
		t.Fatal("platform restriction", err)
	}
	if _, err = app.DB.WorkflowMailbox(ctx, email, "github", nil, 0); err != nil {
		t.Fatal("restriction leaked to another platform", err)
	}
	if err = app.DB.MarkPlatformBan(ctx, mail.PlatformBan{Email: email, Platform: "claude", State: mail.BanRegistered}); err != nil {
		t.Fatal(err)
	}
	other, err := app.DB.UpsertSource(mail.Source{Name: "other", Username: "other@example.test", Host: "imap.example.test", Password: "fixture", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = app.DB.SetMailbox(ctx, email, other.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if _, err = box.WaitForMagicLink(ctx, time.Second); err == nil || !strings.Contains(err.Error(), "来源已变化") {
		t.Fatal("source switch", err)
	}
	app.Close()
	if _, err = box.WaitForMagicLink(ctx, time.Second); err == nil || strings.Contains(err.Error(), "超时") {
		t.Fatal("database failure swallowed", err)
	}
}

func TestProtocolMailboxRequiresExplicitPoolSelection(t *testing.T) {
	r := gin.New()
	s := &Server{}
	r.POST("/register", s.claudeHTTPRegister)
	for _, body := range []string{`{"email":"test@icloud.com"}`, `{"email":"test@icloud.com","mailbox":"icloud"}`, `{"email":"test@outlook.com","refresh_token":"fixture"}`} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "mailbox_required") {
			t.Fatal(w.Code, w.Body)
		}
	}
}
