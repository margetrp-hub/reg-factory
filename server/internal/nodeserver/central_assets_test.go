package nodeserver

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestCentralTaskReceiptPreservesUnknownAndOriginalRevision(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := []byte(strings.Repeat("k", 32))
	h := New(Config{NodeID: "sg", Key: key}, nil, st)
	stamp := time.Now().Unix()
	p := map[string]any{"schema": WorkSchema, "issuer": "work", "node_id": "sg", "operation_id": SubjectID("central-op"),
		"task_id": SubjectID("central-task"), "attempt_id": SubjectID("central-attempt"), "fence": float64(1), "action": "account.authorize",
		"subject_kind": "account", "subject_id": SubjectID("central-account"), "resource_version": float64(3),
		"issued_at": stamp, "expires_at": stamp + 600, "parameters": map[string]any{"asset_ref": map[string]any{"center_id": "center"}}, "command_sha256": strings.Repeat("a", 64)}
	sig, _ := Sign(p, key, false)
	v, err := Verify(Document{p, sig}, key, "sg", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	args, _ := json.Marshal([]string{"--assignment", string(raw)})
	id := "work-" + ls(p["operation_id"])
	if err = st.CreateTask(store.Task{ID: id, Script: "central_asset", ArgsJSON: string(args)}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.ReserveOperation(ls(p["operation_id"]), id, v.Action, 3); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "unknown", "completed"} {
		value := map[string]any{"state": state, "error_code": "", "result_id": SubjectID("central-result"), "credential_revision": 4}
		if err = st.SaveCentralReceipt(id, state, value); err != nil {
			t.Fatal(err)
		}
		doc, e := h.ReceiptFor(v, ls(p["operation_id"]), "interrupted", 75, key, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		receipt := doc["payload"].(map[string]any)
		expected := state
		if state == "pending" {
			expected = "running"
		}
		if receipt["status"] != expected || receipt["artifact_available"] != false {
			t.Fatal(receipt)
		}
		delivery := receipt["result"].(map[string]any)["asset_delivery"].(map[string]any)
		if delivery["execution_finished"] != true || delivery["result_id"] != SubjectID("central-result") {
			t.Fatal("durable result did not release execution capacity", delivery)
		}
		if state == "completed" && receipt["result_resource_version"] != float64(4) {
			t.Fatal(receipt)
		}
		if !strings.Contains(string(raw), "asset_ref") {
			t.Fatal("missing binding")
		}
	}
	if err = st.SaveCentralReceipt(id, "failed", map[string]any{"state": "failed"}); err == nil {
		t.Fatal("terminal overwrite")
	}
	p["subject_id"] = SubjectID("other-account")
	if _, err = h.centralReceipt(v, key); err == nil {
		t.Fatal("changed scope")
	}
}
