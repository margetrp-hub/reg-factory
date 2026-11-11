package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func livenessFixture(t *testing.T, response func(http.ResponseWriter, *http.Request)) (*Server, *gin.Engine) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := &Server{st: st, assets: assetstore.New(root), env: settings.NewEnv(filepath.Join(root, "fixture.env"))}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("actor", "operator"); c.Next() })
	router.PUT("/config", s.configPut)
	router.POST("/checks", s.createCredentialCheck)
	router.GET("/checks", s.listCredentialChecks)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/backend-api/wham/usage" {
			t.Error("unexpected upstream operation", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		response(w, r)
	}))
	t.Cleanup(upstream.Close)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // isolated TLS upstream
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previous; transport.CloseIdleConnections() })
	return s, router
}

func livenessRequest(t *testing.T, router *gin.Engine, method, path, body string, want int) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	if w.Code != want {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
	}
	return w
}

func seedLiveness(t *testing.T, s *Server, ref, identity, token string) {
	t.Helper()
	if _, err := s.st.UpsertAccounts([]store.AccountSeed{{Ref: ref, Platform: "chatgpt", Kind: "oauth", Identity: identity}}); err != nil {
		t.Fatal(err)
	}
	if err := s.assets.Put(assetstore.Asset{Ref: ref, Email: identity, Platform: "chatgpt", Kind: "oauth", Payload: map[string]any{"access_token": token, "refresh_token": "fixture-rt"}}); err != nil {
		t.Fatal(err)
	}
}

