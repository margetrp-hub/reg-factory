package plus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Stripe 常量，与 Python 侧 card_payment.py 保持一致。
const (
	StripeBase    = "https://api.stripe.com"
	StripeVersion = "2025-03-31.basil"
	// StripeBetas 是 Elements 会话用的 beta 列表
	StripeBetas = "2025-03-31.basil;custom_checkout_server_updates_1;custom_checkout_manual_approval_1"

	// 支付相关端点
	RoutePaymentMethod = "/backend-api/payments/payment_method"
	URLPaymentMethod   = AppBase + RoutePaymentMethod
)

// Card 是卡信息。字段名与 Python 的 _card_fields 一致（支持别名）。
type Card struct {
	Number   string // 卡号，也接受 card_number
	CVC      string // 安全码，也接受 cvv
	ExpMonth string // 月，两位
	ExpYear  string // 年，四位
}

// Billing 是账单地址。字段名与 Python 的 _billing_fields 一致。
type Billing struct {
	Name       string
	Email      string
	Line1      string // 也接受 address
	Line2      string
	City       string
	State      string
	PostalCode string // 也接受 zip
	Country    string // 两位国家码，会转大写
	Phone      string
}

// BindResult 是绑卡结果。
type BindResult struct {
	OK            bool   `json:"ok"`
	CheckoutID    string `json:"checkout_id"`
	Processor     string `json:"processor_entity"`
	SetupIntentID string `json:"setup_intent_id"`
	ClientSecret  string `json:"-"`
	PaymentMethod string `json:"payment_method"`
	Customer      string `json:"customer"`
	IntentStatus  string `json:"intent_status"`
	// RequiresAction 表示需要 3DS，无法纯协议完成
	RequiresAction bool           `json:"requires_action"`
	Raw            map[string]any `json:"-"`
}

// PaymentMethodPrepare 是 POST /backend-api/payments/payment_method 的响应摘要。
type PaymentMethodPrepare struct {
	SetupIntentID  string
	ClientSecret   string
	PublishableKey string
	Customer       string
	Raw            map[string]any
}

// PreparePaymentMethod 拿 SetupIntent。
//
// 对应 Python 的：
//
//	payment_method_prepare_payload = _post_json(
//	    session, f"{APP_BASE}/backend-api/payments/payment_method",
//	    {"account_id": account_id}, ...)
func (s *Session) PreparePaymentMethod(ctx context.Context, cfg Config) (*PaymentMethodPrepare, error) {
	if s.AccountID == "" {
		return nil, fmt.Errorf("payment method prepare: missing account id")
	}

	body, _ := json.Marshal(map[string]string{"account_id": s.AccountID})

	out, err := s.postJSON(ctx, cfg, URLPaymentMethod, RoutePaymentMethod, body)
	if err != nil {
		return nil, fmt.Errorf("payment method prepare: %w", err)
	}

	secret := findClientSecret(out)
	setupID := findSetupIntentID(out, secret)
	if setupID == "" || secret == "" {
		return nil, fmt.Errorf("payment method prepare: 缺少 SetupIntent（setup_id=%q secret=%v）",
			setupID, secret != "")
	}

	return &PaymentMethodPrepare{
		SetupIntentID:  setupID,
		ClientSecret:   secret,
		PublishableKey: findPublishableKey(out),
		Customer:       findIdentifier(out, "cus_"),
		Raw:            out,
	}, nil
}

// BindOptions 控制绑卡行为的可选项。
type BindOptions struct {
	Card    Card
	Billing Billing
	// Conf 为 nil 时使用内置默认风控参数
	Conf *ConfigFile
}

