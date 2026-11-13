package api

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/apps/mail"
	"github.com/tiantianGPU/reg-factory/server/internal/auth"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// Only an explicit local disposable PostgreSQL database is accepted. No real
// account, mail provider, RT exchange or payment participates in this journey.
func TestPostgresMailboxAndAssetReceiptJourney(t *testing.T) {
	dsn := os.Getenv("REG_FACTORY_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set REG_FACTORY_TEST_POSTGRES_DSN to a disposable local database")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "/sg_release_test" {
		t.Fatal("explicit local sg_release_test database required")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := fmt.Sprintf("sg_release_%x", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec("DROP SCHEMA " + schema + " CASCADE")
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	st, err := store.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ReadDB().Exec("INSERT INTO tasks(id,script,status,created_at) VALUES('forbidden','central_asset','queued','now')"); err == nil {
		t.Fatal("read-only PostgreSQL pool allowed a write")
	}
	app, err := mail.New(mail.Config{DBPath: u.String(), AdminKey: "synthetic-admin-key"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	source, err := app.DB.UpsertSource(mail.Source{Name: "synthetic", Host: "mail.example.test", Username: "fixture@example.test", Password: "synthetic-secret", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for index, count := range []int{1, 0} {
		n, err := app.DB.ImportMailboxes(context.Background(), source.ID, []string{"fixture@example.test"})
		if err != nil || n != count {
			t.Fatalf("mailbox import/replay %d: count=%d, error=%v", index, n, err)
		}
	}
	ctx := context.Background()
	for _, enabled := range []bool{false, true} {
		if err = app.DB.SetSourceEnabled(ctx, source.ID, enabled); err != nil {
			t.Fatal(err)
		}
	}
	source.Password = ""
	if source, err = app.DB.UpsertSource(source); err != nil || source.Password != "synthetic-secret" {
		t.Fatalf("source update lost credential: %v", err)
	}
	record := mail.MessageRecord{SourceID: source.ID, Mailbox: "INBOX", UID: "fixture-1", AliasHME: "fixture@example.test", ToHeader: "fixture@example.test", BodyText: "verification code: 123456", PrimaryTag: "otp"}
	for _, want := range []bool{true, false} {
		if added, err := app.DB.StoreMessage(record); err != nil || added != want {
			t.Fatalf("message replay: %v %v", added, err)
		}
	}
	if result, err := app.LookupCode("fixture@example.test", 20); err != nil || result.Code != "123456" {
		t.Fatalf("mail lookup: %+v %v", result, err)
	}
	for _, state := range []string{"paused", "blocked", "active"} {
		if err = app.DB.SetMailbox(ctx, "fixture@example.test", source.ID, state); err != nil {
			t.Fatal(err)
		}
		_, err = app.LookupCode("fixture@example.test", 20)
		if (state == "active") != (err == nil) {
			t.Fatalf("mail state %s: %v", state, err)
		}
	}
	if _, err = app.DB.ImportMailboxes(ctx, source.ID, []string{"manual@example.test"}); err != nil {
		t.Fatal(err)
	}
	if err = app.DB.SetMailbox(ctx, "manual@example.test", source.ID, "paused"); err != nil {
		t.Fatal(err)
	}
	if items, total, err := app.DB.PoolPage(ctx, "fixture", "", 0, 25, 0); err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("pool filter: %d %v", total, err)
	}
	if err = app.DB.PruneOlderThan(30); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cfg := &settings.Settings{AppRoot: root, DataDir: root, PanelUser: "operator", PanelPass: "fixture-password", MaxParallel: 0}
	manager := runner.New(cfg, st)
	defer manager.Shutdown(context.Background())
	s := New(cfg, settings.NewEnv(filepath.Join(root, "settings.env")), st, manager, auth.New(cfg), "fixture", app)
	r := gin.New()
	s.Register(r)
	for _, base := range []string{"/api/apps/mail"} {
		for _, authenticated := range []bool{false, true} {
			req := httptest.NewRequest("GET", base+"/pool", nil)
			if authenticated {
				req.SetBasicAuth("operator", "fixture-password")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if (!authenticated && w.Code != 401) || (authenticated && (w.Code != 200 || !strings.Contains(w.Body.String(), "fixture@example.test"))) {
				t.Fatalf("mailbox route %s authenticated=%v status=%d body=%s", base, authenticated, w.Code, w.Body.String())
			}
		}
		req := httptest.NewRequest("GET", base+"/config", nil)
		req.SetBasicAuth("operator", "fixture-password")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"dbPath":"PostgreSQL"`) || strings.Contains(w.Body.String(), "sg-test-only") {
			t.Fatalf("mailbox configuration exposed connection data or failed: status=%d", w.Code)
		}
	}
	if err = st.CreateTask(store.Task{ID: "central-fixture", Script: "central_asset", ArgsJSON: "[]"}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"unknown", "completed", "completed"} {
		if err = st.SaveCentralReceipt("central-fixture", state, map[string]any{"state": state, "result_id": "synthetic-result"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.SaveCentralReceipt("central-fixture", "failed", map[string]any{"state": "failed"}); err == nil {
		t.Fatal("terminal asset result was overwritten")
	}
	st.DeferCentral("central-fixture")
	if err = st.AcknowledgeCentral("central-fixture"); err != nil {
		t.Fatal(err)
	}
	task, err := st.GetTask("central-fixture")
	if err != nil || task.Status != "succeeded" {
		t.Fatalf("terminal task lost: %+v %v", task, err)
	}
	if pending, err := st.PendingCentral(); err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged result remained pending: %d %v", len(pending), err)
	}
	if _, err = st.UpsertAccounts([]store.AccountSeed{{Ref: "fixture-account", Identity: "fixture@example.test", Platform: "chatgpt", Kind: "oauth"}}); err != nil {
		t.Fatal(err)
	}
	if err = st.MarkATHealth("fixture-account", "ok", "synthetic check", 0); err != nil {
		t.Fatal(err)
	}
	refresh := store.Task{ID: "fixture-refresh", Script: "asset_ops", ArgsJSON: `["refresh","--ref","fixture-account","--platform","chatgpt"]`, Actor: "operator"}
	var wg sync.WaitGroup
	accepted := make(chan string, 6)
	for index := 0; index < 6; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			request := refresh
			request.ID = fmt.Sprintf("refresh-%d", index)
			id, _, err := st.QueueAccountRefresh(request, "fixture-account")
			if err != nil {
				t.Error(err)
			} else {
				accepted <- id
			}
		}(index)
	}
	wg.Wait()
	close(accepted)
	original := ""
	for id := range accepted {
		if original == "" {
			original = id
		}
		if id != original {
			t.Fatal("duplicate RT operation admitted")
		}
	}
	if original == "" {
		t.Fatal("no refresh admitted")
	}
	if _, err = st.UnfinishedCredentials(); err != nil {
		t.Fatal(err)
	}
	if err = st.BeginAccountRefresh(original, "fixture-account", "synthetic-sha"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	ended, err := st.GetTask(original)
	if err != nil || ended.Status != "failed" {
		t.Fatalf("restart did not close unfinished credential operation: %+v %v", ended, err)
	}
	id, created, err := st.QueueAccountRefresh(refresh, "fixture-account")
	if err != nil || !created {
		t.Fatalf("failed operation blocked retry: %v", err)
	}
	if err = st.FinishAccountRefresh(id, "fixture-account", "failed", 1, "synthetic upstream rejection", ""); err != nil {
		t.Fatal(err)
	}
	if err = st.Stage(id, store.StageEvent{Stage: "credential_result", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if stages, err := st.CredentialStages(id); err != nil || len(stages) != 1 {
		t.Fatalf("stage history: %d %v", len(stages), err)
	}
	if page, err := st.QueryTasks(store.TaskQuery{CredentialPlatform: "chatgpt", CredentialAction: "refresh"}); err != nil || page.Total != 2 {
		t.Fatalf("refresh history: %+v %v", page, err)
	}
	checkID, err := st.ScheduleLivenessCheck(time.Now(), time.Hour)
	if err != nil || checkID == "" {
		t.Fatalf("local check admission: %v", err)
	}
	if counts, err := st.CredentialCheckCounts([]string{checkID}); err != nil || counts[checkID].Total != 1 {
		t.Fatalf("check totals: %+v %v", counts, err)
	}
	if _, err = st.HasActiveLivenessScan("chatgpt"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/plus/ip-pool", "/api/plus/pool", "/api/accounts/summary", "/api/accounts"} {
		req := httptest.NewRequest("GET", path, nil)
		req.SetBasicAuth("operator", "fixture-password")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("PG read endpoint %s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
