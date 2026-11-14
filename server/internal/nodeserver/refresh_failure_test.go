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
		{"totp-v2", "account.failure.v2", "mfa_totp_verification_failed", "stdout", "WORK_ACCOUNT_MFA_TOTP_VERIFICATION_FAILED", 1},
		{"unsupported-platform", "account.failure.v2", "platform_authorization_unsupported", "stdout", "WORK_ACCOUNT_PLATFORM_AUTHORIZATION_UNSUPPORTED", 1},
		{"email-code", "account.failure.v2", "email_otp_not_found", "stdout", "WORK_ACCOUNT_EMAIL_OTP_NOT_FOUND", 1},
		{"refresh-rejected", "account.failure.v2", "invalid_grant", "stdout", "WORK_ACCOUNT_INVALID_GRANT", 1},
		{"worker-exit", "account.failure.v2", "credential_worker_exited", "stdout", "WORK_ACCOUNT_CREDENTIAL_WORKER_EXITED", 1},
		{"invalid-v2-reason", "account.failure.v2", "provider-private", "stdout", "WORK_ACCOUNT_FAILURE_REASON_INVALID", 1},
		{"banned", "account.failure.v1", "account_banned", "stdout", "ACCOUNT_BANNED", 1},
		{"workspace", "account.failure.v1", "workspace_deactivated", "stdout", "WORKSPACE_DEACTIVATED", 1},
		{"workspace-alias", "account.failure.v1", "deactivated_workspace", "stdout", "DEACTIVATED_WORKSPACE", 1},
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
			// Work uses the operation ticket; runner events use the local task ID.
			// Keeping both identical hid lost failure reasons in real receipts.
			const ticket, localID = "failure-operation", "work-failure-operation"
			if err = st.CreateTask(store.Task{ID: localID, Script: "asset_ops"}); err != nil {
				t.Fatal(err)
			}
			if _, _, err = st.ReserveOperation(ticket, localID, "account.authorize", 1); err != nil {
				t.Fatal(err)
			}
			line, _ := json.Marshal(map[string]any{"result": "failed", "error_schema": sample.schema, "error_code": sample.code, "message": "provider-private"})
			if err = st.AppendEvent(localID, sample.stream, string(line)); err != nil {
				t.Fatal(err)
			}
			h := New(Config{NodeID: "fixture-node"}, nil, st)
			verified := &Verified{Action: "account.authorize", Subject: "fixture-subject", Payload: map[string]any{
				"subject_id": "fixture-subject", "resource_version": int64(1), "task_id": "failure-task",
				"operation_id": "failure-operation", "action": "account.authorize",
			}}
			key, now := bytes.Repeat([]byte("k"), 32), time.Now()
			doc, err := h.ReceiptFor(verified, ticket, "failed", sample.exit, key, now)
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
			if err = st.AppendEvent(localID, "stdout", "process finished"); err != nil {
				t.Fatal(err)
			}
			replayed, err := h.ReceiptFor(verified, ticket, "failed", sample.exit, key, now.Add(time.Second))
			if err != nil || replayed["payload"].(map[string]any)["result_sha256"] != payload["result_sha256"] {
				t.Fatal("poll changed result", err)
			}
		})
	}
}