// BindCardWith 是 BindCard 的完整版，支持配置驱动的风控参数。
func (s *Session) BindCardWith(
	ctx context.Context,
	cfg Config,
	checkoutID, processor, publishableKey string,
	opts BindOptions,
) (*BindResult, error) {
	if checkoutID == "" {
		return nil, fmt.Errorf("bind: missing checkout id")
	}
	if processor == "" {
		processor = "openai_llc"
	}
	if !strings.HasPrefix(checkoutID, "oaics_") {
		return nil, fmt.Errorf(
			"bind: 需要 oaics_ 开头的 checkout（当前 %s），cs_ 路径尚未实现",
			truncate(checkoutID, 20))
	}

	norm, err := normalizeCard(opts.Card)
	if err != nil {
		return nil, err
	}

	// 步骤 1：查 checkout 上下文
	ctxURL := fmt.Sprintf("%s/backend-api/payments/checkout/%s/%s",
		AppBase, processor, checkoutID)
	ctxRoute := "/backend-api/payments/checkout/{processor_entity}/{checkout_session_id}"
	if _, err := s.getJSON(ctx, cfg, ctxURL, ctxRoute,
		fmt.Sprintf("%s/checkout/%s/%s", AppBase, processor, checkoutID)); err != nil {
		return nil, fmt.Errorf("strong bind checkout context: %w", err)
	}

	// 步骤 2：拿 SetupIntent
	prep, err := s.PreparePaymentMethod(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if prep.PublishableKey != "" && publishableKey != "" &&
		prep.PublishableKey != publishableKey {
		return nil, fmt.Errorf(
			"payment method prepare: SetupIntent merchant 分片不一致（%s vs %s）",
			mask(prep.PublishableKey), mask(publishableKey))
	}

	// 步骤 3：确认（用配置的风控参数）
	result, err := s.confirmSetupIntentWith(ctx, cfg, prep, checkoutID, processor,
		publishableKey, norm, opts.Billing, opts.Conf)
	if err != nil {
		return nil, err
	}
	result.CheckoutID = checkoutID
	result.Processor = processor
	return result, nil
}

// BindCard 用「oaics_ 强直绑」路径完成绑卡。
//
// 对应 Python 的 strong_bind_direct 分支：
//  1. GET  /backend-api/payments/checkout/{processor}/{checkout_id}   查上下文
//  2. POST /backend-api/payments/payment_method                     拿 SetupIntent
//  3. POST https://api.stripe.com/v1/setup_intents/{id}/confirm      绑卡
//
// 卡数据内联在第三步的 form 里，不单独创建 PaymentMethod。
func (s *Session) BindCard(
	ctx context.Context,
	cfg Config,
	checkoutID, processor, publishableKey string,
	card Card,
	billing Billing,
) (*BindResult, error) {
	if checkoutID == "" {
		return nil, fmt.Errorf("bind: missing checkout id")
	}
	if processor == "" {
		processor = "openai_llc"
	}

	// 强直绑只支持 oaics_ 上下文
	if !strings.HasPrefix(checkoutID, "oaics_") {
		return nil, fmt.Errorf(
			"bind: 需要 oaics_ 开头的 checkout（当前 %s），cs_ 路径尚未实现",
			truncate(checkoutID, 20))
	}

	// 归一化卡字段
	norm, err := normalizeCard(card)
	if err != nil {
		return nil, err
	}

	// 步骤 1：查 checkout 上下文（主要是为了触发 attestation 捕获）
	ctxURL := fmt.Sprintf("%s/backend-api/payments/checkout/%s/%s",
		AppBase, processor, checkoutID)
	ctxRoute := "/backend-api/payments/checkout/{processor_entity}/{checkout_session_id}"
	if _, err := s.getJSON(ctx, cfg, ctxURL, ctxRoute,
		fmt.Sprintf("%s/checkout/%s/%s", AppBase, processor, checkoutID)); err != nil {
		return nil, fmt.Errorf("strong bind checkout context: %w", err)
	}

	// 步骤 2：拿 SetupIntent
	prep, err := s.PreparePaymentMethod(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// merchant 分片必须一致，否则 PaymentMethod 提交到别的 Stripe 账号会被拒
	if prep.PublishableKey != "" && publishableKey != "" &&
		prep.PublishableKey != publishableKey {
		return nil, fmt.Errorf(
			"payment method prepare: SetupIntent merchant 分片不一致（%s vs %s）",
			mask(prep.PublishableKey), mask(publishableKey))
	}

	// 步骤 3：确认 SetupIntent，卡数据内联
	result, err := s.confirmSetupIntent(ctx, cfg, prep, checkoutID, processor,
		publishableKey, norm, billing)
	if err != nil {
		return nil, err
	}
	result.CheckoutID = checkoutID
	result.Processor = processor
	return result, nil
}

// confirmSetupIntent 提交卡数据并确认 SetupIntent。
//
// 对应 Python 的 _setup_confirm。
func (s *Session) confirmSetupIntent(
	ctx context.Context,
	cfg Config,
	prep *PaymentMethodPrepare,
	checkoutID, processor, publishableKey string,
	card Card,
	billing Billing,
) (*BindResult, error) {
	return s.confirmSetupIntentWith(ctx, cfg, prep, checkoutID, processor,
		publishableKey, card, billing, nil)
}

// confirmSetupIntentWith 是带配置的版本。
func (s *Session) confirmSetupIntentWith(
	ctx context.Context,
	cfg Config,
	prep *PaymentMethodPrepare,
	checkoutID, processor, publishableKey string,
	card Card,
	billing Billing,
	conf *ConfigFile,
) (*BindResult, error) {
	stripeJSID := s.StripeJSID
	if stripeJSID == "" {
		stripeJSID = newUUID()
		s.StripeJSID = stripeJSID
	}

	// 风控参数：有配置就用配置，没有就走内置默认
	if conf == nil {
		def := builtinDefaults()
		conf = &def
	}
	hexLen := conf.DeviceIDHexLen()

	// Stripe.js 的会话标识：uuid + N 位 hex
	guid := newUUID() + randHex(hexLen)
	muid := newUUID() + randHex(hexLen)
	sid := newUUID() + randHex(hexLen)

	// 填卡停留时间（毫秒），默认对数正态分布
	timeOnPage := conf.TimeOnPage()

	// 粘贴字段：加权随机，空字符串表示全手输
	pasted := conf.PastedFields()
	// payment_user_agent 用配置的 Stripe.js 版本
	userAgent := conf.PaymentUserAgent()

	form := url.Values{}
	// 基础字段
	form.Set("set_as_default_payment_method", "true")
	form.Set("expected_payment_method_type", "card")
	form.Set("use_stripe_sdk", "true")
	form.Set("key", publishableKey)
	form.Set("_stripe_version", StripeVersion)
	form.Set("client_secret", prep.ClientSecret)

	// 归因元数据
	form.Set("client_attribution_metadata[client_session_id]", stripeJSID)
	form.Set("client_attribution_metadata[merchant_integration_source]", "elements")
	form.Set("client_attribution_metadata[merchant_integration_subtype]", "card-element")
	form.Set("client_attribution_metadata[merchant_integration_version]", "2017")

	// 卡数据（内联）
	form.Set("payment_method_data[type]", "card")
	form.Set("payment_method_data[allow_redisplay]", "always")
	form.Set("payment_method_data[card][number]", card.Number)
	form.Set("payment_method_data[card][cvc]", card.CVC)
	form.Set("payment_method_data[card][exp_month]", card.ExpMonth)
	form.Set("payment_method_data[card][exp_year]", card.ExpYear)

	// 反检测字段
	form.Set("payment_method_data[guid]", guid)
	form.Set("payment_method_data[muid]", muid)
	form.Set("payment_method_data[sid]", sid)
	// pasted_fields 为空表示用户全部手输，此时不传该字段
	if pasted != "" {
		form.Set("payment_method_data[pasted_fields]", pasted)
	}
	form.Set("payment_method_data[payment_user_agent]", userAgent)
	form.Set("payment_method_data[referrer]", AppBase)
	form.Set("payment_method_data[time_on_page]", fmt.Sprintf("%d", timeOnPage))

	form.Set("payment_method_data[client_attribution_metadata][client_session_id]", stripeJSID)
	form.Set("payment_method_data[client_attribution_metadata][merchant_integration_source]", "elements")
	form.Set("payment_method_data[client_attribution_metadata][merchant_integration_subtype]", "card-element")
	form.Set("payment_method_data[client_attribution_metadata][merchant_integration_version]", "2017")

	// 账单
	name := billing.Name
	if name == "" {
		name = "Card Holder"
	}
	form.Set("payment_method_data[billing_details][name]", name)
	if billing.Email != "" {
		form.Set("payment_method_data[billing_details][email]", billing.Email)
	}
	if billing.Line1 != "" {
		form.Set("payment_method_data[billing_details][address][line1]", billing.Line1)
	}
	if billing.Line2 != "" {
		form.Set("payment_method_data[billing_details][address][line2]", billing.Line2)
	}
	if billing.City != "" {
		form.Set("payment_method_data[billing_details][address][city]", billing.City)
	}
	if billing.State != "" {
		form.Set("payment_method_data[billing_details][address][state]", billing.State)
	}
	if billing.PostalCode != "" {
		form.Set("payment_method_data[billing_details][address][postal_code]", billing.PostalCode)
		// 有邮编时，如果原本就没粘贴过 zip，补上
		if pasted != "" && !strings.Contains(pasted, "zip") {
			form.Set("payment_method_data[pasted_fields]", pasted+",zip")
		}
	}
	if billing.Country != "" {
		form.Set("payment_method_data[billing_details][address][country]",
			strings.ToUpper(billing.Country))
	}
	if billing.Phone != "" {
		form.Set("payment_method_data[billing_details][phone]", billing.Phone)
	}

	// hCaptcha token（如果上游给了）
	if s.HCaptchaToken != "" {
		form.Set("radar_options[hcaptcha_token]", s.HCaptchaToken)
	}

	// Elements 会话归因（从 prepare 响应里找）
	if eid := findIdentifier(prep.Raw, "elements_session_"); eid != "" {
		form.Set("payment_method_data[client_attribution_metadata][elements_session_id]", eid)
	}
	if wid := pickString(prep.Raw, "wallet_config_id"); wid != "" {
		form.Set("payment_method_data[client_attribution_metadata][wallet_config_id]", wid)
		form.Set("client_attribution_metadata[wallet_config_id]", wid)
	}

	confirmURL := fmt.Sprintf("%s/v1/setup_intents/%s/confirm", StripeBase, prep.SetupIntentID)
	referer := fmt.Sprintf("%s/checkout/%s/%s", AppBase, processor, checkoutID)

	timeout := timeoutOf(cfg)
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, confirmURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("构造 setup confirm 请求: %w", err)
	}
	for k, v := range s.stripeHeaders(publishableKey, referer) {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("setup confirm 请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读 setup confirm 响应: %w", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("解析 setup confirm 响应: %w (body: %s)",
			err, truncate(string(raw), 200))
	}

	// 402 在 Python 侧会重试，这里先如实返回完整原因
	if resp.StatusCode == 402 {
		return nil, fmt.Errorf("setup confirm: HTTP 402: %s",
			truncate(string(raw), 3000))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("setup confirm: HTTP %d: %s",
			resp.StatusCode, truncate(string(raw), 300))
	}

	// 检查 intent 状态
	status := strings.ToLower(pickString(parsed, "status"))
	res := &BindResult{
		SetupIntentID: prep.SetupIntentID,
		ClientSecret:  prep.ClientSecret,
		Customer:      firstNonEmpty(findIdentifier(parsed, "cus_"), prep.Customer),
		IntentStatus:  status,
		PaymentMethod: firstNonEmpty(
			pickString(parsed, "payment_method", "payment_method_id"),
			findIdentifier(parsed, "pm_")),
		Raw: parsed,
	}

	switch status {
	case "succeeded":
		res.OK = true
	case "requires_action":
		res.RequiresAction = true
		return res, fmt.Errorf("setup confirm: requires_action（需要 3DS，纯协议无法完成）")
	case "":
		return res, fmt.Errorf("setup confirm: 响应缺少 status 字段")
	default:
		return res, fmt.Errorf("setup confirm: 意外的 intent 状态 %q", status)
	}

	if res.PaymentMethod == "" {
		return res, fmt.Errorf("setup confirm: 缺少 PaymentMethod")
	}
	return res, nil
}

// ---------------------------------------------------------------- HTTP 辅助

// postJSON 发一个带 app headers 的 JSON POST。
func (s *Session) postJSON(ctx context.Context, cfg Config, rawURL, route string,
	body []byte) (map[string]any, error) {
	timeout := timeoutOf(cfg)
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, rawURL,
		strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	for k, v := range s.headers(route) {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读响应: %w", err)
	}
	s.captureUpdates(resp)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode,
			truncate(string(raw), 300))
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析响应: %w", err)
	}
	return out, nil
}

