package egress

import "net/http"

// CachedCloakTransport 是 cachedCloakTransport 的导出版。
func CachedCloakTransport(presetName string, useH2 bool) http.RoundTripper {
	return cachedCloakTransport(presetName, useH2)
}

// CachedUTLSTransport 是 cachedUTLSTransport 的导出版。
func CachedUTLSTransport(fp string, forceH1 bool) http.RoundTripper {
	return cachedUTLSTransport(fp, forceH1)
}

// CachedUTLSH2Transport 是 cachedUTLSH2Transport 的导出版。
func CachedUTLSH2Transport(fp string) http.RoundTripper {
	return cachedUTLSH2Transport(fp)
}

// DefaultTimeout 是默认超时秒数。
const DefaultTimeout = 60

// TransportForCloak 按配置选 httpcloak 或内置 transport。
func TransportForCloak(fp string, alpn ALPNMode, useCloak bool) http.RoundTripper {
	return transportForCloak(fp, alpn, useCloak)
}
