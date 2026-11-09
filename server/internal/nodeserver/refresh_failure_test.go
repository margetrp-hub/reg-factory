package nodeserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestRefreshFailureReceiptReasonAndHistoricalDigest(t *testing.T) {
	for _, sample := range []struct {
		name, schema, code, stream, expected string
		exit                                 int
	}{
		{"new", "account.failure.v1", "mfa_totp_verification_failed", "stdout", "WORK_ACCOUNT_MFA_TOTP_VERIFICATION_FAILED", 1},
		{"legacy", "", "mfa_totp_verification_failed", "stdout", "WORK_ACCOUNT_REFRESH_FAILED", 1},
		{"untrusted", "account.failure.v1", "provider-private", "stdout", "WORK_ACCOUNT_REFRESH_FAILED", 1},
		{"stderr", "account.failure.v1", "mfa_totp_verification_failed", "stderr", "WORK_ACCOUNT_REFRESH_FAILED", 1},
		{"unconfirmed", "account.failure.v1", "mfa_totp_verification_failed", "stdout", "WORK_ACCOUNT_MFA_TOTP_VERIFICATION_FAILED", 75},
	} {
		t.Run(sample.name, func(t *testing.T) {
			st, err := store.Open(filepath.Join(t.TempDir(), "node.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err = st.CreateTask(store.Task{ID: "failure-task", Script: "asset_ops"}); err != nil {
				t.Fatal(err)
			}
			line, _ := json.Marshal(map[string]any{"result": "failed", "error_schema": sample.schema, "error_code": sample.code, "message": "provider-private"})
			if err = st.AppendEvent("failure-task", sample.stream, string(line)); err != nil {
				t.Fatal(err)
			}
			h := New(Config{NodeID: "fixture-node"}, nil, st)
			verified := &Verified{Action: "account.authorize", Subject: "fixture-subject", Payload: map[string]any{
				"subject_id": "fixture-subject", "resource_version": int64(1), "task_id": "failure-task",
				"operation_id": "failure-operation", "action": "account.authorize",
			}}
			key, now := bytes.Repeat([]byte("k"), 32), time.Now()
			doc, err := h.ReceiptFor(verified, "failure-task", "failed", sample.exit, key, now)
			if err != nil {
				t.Fatal(err)
			}
			payload := doc["payload"].(map[string]any)
			if payload["result"].(map[string]any)["error_code"] != sample.expected || payload["artifact_available"] != false || payload["result_resource_version"] != nil {
				t.Fatal("incorrect failure evidence", payload)
			}
			if sample.exit == 75 && payload["status"] != "failed" {
				t.Fatal("unconfirmed result did not end as failure", payload)
			}
			raw, _ := json.Marshal(doc)
			if strings.Contains(string(raw), "provider-private") {
				t.Fatal("provider details leaked")
			}
			if sample.name == "legacy" {
				old := h.fullResult(verified, "failed", 1)
				old["error_code"] = "WORK_ACCOUNT_REFRESH_FAILED"
				canonical, err := Canonical(old)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(canonical)
				if payload["result_sha256"] != hex.EncodeToString(digest[:]) {
					t.Fatal("historical result digest changed")
				}
			}
			if err = st.AppendEvent("failure-task", "stdout", "process finished"); err != nil {
				t.Fatal(err)
			}
			replayed, err := h.ReceiptFor(verified, "failure-task", "failed", sample.exit, key, now.Add(time.Second))
			if err != nil || replayed["payload"].(map[string]any)["result_sha256"] != payload["result_sha256"] {
				t.Fatal("poll changed result", err)
			}
		})
	}
}