// getJSON 发一个带 app headers 的 JSON GET。
func (s *Session) getJSON(ctx context.Context, cfg Config, rawURL, route, referer string) (map[string]any, error) {
	timeout := timeoutOf(cfg)
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求: %w", err)
	}
	for k, v := range s.headers(route) {
		if k == "Content-Type" {
			continue // GET 不带
		}
		req.Header.Set(k, v)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读响应: %w", err)
	}
	s.captureUpdates(resp)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode,
			truncate(string(raw), 300))
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析响应: %w", err)
	}
	return out, nil
}

// stripeHeaders 组装打 api.stripe.com 的请求头。
//
// 与 Python 的 _stripe_headers 对齐：
//   - 不带 Authorization（pk_ 不是 Bearer）
//   - Origin/Referer 伪装成 js.stripe.com
//   - 清掉所有 oai-* 和 chatgpt-account-id
func (s *Session) stripeHeaders(publishableKey, referer string) map[string]string {
	h := map[string]string{
		"Origin":       "https://js.stripe.com",
		"Referer":      "https://js.stripe.com/",
		"Accept":       "application/json",
		"Content-Type": "application/x-www-form-urlencoded",
		"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
	}
	if referer != "" {
		h["Referer"] = referer
	}
	// 不设 Stripe-Version header。
	// Stripe 要求 header 和 _stripe_version 表单参数只能出现一个，
	// 否则报错 Please specify the Stripe API version in either ... but not both.
	// Python 侧 _setup_confirm 只在 body 里设，header 不设。
	return h
}

