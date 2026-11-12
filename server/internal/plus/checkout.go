// Package plus 实现 ChatGPT Plus 直充的纯协议链路。
//
// 从 vendor/chatgpt_plus/standalone_core/card_payment.py 移植，
// 第一阶段只做「提链」（创建 checkout session），不涉及 Stripe 支付。
//
// 设计要点
//   - 不使用浏览器，纯 HTTP
//   - 首次提链只需要基础 headers，attestation / pending-updates 是
//     后续请求才需要的（从响应里捕获），这里实现了捕获逻辑但不强依赖
//   - 所有字段名与 Python 版严格一致，便于双跑对比
package plus

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// AppBase 是 ChatGPT 主站
	AppBase = "https://chatgpt.com"
	// CheckoutURL 是建单端点
	CheckoutURL = AppBase + "/backend-api/payments/checkout"
	// CheckoutRoute 用于 x-openai-target-path / x-openai-target-route
	CheckoutRoute = "/backend-api/payments/checkout"

	// PlanPlus 是 Plus 套餐标识
	PlanPlus = "chatgptplusplan"
	// EntryPricingModal 是能填充 checkout_state 的入口
	EntryPricingModal = "all_plans_pricing_modal"
	// UIModeCustom 是自定义 UI 模式
	UIModeCustom = "custom"
)

// Config 是 Plus 直充配置，对应 Python 的 standalone_config.json。
type Config struct {
	Country        string `json:"country"`         // 建单国家，如 PH
	Currency       string `json:"currency"`        // 建单币种，如 PHP
	BindCountry    string `json:"bind_country"`    // 绑卡国家，如 US
	BindCurrency   string `json:"bind_currency"`   // 绑卡币种，如 USD
	PromoCampaign  string `json:"promo_campaign"`  // 促销 ID，如 plus-1-month-free
	RejectNonzero  bool   `json:"reject_nonzero"`  // 金额非 0 直接拒绝
	Timeout        int    `json:"timeout"`         // 单请求超时秒
	TLSImpersonate string `json:"tls_impersonate"` // TLS 指纹，如 chrome146
	ALPN           string `json:"alpn"`            // h1 / h2，默认 h1
	UseCloak       bool   `json:"use_cloak"`       // 用 httpcloak（默认 true）
	Proxy          string `json:"proxy"`           // 出口代理，http://user:pass@host:port
}

// DefaultConfig 返回与 SG 现有 standalone_config.json 一致的默认值。
func DefaultConfig() Config {
	return Config{
		Country:        "PH",
		Currency:       "PHP",
		BindCountry:    "US",
		BindCurrency:   "USD",
		PromoCampaign:  "plus-1-month-free",
		RejectNonzero:  true,
		Timeout:        DefaultTimeout,
		TLSImpersonate: DefaultFingerprint,
		UseCloak:       true,
	}
}

// BillingDetails 是建单的账单区域信息。
type BillingDetails struct {
	Country  string `json:"country"`
	Currency string `json:"currency"`
}

// PromoCampaign 是促销参数。字段名与 Python 版一致。
type PromoCampaign struct {
	PromoCampaignID        string `json:"promo_campaign_id"`
	IsCouponFromQueryParam bool   `json:"is_coupon_from_query_param"`
}

// CheckoutRequest 是建单请求体。
type CheckoutRequest struct {
	EntryPoint     string         `json:"entry_point"`
	PlanName       string         `json:"plan_name"`
	BillingDetails BillingDetails `json:"billing_details"`
	CheckoutUIMode string         `json:"checkout_ui_mode"`
	PromoCampaign  *PromoCampaign `json:"promo_campaign,omitempty"`
}

// CheckoutResult 是建单结果。
type CheckoutResult struct {
	CheckoutID      string `json:"checkout_id"`
	ProcessorEntity string `json:"processor_entity"`
	PublishableKey  string `json:"publishable_key"`
	URL             string `json:"url"`
	Amount          string `json:"amount"`
	Currency        string `json:"currency"`
	Country         string `json:"country"`
	Raw             map[string]any
}

