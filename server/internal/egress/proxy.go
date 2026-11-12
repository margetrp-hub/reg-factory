package egress

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ProxySpec 描述一个出口代理。
//
// 支持 http / https / socks5 三种 scheme，与 httpcloak 的 ProxyConfig 对应。
type ProxySpec struct {
	// URL 完整代理地址，如 http://user:pass@host:3010
	// 也接受 socks5://host:1080
	URL string
	// Username / Password 可选，URL 里没带时用
	Username string
	Password string
	// Egress 期望的出口国家（如 "US"），用于取代理后校验
	Egress string
	// SessionID 粘性会话标识，同一 sid 复用同一出口
	SessionID string
}

// ParseProxyURL 把一段代理地址解析成 ProxySpec。
//
// 接受的形式：
//
//	http://user:pass@host:3010
//	socks5://host:1080
//	host:3010                       ← 无 scheme，按 http 处理
func ParseProxyURL(raw string) (ProxySpec, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ProxySpec{}, fmt.Errorf("代理地址为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ProxySpec{}, fmt.Errorf("解析代理地址: %w", err)
	}
	if u.Host == "" {
		return ProxySpec{}, fmt.Errorf("代理地址缺少 host: %s", raw)
	}

	spec := ProxySpec{URL: raw}
	if u.User != nil {
		spec.Username = u.User.Username()
		if p, ok := u.User.Password(); ok {
			spec.Password = p
		}
	}
	return spec, nil
}

// String 返回脱敏后的代理描述，用于日志。
func (p ProxySpec) String() string {
	if p.URL == "" {
		return "(直连)"
	}
	redacted := p.URL
	if i := strings.Index(redacted, "@"); i > 0 {
		if j := strings.Index(redacted, "://"); j >= 0 {
			redacted = redacted[:j+3] + "***@" + redacted[i+1:]
		}
	}
	if p.Egress != "" {
		return redacted + " [egress=" + p.Egress + "]"
	}
	return redacted
}

// IsDirect 判断是否直连。
func (p ProxySpec) IsDirect() bool {
	return strings.TrimSpace(p.URL) == ""
}

// EgressInfo 是一次出口探测的结果。
type EgressInfo struct {
	IP        string `json:"ip"`
	Country   string `json:"country"`
	City      string `json:"city"`
	Region    string `json:"region"`
	ISP       string `json:"org"`
	LatencyMs int64  `json:"-"`
}

// isProxyConnError 粗略判断是否为代理连接层错误。
func isProxyConnError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, k := range []string{"proxyconnect", "proxy", "socks", "connect tunnel"} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	return false
}
