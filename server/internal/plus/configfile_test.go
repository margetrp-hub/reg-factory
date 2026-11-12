package plus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltinDefaults 验证无配置文件时的默认值合理。
func TestBuiltinDefaults(t *testing.T) {
	cf := builtinDefaults()

	if cf.Execution.Prefer != "protocol" {
		t.Errorf("prefer: %q", cf.Execution.Prefer)
	}
	if len(cf.Payment.Methods) == 0 {
		t.Error("methods 不能为空")
	}
	if cf.Risk.TimeOnPage.Distribution != "lognormal" {
		t.Errorf("默认应是对数正态: %q", cf.Risk.TimeOnPage.Distribution)
	}
	if cf.Defaults.ALPN != "h1" {
		t.Errorf("alpn: %q", cf.Defaults.ALPN)
	}
}

// TestResolveSecret 验证敏感值解析。
func TestResolveSecret(t *testing.T) {
	// 原样
	if got := ResolveSecret("plain-value"); got != "plain-value" {
		t.Errorf("原样: %q", got)
	}
	// 空
	if got := ResolveSecret(""); got != "" {
		t.Errorf("空: %q", got)
	}
	// env
	t.Setenv("TEST_SECRET_X", "from-env")
	if got := ResolveSecret("env:TEST_SECRET_X"); got != "from-env" {
		t.Errorf("env: %q", got)
	}
	// env 不存在
	if got := ResolveSecret("env:NOT_SET_XYZ"); got != "" {
		t.Errorf("不存在的 env 应返回空: %q", got)
	}
	// file
	dir := t.TempDir()
	f := filepath.Join(dir, "secret.txt")
	os.WriteFile(f, []byte("  from-file\n"), 0o600)
	if got := ResolveSecret("file:" + f); got != "from-file" {
		t.Errorf("file: %q", got)
	}
	// file 不存在
	if got := ResolveSecret("file:/nonexistent/path"); got != "" {
		t.Errorf("不存在的 file 应返回空: %q", got)
	}
}

// TestTimeOnPageDistribution 验证对数正态分布的形状。
func TestTimeOnPageDistribution(t *testing.T) {
	cf := builtinDefaults()
	cf.Risk.TimeOnPage = TimeOnPageCfg{
		Distribution: "lognormal", Mu: 4.32, Sigma: 0.75,
		MinSeconds: 25, MaxSeconds: 300,
	}

	const n = 2000
	var sum int
	var min, max int = 1 << 30, 0
	under30, over120 := 0, 0
	for i := 0; i < n; i++ {
		ms := cf.TimeOnPage()
		if ms < 25_000 || ms > 300_000 {
			t.Fatalf("越界: %d ms", ms)
		}
		sum += ms
		if ms < min {
			min = ms
		}
		if ms > max {
			max = ms
		}
		if ms < 30_000 {
			under30++
		}
		if ms > 120_000 {
			over120++
		}
	}
	avg := sum / n / 1000

	// 对数正态 mu=4.32 → 中位数 e^4.32 ≈ 75 秒，均值会更大些
	if avg < 60 || avg > 140 {
		t.Errorf("平均 %d 秒，偏离预期（中位数应约 75 秒）", avg)
	}
	// 下界确实被触及（对数正态的左尾会被 clamp 到 min_seconds）
	if min >= 30_000 {
		t.Errorf("应该出现过 30 秒以下的值（被 clamp 到下界），min=%d", min)
	}
	if min < 25_000 {
		t.Errorf("不该低于下界 25 秒，min=%d", min)
	}
	// 长尾存在但不是主流
	if over120 == 0 {
		t.Error("对数正态应该出现超过 120 秒的长尾")
	}
	// 大部分应该在 30-120 秒之间
	if under30+over120 > n/2 {
		t.Errorf("分布过于分散：under30=%d over120=%d / %d", under30, over120, n)
	}
}

// TestTimeOnPageUniform 验证均匀分布模式。
func TestTimeOnPageUniform(t *testing.T) {
	cf := builtinDefaults()
	cf.Risk.TimeOnPage = TimeOnPageCfg{
		Distribution: "uniform", MinSeconds: 10, MaxSeconds: 20,
	}
	for i := 0; i < 200; i++ {
		ms := cf.TimeOnPage()
		if ms < 10_000 || ms > 20_000 {
			t.Fatalf("均匀分布越界: %d", ms)
		}
	}
}

// TestTimeOnPageFixed 验证固定值模式。
func TestTimeOnPageFixed(t *testing.T) {
	cf := builtinDefaults()
	cf.Risk.TimeOnPage = TimeOnPageCfg{
		Distribution: "fixed", FixedSeconds: 42,
		MinSeconds: 10, MaxSeconds: 100,
	}
	for i := 0; i < 10; i++ {
		if ms := cf.TimeOnPage(); ms != 42_000 {
			t.Fatalf("固定值应为 42000: %d", ms)
		}
	}
	// 固定值超出边界时被 clamp
	cf.Risk.TimeOnPage.FixedSeconds = 999
	if ms := cf.TimeOnPage(); ms != 100_000 {
		t.Errorf("应被 clamp 到 100000: %d", ms)
	}
}

