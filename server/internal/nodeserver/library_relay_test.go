package nodeserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

// TestLibraryRelaySignature 用固定输入产出 relay 域签名样例，
// Team 侧（Python）可据此自测实现是否一致。
func TestLibraryRelaySignature(t *testing.T) {
	key := []byte("363acbf9949faa9760c039df76fa6023cd6a7a862d53c3e5c39a2649e430cc45")
	payload := map[string]any{
		"schema":     "work.inventory-relay.v1",
		"node_id":    "node-sg-regfactory",
		"request_id": "lib-1762300000",
		"issued_at":  int64(1762300000),
		"expires_at": int64(1762300120),
		"data": map[string]any{
			"q": "", "status": "", "page": int64(1), "page_size": int64(50),
			"source": "account_center",
		},
	}
	body, err := Canonical(payload)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(relayDomain))
	mac.Write(body)
	t.Logf("canonical   = %s", string(body))
	t.Logf("relayDomain = %q", relayDomain)
	t.Logf("relay_sig   = %s", hex.EncodeToString(mac.Sum(nil)))

	// 反向验证：用同签名构造请求，verifyLibraryRequest 必须通过
	doc := map[string]any{
		"signature": hex.EncodeToString(mac.Sum(nil)),
		"payload":   payload,
	}
	if _, err := verifyLibraryRequest(doc, key, "node-sg-regfactory", time.Unix(1762300060, 0)); err != nil {
		t.Fatalf("verify 应该通过: %v", err)
	}
	t.Logf("verify      = ok")
}
