package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// Exercises the actual panel entry, Go queue/runner, Python CLI, HTTP exchange,
// atomic save, terminal task, automatic-check queue and old-reference download.
func TestCurrentCredentialJourneyThroughPythonWorker(t *testing.T) {
	python := os.Getenv("REG_FACTORY_TEST_PYTHON")
	if python == "" {
		t.Skip("set REG_FACTORY_TEST_PYTHON for the cross-language acceptance journey")
	}
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "regd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assets := assetstore.New(root)
	for _, ref := range []string{"old", "current"} {
		st.UpsertAccounts([]store.AccountSeed{{Ref: ref, Platform: "chatgpt", Kind: "oauth", Identity: "same@example.test"}})
		if err := assets.Put(assetstore.Asset{Ref: ref, Platform: "chatgpt", Kind: "oauth", Email: "same@example.test", Payload: map[string]any{
			"email": "same@example.test", "access_token": ref + "-at", "refresh_token": ref + "-rt"}}); err != nil {
			t.Fatal(err)
		}
	}
	var mode, calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		r.ParseForm()
		if r.Method != "POST" || r.FormValue("grant_type") != "refresh_token" {
			t.Error("unexpected upstream action")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if mode.Load() == 2 {
			w.WriteHeader(504)
			fmt.Fprint(w, `{"error":"gateway_timeout"}`)
			return
		}
		if mode.Load() == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		fmt.Fprintf(w, `{"access_token":"new-%d-at","refresh_token":"new-%d-rt"}`, calls.Load(), calls.Load())
	}))
	defer upstream.Close()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(root, "worker")
	os.MkdirAll(filepath.Join(app, "tools"), 0700)
	// Only the fixture transport changes. The released CLI and persistence code run unmodified.
	script := fmt.Sprintf("import sys\nsys.path.insert(0,%q)\nfrom tools import asset_ops\nasset_ops.OPENAI_TOKEN_URL=%q\nraise SystemExit(asset_ops.main())\n", repo, upstream.URL)
	if err := os.WriteFile(filepath.Join(app, "tools", "asset_ops.py"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &settings.Settings{DataDir: root, EnvFile: filepath.Join(root, ".env"), AppRoot: app, Python: python, MaxParallel: 1}
	m := runner.New(cfg, st)
	defer m.Shutdown(context.Background())
	s := &Server{cfg: cfg, st: st, run: m, assets: assets}
	r := gin.New()
	r.POST("/refresh", s.accountRefresh)
	r.GET("/export", s.assetExport)
	invoke := func(id string) store.Task {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/refresh", strings.NewReader(`{"ref":"old","request_id":"`+id+`"}`)))
		if w.Code != 202 && w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var ack struct {
			ID string `json:"task_id"`
		}
		json.Unmarshal(w.Body.Bytes(), &ack)
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(30 * time.Millisecond) {
			task, _ := st.GetTask(ack.ID)
			if task.Status != "queued" && task.Status != "running" {
				return task
			}
		}
		t.Fatal("worker did not finish")
		return store.Task{}
	}
	first := invoke("b552c0f8-0ec8-4fa9-9871-c482f7741001")
	if first.Status != "succeeded" {
		events, _ := st.TailEvents(first.ID, 20)
		t.Fatalf("first boundary failed: %s %+v", first.Status, events)
	}
	check, err := st.GetTask(store.RefreshVerificationID(first.ID))
	if err != nil || check.Status != "queued" {
		t.Fatal("automatic AT check lost", err)
	}
	a, _ := assets.Get("old")
	b, _ := assets.Get("current")
	if a.Payload["access_token"] != "new-1-at" || b.Payload["access_token"] != a.Payload["access_token"] || a.CredentialUpdatedAt == "" {
		t.Fatal("old reference or timestamp stale")
	}
	mode.Store(1)
	failed := invoke("b552c0f8-0ec8-4fa9-9871-c482f7741002")
	if failed.Status != "failed" {
		t.Fatal("explicit failure did not terminate", failed.Status)
	}
	unchanged, _ := assets.Get("old")
	if unchanged.ResultSHA != a.ResultSHA || unchanged.CredentialUpdatedAt != a.CredentialUpdatedAt {
		t.Fatal("failure replaced successful credentials")
	}
	mode.Store(0)
	last := invoke("b552c0f8-0ec8-4fa9-9871-c482f7741003")
	if last.Status != "succeeded" || calls.Load() != 3 {
		t.Fatal("retry failed or duplicated exchange", last.Status, calls.Load())
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/export?ref=old", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "new-3-at") || strings.Contains(w.Body.String(), "new-1-at") {
		t.Fatal("download did not use latest", w.Code)
	}
	mode.Store(2)
	lost := invoke("b552c0f8-0ec8-4fa9-9871-c482f7741004")
	if lost.Status != "failed" || lost.ExitCode != 75 {
		t.Fatal("unconfirmed exchange did not terminate as failure", lost.Status)
	}
	account, _ := st.GetAccount("old")
	current, _ := assets.Get("old")
	if account.LibraryStatus == "leased" || current.Payload["access_token"] != "new-3-at" {
		t.Fatal("failed exchange retained a lock or replaced credentials")
	}
}
