package runner

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

	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/assetwork"
)

func centralTask(scope map[string]any) store.Task {
	raw, _ := json.Marshal(scope)
	args, _ := json.Marshal([]string{"--assignment", string(raw)})
	return store.Task{ID: "work-" + scope["operation_id"].(string), Script: "central_asset", ArgsJSON: string(args)}
}

func TestCentralATCheckRunsInGoWithoutRTOrPython(t *testing.T) {
	f := assetwork.New(t)
	scope := f.Scope("asset.liveness")
	calls := 0
	responseCode, responseBody := 200, `{"ok":true}`
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Path != "/backend-api/wham/usage" || r.Header.Get("Authorization") != "Bearer at-fixture" || r.ContentLength > 0 || strings.Contains(r.URL.String(), "rt-fixture") {
			t.Error("AT check used the wrong action or material")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(responseCode)
		w.Write([]byte(responseBody))
	}))
	defer upstream.Close()
	previous := http.DefaultTransport
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // isolated local TLS fixture
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", upstream.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections() }()
	m := &Manager{cfg: &settings.Settings{Python: "missing-python"}}
	value, err := m.executeCentral(context.Background(), centralTask(scope), nil, map[string]any{"document": map[string]any{"accounts": []any{map[string]any{"email": "fixture@example.invalid", "credentials": map[string]any{"access_token": "at-fixture", "refresh_token": "rt-fixture"}}}}, "recovery": map[string]any{"password": "must-not-be-used"}})
	if err != nil || value["liveness"] != "alive" || value["document"] != nil || calls != 1 {
		t.Fatal(value, err, calls)
	}
	for _, state := range []string{"banned", "workspace_deactivated", "needs_refresh"} {
		providerCode := map[string]string{"banned": "account_banned", "workspace_deactivated": "deactivated_workspace", "needs_refresh": "invalid_token"}[state]
		responseCode, responseBody = 401, `{"error":{"code":"`+providerCode+`"}}`
		value, err = m.executeCentral(context.Background(), centralTask(scope), nil, map[string]any{"document": map[string]any{"access_token": "at-fixture", "refresh_token": "rt-fixture"}})
		if err != nil || value["source_status"] != state || value["liveness"] != state {
			t.Fatal(value, err)
		}
	}
	if calls != 4 {
		t.Fatal("explicit rejection was retried", calls)
	}
}

func TestCentralCancelledBeforeStartDoesNotClaimOrExecute(t *testing.T) {
	f := assetwork.New(t)
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := &Manager{cfg: &settings.Settings{DataDir: root, Python: "missing-python"}, st: st, procs: map[string]*proc{}, stop: make(chan struct{})}
	task := centralTask(f.Scope("account.authorize"))
	if err = m.Submit(task); err != nil {
		t.Fatal(err)
	}
	if err = m.Stop(task.ID); err != nil {
		t.Fatal(err)
	}
	task, _ = st.GetTask(task.ID)
	f.Mode("absent")
	if err = m.ReplayCentral(task); err != nil {
		t.Fatal(err)
	}
	value, _ := st.CentralReceipt(task.ID)
	if value["state"] != "failed" || value["preflight"] != true || f.Calls("claim") != 0 || f.Calls("read") != 0 {
		t.Fatal(value)
	}
	if err = m.ReplayCentral(task); err != nil {
		t.Fatal("repeat ACK", err)
	}
}

func TestCentralRecoveryDoesNotRaceRunningExecution(t *testing.T) {
	f := assetwork.New(t)
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	task := centralTask(f.Scope("account.refresh"))
	st.CreateTask(task)
	st.MarkRunning(task.ID, 0)
	m := &Manager{cfg: &settings.Settings{DataDir: root}, st: st}
	// An administrative replay must refuse the live task, even when given a
	// stale stopped snapshot; otherwise it could publish a false unknown result.
	task.Status = "stopped"
	task.StartedAt = time.Now().Format(time.RFC3339)
	if err = m.ReplayCentral(task); err == nil {
		t.Fatal("running task was replayed")
	}
	if f.Calls("result") != 0 {
		t.Fatal("replay raced execution")
	}
}