// ---------------------------------------------------------------- 解析辅助

var (
	reDigits = regexp.MustCompile(`\D`)
)

// normalizeCard 清洗并校验卡字段。
//
// 对应 Python 的 _card_fields。
func normalizeCard(c Card) (Card, error) {
	out := Card{
		Number:   reDigits.ReplaceAllString(c.Number, ""),
		CVC:      reDigits.ReplaceAllString(c.CVC, ""),
		ExpMonth: reDigits.ReplaceAllString(c.ExpMonth, ""),
		ExpYear:  reDigits.ReplaceAllString(c.ExpYear, ""),
	}
	if out.ExpMonth != "" && len(out.ExpMonth) == 1 {
		out.ExpMonth = "0" + out.ExpMonth
	}
	if len(out.ExpYear) == 2 {
		out.ExpYear = "20" + out.ExpYear
	}
	if out.Number == "" || out.CVC == "" || len(out.ExpMonth) != 2 || len(out.ExpYear) != 4 {
		return out, fmt.Errorf("invalid card fields")
	}
	if len(out.Number) < 12 || len(out.Number) > 19 {
		return out, fmt.Errorf("invalid card number length")
	}
	if len(out.CVC) != 3 && len(out.CVC) != 4 {
		return out, fmt.Errorf("invalid card security code length")
	}
	return out, nil
}

