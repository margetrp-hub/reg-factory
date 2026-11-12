package egress

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 设计边界
// --------
// Client 只管「怎么把一个 HTTP 请求送出去，并且像真人」：
//
//	· TLS 指纹（httpcloak / utls）
//	· 出口代理
//	· cookie 跟踪
//	· attestation / pending-updates 捕获
//	· 预热
//	· 出口探测
//	· 阶段切换
//
// Client 不知道什么是「Plus」「提链」「绑卡」——
// 那些是业务包（internal/plus、internal/register 等）的事。
//
// 业务包内嵌 Client，用它的 Do / Get / Post 发请求。
type Client struct {
	// ---- 身份 ----
	// Token 是 access_token。有些端点的探测需要它（如 /api/auth/session），
	// 但没有 token 时也能做出口探测和预热，所以可以是空。
	Token string
	// DeviceID 是 oai-device-id，请求头要用
	DeviceID string
	// Locale / Timezone 影响请求头和指纹
	Locale   string
	Timezone string

	// ---- 网络 ----
	// Fingerprint 是 httpcloak 的 preset 名（chrome-latest 等）
	Fingerprint string
	// Proxy 当前出口
	Proxy ProxySpec
	// ALPN 协议偏好
	ALPN ALPNMode
	// ForceHTTP1 强制 HTTP/1.1
	ForceHTTP1 bool
	// UseCloak 用 httpcloak 还是内置 utls
	UseCloak bool

	// ---- 从响应里捕获的状态 ----
	// Attestation 是 OAI-Web-Deployment-Attestation
	Attestation string
	// PendingUpdates 是 x-oai-is-pending-updates 里的数组
	PendingUpdates []string

	// ---- 内部 ----
	client   *http.Client
	cookieMu sync.RWMutex
	cookies  map[string]string
}

// ClientConfig 是构造 Client 的参数。
type ClientConfig struct {
	Token       string
	DeviceID    string
	Locale      string
	Timezone    string
	Fingerprint string
	Proxy       string // 原始代理地址，内部解析
	ALPN        string // h1 / h2
	UseCloak    bool
	Timeout     int // 秒
}

// NewClient 构造一个网络客户端。
func NewClient(cfg ClientConfig) *Client {
	fp := strings.TrimSpace(cfg.Fingerprint)
	if fp == "" {
		fp = DefaultCloakPreset
	}
	alpn := ALPNMode(strings.ToLower(strings.TrimSpace(cfg.ALPN)))
	if alpn != ALPNH2First {
		alpn = ALPNH1
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 60
	}

	var spec ProxySpec
	if p := strings.TrimSpace(cfg.Proxy); p != "" {
		if parsed, err := ParseProxyURL(p); err == nil {
			spec = parsed
		}
	}

	c := &Client{
		Token:       strings.TrimSpace(cfg.Token),
		DeviceID:    strings.TrimSpace(cfg.DeviceID),
		Locale:      firstNonEmptyStr(cfg.Locale, "en-US"),
		Timezone:    firstNonEmptyStr(cfg.Timezone, "Asia/Shanghai"),
		Fingerprint: fp,
		Proxy:       spec,
		ALPN:        alpn,
		UseCloak:    cfg.UseCloak,
		cookies:     map[string]string{},
	}
	if c.DeviceID == "" {
		c.DeviceID = newUUIDStr()
	}

	c.client = &http.Client{
		Timeout:   time.Duration(timeout) * time.Second,
		Transport: c.transportFor(fp, alpn, spec),
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c
}

// transportFor 按指纹和代理选 transport。
func (c *Client) transportFor(fp string, alpn ALPNMode, spec ProxySpec) http.RoundTripper {
	useH2 := alpn == ALPNH2First
	if !c.UseCloak {
		return transportForBuiltin(fp, alpn)
	}
	if !spec.IsDirect() {
		return cachedCloakProxyTransport(fp, useH2, spec)
	}
	return cachedCloakTransport(fp, useH2)
}

// ---------------------------------------------------------------- 基础 HTTP

// Do 发一个请求。会自动带上基础头、吸收 cookie、捕获 attestation。
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	// 补基础头（调用方已设的不覆盖）
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", defaultUserAgent())
	}
	if req.Header.Get("Accept-Language") == "" {
		req.Header.Set("Accept-Language", c.Locale)
	}
	if c.Token != "" && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	c.captureUpdates(resp)
	c.absorbCookies(resp)
	return resp, nil
}

// Client 暴露底层 http.Client，给需要自定义 dial 的场景。
func (c *Client) HTTPClient() *http.Client {
	return c.client
}

