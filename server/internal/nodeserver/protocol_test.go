package nodeserver

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func testPayload(now time.Time) map[string]any {
	return map[string]any{
		"schema":           WorkSchema,
		"issuer":           "team-platform",
		"node_id":          "node-sg-1",
		"operation_id":     "op-0001",
		"task_id":          "task-0001",
		"attempt_id":       "att-1",
		"fence":            int64(3),
		"action":           "account.register",
		"subject_kind":     "mailbox",
		"subject_id":       "mbx-0001",
		"resource_version": int64(1),
		"issued_at":        now.Unix(),
		"expires_at":       now.Unix() + 300,
		"parameters":       map[string]any{"goal": "register"},
		"command_sha256":   strings.Repeat("a", 64),
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	now := time.Now()
	payload := testPayload(now)
	sig, err := Sign(payload, key, false)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	verified, err := Verify(Document{Payload: payload, Signature: sig}, key, "node-sg-1", now)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if verified.Action != "account.register" || verified.Subject != "mbx-0001" {
		t.Fatalf("unexpected verified: %+v", verified)
	}
}

func TestVerifyAcceptsRefreshCustodyGrant(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	now := time.Now()
	payload := testPayload(now)
	payload["action"] = "account.refresh"
	payload["subject_kind"] = "account"
	payload["subject_id"] = "account-0001"
	payload["parameters"] = map[string]any{
		"custody_grant": map[string]any{
			"payload":   map[string]any{"purpose": "intake"},
			"signature": strings.Repeat("a", 64),
		},
	}
	sig, err := Sign(payload, key, false)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := Verify(Document{Payload: payload, Signature: sig}, key, "node-sg-1", now); err != nil {
		t.Fatalf("refresh custody grant should be accepted by the Work envelope: %v", err)
	}
}

func TestVerifyRejects(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	now := time.Now()

	cases := []struct {
		name    string
		mutate  func(map[string]any) map[string]any
		nodeID  string
		verKey  []byte
		wantErr string
	}{
		{
			name:   "wrong node",
			mutate: func(p map[string]any) map[string]any { return p },
			nodeID: "other-node", verKey: key, wantErr: "WORK_TARGET_INVALID",
		},
		{
			name:   "expired",
			mutate: func(p map[string]any) map[string]any { p["expires_at"] = now.Unix() - 10; return p },
			nodeID: "node-sg-1", verKey: key, wantErr: "WORK_AUTHORIZATION_EXPIRED",
		},
		{
			name: "unknown param",
			mutate: func(p map[string]any) map[string]any {
				p["parameters"] = map[string]any{"goal": "register", "evil": true}
				return p
			},
			nodeID: "node-sg-1", verKey: key, wantErr: "WORK_PARAMETERS_INVALID",
		},
		{
			name:   "wrong subject kind",
			mutate: func(p map[string]any) map[string]any { p["subject_kind"] = "account"; return p },
			nodeID: "node-sg-1", verKey: key, wantErr: "WORK_SUBJECT_INVALID",
		},
	}
	for _, tc := range cases {
		payload := tc.mutate(testPayload(now))
		sig, err := Sign(payload, key, false)
		if err != nil {
			t.Fatalf("%s: sign: %v", tc.name, err)
		}
		_, err = Verify(Document{Payload: payload, Signature: sig}, tc.verKey, tc.nodeID, now)
		if err == nil {
			t.Fatalf("%s: 期望失败", tc.name)
		}
		if proto, ok := err.(*ProtocolError); !ok || proto.Code != tc.wantErr {
			t.Fatalf("%s: 期望 %s，实际 %v", tc.name, tc.wantErr, err)
		}
	}
}

func TestReceiptSignature(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	now := time.Now()
	doc, err := Receipt("task-0001", "node-sg-1", "succeeded", 0, nil, key, now)
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	payload := doc["payload"].(map[string]any)
	if payload["schema"] != ReceiptSchema {
		t.Fatalf("schema: %v", payload["schema"])
	}
	sig, _ := Sign(payload, key, true)
	if sig != doc["signature"] {
		t.Fatalf("回执签名应与 read domain 一致")
	}
}
