package egress

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Fingerprints 是支持的 TLS 指纹名，对应 Python 侧 curl_cffi 的 impersonate 参数。
//
// 用法：Config.TLSImpersonate = "chrome146"
var Fingerprints = map[string]utls.ClientHelloID{
	"chrome":     utls.HelloChrome_Auto,
	"chrome120":  utls.HelloChrome_120,
	"chrome131":  utls.HelloChrome_131,
	"chrome133a": utls.HelloChrome_133,
	"chrome146":  utls.HelloChrome_Auto, // 上游尚无 146，用 Auto 跟随最新
	"safari":     utls.HelloSafari_Auto,
	"firefox":    utls.HelloFirefox_Auto,
	"edge":       utls.HelloEdge_Auto,
	"random":     utls.HelloRandomized,
	"ios":        utls.HelloIOS_Auto,
	"android11":  utls.HelloAndroid_11_OkHttp,
}

// DefaultFingerprint 与 Python 侧 _new_session 的默认值保持一致。
const DefaultFingerprint = "chrome146"

// resolveFingerprint 把配置里的字符串映射成 utls 的 ClientHelloID。
func resolveFingerprint(name string) utls.ClientHelloID {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return utls.HelloChrome_Auto
	}
	if id, ok := Fingerprints[n]; ok {
		return id
	}
	// 前缀匹配：chrome146 → chrome
	for prefix, id := range Fingerprints {
		if strings.HasPrefix(n, prefix) {
			return id
		}
	}
	return utls.HelloChrome_Auto
}

// newUTLSTransport 构造一个用 uTLS 做 TLS 握手的 http.Transport。
//
// 为什么需要这个：
//
//	标准 net/http 的 ClientHello 与真实 Chrome 有明显差异（扩展顺序、
//	GREASE 值、ALPN 列表），Cloudflare 一类的边缘会据此拦截。
//	curl_cffi 在 Python 侧做的就是这件事，utls 是 Go 侧的等价物。
//
// 参数
//
//	fingerprint  指纹名，见 Fingerprints
//	forceH1      强制 HTTP/1.1。有些端点在 h2 下的行为与 h1 不同，
//	             排查时可以先降到 h1 对比
//
// 注意：当前实现固定 h1.1。utls 的 HTTP/2 需要额外的帧层实现，
// 先把请求跑通比追 h2 更重要；若某端点强制要求 h2，再打开 forceH2 分支。
func newUTLSTransport(fingerprint string, forceH1 bool) *http.Transport {
	helloID := resolveFingerprint(fingerprint)

	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	return &http.Transport{
		// 不走系统代理环境变量，与 Python 侧 trust_env=False 对齐
		Proxy: nil,

		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},

		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
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

			// 关键：HelloChrome_Auto 自带的 ALPN 扩展会覆盖 Config.NextProtos，
			// 必须先把 spec 取出来改掉 ALPN，再用 HelloCustom 应用。
			// 否则服务端协商出 h2，而 net/http 用 h1 解析 h2 帧，
			// 报 malformed HTTP response。
			spec, err := utls.UTLSIdToSpec(helloID)
			if err != nil {
				rawConn.Close()
				return nil, fmt.Errorf("build ClientHelloSpec for %s: %w",
					helloID.Str(), err)
			}
			forceALPN(&spec, "http/1.1")

			uconn := utls.UClient(rawConn, cfg, utls.HelloCustom)
			if err := uconn.ApplyPreset(&spec); err != nil {
				rawConn.Close()
				return nil, fmt.Errorf("apply ClientHelloSpec: %w", err)
			}
			if err := uconn.HandshakeContext(ctx); err != nil {
				rawConn.Close()
				id := helloID
				return nil, fmt.Errorf("utls handshake %s (%s): %w",
					host, id.Str(), err)
			}
			return uconn, nil
		},

		// 当前固定 h1.1
		ForceAttemptHTTP2:     !forceH1,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		DisableCompression:    false,
	}
}

// utlsTransportCache 缓存已构造的 transport，按指纹名分桶。
//
// transport 内部维护连接池，复用它对性能有实际意义；
// 但不同指纹不能共用，所以按名字分。
var (
	utlsCacheMu sync.Mutex
	utlsCache   = map[string]*http.Transport{}
)

func cachedUTLSTransport(fingerprint string, forceH1 bool) *http.Transport {
	key := strings.ToLower(strings.TrimSpace(fingerprint)) + "|h1=" + boolStr(forceH1)
	utlsCacheMu.Lock()
	defer utlsCacheMu.Unlock()
	if tr, ok := utlsCache[key]; ok {
		return tr
	}
	tr := newUTLSTransport(fingerprint, forceH1)
	utlsCache[key] = tr
	return tr
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// CloseIdleConnections 释放所有缓存的空闲连接。
//
// 进程退出或切换指纹时调用。
func CloseIdleConnections() {
	utlsCacheMu.Lock()
	defer utlsCacheMu.Unlock()
	for _, tr := range utlsCache {
		tr.CloseIdleConnections()
	}
	utlsCache = map[string]*http.Transport{}
}

// TLSInfo 描述当前会话使用的 TLS 配置，便于日志和自检。
func (s *Client) TLSInfo() map[string]any {
	id := resolveFingerprint(s.Fingerprint)
	return map[string]any{
		"fingerprint": s.Fingerprint,
		"force_h1":    s.ForceHTTP1,
		"resolved":    id.Str(),
	}
}

// forceALPN 把 ClientHelloSpec 里的 ALPN 扩展改成指定的协议列表。
//
// 没有 ALPN 扩展时追加一个。这是让 utls 与 net/http 协同工作的关键：
// net/http 的 h1 传输无法解析 h2 帧，必须把协商结果锁定在 http/1.1。
func forceALPN(spec *utls.ClientHelloSpec, protocols ...string) {
	if len(protocols) == 0 {
		return
	}
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = protocols
			return
		}
	}
	spec.Extensions = append(spec.Extensions,
		&utls.ALPNExtension{AlpnProtocols: protocols})
}
