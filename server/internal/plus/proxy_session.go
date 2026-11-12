package plus

// 这里保留挂在 *Session 上的出口相关方法。
//
// 数据结构和解析逻辑已经迁到 internal/egress，但方法需要 Session 的
// client / Proxy / Fingerprint 等字段，所以留在这个包里。

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/tiantianGPU/reg-factory/server/internal/egress"
)

// ---------------------------------------------------------------- 出口校验

// ProbeEgress 探测当前会话的出口信息。
func (s *Session) ProbeEgress() (*egress.EgressInfo, error) {
	resp, err := s.getJSONLoose("https://ipinfo.io/json")
	if err != nil {
		return nil, fmt.Errorf("探测出口: %w", err)
	}
	info := &egress.EgressInfo{
		IP:      pickString(resp, "ip"),
		Country: strings.ToUpper(pickString(resp, "country")),
		City:    pickString(resp, "city"),
		Region:  pickString(resp, "region"),
		ISP:     pickString(resp, "org"),
	}
	if info.IP == "" {
		return nil, fmt.Errorf("探测出口: 响应缺少 ip 字段")
	}
	return info, nil
}

// VerifyEgress 校验出口是否符合预期国家。
func (s *Session) VerifyEgress(want string) (string, error) {
	want = strings.ToUpper(strings.TrimSpace(want))
	if want == "" {
		return "", nil
	}
	info, err := s.ProbeEgress()
	if err != nil {
		return "", err
	}
	if info.Country != want {
		return fmt.Sprintf("出口国家 %s ≠ 期望 %s（IP %s, %s）",
			info.Country, want, info.IP, info.ISP), nil
	}
	return "", nil
}

// ---------------------------------------------------------------- 代理 transport

// ---------------------------------------------------------------- 辅助

// getJSONLoose 发一个不依赖 session 默认头的 GET。
func (s *Session) getJSONLoose(rawURL string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "+
			"(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36")

	resp, err := s.client.Do(req)
	if err != nil {
		if isProxyConnError(err) {
			return nil, fmt.Errorf("代理连接失败: %w", err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var out map[string]any
	if err := decodeJSONBody(resp, &out); err != nil {
		return nil, err
	}
	return out, nil
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
