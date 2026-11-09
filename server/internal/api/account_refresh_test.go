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
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestRefreshReconciliationTestsNewATWithoutExchangingRT(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.UpsertAccounts([]store.AccountSeed{{Ref: "one", Platform: "chatgpt", Kind: "oauth"}})
	assets := assetstore.New(root)
	original := assetstore.Asset{Ref: "one", Platform: "chatgpt", Kind: "oauth", Payload: map[string]any{"access_token": "old-at", "refresh_token": "old-rt"}}
	assets.Put(original)
	source, _ := assets.Get("one")
	task := store.Task{ID: "original", Script: "asset_ops", ArgsJSON: `["refresh","--ref","one","--platform","chatgpt"]`}
	st.QueueAccountRefresh(task, "one")
	st.BeginAccountRefresh(task.ID, "one", source.ResultSHA)
	st.FinishAccountRefresh(task.ID, "one", "unknown", 75, "network lost", "")
	var calls atomic.Int32
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("Authorization") != "Bearer new-at" {
			t.Error("unexpected exchange or token", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"plan_type":"team"}`))
	}))
	defer upstream.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local TLS fixture only
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections() }()
	server := &Server{st: st, assets: assets, env: settings.NewEnv(filepath.Join(root, "missing.env"))}
	router := gin.New()
	router.POST("/refresh/:id/reconcile", server.reconcileAccountRefresh)
	router.POST("/refresh/:id/resolve", server.resolveAccountRefresh)
	invoke := func(path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
		return w
	}
	response := invoke("/refresh/original/reconcile", "")
	if response.Code != 409 || calls.Load() != 0 {
		t.Fatal("old AT accepted as refresh", response.Code, calls.Load())
	}
	original.Payload["access_token"] = "new-at"
	original.Payload["refresh_token"] = "new-rt"
	assets.Put(original)
	before, _ := assets.Get("one")
	response = invoke("/refresh/original/reconcile", "")
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	after, _ := assets.Get("one")
	if after.ResultSHA != before.ResultSHA || calls.Load() != 1 {
		t.Fatal("reconciliation modified credentials or exchanged RT")
	}
	var detail struct{ Task store.Task }
	json.Unmarshal(response.Body.Bytes(), &detail)
	if detail.Task.Status != "succeeded" {
		t.Fatal(detail.Task.Status)
	}
	response = invoke("/refresh/original/reconcile", "")
	if response.Code != 200 || calls.Load() != 1 {
		t.Fatal("replay probed again")
	}
	account, _ := st.GetAccount("one")
	if account.RefreshState != "succeeded" {
		t.Fatal(account.RefreshState)
	}
	// Legacy uncertain tasks have no source digest, but retain a manual owner.
	legacy := task
	legacy.ID = "legacy"
	st.CreateTask(legacy)
	st.MarkRunning(legacy.ID, 1)
	st.Finish(legacy.ID, "unknown", 75, "old uncertain")
	if response = invoke("/refresh/legacy/resolve", `{"reason":""}`); response.Code != 400 {
		t.Fatal("accepted missing evidence")
	}
	response = invoke("/refresh/legacy/resolve", `{"reason":"上游查询确认未执行本次刷新"}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	response = invoke("/refresh/legacy/resolve", `{"reason":"上游查询确认未执行本次刷新"}`)
	if response.Code != 200 {
		t.Fatal("manual replay failed")
	}
}