// SetTimeout 改超时。
func (c *Client) SetTimeout(seconds int) {
	if seconds > 0 {
		c.client.Timeout = time.Duration(seconds) * time.Second
	}
}

// ---------------------------------------------------------------- 状态捕获

// captureUpdates 从响应头抓 attestation 和 pending updates。
func (c *Client) captureUpdates(resp *http.Response) {
	if v := resp.Header.Get("OAI-Web-Deployment-Attestation"); v != "" {
		c.Attestation = v
	}
	if v := resp.Header.Get("x-oai-is-pending-updates"); v != "" {
		var parsed struct {
			Updates []string `json:"updates"`
		}
		if jsonUnmarshal([]byte(v), &parsed) == nil && len(parsed.Updates) > 0 {
			c.PendingUpdates = parsed.Updates
		}
	}
}

// absorbCookies 把 Set-Cookie 记进 Client。
func (c *Client) absorbCookies(resp *http.Response) {
	if resp == nil {
		return
	}
	for _, ck := range resp.Cookies() {
		if ck == nil || ck.Name == "" {
			continue
		}
		c.cookieMu.Lock()
		c.cookies[ck.Name] = ck.Value
		c.cookieMu.Unlock()
	}
}

// CookieCount 返回已知 cookie 数量。
func (c *Client) CookieCount() int {
	c.cookieMu.RLock()
	defer c.cookieMu.RUnlock()
	return len(c.cookies)
}

// HasCookie 判断 cookie 是否存在。
func (c *Client) HasCookie(name string) bool {
	c.cookieMu.RLock()
	defer c.cookieMu.RUnlock()
	v, ok := c.cookies[name]
	return ok && strings.TrimSpace(v) != ""
}

// CookieNames 返回 cookie 名字列表。
func (c *Client) CookieNames() []string {
	c.cookieMu.RLock()
	defer c.cookieMu.RUnlock()
	out := make([]string, 0, len(c.cookies))
	for k := range c.cookies {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------- 请求头

// AppHeaders 组装打 chatgpt.com 的请求头。
//
// 参数
//
//	route    用于 x-openai-target-path / route
//	referer  可空
func (c *Client) AppHeaders(route, referer string) map[string]string {
	h := map[string]string{
		"User-Agent":         defaultUserAgent(),
		"Accept":             "*/*",
		"Accept-Language":    c.Locale,
		"Origin":             "https://chatgpt.com",
		"Referer":            firstNonEmptyStr(referer, "https://chatgpt.com/"),
		"Content-Type":       "application/json",
		"oai-device-id":      c.DeviceID,
		"oai-language":       c.Locale,
		"sec-ch-ua":          `"Not;A=Brand";v="8", "Chromium";v="146", "Google Chrome";v="146"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"priority":           "u=1, i",
	}
	if c.Token != "" {
		h["Authorization"] = "Bearer " + c.Token
	}
	if route != "" {
		h["x-openai-target-path"] = route
		h["x-openai-target-route"] = route
	}
	if c.Attestation != "" && strings.HasPrefix(route, "/backend-api/payments/") {
		h["OAI-Web-Deployment-Attestation"] = c.Attestation
	}
	if len(c.PendingUpdates) > 0 {
		if b, err := jsonMarshal(map[string]any{
			"v": 3, "updates": c.PendingUpdates,
		}); err == nil {
			h["x-oai-is-pending-updates"] = string(b)
		}
	}
	return h
}

// StripeHeaders 组装打 api.stripe.com 的请求头。
//
// 关键：不带 Authorization（pk_ 不是 Bearer），不带任何 oai-*。
func (c *Client) StripeHeaders(referer string) map[string]string {
	h := map[string]string{
		"Origin":       "https://js.stripe.com",
		"Referer":      firstNonEmptyStr(referer, "https://js.stripe.com/"),
		"Accept":       "application/json",
		"Content-Type": "application/x-www-form-urlencoded",
		"User-Agent":   defaultUserAgent(),
	}
	return h
}

// BootstrapHeaders 组装 bootstrap 请求头（GET，无 Content-Type）。
func (c *Client) BootstrapHeaders(route string) map[string]string {
	h := c.AppHeaders(route, "")
	delete(h, "Content-Type")
	return h
}

// ---------------------------------------------------------------- 上下文

// ContextWithTimeout 按 Client 配置生成带超时的 Context。
func (c *Client) ContextWithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	t := c.client.Timeout
	if t <= 0 {
		t = 60 * time.Second
	}
	return context.WithTimeout(ctx, t)
}

// ---------------------------------------------------------------- 辅助

func defaultUserAgent() string {
	return "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// newUUIDStr 生成 v4 UUID。
func newUUIDStr() string {
	return newUUIDInternal()
}

// ---------------------------------------------------------------- 指纹
