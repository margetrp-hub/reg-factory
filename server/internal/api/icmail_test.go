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
	"github.com/tiantianGPU/reg-factory/server/internal/apps/icmail"
	"github.com/tiantianGPU/reg-factory/server/internal/auth"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestICMailMountedAuthenticationAndPublicCode(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
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
	app, err := icmail.New(icmail.Config{DBPath: filepath.Join(root, "mail.db"), AdminKey: "separate-key", TurnstileSecret: "fixture", ExpectedHost: "example.test", TurnstileVerifyURL: verify.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	app.DB.UpsertSource(icmail.Source{Name: "fixture", Host: "example.test", Password: "never-return-this"})
	app.DB.StoreMessage(icmail.MessageRecord{SourceID: 1, Mailbox: "INBOX", UID: "1", ToHeader: "user@example.test", Subject: "secret subject", BodyText: "verification code: 123456"})
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
		if w := invoke("GET", "/api/apps/icmail"+path, "", false); w.Code != 401 {
			t.Fatal("anonymous management", path, w.Code)
		}
		if w := invoke("GET", "/api/apps/icmail"+path, "", true); w.Code != 200 || strings.Contains(w.Body.String(), "never-return-this") {
			t.Fatal("management auth", path, w.Code, w.Body.String())
		}
	}
	if w := invoke("POST", "/api/apps/icmail/public/code", `{"email":"user@example.test","turnstileToken":"bad"}`, false); w.Code != 401 {
		t.Fatal("Turnstile bypass", w.Code)
	}
	w := invoke("POST", "/api/apps/icmail/public/code", `{"email":"user@example.test","turnstileToken":"valid-fixture"}`, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "123456") || strings.Contains(w.Body.String(), "secret subject") {
		t.Fatal("public projection", w.Code, w.Body.String())
	}
	if err = app.DB.SetAliasState(context.Background(), "user@example.test", "paused"); err != nil {
		t.Fatal(err)
	}
	w = invoke("POST", "/api/apps/icmail/public/code", `{"email":"user@example.test","turnstileToken":"valid-fixture"}`, false)
	if w.Code != 409 {
		t.Fatal("public pause", w.Code, w.Body.String())
	}
	mailbox := icMailbox{app: app, email: "user@example.test"}
	if _, err = mailbox.WaitForMagicLink(context.Background(), time.Millisecond); err == nil || !strings.Contains(err.Error(), "暂停") {
		t.Fatal("protocol pause", err)
	}
	if w = invoke("GET", "/api/apps/icmail/prune?days=1", "", true); w.Code == 200 {
		t.Fatal("destructive GET retained")
	}
	for _, days := range []string{"0", "-1", "3651", "invalid", "1.5"} {
		if w = invoke("POST", "/api/apps/icmail/prune?days="+days, "", true); w.Code != 400 {
			t.Fatal("invalid retention accepted", days, w.Code)
		}
	}
}