// Session 持有一个账号的 HTTP 会话状态。
type Session struct {
	Token         string
	AccountID     string
	DeviceID      string
	Locale        string
	Timezone      string
	Fingerprint   string    // TLS 指纹名
	ForceHTTP1    bool      // 强制 HTTP/1.1
	ALPN          ALPNMode  // h1 / h2
	Proxy         ProxySpec // 出口代理（零值表示直连）
	cookieMu      sync.RWMutex
	cookies       map[string]string
	StripeJSID    string // Stripe.js 会话 id（绑卡时生成一次复用）
	HCaptchaToken string // Stripe 侧 hCaptcha token

	// 从响应里捕获的，后续请求要用
	Attestation    string
	PendingUpdates []string

	client *http.Client
}

// NewSession 建一个会话。token 是 ChatGPT 的 access_token。
//
// 使用默认 TLS 指纹（Config.TLSImpersonate 的默认值）。
// 想指定指纹请用 NewSessionWithConfig。
func NewSession(token, accountID string) *Session {
	return NewSessionWithConfig(token, accountID, DefaultConfig())
}

// NewSessionWithConfig 用指定配置建会话。
//
// 指纹从 cfg.TLSImpersonate 读，不写死：
//
//	cfg := plus.DefaultConfig()
//	cfg.TLSImpersonate = "chrome131"
//	s := plus.NewSessionWithConfig(token, acct, cfg)
func NewSessionWithConfig(token, accountID string, cfg Config) *Session {
	fp := strings.TrimSpace(cfg.TLSImpersonate)
	if fp == "" {
		fp = DefaultFingerprint
	}
	var proxySpec ProxySpec
	if strings.TrimSpace(cfg.Proxy) != "" {
		if spec, perr := ParseProxyURL(cfg.Proxy); perr == nil {
			proxySpec = spec
		}
	}
	alpn := ALPNMode(strings.ToLower(strings.TrimSpace(string(cfg.ALPN))))
	if alpn != ALPNH2First {
		alpn = ALPNH1
	}
	return &Session{
		Token:       strings.TrimSpace(token),
		AccountID:   strings.TrimSpace(accountID),
		DeviceID:    newUUID(),
		Locale:      "en-US",
		Timezone:    "Asia/Manila",
		Proxy:       proxySpec,
		Fingerprint: fp,
		ForceHTTP1:  alpn == ALPNH1,
		ALPN:        alpn,
		client: &http.Client{
			Timeout: time.Duration(timeoutOf(cfg)) * time.Second,
			// 用 uTLS 做握手，模拟真实浏览器的 ClientHello
			Transport: pickTransport(fp, alpn, cfg),
			// 不跟随重定向，方便观察 302 语义
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// CreateCheckout 建一个 checkout session。
//
// 对应 Python 的 _checkout_create。
func (s *Session) CreateCheckout(ctx context.Context, cfg Config) (*CheckoutResult, error) {
	if s.Token == "" {
		return nil, fmt.Errorf("access_token 为空")
	}

	country := strings.ToUpper(strings.TrimSpace(cfg.Country))
	if country == "" {
		country = "PH"
	}
	currency := strings.ToUpper(strings.TrimSpace(cfg.Currency))
	if currency == "" {
		currency = "PHP"
	}

	body := CheckoutRequest{
		EntryPoint:     EntryPricingModal,
		PlanName:       PlanPlus,
		BillingDetails: BillingDetails{Country: country, Currency: currency},
		CheckoutUIMode: UIModeCustom,
	}
	if promo := strings.TrimSpace(cfg.PromoCampaign); promo != "" {
		body.PromoCampaign = &PromoCampaign{
			PromoCampaignID:        promo,
			IsCouponFromQueryParam: false,
		}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体: %w", err)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		CheckoutURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	for k, v := range s.headers(CheckoutRoute) {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读响应: %w", err)
	}

	// 捕获 attestation / pending updates，供后续请求使用
	s.captureUpdates(resp)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("checkout create: HTTP %d: %s",
			resp.StatusCode, truncate(string(rawBody), 300))
	}

	var parsed map[string]any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return nil, fmt.Errorf("解析响应: %w (body: %s)", err, truncate(string(rawBody), 200))
	}

	return s.buildResult(parsed), nil
}

