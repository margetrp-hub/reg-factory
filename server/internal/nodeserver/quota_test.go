package nodeserver

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func quotaCall(t *testing.T, st *store.Store, payload map[string]any, path string) map[string]any {
	t.Helper()
	key := bytes.Repeat([]byte("k"), 32)
	h := New(Config{NodeID: "node-sg-1", Key: key, ReceiptKey: key, Platform: "chatgpt"}, nil, st)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/tasks", h.acceptTask)
	r.POST("/tasks/:task_id/result", h.taskReceipt)
	r.POST("/resolve", h.resolveTask)
	sig, err := Sign(payload, key, path != "/tasks")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(Document{Payload: payload, Signature: sig})
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest("POST", path, bytes.NewReader(raw)))
	if response.Code != 200 {
		t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if path != "/tasks" {
		expected, err := Sign(result["payload"].(map[string]any), key, false)
		if err != nil || expected != result["signature"] {
			t.Fatal("invalid signed receipt")
		}
	}
	return result
}

func quotaPayload() map[string]any {
	p := testPayload(time.Now())
	p["action"], p["subject_kind"], p["subject_id"] = "account.quota", "account", "account-0001"
	p["parameters"] = map[string]any{}
	return p
}

func TestQuotaSnapshotSurvivesReplayInventoryChangeAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.UpsertAccounts([]store.AccountSeed{{Ref: "creds:test", Platform: "chatgpt"}}); err != nil {
		t.Fatal(err)
	}
	if err = st.MarkHealth("creds:test", "ok", ""); err != nil {
		t.Fatal(err)
	}
	p := quotaPayload()
	ack := quotaCall(t, st, p, "/tasks")
	if len(ack) != 6 || ack["quota"] != nil {
		t.Fatalf("nonstandard ACK: %v", ack)
	}
	first := quotaCall(t, st, p, "/tasks/op-0001/result")
	value := first["payload"].(map[string]any)
	if value["status"] != "completed" || value["artifact_available"] != false || value["result_resource_version"] != nil || value["result_account_id"] != nil {
		t.Fatalf("quota invented account side effect: %v", value)
	}
	if value["result"].(map[string]any)["observation"].(map[string]any)["available"] != float64(1) {
		t.Fatal("quota snapshot missing")
	}
	if err = st.MarkHealth("creds:test", "expired", ""); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ack, quotaCall(t, st, p, "/tasks")) {
		t.Fatal("duplicate acceptance changed")
	}
	if !reflect.DeepEqual(first, quotaCall(t, st, p, "/resolve")) {
		t.Fatal("inventory change mutated original result")
	}
	account, err := st.GetAccount("creds:test")
	if err != nil || account.Version != 1 {
		t.Fatal("quota advanced account version")
	}
	st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !reflect.DeepEqual(first, quotaCall(t, st, p, "/tasks/op-0001/result")) {
		t.Fatal("restart changed original receipt")
	}
}

func TestLegacyQuotaWithoutTaskRecoversCancelledWithoutRerunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ReserveOperation("op-0001", "work-task-0001", "account.quota", 1); err != nil {
		t.Fatal(err)
	}
	// Other operations without a task must not be synthesized or cancelled.
	if _, _, err = st.ReserveOperation("op-refresh", "work-refresh", "account.authorize", 1); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := quotaPayload()
	first := quotaCall(t, st, p, "/resolve")
	value := first["payload"].(map[string]any)
	if value["status"] != "cancelled" || value["result"].(map[string]any)["error_code"] != "WORK_QUOTA_LEGACY_CANCELLED" {
		t.Fatalf("legacy state not closed: %v", value)
	}
	if value["result_resource_version"] != nil || value["artifact_available"] != false {
		t.Fatal("cancelled quota fabricated result")
	}
	quotaCall(t, st, p, "/tasks")
	if !reflect.DeepEqual(first, quotaCall(t, st, p, "/tasks/op-0001/result")) {
		t.Fatal("duplicate resumed cancelled quota")
	}
	if _, err := st.GetTask("work-refresh"); err == nil {
		t.Fatal("migration touched mutation operation")
	}
	if err = st.RecoverLegacyQuota(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, quotaCall(t, st, p, "/resolve")) {
		t.Fatal("recovery changed terminal receipt")
	}
}
