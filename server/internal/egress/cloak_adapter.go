package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	shttp "github.com/sardanioss/http"
	"github.com/sardanioss/httpcloak/dns"
	"github.com/sardanioss/httpcloak/fingerprint"
	"github.com/sardanioss/httpcloak/transport"
)

// cloakRoundTripper 把 httpcloak 的 transport 包成标准库的 http.RoundTripper。
//
// 背景
// ----
// httpcloak 用的是自己 fork 的 github.com/sardanioss/http（net/http 的副本），
// 它的 Transport.RoundTrip 签名是：
//
//	RoundTrip(*shttp.Request) (*shttp.Response, error)
//
// 而不是标准库的 *net/http.Request。所以不能直接塞进 http.Client.Transport，
// 需要一层转换。
//
// 好在 shttp.Request / shttp.Response 的字段名与标准库一致，
// 转换是逐字段赋值，不涉及语义差异。
type cloakRoundTripper struct {
	inner interface {
		RoundTrip(*shttp.Request) (*shttp.Response, error)
	}
}

// RoundTrip 实现 net/http.RoundTripper。
func (c *cloakRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	sreq := toSHTTPRequest(req)
	sresp, err := c.inner.RoundTrip(sreq)
	if err != nil {
		return nil, err
	}
	return toNetHTTPResponse(sresp), nil
}

// CloseIdleConnections 透传给底层 transport（如果支持）。
func (c *cloakRoundTripper) CloseIdleConnections() {
	if closer, ok := c.inner.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// toSHTTPRequest 把标准库请求转成 sardanioss/http 的请求。
//
// 关键点：
//   - Body 直接复用同一个 io.ReadCloser，不复制
//   - Context 通过 WithContext 挂上，否则取消信号传不到底层
//   - Header 用深拷贝，避免两边共享 map 后被并发写
func toSHTTPRequest(req *http.Request) *shttp.Request {
	out := &shttp.Request{
		Method:           req.Method,
		URL:              req.URL,
		Proto:            req.Proto,
		ProtoMajor:       req.ProtoMajor,
		ProtoMinor:       req.ProtoMinor,
		Header:           toSHeader(req.Header),
		Body:             req.Body,
		ContentLength:    req.ContentLength,
		Host:             req.Host,
		RemoteAddr:       req.RemoteAddr,
		RequestURI:       req.RequestURI,
		TransferEncoding: req.TransferEncoding,
		Close:            req.Close,
	}
	if req.GetBody != nil {
		out.GetBody = func() (io.ReadCloser, error) {
			return req.GetBody()
		}
	}
	if req.URL != nil {
		out.Host = req.URL.Host
	}
	if req.Context() != nil {
		out = out.WithContext(req.Context())
	}
	return out
}

// toNetHTTPResponse 把 sardanioss/http 的响应转回标准库响应。
func toNetHTTPResponse(sresp *shttp.Response) *http.Response {
	if sresp == nil {
		return nil
	}
	out := &http.Response{
		Status:           sresp.Status,
		StatusCode:       sresp.StatusCode,
		Proto:            sresp.Proto,
		ProtoMajor:       sresp.ProtoMajor,
		ProtoMinor:       sresp.ProtoMinor,
		Header:           toNetHeader(sresp.Header),
		Body:             sresp.Body,
		ContentLength:    sresp.ContentLength,
		TransferEncoding: sresp.TransferEncoding,
		Close:            sresp.Close,
		Uncompressed:     sresp.Uncompressed,
		Request:          nil, // 由 http.Client 自己填
	}
	// sresp.TLS 是 *sardanioss/utls.ConnectionState，与 crypto/tls 的类型不同。
	// 业务不依赖这个字段，留空即可；需要协议版本时看 Proto/ProtoMajor。
	if len(sresp.Trailer) > 0 {
		out.Trailer = toNetHeader(sresp.Trailer)
	}
	return out
}

// toSHeader 把标准库 Header 转成 sardanioss/http 的 Header。
func toSHeader(h http.Header) shttp.Header {
	if h == nil {
		return nil
	}
	out := make(shttp.Header, len(h))
	for k, vv := range h {
		cp := make([]string, len(vv))
		copy(cp, vv)
		out[k] = cp
	}
	return out
}

// toNetHeader 把 sardanioss/http 的 Header 转成标准库 Header。
func toNetHeader(h shttp.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vv := range h {
		cp := make([]string, len(vv))
		copy(cp, vv)
		out[k] = cp
	}
	return out
}

// newCloakRoundTripper 构造一个可直接用于 http.Client 的 httpcloak transport。
func newCloakRoundTripper(presetName string, useH2 bool) http.RoundTripper {
	preset := fingerprint.Get(presetName)
	cache := dns.NewCache()
	if useH2 {
		return &cloakRoundTripper{inner: transport.NewHTTP2Transport(preset, cache)}
	}
	return &cloakRoundTripper{inner: transport.NewHTTP1Transport(preset, cache)}
}

// doCloak 用 httpcloak 直接发一个请求，返回标准库响应。
//
// 给不走 Session 的临时调用用（自检、探活）。
func doCloak(ctx context.Context, presetName string, useH2 bool,
	req *http.Request) (*http.Response, error) {
	tr := newCloakRoundTripper(presetName, useH2)
	client := &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	return client.Do(req)
}

// newCloakRoundTripperWithProxy 构造一个带代理的 httpcloak transport。
//
// httpcloak 的代理配置在 transport.ProxyConfig 里，通过
// NewHTTP1TransportWithProxy / NewHTTP2TransportWithProxy 传入。
func newCloakRoundTripperWithProxy(
	presetName string, useH2 bool, spec ProxySpec,
) http.RoundTripper {
	preset := fingerprint.Get(presetName)
	cache := dns.NewCache()

	var pc *transport.ProxyConfig
	if !spec.IsDirect() {
		pc = &transport.ProxyConfig{
			URL:      spec.URL,
			Username: spec.Username,
			Password: spec.Password,
		}
	}

	if useH2 {
		return &cloakRoundTripper{
			inner: transport.NewHTTP2TransportWithProxy(preset, cache, pc),
		}
	}
	return &cloakRoundTripper{
		inner: transport.NewHTTP1TransportWithProxy(preset, cache, pc),
	}
}

// decodeJSONBody 把响应体解成 JSON 对象。
func decodeJSONBody(resp *http.Response, out any) error {
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读响应: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("解析 JSON: %w (body: %s)", err, Truncate(string(raw), 200))
	}
	return nil
}
