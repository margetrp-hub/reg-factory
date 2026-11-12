package plus

import (
	"encoding/json"
	"testing"
)

// TestCheckoutRequestBody 验证请求体序列化结果与 Python 版严格一致。
//
// Python 的 _checkout_create 生成的 body：
//
//	{
//	  "entry_point": "all_plans_pricing_modal",
//	  "plan_name": "chatgptplusplan",
//	  "billing_details": {"country": "PH", "currency": "PHP"},
//	  "checkout_ui_mode": "custom",
//	  "promo_campaign": {
//	    "promo_campaign_id": "plus-1-month-free",
//	    "is_coupon_from_query_param": false
//	  }
//	}
func TestCheckoutRequestBody(t *testing.T) {
	cfg := DefaultConfig()
	body := CheckoutRequest{
		EntryPoint:     EntryPricingModal,
		PlanName:       PlanPlus,
		BillingDetails: BillingDetails{Country: cfg.Country, Currency: cfg.Currency},
		CheckoutUIMode: UIModeCustom,
		PromoCampaign: &PromoCampaign{
			PromoCampaignID:        cfg.PromoCampaign,
			IsCouponFromQueryParam: false,
		},
	}

	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	want := `{"entry_point":"all_plans_pricing_modal","plan_name":"chatgptplusplan","billing_details":{"country":"PH","currency":"PHP"},"checkout_ui_mode":"custom","promo_campaign":{"promo_campaign_id":"plus-1-month-free","is_coupon_from_query_param":false}}`

	if string(got) != want {
		t.Errorf("请求体不匹配\n got: %s\nwant: %s", got, want)
	}
}

// TestCheckoutRequestBodyNoPromo 验证不带促销时的 body。
func TestCheckoutRequestBodyNoPromo(t *testing.T) {
	body := CheckoutRequest{
		EntryPoint:     EntryPricingModal,
		PlanName:       PlanPlus,
		BillingDetails: BillingDetails{Country: "US", Currency: "USD"},
		CheckoutUIMode: UIModeCustom,
	}
	got, _ := json.Marshal(body)
	want := `{"entry_point":"all_plans_pricing_modal","plan_name":"chatgptplusplan","billing_details":{"country":"US","currency":"USD"},"checkout_ui_mode":"custom"}`

	if string(got) != want {
		t.Errorf("不匹配\n got: %s\nwant: %s", got, want)
	}
	// promo_campaign 必须是 omitempty，不能出现 null
	if contains(string(got), "promo_campaign") {
		t.Errorf("无促销时不应出现 promo_campaign 字段: %s", got)
	}
}

// TestConfigDefaults 验证默认配置与 SG 现有 standalone_config.json 一致。
func TestConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"country", cfg.Country, "PH"},
		{"currency", cfg.Currency, "PHP"},
		{"bind_country", cfg.BindCountry, "US"},
		{"bind_currency", cfg.BindCurrency, "USD"},
		{"promo_campaign", cfg.PromoCampaign, "plus-1-month-free"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if !cfg.RejectNonzero {
		t.Error("reject_nonzero 必须为 true")
	}
}

// TestHeaders 验证请求头关键字段。
func TestHeaders(t *testing.T) {
	s := NewSession("test-token-abc", "acct-123")
	h := s.headers(CheckoutRoute)

	required := map[string]string{
		"Authorization":         "Bearer test-token-abc",
		"chatgpt-account-id":    "acct-123",
		"Origin":                "https://chatgpt.com",
		"Referer":               "https://chatgpt.com/",
		"Content-Type":          "application/json",
		"x-openai-target-path":  "/backend-api/payments/checkout",
		"x-openai-target-route": "/backend-api/payments/checkout",
	}
	for k, want := range required {
		if got := h[k]; got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}

	// 首次请求不该带 attestation
	if v, ok := h["OAI-Web-Deployment-Attestation"]; ok {
		t.Errorf("首次请求不应带 attestation，实际: %q", v)
	}
	// device-id 必须是 UUID v4 形状
	if len(h["oai-device-id"]) != 36 {
		t.Errorf("oai-device-id 形状不对: %q", h["oai-device-id"])
	}
}

