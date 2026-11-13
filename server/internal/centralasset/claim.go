package centralasset

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const claimDomain = "aishop-node-asset-claim-v1\n"

func claimSignature(value map[string]any, key string) string {
	// Claim fields and the scoped receipt are ASCII identifiers/integers.
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(claimDomain))
	mac.Write(bytes.TrimSuffix(raw.Bytes(), []byte("\n")))
	return hex.EncodeToString(mac.Sum(nil))
}

func number(v any) int64 {
	switch n := v.(type) {
	case float64:
		if n > 0 && n < 9007199254740991 && n == float64(int64(n)) {
			return int64(n)
		}
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		v, _ := n.Int64()
		return v
	}
	return 0
}

func (c Config) claim(ctx context.Context, attempt string) (map[string]any, error) {
	now := time.Now().Unix()
	p := map[string]any{"schema": "aishop.node-asset-claim.v1", "node_id": c.NodeID, "center_id": c.CenterID,
		"attempt_id": attempt, "request_id": uuid.NewString(), "issued_at": now, "expires_at": now + 120}
	raw, _ := json.Marshal(map[string]any{"payload": p, "signature": claimSignature(p, c.ReceiptKey)})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Origin, "/")+"/internal/work/v1/asset-claim", bytes.NewReader(raw))
	if err != nil {
		return nil, errors.New("ASSET_CLAIM_REQUEST_INVALID")
	}
	request.Header.Set("Content-Type", "application/json")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("ASSET_RELAY_UNAVAILABLE")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("ASSET_CLAIM_UNAVAILABLE")
	}
	raw, err = io.ReadAll(io.LimitReader(response.Body, 380*1024+1))
	var doc map[string]any
	if err != nil || len(raw) > 380*1024 || json.Unmarshal(raw, &doc) != nil || len(doc) != 2 {
		return nil, errors.New("ASSET_CLAIM_RECEIPT_INVALID")
	}
	r, ok := doc["payload"].(map[string]any)
	sig, sigOK := doc["signature"].(string)
	if !ok || len(r) != 8 || !sigOK || !hmac.Equal([]byte(sig), []byte(claimSignature(r, c.NodeKey))) || r["schema"] != "aishop.node-asset-claim-receipt.v1" {
		return nil, errors.New("ASSET_CLAIM_RECEIPT_INVALID")
	}
	for _, k := range []string{"node_id", "center_id", "attempt_id", "request_id"} {
		if r[k] != p[k] {
			return nil, errors.New("ASSET_CLAIM_RECEIPT_INVALID")
		}
	}
	issued, expires := number(r["issued_at"]), number(r["expires_at"])
	result, ok := r["result"].(map[string]any)
	if !ok || issued < now-120 || issued > now+30 || expires <= now || expires > issued+120 {
		return nil, errors.New("ASSET_CLAIM_RECEIPT_INVALID")
	}
	return result, nil
}
