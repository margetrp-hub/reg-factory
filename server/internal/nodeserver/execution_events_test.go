package nodeserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestExecutionLogsSignedBoundedAndRedacted(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err = st.ReserveOperation("op-test", "work-task-test", "account.authorize", 1); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(store.Task{ID: "work-task-test", Script: "asset_ops"}); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"正在刷新原账号 🚀 <ready> & starting original refresh", "OAuth HTTP 401 password=never-show email=secret@example.test otp=123456", `{"refresh_token":"do-not-show","message":"retry unavailable"}`, "last persisted line"} {
		if err = st.AppendEvent("work-task-test", "stdout", line); err != nil {
			t.Fatal(err)
		}
	}
	key := bytes.Repeat([]byte("k"), 32)
	h := New(Config{NodeID: "node-sg-1", Key: key, ReceiptKey: key}, nil, st)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/tasks/:task_id/events", h.executionEvents)
	now := time.Now().Unix()
	p := map[string]any{"schema": "account-node.events-request.v1", "issuer": "work", "node_id": "node-sg-1", "operation_id": "op-test", "task_id": "task-test", "attempt_id": "attempt-test", "fence": 1, "command_sha256": strings.Repeat("a", 64), "node_task_id": "op-test", "request_id": "request-test", "issued_at": now, "expires_at": now + 120, "page": 1, "page_size": 2}
	call := func(payload map[string]any, origin string, want int) map[string]any {
		t.Helper()
		sig, _ := signWorkEvents(payload, key)
		raw, _ := json.Marshal(Document{Payload: payload, Signature: sig})
		req := httptest.NewRequest("POST", "/tasks/op-test/events", bytes.NewReader(raw))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	first := call(p, "", 200)
	payload := first["payload"].(map[string]any)
	sig, _ := signWorkEvents(payload, key)
	if sig != first["signature"] || payload["total"] != float64(4) || payload["has_more"] != true || payload["raw_output_available"] != false {
		t.Fatal(first)
	}
	raw, _ := json.Marshal(first)
	for _, secret := range []string{"never-show", "secret@example.test", "123456"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("log secret leaked")
		}
	}
	if !strings.Contains(string(raw), "OAuth HTTP 401") {
		t.Fatal("real execution detail discarded")
	}
	if !strings.Contains(string(raw), "正在刷新原账号 🚀") {
		t.Fatal("Unicode execution output was discarded")
	}
	p["page"] = 2
	second := call(p, "", 200)
	raw, _ = json.Marshal(second)
	if strings.Contains(string(raw), "do-not-show") || !strings.Contains(string(raw), "last persisted line") {
		t.Fatal("wrong second page")
	}
	p["task_id"] = "another-task"
	call(p, "", 403)
	p["task_id"] = "task-test"
	p["page_size"] = 51
	call(p, "", 403)
	p["page_size"] = 2
	call(p, "https://example.test", 403)
	p["expires_at"] = now - 1
	call(p, "", 403)
	task, _ := st.GetTask("work-task-test")
	if task.Status != "queued" {
		t.Fatal("log read executed or changed task")
	}
}

func TestExecutionLogSanitizerLimitsAndSensitiveForms(t *testing.T) {
	line := safeExecutionText(`Authorization: Bearer secret-value`)
	if strings.Contains(line, "secret-value") {
		t.Fatal(line)
	}
	line = safeExecutionText(`callback ?code=short-secret 密码=bad-value token=token-value`)
	for _, secret := range []string{"short-secret", "bad-value", "token-value"} {
		if strings.Contains(line, secret) {
			t.Fatal(line)
		}
	}
	if len([]rune(safeExecutionText(strings.Repeat("输出 ", 1000)))) > 2049 {
		t.Fatal("unbounded output")
	}
}

func TestExecutionLogsUnicodeSignatureMatchesWorkCanonical(t *testing.T) {
	payload := map[string]any{"page": 1, "items": []map[string]any{{"text": "正在刷新 🚀 <ready> &\nnext"}}}
	signature, err := signWorkEvents(payload, bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	// Independently calculated HMAC for sorted compact ASCII-escaped JSON,
	// without HTML escaping, under account-node-work-events-v1.
	if signature != "b85214f4b12145d0c405cc41baedc3d4e18529aed860bc7484b0823c58f262f5" {
		t.Fatalf("Unicode canonical signature mismatch: %s", signature)
	}
	if _, err := Canonical(payload); err == nil {
		t.Fatal("legacy task protocol ASCII restriction changed")
	}
}

// Optional cross-repository fixture: Work's actual executionEvents handler
// sends its signed reads here and verifies these actual SG handler responses.
func TestExecutionLogsInteropServer(t *testing.T) {
	addr := os.Getenv("SG_WORK_EVENTS_FIXTURE_ADDR")
	if addr == "" {
		t.Skip("cross-repository fixture not requested")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "interop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	operation := "11111111-1111-4111-8111-111111111111"
	task := "work-22222222-2222-4222-8222-222222222222"
	if _, _, err = st.ReserveOperation(operation, task, "account.refresh", 1); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(store.Task{ID: task, Script: "asset_ops"}); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"正在刷新 🚀 <ready> &\nnext", "password=never-show token=hidden-value", "已完成"} {
		if err = st.AppendEvent(task, "stdout", line); err != nil {
			t.Fatal(err)
		}
	}
	h := New(Config{NodeID: "account-1", Key: bytes.Repeat([]byte("x"), 32), ReceiptKey: bytes.Repeat([]byte("y"), 32)}, nil, st)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/internal/work/v1/tasks/:task_id/events", h.executionEvents)
	stopped := make(chan struct{})
	r.POST("/fixture/stop", func(c *gin.Context) { c.Status(204); close(stopped) })
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(r)
	server.Listener.Close()
	server.Listener = listener
	server.StartTLS()
	defer server.Close()
	fmt.Println("SG_INTEROP_READY", server.URL)
	select {
	case <-stopped:
	case <-time.After(120 * time.Second):
		t.Fatal("interop caller did not finish")
	}
}