// TestBuildResult 验证响应解析。
func TestBuildResult(t *testing.T) {
	s := NewSession("t", "a")

	// 情形一：常见形状
	m := map[string]any{
		"checkout_session_id": "oaics_abc123",
		"processor_entity":    "openai_llc",
		"publishable_key":     "pk_live_xxx",
		"url":                 "https://chatgpt.com/checkout/openai_llc/oaics_abc123",
		"amount":              "0.00",
		"currency":            "PHP",
		"country":             "PH",
	}
	r := s.buildResult(m)
	if r.CheckoutID != "oaics_abc123" {
		t.Errorf("checkout_id = %q", r.CheckoutID)
	}
	if r.ProcessorEntity != "openai_llc" {
		t.Errorf("processor_entity = %q", r.ProcessorEntity)
	}
	if r.PublishableKey != "pk_live_xxx" {
		t.Errorf("publishable_key = %q", r.PublishableKey)
	}
	if r.Amount != "0.00" {
		t.Errorf("amount = %q", r.Amount)
	}

	// 情形二：缺字段时的默认值
	r2 := s.buildResult(map[string]any{})
	if r2.ProcessorEntity != "openai_llc" {
		t.Errorf("processor_entity 默认值应为 openai_llc，实际 %q", r2.ProcessorEntity)
	}
	if r2.Amount != "unknown" {
		t.Errorf("amount 默认值应为 unknown，实际 %q", r2.Amount)
	}

	// 情形三：真实响应结构（2026-11 实测 SG 建单）
	// subtotal 2752 + GST 248 = 3000 分 = 30.00
	r3 := s.buildResult(map[string]any{
		"checkout_state": map[string]any{
			"currency": "sgd",
			"lineItems": []any{
				map[string]any{
					"productId": "prod_N9DMeqbltqZchv",
					"quantity":  float64(1),
					"subtotal": map[string]any{
						"amount":           nil,
						"minorUnitsAmount": float64(2752),
					},
					"recurring": map[string]any{"interval": "month"},
				},
			},
			"taxAmounts": []any{
				map[string]any{
					"displayName":      "GST",
					"minorUnitsAmount": float64(248),
				},
			},
			"discountAmounts": []any{},
		},
	})
	if r3.Amount != "30.00" {
		t.Errorf("真实响应金额解析失败: got %q, want 30.00", r3.Amount)
	}
}

// TestExtractAmountZero 验证 0 元单能被正确识别。
func TestExtractAmountZero(t *testing.T) {
	s := NewSession("t", "a")

	// 促销生效：subtotal 2752，discount 2752，税 0 → 0 元
	m := map[string]any{
		"checkout_state": map[string]any{
			"lineItems": []any{
				map[string]any{
					"subtotal": map[string]any{"minorUnitsAmount": float64(2752)},
				},
			},
			"taxAmounts": []any{},
			"total": map[string]any{
				"discount": map[string]any{"minorUnitsAmount": float64(2752)},
			},
		},
	}
	r := s.buildResult(m)
	if r.Amount != "0.00" {
		t.Errorf("0 元单解析失败: got %q, want 0.00", r.Amount)
	}
	if !IsZero(r.Amount) {
		t.Errorf("IsZero(%q) 应为 true", r.Amount)
	}
}

// TestIsZero 覆盖各种金额形态。
func TestIsZero(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"0.00", true},
		{"0", true},
		{"PHP 0.00", true},
		{"27.52", false},
		{"30.00", false},
		{"unknown", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsZero(c.in); got != c.want {
			t.Errorf("IsZero(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestNewUUID 验证 UUID 形状。
func TestNewUUID(t *testing.T) {
	a, b := newUUID(), newUUID()
	if len(a) != 36 {
		t.Errorf("UUID 长度 = %d", len(a))
	}
	if a == b {
		t.Error("两次生成不应相同")
	}
	// 第 15 个字符是版本号 4
	if a[14] != '4' {
		t.Errorf("版本位不是 4: %q", a)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
