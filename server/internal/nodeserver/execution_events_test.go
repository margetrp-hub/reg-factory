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
	databasePath := filepath.Join(t.TempDir(), "node.db")
	st, err := store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	operation, taskID, attempt := SubjectID("op-test"), SubjectID("task-test"), SubjectID("attempt-test")
	localID := "work-" + taskID
	if _, _, err = st.ReserveOperation(operation, localID, "account.authorize", 1); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTask(store.Task{ID: localID, Script: "asset_ops"}); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"正在刷新原账号 🚀 <ready> & starting original refresh", "OAuth HTTP 401 password=never-show email=secret@example.test otp=123456", `{"refresh_token":"do-not-show","message":"retry unavailable"}`, "last persisted line"} {
		if err = st.AppendEvent(localID, "stdout", line); err != nil {
			t.Fatal(err)
		}
	}
	if err = st.AppendEvent(localID, "stage", `{"stage":"oauth_started","status":"unknown","error_code":"PRODUCTION_OAUTH_RESULT_UNKNOWN","next_action":"授权诊断 oauth_cloudflare_challenge；页面 edge-challenge；HTTP 403"}`); err != nil {
		t.Fatal(err)
	}
	if err = st.AppendEvent(localID, "stage", `{"stage":"oauth_started","status":"waiting_user","action":"原账号继续；password=private-history HTTP 503"}`); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte("k"), 32)
	h := New(Config{NodeID: "node-sg-1", Key: key, ReceiptKey: key}, nil, st)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/tasks/:task_id/events", h.executionEvents)
	now := time.Now().Unix()
	p := map[string]any{"schema": "account-node.events-request.v1", "issuer": "work", "node_id": "node-sg-1", "operation_id": operation, "task_id": taskID, "attempt_id": attempt, "fence": 1, "command_sha256": strings.Repeat("a", 64), "node_task_id": operation, "request_id": SubjectID("request-test"), "issued_at": now, "expires_at": now + 120, "page": 1, "page_size": 2}
	if err := h.bindExecutionEvents(&Verified{Payload: p}, localID); err != nil {
		t.Fatal(err)
	}
	call := func(payload map[string]any, origin string, want int) map[string]any {
		t.Helper()
		sig, _ := signWorkEvents(payload, key)
		raw, _ := json.Marshal(Document{Payload: payload, Signature: sig})
		req := httptest.NewRequest("POST", "/tasks/"+operation+"/events", bytes.NewReader(raw))
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
	if sig != first["signature"] || payload["total"] != float64(6) || payload["has_more"] != true || payload["raw_output_available"] != false {
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
	call(p, "", 409)
	st.Close()
	st, err = store.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	call(p, "", 409)
	// Every scope field must match even when the caller knows the signing key.
	for key, value := range map[string]any{"issuer": "another-work", "node_id": "another-node", "task_id": SubjectID("foreign-task"), "attempt_id": SubjectID("foreign-attempt"), "fence": 2, "command_sha256": strings.Repeat("b", 64)} {
		original := p[key]
		p[key] = value
		p["request_id"] = SubjectID("foreign-" + key)
		call(p, "", 403)
		p[key] = original
	}
	p["request_id"] = SubjectID("second-page")
	p["page"] = 2
	second := call(p, "", 200)
	raw, _ = json.Marshal(second)
	if strings.Contains(string(raw), "do-not-show") || !strings.Contains(string(raw), "last persisted line") {
		t.Fatal("wrong second page")
	}
	p["page"] = 3
	p["request_id"] = SubjectID("third-page")
	third := call(p, "", 200)
	raw, _ = json.Marshal(third)
	for _, diagnostic := range []string{"oauth_cloudflare_challenge", "edge-challenge", "HTTP 403", "原账号继续", "HTTP 503"} {
		if !strings.Contains(string(raw), diagnostic) {
			t.Fatal("production recovery diagnostic was discarded", string(raw))
		}
	}
	if strings.Contains(string(raw), "private-history") {
		t.Fatal("historical guidance was not redacted")
	}
	p["task_id"] = "another-task"
	call(p, "", 403)
	p["task_id"] = taskID
	p["page_size"] = 51
	call(p, "", 403)
	p["page_size"] = 2
	call(p, "https://example.test", 403)
	p["expires_at"] = now - 1
	call(p, "", 403)
	task, _ := st.GetTask(localID)
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

func TestProductionEventsRestoreOriginalScopeAndUseOperationTask(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "production.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := bytes.Repeat([]byte("s"), 32)
	h := New(Config{Enabled: true, NodeID: "sg", Key: key, ReceiptKey: key}, nil, db)
	router := gin.New()
	h.Register(router)
	op, task, slot := SubjectID("original-production"), SubjectID("different-task"), SubjectID("slot")
	params := map[string]any{"production_ref": slot, "order_ref": SubjectID("order"), "product_id": 34, "seat_type": "standard", "workspace_ids": []any{}, "mode": "produce", "source_ref": "", "source_sha256": "", "budget_units": 0}
	now := time.Now().Unix()
	p := map[string]any{"schema": WorkSchema, "issuer": "work", "node_id": "sg", "operation_id": op, "task_id": task, "attempt_id": SubjectID("attempt"), "fence": 1, "action": productionAction, "subject_kind": "production", "subject_id": slot, "resource_version": 1, "issued_at": now, "expires_at": now + 120, "parameters": params, "command_sha256": productionDigest(params)}
	// An admission from before event bindings existed still retains the full
	// original request digest. Restore only through the normal signed read.
	v := &Verified{Payload: p}
	if err := db.ReserveProductionAdmission(op, productionScope(v), string(mustCanonicalForEventTest(t, params))); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendEvent("work-"+op, "system", "等待原生产结果"); err != nil {
		t.Fatal(err)
	}
	post := func(path string, doc any, want int) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(doc)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", path, bytes.NewReader(raw)))
		if w.Code != want {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return out
	}
	read := map[string]any{}
	for _, field := range []string{"issuer", "node_id", "operation_id", "task_id", "attempt_id", "fence", "command_sha256", "issued_at", "expires_at"} {
		read[field] = p[field]
	}
	read["schema"], read["node_task_id"], read["request_id"], read["page"], read["page_size"] = "account-node.events-request.v1", op, SubjectID("logs"), 1, 50
	sig, _ := signWorkEvents(read, key)
	post("/internal/work/v1/tasks/"+op+"/events", Document{Payload: read, Signature: sig}, 404)
	p["attempt_id"] = SubjectID("foreign-attempt")
	sig, _ = Sign(p, key, true)
	post("/internal/work/v1/resolve", Document{Payload: p, Signature: sig}, 409)
	if _, _, err := db.WorkEventScope(op); err == nil {
		t.Fatal("foreign read established original scope")
	}
	p["attempt_id"] = read["attempt_id"]
	sig, _ = Sign(p, key, true)
	post("/internal/work/v1/resolve", Document{Payload: p, Signature: sig}, 200)
	sig, _ = signWorkEvents(read, key)
	out := post("/internal/work/v1/tasks/"+op+"/events", Document{Payload: read, Signature: sig}, 200)
	if out["payload"].(map[string]any)["total"] != float64(1) {
		t.Fatal(out)
	}
	if db.HasProductionRun(op) {
		t.Fatal("reading production logs started an executor")
	}
}

func mustCanonicalForEventTest(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := Canonical(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAuthorizationDiagnosticPreservesRetryAndDropsProviderSecrets(t *testing.T) {
	line := safeExecutionText(`[reauthorize] {"stage":"authorize","status":"retrying","error_code":"oauth_tls_handshake_failed","page_type":"login-check","http_status":0,"token_exchange_started":false,"attempt":1,"max_attempts":3,"retry_in_seconds":1,"password":"private-password","message":"private-provider-body"}`)
	for _, want := range []string{"oauth_tls_handshake_failed", "第1/3次", "1秒后自动重试", "尚未交换凭据"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %s in %s", want, line)
		}
	}
	for _, secret := range []string{"private-password", "private-provider-body", "HTTP 302"} {
		if strings.Contains(line, secret) {
			t.Fatal("invalid diagnostic", line)
		}
	}
	old := safeExecutionText(`[reauthorize] {"stage":"authorize","error_code":"login_continue_failed","page_type":"log-in","http_status":302,"token_exchange_started":false}`)
	if !strings.Contains(old, "login_continue_failed") || !strings.Contains(old, "HTTP 302") {
		t.Fatal(old)
	}
	malformed := safeExecutionText(`[reauthorize] {"stage":"authorize","error_code":"oauth_tls_handshake_failed","page_type":"password=private-page","http_status":999999,"token_exchange_started":false}`)
	if strings.Contains(malformed, "private-page") || strings.Contains(malformed, "999999") {
		t.Fatal(malformed)
	}
	unknown := safeExecutionText(`[reauthorize] {"stage":"authorize","error_code":"password=private-error","token":"private-token","token_exchange_started":false}`)
	if strings.Contains(unknown, "private-error") || strings.Contains(unknown, "private-token") {
		t.Fatal(unknown)
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
	assignment := map[string]any{"issuer": "work-main", "node_id": "account-1", "operation_id": operation, "task_id": strings.TrimPrefix(task, "work-"), "attempt_id": "33333333-3333-4333-8333-333333333333", "fence": 1, "command_sha256": strings.Repeat("a", 64)}
	if err := h.bindExecutionEvents(&Verified{Payload: assignment}, task); err != nil {
		t.Fatal(err)
	}
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
