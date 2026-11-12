package egress

// 补齐 client.go 需要但 cloak.go 未提供的 transport 函数。

import (
	"net/http"
	"strings"
	"sync"
)

var (
	proxyTrMu    sync.Mutex
	proxyTrCache = map[string]http.RoundTripper{}
)

// cachedCloakProxyTransport 缓存带代理的 httpcloak transport。
func cachedCloakProxyTransport(presetName string, useH2 bool, spec ProxySpec) http.RoundTripper {
	key := strings.ToLower(strings.TrimSpace(presetName)) +
		"|h2=" + bstr(useH2) + "|" + spec.URL
	proxyTrMu.Lock()
	defer proxyTrMu.Unlock()
	if tr, ok := proxyTrCache[key]; ok {
		return tr
	}
	tr := newCloakRoundTripperWithProxy(presetName, useH2, spec)
	proxyTrCache[key] = tr
	return tr
}

// CachedCloakProxyTransport 是导出版。
func CachedCloakProxyTransport(presetName string, useH2 bool, spec ProxySpec) http.RoundTripper {
	return cachedCloakProxyTransport(presetName, useH2, spec)
}

// NewCloakRoundTripperWithProxy 是导出版。
func NewCloakRoundTripperWithProxy(presetName string, useH2 bool, spec ProxySpec) http.RoundTripper {
	return newCloakRoundTripperWithProxy(presetName, useH2, spec)
}

// transportForBuiltin 用内置 utls 实现。
func transportForBuiltin(fp string, alpn ALPNMode) http.RoundTripper {
	if alpn == ALPNH2First {
		return cachedUTLSH2Transport(fp)
	}
	return cachedUTLSTransport(fp, true)
}

// TransportForBuiltin 是导出版。
func TransportForBuiltin(fp string, alpn ALPNMode) http.RoundTripper {
	return transportForBuiltin(fp, alpn)
}

// transportFor 按 ALPN 选内置 transport。
func transportFor(fp string, alpn ALPNMode) http.RoundTripper {
	return transportForBuiltin(fp, alpn)
}

// CloseProxyIdleConnections 释放代理 transport 的空闲连接。
func CloseProxyIdleConnections() {
	proxyTrMu.Lock()
	defer proxyTrMu.Unlock()
	for _, tr := range proxyTrCache {
		if c, ok := tr.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
	proxyTrCache = map[string]http.RoundTripper{}
}

func bstr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