// TestPastedFieldsWeights 验证加权随机。
func TestPastedFieldsWeights(t *testing.T) {
	cf := builtinDefaults()
	cf.Risk.PastedFields.Weights = map[string]int{
		"none":   50,
		"number": 50,
	}

	const n = 4000
	counts := map[string]int{}
	for i := 0; i < n; i++ {
		counts[cf.PastedFields()]++
	}

	// 两个选项各占约一半
	if counts[""] < n*3/10 || counts[""] > n*7/10 {
		t.Errorf("none 占比异常: %d/%d", counts[""], n)
	}
	if counts["number"] < n*3/10 || counts["number"] > n*7/10 {
		t.Errorf("number 占比异常: %d/%d", counts["number"], n)
	}
	// 不该出现别的
	if counts["number,exp"] != 0 {
		t.Errorf("出现了未配置的选项: %d", counts["number,exp"])
	}
}

// TestPastedFieldsSingleOption 验证单一选项。
func TestPastedFieldsSingleOption(t *testing.T) {
	cf := builtinDefaults()
	cf.Risk.PastedFields.Weights = map[string]int{"number,exp": 10}
	for i := 0; i < 20; i++ {
		if got := cf.PastedFields(); got != "number,exp" {
			t.Errorf("应始终返回 number,exp: %q", got)
		}
	}
}

// TestStripeJSVersion 验证版本号选择。
func TestStripeJSVersion(t *testing.T) {
	cf := builtinDefaults()
	// 指定固定值
	cf.Risk.StripeJSVersion = "abc123"
	if got := cf.StripeJSVersion(); got != "abc123" {
		t.Errorf("固定版本: %q", got)
	}
	// 从列表里选
	cf.Risk.StripeJSVersion = ""
	cf.Risk.StripeJSVersions = []string{"v1"}
	if got := cf.StripeJSVersion(); got != "v1" {
		t.Errorf("列表: %q", got)
	}
	// UA 拼接
	if ua := cf.PaymentUserAgent(); ua != "stripe.js/v1; stripe-js-v3/v1; card-element" {
		t.Errorf("UA: %q", ua)
	}
}

// TestLoadConfigFileMissing 验证配置文件不存在时的行为。
func TestLoadConfigFileMissing(t *testing.T) {
	cf, err := LoadConfigFile("/nonexistent/plus.yaml")
	if err != nil {
		t.Fatalf("文件不存在不该报错: %v", err)
	}
	if cf == nil || cf.Execution.Prefer == "" {
		t.Error("应返回内置默认值")
	}
}

// TestLoadConfigFileValid 验证正常加载。
func TestLoadConfigFileValid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "plus.yaml")
	content := `
execution:
  prefer: browser
payment:
  methods: [link]
  card:
    number: "env:CARD_NUM"
    billing:
      country: US
risk:
  time_on_page:
    distribution: fixed
    fixed_seconds: 60
defaults:
  region: ph-low
  reject_nonzero: false
`
	os.WriteFile(p, []byte(content), 0o600)

	cf, err := LoadConfigFile(p)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cf.Execution.Prefer != "browser" {
		t.Errorf("prefer: %q", cf.Execution.Prefer)
	}
	if len(cf.Payment.Methods) != 1 || cf.Payment.Methods[0] != "link" {
		t.Errorf("methods: %v", cf.Payment.Methods)
	}
	if cf.Payment.Card.Billing.Country != "US" {
		t.Errorf("country: %q", cf.Payment.Card.Billing.Country)
	}
	if cf.Risk.TimeOnPage.Distribution != "fixed" {
		t.Errorf("distribution: %q", cf.Risk.TimeOnPage.Distribution)
	}
	if cf.Defaults.Region != "ph-low" {
		t.Errorf("region: %q", cf.Defaults.Region)
	}
	if cf.Defaults.RejectNonzero {
		t.Error("reject_nonzero 应为 false")
	}
	// 未填的字段要有默认值
	if cf.Risk.StripeJSVersions == nil || len(cf.Risk.StripeJSVersions) == 0 {
		t.Error("缺省字段应被补齐")
	}
}

// TestCardReady 验证卡信息检查。
func TestCardReady(t *testing.T) {
	cf := builtinDefaults()

	// 未配置
	ok, msg := cf.CardReady()
	if ok {
		t.Error("未配卡时应返回 false")
	}
	if !strings.Contains(msg, "卡号") {
		t.Errorf("提示应提到卡号: %q", msg)
	}

	// 配好
	t.Setenv("CARD_NUMBER", "4288520256092646")
	t.Setenv("CARD_CVC", "252")
	t.Setenv("CARD_M", "11")
	t.Setenv("CARD_Y", "2029")
	cf.Payment.Card.Number = "env:CARD_NUMBER"
	cf.Payment.Card.CVC = "env:CARD_CVC"
	cf.Payment.Card.ExpMonth = "env:CARD_M"
	cf.Payment.Card.ExpYear = "env:CARD_Y"

	ok, msg = cf.CardReady()
	if !ok {
		t.Errorf("应通过: %s", msg)
	}
	c := cf.ResolvedCard()
	if c.Number != "4288520256092646" {
		t.Errorf("卡号解析: %q", c.Number)
	}
}

// TestResolvedBilling 验证账单地址解析。
func TestResolvedBilling(t *testing.T) {
	cf := builtinDefaults()
	cf.Payment.Card.Billing = BillingCfg{
		Name: "Test User", Line1: "1 Main St", City: "Wilmington",
		State: "DE", PostalCode: "19801", Country: "US",
	}
	b := cf.ResolvedBilling()
	if b.Name != "Test User" || b.PostalCode != "19801" || b.Country != "US" {
		t.Errorf("解析结果: %+v", b)
	}
}
