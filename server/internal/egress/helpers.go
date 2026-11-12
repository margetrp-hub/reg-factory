package egress

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// IsDatacenterISP 粗判机房 ISP。
//
// ipinfo 的 org 字段形如 "AS31898 Oracle Corporation"、
// "AS15169 Google LLC"、"AS13335 Cloudflare, Inc."。
func IsDatacenterISP(isp string) bool {
	low := strings.ToLower(isp)
	for _, k := range []string{
		"oracle", "amazon", "aws", "google", "microsoft", "azure",
		"cloudflare", "digitalocean", "linode", "vultr", "hetzner",
		"ovh", "contabo", "hosting", "datacenter", "data center",
		"server", "cloud", "vps", "colo",
	} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// Truncate 截断字符串用于日志。
func Truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------- JSON 包装

// jsonUnmarshal 是 json.Unmarshal 的薄包装。
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// jsonMarshal 是 json.Marshal 的薄包装。
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

// ---------------------------------------------------------------- UUID

// newUUIDInternal 生成 v4 UUID。
func newUUIDInternal() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// RandHex 生成 n 位十六进制随机串。
func RandHex(n int) string {
	const d = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = d[randIntn(16)]
	}
	return string(b)
}

// PickString 按顺序取第一个非空字符串值。
func PickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------- 阶段

// randIntn 是 math/rand.Intn 的包装。
func randIntn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(time.Now().UnixNano()>>3) % n
}