// buildResult 从响应里抽关键字段。
func (s *Session) buildResult(m map[string]any) *CheckoutResult {
	r := &CheckoutResult{Raw: m}

	// checkout_id 可能在顶层，也可能在 session 里
	r.CheckoutID = pickString(m,
		"checkout_session_id", "checkout_id", "session_id", "id")

	// processor_entity 决定后续 URL 路径（openai_llc / openai_ie）
	r.ProcessorEntity = pickString(m, "processor_entity")
	if r.ProcessorEntity == "" {
		r.ProcessorEntity = "openai_llc"
	}

	// publishable_key 可能嵌在 stripe 相关字段里
	r.PublishableKey = pickString(m, "publishable_key", "stripe_publishable_key")
	if r.PublishableKey == "" {
		if stripe, ok := m["stripe"].(map[string]any); ok {
			r.PublishableKey = pickString(stripe, "publishable_key", "key")
		}
	}

	r.URL = pickString(m, "url", "checkout_url", "link")

	// 币种和地区：优先从 checkout_state 取，其次 billing_details
	r.Currency = strings.ToUpper(extractCurrency(m))
	r.Country = strings.ToUpper(extractCountry(m))
	r.Amount = extractAmount(m)
	return r
}

// extractAmount 从响应里取应付金额。
//
// 真实响应结构（2026-11 实测）：
//
//	checkout_state.lineItems[0].subtotal.minorUnitsAmount  ← 小计，整数分
//	checkout_state.taxAmounts[].minorUnitsAmount           ← 税，整数分
//	checkout_state.total.discount.minorUnitsAmount         ← 折扣，整数分
//	checkout_state.currency                                ← 币种小写
//
// 金额是整数分，需要除以 100。
func extractAmount(m map[string]any) string {
	state, ok := m["checkout_state"].(map[string]any)
	if !ok {
		// 退化：顶层找字符串型金额
		if s := pickString(m, "amount", "amount_due"); s != "" {
			return s
		}
		return "unknown"
	}

	// 1. 小计
	var subtotal, tax, discount float64
	var found bool

	if items, ok := state["lineItems"].([]any); ok && len(items) > 0 {
		if item, ok := items[0].(map[string]any); ok {
			if sub, ok := item["subtotal"].(map[string]any); ok {
				subtotal, found = minorUnits(sub["minorUnitsAmount"])
			}
		}
	}

	// 2. 税
	if taxes, ok := state["taxAmounts"].([]any); ok {
		for _, t := range taxes {
			if tm, ok := t.(map[string]any); ok {
				if v, ok := minorUnits(tm["minorUnitsAmount"]); ok {
					tax += v
				}
			}
		}
	}

	// 3. 折扣
	if total, ok := state["total"].(map[string]any); ok {
		if d, ok := total["discount"].(map[string]any); ok {
			if v, ok := minorUnits(d["minorUnitsAmount"]); ok {
				discount = v
			}
		}
	}

	if !found {
		// 有 total.total 之类就直接用
		if total, ok := state["total"].(map[string]any); ok {
			if v, ok := minorUnits(total["minorUnitsAmount"]); ok {
				return formatAmount(v)
			}
		}
		return "unknown"
	}

	final := subtotal + tax - discount
	return formatAmount(final)
}

// minorUnits 把 JSON 里的数字番成 float64。
func minorUnits(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	}
	return 0, false
}

// formatAmount 把整数分转成 "27.52" 形式。
func formatAmount(minor float64) string {
	return strconv.FormatFloat(minor/100, 'f', 2, 64)
}

