package nodeserver

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestLibraryCanonicalShape 打印 library 请求的 canonical 字节与签名，
// 用于与 Team 侧实现对齐（python/ps 容易在数字/顺序上产生差异）。
func TestLibraryCanonicalShape(t *testing.T) {
	key := []byte("363acbf9949faa9760c039df76fa6023cd6a7a862d53c3e5c39a2649e430cc45")
	now := time.Unix(1762300000, 0)

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
	raw, err := Canonical(payload)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	t.Logf("canonical = %s", string(raw))

	sig, err := signLibrary(payload, key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	t.Logf("signature = %s", sig)

	// 反序列化后的数字是 float64，验证与 int64 版本签名一致
	var round map[string]any
	if err := jsonUnmarshal(raw, &round); err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	raw2, _ := Canonical(round)
	t.Logf("roundtrip canonical = %s", string(raw2))
	sig2, _ := signLibrary(round, key)
	t.Logf("roundtrip signature = %s (match=%v)", sig2, sig == sig2)

	_ = hex.EncodeToString
	_ = fmt.Sprintf
	_ = now
}

func jsonUnmarshal(raw []byte, target any) error {
	return json.Unmarshal(raw, target)
}