// findClientSecret 找 client_secret。
//
// 对应 Python 的 _find_client_secret：
//  1. 直接找 client_secret / clientSecret
//  2. 遍历找 seti_*_secret_* / pi_*_secret_* / cs_*_secret_*
func findClientSecret(m map[string]any) string {
	if v := pickString(m, "client_secret", "clientSecret"); v != "" {
		return v
	}
	var walk func(any, int) string
	walk = func(v any, depth int) string {
		if depth <= 0 {
			return ""
		}
		switch node := v.(type) {
		case map[string]any:
			for _, child := range node {
				if s, ok := child.(string); ok {
					if strings.Contains(s, "_secret_") &&
						(strings.HasPrefix(s, "seti_") ||
							strings.HasPrefix(s, "pi_") ||
							strings.HasPrefix(s, "cs_")) {
						return s
					}
				}
				if got := walk(child, depth-1); got != "" {
					return got
				}
			}
		case []any:
			for _, child := range node {
				if got := walk(child, depth-1); got != "" {
					return got
				}
			}
		}
		return ""
	}
	return walk(m, 5)
}

// findSetupIntentID 找 seti_ 开头的 SetupIntent id。
//
// 对应 Python 的 _setup_intent_id：优先直接字段，其次从 client_secret 截取。
func findSetupIntentID(m map[string]any, clientSecret string) string {
	direct := pickString(m, "setup_intent_id", "setupIntentId", "id")
	if strings.HasPrefix(direct, "seti_") && !strings.Contains(direct, "_secret_") {
		return direct
	}
	if clientSecret != "" {
		if base, _, ok := strings.Cut(clientSecret, "_secret_"); ok &&
			strings.HasPrefix(base, "seti_") {
			return base
		}
	}
	cand := findIdentifier(m, "seti_")
	if base, _, ok := strings.Cut(cand, "_secret_"); ok {
		cand = base
	}
	if strings.HasPrefix(cand, "seti_") {
		return cand
	}
	return ""
}

// findIdentifier 递归找以 prefix 开头的字符串。
func findIdentifier(m any, prefix string) string {
	var walk func(any, int) string
	walk = func(v any, depth int) string {
		if depth <= 0 {
			return ""
		}
		switch node := v.(type) {
		case string:
			// 数组元素本身就是候选字符串
			if strings.HasPrefix(node, prefix) {
				return node
			}
		case map[string]any:
			for _, child := range node {
				if got := walk(child, depth-1); got != "" {
					return got
				}
			}
		case []any:
			for _, child := range node {
				if got := walk(child, depth-1); got != "" {
					return got
				}
			}
		}
		return ""
	}
	return walk(m, 5)
}

func randHex(n int) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hexdigits[rand.Intn(16)]
	}
	return string(b)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func mask(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:12] + "..." + s[len(s)-4:]
}

// GetJSONForTest 暴露 getJSON 给命令行工具使用。
//
// 生产代码不应该用它，仅供 cmd/ 下面的验证工具调用。
func (s *Session) GetJSONForTest(ctx context.Context, cfg Config,
	rawURL, route, referer string) (map[string]any, error) {
	return s.getJSON(ctx, cfg, rawURL, route, referer)
}
