package plus

import (
	"strings"
	"testing"
)

// TestNormalizeCard 验证卡字段清洗与校验。
func TestNormalizeCard(t *testing.T) {
	// 正常卡
	out, err := normalizeCard(Card{
		Number: "4288 5202 5609 2646", CVC: "252",
		ExpMonth: "11", ExpYear: "29",
	})
	if err != nil {
		t.Fatalf("正常卡应通过: %v", err)
	}
	if out.Number != "4288520256092646" {
		t.Errorf("卡号清洗失败: %q", out.Number)
	}
	if out.ExpYear != "2029" {
		t.Errorf("两位年份应补成四位: %q", out.ExpYear)
	}
	if out.ExpMonth != "11" {
		t.Errorf("月份: %q", out.ExpMonth)
	}

	// 一位月份补零
	out2, _ := normalizeCard(Card{Number: "4288520256092646", CVC: "252",
		ExpMonth: "1", ExpYear: "2029"})
	if out2.ExpMonth != "01" {
		t.Errorf("一位月份应补零: %q", out2.ExpMonth)
	}

	// 错误：卡号太短
	if _, err := normalizeCard(Card{Number: "123", CVC: "252",
		ExpMonth: "11", ExpYear: "2029"}); err == nil {
		t.Error("短卡号应报错")
	}
	// 错误：CVC 长度
	if _, err := normalizeCard(Card{Number: "4288520256092646", CVC: "12",
		ExpMonth: "11", ExpYear: "2029"}); err == nil {
		t.Error("两位 CVC 应报错")
	}
	// 错误：月份位数
	if _, err := normalizeCard(Card{Number: "4288520256092646", CVC: "252",
		ExpMonth: "123", ExpYear: "2029"}); err == nil {
		t.Error("三位月份应报错")
	}
}

// TestFindClientSecret 验证 client_secret 提取。
func TestFindClientSecret(t *testing.T) {
	// 直接字段
	m1 := map[string]any{"client_secret": "seti_abc_secret_xyz"}
	if got := findClientSecret(m1); got != "seti_abc_secret_xyz" {
		t.Errorf("直接字段: %q", got)
	}

	// 嵌套
	m2 := map[string]any{
		"data": map[string]any{
			"setup": map[string]any{"client_secret": "seti_def_secret_uvw"},
		},
	}
	if got := findClientSecret(m2); got != "seti_def_secret_uvw" {
		t.Errorf("嵌套: %q", got)
	}

	// 只在字符串值里
	m3 := map[string]any{"note": "seti_ghi_secret_rst"}
	if got := findClientSecret(m3); got != "seti_ghi_secret_rst" {
		t.Errorf("字符串遍历: %q", got)
	}

	// 无
	if got := findClientSecret(map[string]any{"a": 1}); got != "" {
		t.Errorf("应返回空: %q", got)
	}
}

// TestFindSetupIntentID 验证 SetupIntent id 提取。
func TestFindSetupIntentID(t *testing.T) {
	// 直接给 setup_intent_id
	m1 := map[string]any{"setup_intent_id": "seti_abc123"}
	if got := findSetupIntentID(m1, ""); got != "seti_abc123" {
		t.Errorf("直接字段: %q", got)
	}

	// 从 client_secret 截取
	m2 := map[string]any{}
	if got := findSetupIntentID(m2, "seti_xyz_secret_qqq"); got != "seti_xyz" {
		t.Errorf("从 secret 截取: %q", got)
	}

	// id 字段是 seti_ 但含 _secret_ 时应跳过，改走 client_secret
	m3 := map[string]any{"id": "seti_bad_secret_xxx"}
	if got := findSetupIntentID(m3, "seti_good_secret_yyy"); got != "seti_good" {
		t.Errorf("应优先用 secret 里的 id: %q", got)
	}

	// 递归找 seti_
	m4 := map[string]any{
		"nested": map[string]any{"x": "seti_deep111"},
	}
	if got := findSetupIntentID(m4, ""); got != "seti_deep111" {
		t.Errorf("递归: %q", got)
	}

	// 无
	if got := findSetupIntentID(map[string]any{}, ""); got != "" {
		t.Errorf("应返回空: %q", got)
	}
}

// TestFindIdentifier 验证前缀查找。
func TestFindIdentifier(t *testing.T) {
	m := map[string]any{
		"a": "cus_abc",
		"b": map[string]any{"c": "pm_xyz"},
		"d": []any{"elements_session_123"},
	}
	if got := findIdentifier(m, "cus_"); got != "cus_abc" {
		t.Errorf("cus_: %q", got)
	}
	if got := findIdentifier(m, "pm_"); got != "pm_xyz" {
		t.Errorf("pm_: %q", got)
	}
	if got := findIdentifier(m, "elements_session_"); got != "elements_session_123" {
		t.Errorf("elements: %q", got)
	}
	if got := findIdentifier(m, "nope_"); got != "" {
		t.Errorf("不存在应返回空: %q", got)
	}
}

// TestStripeHeaders 验证打 Stripe 的请求头。
func TestStripeHeaders(t *testing.T) {
	s := NewSession("t", "a")
	h := s.stripeHeaders("pk_live_test", "https://chatgpt.com/checkout/openai_llc/oaics_x")

	// 不能带 Authorization
	if _, ok := h["Authorization"]; ok {
		t.Error("打 Stripe 不应带 Authorization")
	}
	// 不能带 oai-* 或 chatgpt-account-id
	for k := range h {
		lower := strings.ToLower(k)
		if strings.HasPrefix(lower, "oai-") || lower == "chatgpt-account-id" {
			t.Errorf("不应包含 %s", k)
		}
	}
	if h["Content-Type"] != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type: %q", h["Content-Type"])
	}
	if h["Origin"] != "https://js.stripe.com" {
		t.Errorf("Origin: %q", h["Origin"])
	}
	// 不该设 Stripe-Version header：Stripe 要求 header 和 _stripe_version
	// 表单参数只能出现一个，Python 侧只在 body 里设。
	if v, ok := h["Stripe-Version"]; ok {
		t.Errorf("不应设 Stripe-Version header，实际: %q", v)
	}
	if !strings.Contains(h["Referer"], "chatgpt.com/checkout") {
		t.Errorf("Referer 应指向 checkout 页: %q", h["Referer"])
	}
}

// TestBindCardRequiresOAICS 验证只接受 oaics_ 前缀。
func TestBindCardRequiresOAICS(t *testing.T) {
	s := NewSession("t", "a")
	cfg := DefaultConfig()

	_, err := s.BindCard(nil, cfg, "cs_live_abc", "openai_llc", "pk_live_x",
		Card{Number: "4288520256092646", CVC: "252", ExpMonth: "11", ExpYear: "2029"},
		Billing{})
	if err == nil {
		t.Fatal("cs_ 开头的 checkout 应报错")
	}
	if !strings.Contains(err.Error(), "oaics_") {
		t.Errorf("错误信息应提到 oaics_: %v", err)
	}
}

// TestRandHex 验证 hex 生成。
func TestRandHex(t *testing.T) {
	a := randHex(6)
	if len(a) != 6 {
		t.Errorf("长度: %d", len(a))
	}
	for _, c := range a {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("非 hex 字符: %c", c)
		}
	}
}

// TestFirstNonEmpty 验证取值优先级。
func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "b", "c"); got != "b" {
		t.Errorf("应返回 b: %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("应返回空: %q", got)
	}
}