// headers 组装 ChatGPT 应用侧请求头。
//
// 对应 Python 的 _new_session 的 session.headers + _app_headers。
func (s *Session) headers(route string) map[string]string {
	h := map[string]string{
		"User-Agent":            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
		"Accept":                "*/*",
		"Accept-Language":       s.locale(),
		"Authorization":         "Bearer " + s.Token,
		"Origin":                AppBase,
		"Referer":               AppBase + "/",
		"Content-Type":          "application/json",
		"oai-device-id":         s.DeviceID,
		"oai-language":          s.locale(),
		"sec-ch-ua":             `"Not;A=Brand";v="8", "Chromium";v="146", "Google Chrome";v="146"`,
		"sec-ch-ua-mobile":      "?0",
		"sec-ch-ua-platform":    `"Windows"`,
		"priority":              "u=1, i",
		"x-openai-target-path":  route,
		"x-openai-target-route": route,
	}

	if s.AccountID != "" {
		h["chatgpt-account-id"] = s.AccountID
	}
	// 只有从响应里拿到 attestation 后才带
	if s.Attestation != "" && strings.HasPrefix(route, "/backend-api/payments/") {
		h["OAI-Web-Deployment-Attestation"] = s.Attestation
	}
	if len(s.PendingUpdates) > 0 {
		if b, err := json.Marshal(map[string]any{
			"v": 3, "updates": s.PendingUpdates,
		}); err == nil {
			h["x-oai-is-pending-updates"] = string(b)
		}
	}
	return h
}

// captureUpdates 从响应头里抓 attestation 和 pending updates。
//
// 对应 Python 的 _capture_oai_update / _adopt_auth_session_token。
func (s *Session) captureUpdates(resp *http.Response) {
	if v := resp.Header.Get("OAI-Web-Deployment-Attestation"); v != "" {
		s.Attestation = v
	}
	if v := resp.Header.Get("x-oai-is-pending-updates"); v != "" {
		var parsed struct {
			Updates []string `json:"updates"`
		}
		if json.Unmarshal([]byte(v), &parsed) == nil && len(parsed.Updates) > 0 {
			s.PendingUpdates = parsed.Updates
		}
	}
}

func (s *Session) locale() string {
	if s.Locale != "" {
		return s.Locale
	}
	return "en-US"
}

// ---------------------------------------------------------------- 工具

// pickString 按顺序取第一个非空字符串值。
func pickString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if str, ok := v.(string); ok && strings.TrimSpace(str) != "" {
				return strings.TrimSpace(str)
			}
		}
	}
	return ""
}

// newUUID 生成 v4 UUID，用于 oai-device-id。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// IsZero 判断金额字符串是否为零。
//
// Python 版返回的 amount 可能是 "0.00" / "0" / "PHP 0.00" / "unknown"。
// 只有能确定是 0 才算零，无法判定时返回 false（保守）。
func IsZero(amount string) bool {
	s := strings.TrimSpace(amount)
	if s == "" || strings.EqualFold(s, "unknown") {
		return false
	}
	var digits strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || r == '.' || r == '-' {
			digits.WriteRune(r)
		}
	}
	v, err := strconv.ParseFloat(digits.String(), 64)
	if err != nil {
		return false
	}
	return v == 0
}

// extractCurrency 从响应里取币种，小写转大写。
//
// 优先级：checkout_state.currency > billing_details.currency > 空
func extractCurrency(m map[string]any) string {
	if state, ok := m["checkout_state"].(map[string]any); ok {
		if s := pickString(state, "currency"); s != "" {
			return s
		}
	}
	if bd, ok := m["billing_details"].(map[string]any); ok {
		if s := pickString(bd, "currency"); s != "" {
			return s
		}
	}
	return pickString(m, "currency")
}

// extractCountry 从响应里取建单地区。
//
// 优先级：billing_details.country > checkout_state.billingAddress.address.country > 空
func extractCountry(m map[string]any) string {
	if bd, ok := m["billing_details"].(map[string]any); ok {
		if s := pickString(bd, "country"); s != "" {
			return s
		}
	}
	if state, ok := m["checkout_state"].(map[string]any); ok {
		if ba, ok := state["billingAddress"].(map[string]any); ok {
			if addr, ok := ba["address"].(map[string]any); ok {
				if s := pickString(addr, "country"); s != "" {
					return s
				}
			}
		}
	}
	return pickString(m, "country")
}

// timeoutOf 取配置里的超时秒数，缺省用 DefaultTimeout。
func timeoutOf(cfg Config) int {
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return DefaultTimeout
}

// HTTPTransport 暴露底层 transport，便于自检和外部测试使用。
func (s *Session) HTTPTransport() http.RoundTripper {
	if s.client == nil {
		return nil
	}
	return s.client.Transport
}