func TestLivenessCurrentLibraryLatestATAndDisabledIsolation(t *testing.T) {
	tokens := []string{}
	s, router := livenessFixture(t, func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer expired-at" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"plan_type":"free"}`))
	})
	seedLiveness(t, s, "old:a", "a@example.test", "old-at")
	seedLiveness(t, s, "import:a", "a@example.test", "before-queue-at")
	seedLiveness(t, s, "import:b", "b@example.test", "expired-at")
	at := time.Now().UTC().Truncate(time.Second)
	state := livenessSchedule{}
	tick := func(at time.Time) {
		t.Helper()
		if err := s.livenessTick(context.Background(), at, &state); err != nil {
			t.Fatal(err)
		}
	}
	tick(at)
	tick(at.Add(24 * time.Hour))
	page, _ := s.st.QueryTasks(store.TaskQuery{})
	if page.Total != 0 || len(tokens) != 0 {
		t.Fatal("disabled scheduler submitted work", page.Total, tokens)
	}
	livenessRequest(t, router, "PUT", "/config", `{"updates":{"REG_FACTORY_LIVENESS_ENABLED":"1","REG_FACTORY_LIVENESS_INTERVAL":"1m","REG_FACTORY_LIVENESS_PLATFORMS":"chatgpt,chatgpt,claude"}}`, 200)
	tick(at)
	page, _ = s.st.QueryTasks(store.TaskQuery{})
	if page.Total != 0 {
		t.Fatal("implicit first scan")
	}
	tick(at.Add(time.Minute))
	task, err := s.st.ClaimCredentialCheck()
	if err != nil {
		t.Fatal(err)
	}
	items, _ := s.st.CredentialCheckItems(task.ID)
	if len(items) != 2 || items[0].Ref != "import:a" || task.Actor != "scheduler" {
		t.Fatal("scheduled sources instead of identities", task, items)
	}
	if id, err := s.st.ScheduleLivenessCheck(at.Add(2*time.Minute), time.Minute); err != nil || id != "" {
		t.Fatal("active batch duplicated", id, err)
	}
	// A credential updated after queueing must be the one actually checked.
	seedLiveness(t, s, "import:a", "a@example.test", "latest-at")
	s.executeCredentialCheck(context.Background(), task, s.checkCredential)
	items, _ = s.st.CredentialCheckItems(task.ID)
	if len(tokens) != 2 || tokens[0] != "Bearer latest-at" || items[0].Status != "ok" || items[1].Status != "needs_refresh" {
		t.Fatal(tokens, items)
	}
	if id, err := s.st.ScheduleLivenessCheck(at.Add(time.Minute+time.Second), time.Minute); err != nil || id != "" {
		t.Fatal("same period repeated after completion", id, err)
	}
	w := livenessRequest(t, router, "GET", "/checks?platform=chatgpt", "", 200)
	var history struct {
		Counts map[string]store.CredentialCheckCounts `json:"counts"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &history); err != nil || history.Counts[task.ID].Total != 2 || history.Counts[task.ID].NeedsRefresh != 1 {
		t.Fatal("missing live per-account counts", w.Body, err)
	}
	livenessRequest(t, router, "PUT", "/config", `{"updates":{"REG_FACTORY_LIVENESS_ENABLED":"0"}}`, 200)
	tick(at.Add(10 * time.Hour))
	page, _ = s.st.QueryTasks(store.TaskQuery{})
	if page.Total != 1 {
		t.Fatal("disable still scheduled", page.Total)
	}
	// Manual and system post-refresh checks remain available while scheduling is off.
	for _, actor := range []string{"operator", "system"} {
		if err = s.st.CreateCredentialCheck(store.Task{ID: "check-" + actor, Actor: actor}, []store.Account{{Ref: "import:a", Identity: "a@example.test", Platform: "chatgpt"}}); err != nil {
			t.Fatal(err)
		}
		current, err := s.st.ClaimCredentialCheck()
		if err != nil {
			t.Fatal(err)
		}
		s.executeCredentialCheck(context.Background(), current, s.checkCredential)
		finished, _ := s.st.GetTask(current.ID)
		if finished.Status != "succeeded" {
			t.Fatal("disabled setting stopped independent check", actor, finished)
		}
	}
	asset, _ := s.assets.Get("import:a")
	if asset.Payload["refresh_token"] != "fixture-rt" || len(tokens) != 4 {
		t.Fatal("unexpected credential mutation or check count", len(tokens))
	}
}

func TestLivenessUnknownRecoveryAndDisablePendingItems(t *testing.T) {
	uncertain, calls := true, 0
	s, router := livenessFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if uncertain && r.Header.Get("Authorization") == "Bearer b-at" {
			w.Write([]byte(`not-json`))
			return
		}
		w.Write([]byte(`{"plan_type":"free"}`))
	})
	for _, name := range []string{"a", "b", "c"} {
		seedLiveness(t, s, "import:"+name, name+"@example.test", name+"-at")
	}
	livenessRequest(t, router, "PUT", "/config", `{"updates":{"REG_FACTORY_LIVENESS_ENABLED":"1"}}`, 200)
	id, err := s.st.ScheduleLivenessCheck(time.Now(), time.Hour)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	task, _ := s.st.ClaimCredentialCheck()
	s.executeCredentialCheck(context.Background(), task, s.checkCredential)
	items, _ := s.st.CredentialCheckItems(id)
	if calls != 2 || items[0].Status != "ok" || items[1].Status != "unknown" || items[2].Status != "pending" {
		t.Fatal(calls, items)
	}
	uncertain = false
	livenessRequest(t, router, "PUT", "/config", `{"updates":{"REG_FACTORY_LIVENESS_ENABLED":"0"}}`, 200)
	body, _ := json.Marshal(credentialCheckRequest{RequestID: "c40c7ba4-2264-4c8e-bc0b-b28191a45a72", Platform: "chatgpt", ParentID: id, Refs: []string{"import:b", "import:c"}})
	livenessRequest(t, router, "POST", "/checks", string(body), 202)
	task, _ = s.st.ClaimCredentialCheck()
	s.executeCredentialCheck(context.Background(), task, s.checkCredential)
	finished, _ := s.st.GetTask(task.ID)
	original, _ := s.st.GetTask(id)
	if finished.Status != "succeeded" || original.Status != "unknown" || calls != 4 {
		t.Fatal("recovery lost original evidence", finished, original, calls)
	}
	// Already queued automatic work is also stopped before another account request.
	id, err = s.st.ScheduleLivenessCheck(time.Now().Add(2*time.Hour), time.Hour)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	task, _ = s.st.ClaimCredentialCheck()
	s.executeCredentialCheck(context.Background(), task, s.checkCredential)
	finished, _ = s.st.GetTask(id)
	if finished.Status != "stopped" || calls != 4 {
		t.Fatal("disabled queued check reached upstream", finished, calls)
	}
}

func TestLivenessEmptyAndInvalidConfigurationCannotReportSuccess(t *testing.T) {
	s, router := livenessFixture(t, func(http.ResponseWriter, *http.Request) { t.Fatal("empty library contacted upstream") })
	livenessRequest(t, router, "PUT", "/config", `{"updates":{"REG_FACTORY_LIVENESS_ENABLED":"1","REG_FACTORY_LIVENESS_INTERVAL":"0s"}}`, 400)
	if s.env.Get("REG_FACTORY_LIVENESS_ENABLED") != "" {
		t.Fatal("invalid configuration saved")
	}
	livenessRequest(t, router, "PUT", "/config", `{"updates":{"REG_FACTORY_LIVENESS_ENABLED":"1"}}`, 200)
	state := livenessSchedule{}
	at := time.Now()
	for _, current := range []time.Time{at, at.Add(6 * time.Hour)} {
		if err := s.livenessTick(context.Background(), current, &state); err != nil {
			t.Fatal(err)
		}
	}
	page, _ := s.st.QueryTasks(store.TaskQuery{})
	if page.Total != 0 {
		t.Fatal("empty scan masqueraded as successful work", page)
	}
}
