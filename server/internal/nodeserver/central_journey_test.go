package nodeserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/assetwork"
)

func centralJourneyStore(t *testing.T, root string) *store.Store {
	t.Helper()
	dsn := os.Getenv("REG_FACTORY_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = filepath.Join(root, "tasks.db")
	} else {
		u, err := url.Parse(dsn)
		if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "/sg_release_test" {
			t.Fatal("disposable local sg_release_test required")
		}
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("central_go_%x", time.Now().UnixNano())
		if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func waitCentral(t *testing.T, st *store.Store, m *runner.Manager, id string) store.Task {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		task, err := st.GetTask(id)
		if err == nil && task.Status != "queued" && task.Status != "running" && m.RunningCount() == 0 {
			return task
		}
	}
	logs, _ := st.TailEvents(id, 30)
	task, _ := st.GetTask(id)
	t.Fatalf("task not settled: %s %s logs=%v", task.Status, task.Error, logs)
	return store.Task{}
}

// Real Work ingress -> Go scheduler -> released Python executor -> local HTTP
// token server -> encrypted journal -> signed Work relay -> local transaction.
func TestCentralSignedIngressThroughPlatformExecutorAndRecovery(t *testing.T) {
	for _, mode := range []string{"normal", "lost", "offline", "auto", "stop", "nested", "account_banned", "workspace_deactivated", "invalid_grant"} {
		t.Run(mode, func(t *testing.T) {
			python := os.Getenv("REG_FACTORY_TEST_PYTHON")
			if python == "" {
				python = "python"
			}
			python, err := exec.LookPath(python)
			if err != nil {
				t.Fatal(err)
			}
			f := assetwork.New(t)
			if mode == "nested" {
				f.Document(map[string]any{"accounts": []any{map[string]any{"email": "fixture@example.invalid", "credentials": map[string]any{"access_token": "fixture-at", "refresh_token": "fixture-rt"}}}})
			}
			f.Mode(mode)
			if mode == "auto" {
				f.Mode("offline")
			}
			root := t.TempDir()
			st := centralJourneyStore(t, root)
			t.Setenv("REG_FACTORY_PRODUCTION_PROXY", "")
			var upstreamCalls atomic.Int32
			entered := make(chan struct{}, 1)
			token := "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"fixture@example.invalid"}`)) + ".fixture"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				r.ParseForm()
				if r.Method != "POST" || r.Form.Get("refresh_token") != "fixture-rt" {
					t.Error("unexpected upstream action")
					w.WriteHeader(400)
					return
				}
				select {
				case entered <- struct{}{}:
				default:
				}
				if mode == "stop" {
					select {
					case <-r.Context().Done():
					case <-time.After(10 * time.Second):
					}
					return
				}
				if mode == "account_banned" || mode == "workspace_deactivated" || mode == "invalid_grant" {
					w.WriteHeader(401)
					json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": mode}})
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"access_token": token, "refresh_token": "new-rt", "expires_in": 3600})
			}))
			defer upstream.Close()
			repo, _ := filepath.Abs("../../..")
			app := filepath.Join(root, "app")
			os.MkdirAll(filepath.Join(app, "tools"), 0700)
			shim := fmt.Sprintf("import sys\nsys.path.insert(0,%q)\nfrom tools import asset_ops,central_asset\nasset_ops.OPENAI_TOKEN_URL=%q\ncentral_asset.main()\n", repo, upstream.URL)
			if err = os.WriteFile(filepath.Join(app, "tools", "central_asset.py"), []byte(shim), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := &settings.Settings{DataDir: root, EnvFile: filepath.Join(root, ".env"), AppRoot: app, Python: python, MaxParallel: 2, GuardPct: 100}
			m := runner.New(cfg, st)
			defer func() { m.Shutdown(context.Background()) }()
			h := New(Config{Enabled: true, NodeID: "sg-test", Key: []byte(f.NodeKey), ReceiptKey: []byte(f.ReceiptKey), Platform: "chatgpt"}, m, st)
			r := gin.New()
			h.Register(r)
			scope := f.Scope("account.refresh")
			sig, _ := Sign(scope, []byte(f.NodeKey), false)
			raw, _ := json.Marshal(Document{scope, sig})
			id := "work-" + scope["operation_id"].(string)
			submit := func() {
				t.Helper()
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest("POST", "/internal/work/v1/tasks", bytes.NewReader(raw)))
				if w.Code != 200 && w.Code != 202 {
					t.Fatalf("submit: %d %s", w.Code, w.Body.String())
				}
			}
			submit()
			submit()
			if mode == "stop" {
				select {
				case <-entered:
				case <-time.After(15 * time.Second):
					t.Fatal("executor did not enter")
				}
				if err = m.Stop(id); err != nil {
					t.Fatal(err)
				}
			}
			task := waitCentral(t, st, m, id)
			want := "succeeded"
			if mode == "account_banned" || mode == "workspace_deactivated" || mode == "invalid_grant" {
				want = "failed"
			}
			if mode == "offline" || mode == "auto" || mode == "stop" {
				want = "waiting_asset"
			}
			if task.Status != want || upstreamCalls.Load() != 1 {
				logs, _ := st.TailEvents(id, 25)
				t.Fatalf("status=%s error=%s upstream=%d logs=%v", task.Status, task.Error, upstreamCalls.Load(), logs)
			}
			if mode == "offline" || mode == "auto" || mode == "stop" {
				m.Shutdown(context.Background())
				cfg.Python = filepath.Join(root, "missing-python")
				m = runner.New(cfg, st)
				h.mgr = m
				if m.CentralRuntimeCheck() != nil {
					t.Fatal("recovery readiness still depends on Python")
				}
				f.Mode("normal")
				if mode == "auto" {
					if _, err = st.WriteDB().Exec("UPDATE central_asset_receipts SET next_attempt=0 WHERE task_id=$1", id); err != nil {
						t.Fatal(err)
					}
					for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(30 * time.Millisecond) {
						observed, _ := st.GetTask(id)
						var ack int
						st.ReadDB().QueryRow("SELECT acknowledged FROM central_asset_receipts WHERE task_id=$1", id).Scan(&ack)
						if observed.Status == "succeeded" && ack == 1 {
							break
						}
					}
					observed, _ := st.GetTask(id)
					if observed.Status != "succeeded" {
						t.Fatal("automatic replay did not settle", observed.Status)
					}
				} else if err = m.ReplayCentral(task); err != nil {
					t.Fatal(err)
				}
				if mode == "stop" {
					unknown, _ := st.GetTask(id)
					if unknown.Status != "unknown" {
						t.Fatal("stopped action falsely settled", unknown.Status)
					}
					f.Mode("closed")
					if err = m.ReplayCentral(task); err != nil {
						t.Fatal(err)
					}
					want = "failed"
				} else {
					want = "succeeded"
				}
			}
			if err = m.ReplayCentral(task); err != nil {
				t.Fatal("repeat ACK", err)
			}
			final, _ := st.GetTask(id)
			if final.Status != want || upstreamCalls.Load() != 1 || f.Calls("claim") != 1 {
				t.Fatal("duplicate execution or wrong terminal", final.Status, upstreamCalls.Load(), f.Calls("claim"))
			}
			v, err := Verify(Document{scope, sig}, []byte(f.NodeKey), "sg-test", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := h.centralReceipt(v, []byte(f.ReceiptKey))
			if err != nil {
				t.Fatal(err)
			}
			p := receipt["payload"].(map[string]any)
			receiptSig, _ := Sign(p, []byte(f.ReceiptKey), false)
			if receipt["signature"] != receiptSig || p["attempt_id"] != scope["attempt_id"] {
				t.Fatal("unbound receipt")
			}
			if want == "succeeded" && p["result_resource_version"] != float64(4) {
				t.Fatal("wrong central version", p)
			}
			stored := f.Result(scope["attempt_id"].(string))
			if mode == "nested" && stored["document"].(map[string]any)["accounts"].([]any)[0].(map[string]any)["credentials"].(map[string]any)["refresh_token"] != "new-rt" {
				t.Fatal("nested credentials did not reach center")
			}
			if mode == "account_banned" || mode == "workspace_deactivated" || mode == "invalid_grant" {
				state := map[string]string{"account_banned": "banned", "workspace_deactivated": "workspace_deactivated", "invalid_grant": "unknown"}[mode]
				if stored["source_status"] != state || stored["document"] != nil || p["result_resource_version"] != nil {
					t.Fatal("failure lost explicit state or published credentials", stored, p)
				}
			}
			logs, _ := st.TailEvents(id, 100)
			logJSON, _ := json.Marshal(logs)
			if strings.Contains(string(logJSON), token) || strings.Contains(string(logJSON), "fixture-rt") || strings.Contains(string(logJSON), "new-rt") {
				t.Fatal("credentials leaked into execution log")
			}
			var acknowledged int
			st.ReadDB().QueryRow("SELECT acknowledged FROM central_asset_receipts WHERE task_id=$1", id).Scan(&acknowledged)
			if acknowledged != 1 {
				t.Fatal("local ACK incomplete")
			}
		})
	}
}
