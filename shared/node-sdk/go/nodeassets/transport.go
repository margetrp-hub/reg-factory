package nodeassets

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const RelayDomain = "aishop-node-assets-relay-v1\n"
const RelayPath = "/internal/work/v1/assets"

// WorkSender uses the existing node receipt key to authenticate to Work.
// Credential encryption still uses the independent per-node asset key.
// Connection failure is returned once; it cannot trigger an external operation.
func WorkSender(origin, centerID string, receiptKey []byte) (Send, error) {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || strings.TrimSpace(origin) != origin || !identifier.MatchString(centerID) || len(receiptKey) < 32 {
		return nil, ErrScope
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Timeout: 20 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context, e Envelope) (Envelope, error) {
		id := make([]byte, 16)
		if _, err := rand.Read(id); err != nil {
			return Envelope{}, err
		}
		h := hex.EncodeToString(id)
		stamp := time.Now().Unix()
		p := map[string]any{"schema": "aishop.node-assets-relay.v1", "node_id": e.NodeID, "center_id": centerID,
			"request_id": fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:]), "issued_at": stamp, "expires_at": stamp + 120,
			"envelope": map[string]any{"schema": e.Schema, "node_id": e.NodeID, "sealed": e.Sealed}}
		raw, err := json.Marshal(p)
		if err != nil {
			return Envelope{}, ErrInvalid
		}
		mac := hmac.New(sha256.New, receiptKey)
		_, _ = mac.Write(append([]byte(RelayDomain), raw...))
		raw, err = json.Marshal(map[string]any{"payload": p, "signature": hex.EncodeToString(mac.Sum(nil))})
		if err != nil {
			return Envelope{}, ErrInvalid
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(origin, "/")+RelayPath, bytes.NewReader(raw))
		if err != nil {
			return Envelope{}, ErrInvalid
		}
		r.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(r)
		if err != nil {
			return Envelope{}, fmt.Errorf("ASSET_RELAY_UNAVAILABLE")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return Envelope{}, fmt.Errorf("ASSET_RELAY_HTTP_%d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 380*1024+1))
		if err != nil || len(body) > 380*1024 || uniqueJSON(body) != nil {
			return Envelope{}, ErrInvalid
		}
		var out Envelope
		d := json.NewDecoder(bytes.NewReader(body))
		d.DisallowUnknownFields()
		if err = d.Decode(&out); err != nil {
			return Envelope{}, ErrInvalid
		}
		return out, nil
	}, nil
}
