package egress

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// ALPNMode 决定 TLS 层声明哪些 ALPN 协议。
type ALPNMode string

const (
	// ALPNH1 只声明 http/1.1。
	// net/http 的 h1 传输可以直接用，最稳，但真实浏览器不会这样声明
	ALPNH1 ALPNMode = "h1"
	// ALPNH2First 声明 ["h2", "http/1.1"]，与 Chrome 一致。
	// 服务端会选 h2，必须用 http2.Transport 收发
	ALPNH2First ALPNMode = "h2"
)

// h2ChromeSettings 记录 Chrome 实际发送的 SETTINGS，供文档与后续 fork 参考。
//
// 当前 golang.org/x/net/http2 不允许覆盖这些值（initialSettings 是 ClientConn
// 内部变量）。要精确匹配需要 fork 该包改 transport.go 里的 initialSettings。
//
// 实测对照（2026-11）：
//
//	SETTINGS                           Chrome      Go 默认
//	HEADER_TABLE_SIZE                  65536       4096
//	ENABLE_PUSH                        0           0
//	MAX_CONCURRENT_STREAMS             1000        不发
//	INITIAL_WINDOW_SIZE                6291456     4194304
//	MAX_HEADER_LIST_SIZE               262144      按需
var h2ChromeSettings = []struct {
	ID    uint16
	Value uint32
	Name  string
}{
	{0x1, 65536, "HEADER_TABLE_SIZE"},
	{0x2, 0, "ENABLE_PUSH"},
	{0x3, 1000, "MAX_CONCURRENT_STREAMS"},
	{0x4, 6291456, "INITIAL_WINDOW_SIZE"},
	{0x6, 262144, "MAX_HEADER_LIST_SIZE"},
}

// newUTLSH2Transport 构造一个走 uTLS 握手的 HTTP/2 Transport。
//
// 与 newUTLSTransport 的区别：
//   - ALPN 声明 ["h2", "http/1.1"]
//   - 用 golang.org/x/net/http2 收发帧
//
// 已知限制：SETTINGS 用 Go 默认值，与 Chrome 有差异，见 h2ChromeSettings。
func newUTLSH2Transport(fingerprint string) *http2.Transport {
	helloID := resolveFingerprint(fingerprint)

	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return &http2.Transport{
		// 不用标准库的 h2 升级路径，明确走 TLS ALPN
		AllowHTTP: false,

		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}

			rawConn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, fmt.Errorf("dial %s: %w", addr, err)
			}

			cfg := &utls.Config{
				ServerName:         host,
				MinVersion:         tls.VersionTLS12,
				InsecureSkipVerify: false,
			}

			spec, err := utls.UTLSIdToSpec(helloID)
			if err != nil {
				rawConn.Close()
				id := helloID
				return nil, fmt.Errorf("build ClientHelloSpec for %s: %w", id.Str(), err)
			}
			// 关键：与 Chrome 一致，优先 h2
			forceALPN(&spec, "h2", "http/1.1")

			uconn := utls.UClient(rawConn, cfg, utls.HelloCustom)
			if err := uconn.ApplyPreset(&spec); err != nil {
				rawConn.Close()
				return nil, fmt.Errorf("apply ClientHelloSpec: %w", err)
			}
			if err := uconn.HandshakeContext(ctx); err != nil {
				rawConn.Close()
				id := helloID
				return nil, fmt.Errorf("utls handshake %s (%s): %w", host, id.Str(), err)
			}

			// 确认服务端真的选了 h2，否则 h2 帧层会直接报错
			state := uconn.ConnectionState()
			if state.NegotiatedProtocol != "h2" {
				uconn.Close()
				return nil, fmt.Errorf(
					"服务端协商出 %q 而非 h2，请改用 ALPN=h1 模式",
					state.NegotiatedProtocol)
			}
			return uconn, nil
		},

		ReadIdleTimeout: 30 * time.Second,
		PingTimeout:     15 * time.Second,
	}
}

// h2Cache 缓存 h2 transport，按指纹分桶。
var (
	h2CacheMu sync.Mutex
	h2Cache   = map[string]*http2.Transport{}
)

func cachedUTLSH2Transport(fingerprint string) *http2.Transport {
	key := strings.ToLower(strings.TrimSpace(fingerprint))
	h2CacheMu.Lock()
	defer h2CacheMu.Unlock()
	if tr, ok := h2Cache[key]; ok {
		return tr
	}
	tr := newUTLSH2Transport(fingerprint)
	h2Cache[key] = tr
	return tr
}

// CloseH2IdleConnections 释放 h2 缓存的空闲连接。
func CloseH2IdleConnections() {
	h2CacheMu.Lock()
	defer h2CacheMu.Unlock()
	for _, tr := range h2Cache {
		tr.CloseIdleConnections()
	}
	h2Cache = map[string]*http2.Transport{}
}

// ChromeH2Settings 返回 Chrome 的 SETTINGS 参考值，供自检和文档使用。
func ChromeH2Settings() map[string]uint32 {
	out := make(map[string]uint32, len(h2ChromeSettings))
	for _, s := range h2ChromeSettings {
		out[s.Name] = s.Value
	}
	return out
}
