package plus

// transport 相关现在全部在 internal/egress。
//
// 这个文件只做转发，不重新实现。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tiantianGPU/reg-factory/server/internal/egress"
)

// ---------------------------------------------------------------- 类型别名

type ALPNMode = egress.ALPNMode
type ProxySpec = egress.ProxySpec
type EgressInfo = egress.EgressInfo
type PurityScore = egress.PurityScore

// ---------------------------------------------------------------- 常量转发

const (
	ALPNH1      = egress.ALPNH1
	ALPNH2First = egress.ALPNH2First

	DefaultCloakPreset = egress.DefaultCloakPreset
	DefaultFingerprint = egress.DefaultFingerprint
	DefaultTimeout     = egress.DefaultTimeout
)

// ---------------------------------------------------------------- 函数转发

func ParseProxyURL(raw string) (egress.ProxySpec, error) {
	return egress.ParseProxyURL(raw)
}

func cachedCloakProxyTransport(fp string, useH2 bool, spec egress.ProxySpec) http.RoundTripper {
	return egress.CachedCloakProxyTransport(fp, useH2, spec)
}

func cachedCloakTransport(fp string, useH2 bool) http.RoundTripper {
	return egress.CachedCloakTransport(fp, useH2)
}

func cachedUTLSTransport(fp string, forceH1 bool) http.RoundTripper {
	return egress.CachedUTLSTransport(fp, forceH1)
}

func cachedUTLSH2Transport(fp string) http.RoundTripper {
	return egress.CachedUTLSH2Transport(fp)
}

func transportFor(fp string, alpn egress.ALPNMode) http.RoundTripper {
	return egress.TransportForBuiltin(fp, alpn)
}

func transportForCloak(fp string, alpn egress.ALPNMode, useCloak bool) http.RoundTripper {
	return egress.TransportForCloak(fp, alpn, useCloak)
}

func newCloakRoundTripperWithProxy(fp string, useH2 bool, spec egress.ProxySpec) http.RoundTripper {
	return egress.NewCloakRoundTripperWithProxy(fp, useH2, spec)
}

func CloseCloakIdleConnections()        { egress.CloseCloakIdleConnections() }
func CloseProxyIdleConnections()        { egress.CloseProxyIdleConnections() }
func CloakPresetValid(name string) bool { return egress.CloakPresetValid(name) }
func IsDatacenterISP(s string) bool     { return egress.IsDatacenterISP(s) }

func EvaluatePurity(ctx context.Context, info *egress.EgressInfo) *egress.PurityScore {
	return egress.EvaluatePurity(ctx, info)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// pickTransport 按配置选 transport：有代理用代理版，没代理用直连版。
func pickTransport(fp string, alpn egress.ALPNMode, cfg Config) http.RoundTripper {
	useH2 := alpn == ALPNH2First
	if !cfg.UseCloak {
		return egress.TransportForBuiltin(fp, alpn)
	}
	if strings.TrimSpace(cfg.Proxy) != "" {
		spec, err := egress.ParseProxyURL(cfg.Proxy)
		if err == nil {
			return egress.CachedCloakProxyTransport(fp, useH2, spec)
		}
	}
	return egress.TransportForCloak(fp, alpn, true)
}

// decodeJSONBody 把响应体解成 JSON 对象。
func decodeJSONBody(resp *http.Response, out any) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读响应: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析 JSON: %w", err)
	}
	return nil
}
