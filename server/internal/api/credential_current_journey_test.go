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
	"github.com/tiantianGPU/reg-factory/server/internal/auth"
	"github.com/tiantianGPU/reg-factory/server/internal/nodeserver"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/testdatabase"
)

// Exercises the actual panel entry, Go queue/runner, Python CLI, HTTP exchange,
// atomic save, terminal task, automatic-check queue and old-reference download.
func TestCurrentCredentialJourneyThroughPythonWorker(t *testing.T) {
	python := os.Getenv("REG_FACTORY_TEST_PYTHON")
	if python == "" {
		t.Skip("set REG_FACTORY_TEST_PYTHON for the cross-language acceptance journey")
	}
	root := t.TempDir()
	st := testdatabase.Open(t, root)
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
		if r.Method != "POST" || (r.FormValue("grant_type") != "refresh_token" && r.FormValue("grant_type") != "fixture_authorize") {
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
	os.MkdirAll(filepath.Join(app, "deploy"), 0700)
	schema, err := os.ReadFile(filepath.Join(repo, "deploy", "config.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "deploy", "config.schema.json"), schema, 0600); err != nil {
		t.Fatal(err)
	}
	// Only the fixture transport changes. The released CLI and persistence code run unmodified.
	script := fmt.Sprintf("import sys,requests\nsys.path.insert(0,%q)\nfrom tools import asset_ops,chatgpt_reauthorize\nfrom common.proxy_switch import production_proxy\nasset_ops.OPENAI_TOKEN_URL='http://shared-capability.invalid/token'\nasync def authorize(document,**kwargs):\n s=requests.Session();s.trust_env=False;s.proxies={'http':production_proxy()}\n return s.post(asset_ops.OPENAI_TOKEN_URL,data={'grant_type':'fixture_authorize'},timeout=5).json()\nchatgpt_reauthorize.authorize_document=authorize\nraise SystemExit(asset_ops.main())\n", repo)
	if err := os.WriteFile(filepath.Join(app, "tools", "asset_ops.py"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &settings.Settings{DataDir: root, EnvFile: filepath.Join(root, ".env"), AppRoot: app, Python: python, MaxParallel: 1,
		PanelUser: "operator", PanelPass: "fixture-password"}
	m := runner.New(cfg, st)
	defer m.Shutdown(context.Background())
	s := New(cfg, settings.NewEnv(cfg.EnvFile), st, m, auth.New(cfg), "fixture", nil)
	r := gin.New()
	s.Register(r)
	nodeserver.New(nodeserver.Config{}, m, st).Register(r)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(cfg.PanelUser, cfg.PanelPass)
		r.ServeHTTP(w, req)
		return w
	}
	// Work is deliberately unconfigured. Local management remains authenticated
	// and fully usable, including the actual panel routes and task history.
	ready := request("GET", "/ready", "")
	var readiness struct {
		Ready bool `json:"ready"`
	}
	if ready.Code != 200 || json.Unmarshal(ready.Body.Bytes(), &readiness) != nil || readiness.Ready {
		t.Fatal("fixture unexpectedly enabled Work", ready.Code)
	}
	anonymous := httptest.NewRecorder()
	r.ServeHTTP(anonymous, httptest.NewRequest("GET", "/api/accounts", nil))
	if anonymous.Code != 401 {
		t.Fatal("standalone mode lost panel authentication", anonymous.Code)
	}
	imported := request("POST", "/api/accounts/import", `{"platform":"chatgpt","accounts":"local@example.test----fixture-password----JBSWY3DPEHPK3PXP"}`)
	if imported.Code != 200 {
		t.Fatal("standalone import failed", imported.Code)
	}
	for _, path := range []string{"/api/health", "/api/accounts?platform=chatgpt", "/api/assets/export?ref=import:local@example.test", "/api/config"} {
		if response := request("GET", path, ""); response.Code != 200 {
			t.Fatal("standalone route failed", path, response.Code)
		}
	}
	invokeMode := func(id, ref string, authorize bool) store.Task {
		body, _ := json.Marshal(map[string]any{"ref": ref, "request_id": id, "allow_relogin": authorize})
		w := request("POST", "/api/accounts/refresh", string(body))
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
	invoke := func(id string) store.Task { return invokeMode(id, "old", false) }
	// Use the authenticated configuration entry, then the real Python worker.
	// The synthetic upstream is an HTTP proxy: the target hostname never resolves.
	missing := request("PUT", "/api/config", `{"updates":{"REG_FACTORY_PRODUCTION_PROXY_SOURCE":"residential","REG_FACTORY_PROXY":"","REG_FACTORY_PROXY_POOL":""}}`)
	if missing.Code != 400 || calls.Load() != 0 {
		t.Fatal("missing connection was accepted or called upstream", missing.Code)
	}
	shared, _ := json.Marshal(map[string]any{"updates": map[string]string{
		"REG_FACTORY_PRODUCTION_PROXY_SOURCE": "residential", "REG_FACTORY_PROXY": upstream.URL, "REG_FACTORY_PROXY_POOL": "",
	}})
	if response := request("PUT", "/api/config", string(shared)); response.Code != 200 {
		t.Fatal("shared configuration failed", response.Code, response.Body)
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
	shared, _ = json.Marshal(map[string]any{"updates": map[string]string{
		"REG_FACTORY_PRODUCTION_PROXY_SOURCE": "clash", "CLASH_PROXY": upstream.URL,
	}})
	if response := request("PUT", "/api/config", string(shared)); response.Code != 200 {
		t.Fatal("connection switch failed", response.Code, response.Body)
	}
	last := invoke("b552c0f8-0ec8-4fa9-9871-c482f7741003")
	if last.Status != "succeeded" || calls.Load() != 3 {
		t.Fatal("retry failed or duplicated exchange", last.Status, calls.Load())
	}
	w := request("GET", "/api/assets/export?ref=old", "")
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
	mode.Store(0)
	blocked := invoke("b552c0f8-0ec8-4fa9-9871-c482f7741005")
	if blocked.Status != "failed" || calls.Load() != 4 || !strings.Contains(blocked.Error, "重新授权") {
		t.Fatal("lost RT was replayed by a new task", blocked.Status, blocked.Error, calls.Load())
	}
	// A different alias of the same account is protected by the same PG history.
	blocked = invokeMode("b552c0f8-0ec8-4fa9-9871-c482f7741006", "current", false)
	if blocked.Status != "failed" || calls.Load() != 4 {
		t.Fatal("alias bypassed RT guard", blocked.Status, calls.Load())
	}
	st.UpsertAccounts([]store.AccountSeed{{Ref: "independent", Platform: "chatgpt", Kind: "oauth", Identity: "other@example.test"}})
	if err := assets.Put(assetstore.Asset{Ref: "independent", Platform: "chatgpt", Kind: "oauth", Email: "other@example.test", Payload: map[string]any{"email": "other@example.test", "access_token": "other-at", "refresh_token": "other-rt"}}); err != nil {
		t.Fatal(err)
	}
	other := invokeMode("b552c0f8-0ec8-4fa9-9871-c482f7741007", "independent", false)
	if other.Status != "succeeded" || calls.Load() != 5 {
		t.Fatal("independent account blocked", other.Status, other.Error, calls.Load())
	}
	authorized := invokeMode("b552c0f8-0ec8-4fa9-9871-c482f7741008", "old", true)
	if authorized.Status != "succeeded" || calls.Load() != 6 {
		t.Fatal("explicit login did not recover", authorized.Status, authorized.Error, calls.Load())
	}
	resumed := invoke("b552c0f8-0ec8-4fa9-9871-c482f7741009")
	if resumed.Status != "succeeded" || calls.Load() != 7 {
		t.Fatal("new successful credentials still fenced", resumed.Status, resumed.Error, calls.Load())
	}
	for _, suffix := range []string{"", "/logs", "/stream"} {
		response := request("GET", "/api/tasks/"+resumed.ID+suffix, "")
		if response.Code != 200 || strings.Contains(response.Body.String(), "new-7-rt") {
			t.Fatal("standalone task evidence failed or exposed credentials", suffix, response.Code)
		}
	}
}
