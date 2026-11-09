package nodeserver

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestReceiptForRefreshUsesDurableVersionOnReplay(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.UpsertAccounts([]store.AccountSeed{{Ref: "creds:test", Identity: "test@example.com"}}); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte("k"), 32)
	if _, fresh, err := st.ReserveOperation("op-refresh", "work-task", "account.refresh", 1); err != nil || !fresh {
		t.Fatalf("reserve: fresh=%v err=%v", fresh, err)
	}
	now := time.Now()
	payload := map[string]any{
		"schema": WorkSchema, "issuer": "team-platform", "node_id": "node-sg-1",
		"operation_id": "op-refresh", "task_id": "task-refresh", "attempt_id": "attempt-refresh",
		"fence": int64(1), "action": "account.refresh", "subject_kind": "account",
		"subject_id": SubjectID("creds:test"), "resource_version": int64(1),
		"issued_at": now.Unix(), "expires_at": now.Unix() + 300,
		"parameters": map[string]any{}, "command_sha256": bytes.Repeat([]byte("a"), 32),
	}
	verified := &Verified{Payload: payload, Action: "account.refresh", Subject: payload["subject_id"].(string), Params: map[string]any{}}
	first, err := New(Config{NodeID: "node-sg-1", Key: key}, nil, st).ReceiptFor(verified, "task-refresh", "succeeded", 0, key, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(Config{NodeID: "node-sg-1", Key: key}, nil, st).ReceiptFor(verified, "task-refresh", "succeeded", 0, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if first["payload"].(map[string]any)["result_resource_version"] != int64(2) || second["payload"].(map[string]any)["result_resource_version"] != int64(2) {
		t.Fatalf("refresh receipt did not reuse durable version: first=%v second=%v", first, second)
	}
	account, err := st.GetAccount("creds:test")
	if err != nil {
		t.Fatal(err)
	}
	if account.Version != 2 {
		t.Fatalf("refresh receipt advanced account twice: %d", account.Version)
	}
}

func TestRefreshUnknownExitPreservesUnknownWithoutAdvancingSource(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UpsertAccounts([]store.AccountSeed{{Ref: "creds:test", Identity: "test@example.com"}}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{NodeID: "node-sg-1"}, nil, st)
	verified := &Verified{Action: "account.authorize", Subject: SubjectID("creds:test"), Payload: map[string]any{
		"subject_id": SubjectID("creds:test"), "resource_version": int64(1), "task_id": "unknown-task",
		"operation_id": "unknown-operation", "action": "account.authorize",
	}}
	for _, sample := range []struct {
		exit     int
		expected string
	}{{75, "unknown"}, {1, "failed"}} {
		doc, err := h.ReceiptFor(verified, "unknown-task", "failed", sample.exit, bytes.Repeat([]byte("k"), 32), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		payload := doc["payload"].(map[string]any)
		if payload["status"] != sample.expected || payload["artifact_available"] != false || payload["result_resource_version"] != nil {
			t.Fatalf("unexpected uncertain result: %v", payload)
		}
		if sample.expected == "failed" && payload["result"].(map[string]any)["error_code"] != "WORK_ACCOUNT_REFRESH_FAILED" {
			t.Fatal("terminal failure has no actionable error code", payload)
		}
	}
	account, err := st.GetAccount("creds:test")
	if err != nil || account.Version != 1 {
		t.Fatalf("unknown changed source version: %v %v", account, err)
	}
}

func TestReceiptForVersionConflictReturnsTerminalEvidence(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.UpsertAccounts([]store.AccountSeed{{Ref: "creds:test", Identity: "test@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if err = st.AdvanceAccountVersion("creds:test", 1); err != nil {
		t.Fatal(err)
	}
	if err = st.AdvanceAccountVersion("creds:test", 2); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := st.ReserveOperation("op-conflict", "work-conflict", "account.refresh", 1); err != nil || !fresh {
		t.Fatalf("reserve: fresh=%v err=%v", fresh, err)
	}
	now := time.Now()
	payload := map[string]any{
		"schema": WorkSchema, "issuer": "team-platform", "node_id": "node-sg-1",
		"operation_id": "op-conflict", "task_id": "task-conflict", "attempt_id": "attempt-conflict",
		"fence": int64(1), "action": "account.refresh", "subject_kind": "account",
		"subject_id": SubjectID("creds:test"), "resource_version": int64(1),
		"issued_at": now.Unix(), "expires_at": now.Unix() + 300,
		"parameters": map[string]any{}, "command_sha256": bytes.Repeat([]byte("a"), 32),
	}
	verified := &Verified{Payload: payload, Action: "account.refresh", Subject: payload["subject_id"].(string), Params: map[string]any{}}
	doc, err := New(Config{NodeID: "node-sg-1", Key: bytes.Repeat([]byte("k"), 32)}, nil, st).ReceiptFor(verified, "task-conflict", "succeeded", 0, bytes.Repeat([]byte("k"), 32), now)
	if err != nil {
		t.Fatal(err)
	}
	value := doc["payload"].(map[string]any)
	if value["status"] != "failed" || value["artifact_available"] != false || value["result_resource_version"] != nil {
		t.Fatalf("version conflict did not become terminal evidence: %v", value)
	}
	if value["result"].(map[string]any)["error_code"] != "WORK_ACCOUNT_VERSION_CONFLICT" {
		t.Fatalf("missing conflict evidence: %v", value["result"])
	}
}
