package plus

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	mathrand "math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------- 配置结构

// ConfigFile 对应 /data/config/plus.yaml。
type ConfigFile struct {
	Execution ExecCfg              `yaml:"execution"`
	Payment   PayCfg               `yaml:"payment"`
	Risk      RiskCfg              `yaml:"risk"`
	Retry     RetryCfg             `yaml:"retry"`
	Stages    StagesCfg            `yaml:"stages"`
	Regions   map[string]RegionDef `yaml:"regions"`
	Defaults  DefaultsCfg          `yaml:"defaults"`
}

// ExecCfg 是执行策略。
type ExecCfg struct {
	// Prefer protocol / browser / auto
	Prefer     string   `yaml:"prefer"`
	FallbackOn []string `yaml:"fallback_on"`
}

// PayCfg 是支付方式。
type PayCfg struct {
	Methods  []string    `yaml:"methods"`
	Card     CardCfg     `yaml:"card"`
	Link     LinkCfg     `yaml:"link"`
	HCaptcha HCaptchaCfg `yaml:"hcaptcha"`
}

// CardCfg 是卡信息。敏感字段支持 env: / file: 引用。
type CardCfg struct {
	Number   string     `yaml:"number"`
	ExpMonth string     `yaml:"exp_month"`
	ExpYear  string     `yaml:"exp_year"`
	CVC      string     `yaml:"cvc"`
	Billing  BillingCfg `yaml:"billing"`
}

// BillingCfg 是账单地址。
type BillingCfg struct {
	Name       string `yaml:"name"`
	Email      string `yaml:"email"`
	Line1      string `yaml:"line1"`
	Line2      string `yaml:"line2"`
	City       string `yaml:"city"`
	State      string `yaml:"state"`
	PostalCode string `yaml:"postal_code"`
	Country    string `yaml:"country"`
	Phone      string `yaml:"phone"`
}

// LinkCfg 是 Stripe Link。
type LinkCfg struct {
	Enabled bool   `yaml:"enabled"`
	Email   string `yaml:"email"`
}

// HCaptchaCfg 是打码平台。
type HCaptchaCfg struct {
	Provider string `yaml:"provider"`
	APIKey   string `yaml:"api_key"`
	Timeout  int    `yaml:"timeout"`
	Retries  int    `yaml:"retries"`
}

// RiskCfg 是风控参数。
type RiskCfg struct {
	TimeOnPage       TimeOnPageCfg   `yaml:"time_on_page"`
	PastedFields     PastedFieldsCfg `yaml:"pasted_fields"`
	StripeJSVersions []string        `yaml:"stripe_js_versions"`
	StripeJSVersion  string          `yaml:"stripe_js_version"`
	DeviceIDHexLen   int             `yaml:"device_id_hex_len"`
}

// TimeOnPageCfg 控制填卡停留时间的分布。
type TimeOnPageCfg struct {
	// Distribution lognormal / uniform / fixed
	Distribution string  `yaml:"distribution"`
	Mu           float64 `yaml:"mu"`
	Sigma        float64 `yaml:"sigma"`
	MinSeconds   int     `yaml:"min_seconds"`
	MaxSeconds   int     `yaml:"max_seconds"`
	FixedSeconds int     `yaml:"fixed_seconds"`
}

// PastedFieldsCfg 控制粘贴字段的加权随机。
type PastedFieldsCfg struct {
	Weights map[string]int `yaml:"weights"`
}

// RetryCfg 是重试策略。
type RetryCfg struct {
	SetupConfirm         int `yaml:"setup_confirm"`
	SetupConfirm402Delay int `yaml:"setup_confirm_402_delay"`
	Checkout             int `yaml:"checkout"`
	CheckoutDelay        int `yaml:"checkout_delay"`
}

// RegionDef 是配置文件里定义的地区。
type RegionDef struct {
	Label    string `yaml:"label"`
	Country  string `yaml:"country"`
	Currency string `yaml:"currency"`
	Egress   string `yaml:"egress"`
	Note     string `yaml:"note"`
	Enabled  bool   `yaml:"enabled"`
}

// DefaultsCfg 是默认值。
type DefaultsCfg struct {
	Region         string `yaml:"region"`
	PromoCampaign  string `yaml:"promo_campaign"`
	RejectNonzero  bool   `yaml:"reject_nonzero"`
	Timeout        int    `yaml:"timeout"`
	ALPN           string `yaml:"alpn"`
	TLSImpersonate string `yaml:"tls_impersonate"`
}

// ---------------------------------------------------------------- 加载

var (
	cfgFileMu sync.RWMutex
	cfgFile   *ConfigFile
	cfgPath   string
)

// DefaultConfigPath 是默认的配置文件位置。
const DefaultConfigPath = "/data/config/plus.yaml"

// LoadConfigFile 读配置文件。路径为空时用 DefaultConfigPath。
//
// 文件不存在时返回内置默认值而不是报错 —— 让程序能在没配置的情况下启动，
// 只是支付相关功能会因为缺卡而失败（在调用时明确报错，不是启动失败）。
func LoadConfigFile(path string) (*ConfigFile, error) {
	if path == "" {
		path = DefaultConfigPath
	}

	var cf ConfigFile
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cf = builtinDefaults()
			cfgFileMu.Lock()
			cfgFile, cfgPath = &cf, path
			cfgFileMu.Unlock()
			return &cf, nil
		}
		return nil, fmt.Errorf("读配置文件 %s: %w", path, err)
	}

	if err := yaml.Unmarshal(raw, &cf); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s: %w", path, err)
	}
	applyBuiltinFallbacks(&cf)

	cfgFileMu.Lock()
	cfgFile, cfgPath = &cf, path
	cfgFileMu.Unlock()
	return &cf, nil
}

// CurrentConfig 返回已加载的配置（可能为 nil）。
func CurrentConfig() *ConfigFile {
	cfgFileMu.RLock()
	defer cfgFileMu.RUnlock()
	return cfgFile
}

// ReloadConfig 重新读配置。
func ReloadConfig() (*ConfigFile, error) {
	cfgFileMu.RLock()
	p := cfgPath
	cfgFileMu.RUnlock()
	if p == "" {
		p = DefaultConfigPath
	}
	return LoadConfigFile(p)
}

// builtinDefaults 返回没有配置文件时的内置默认值。
func builtinDefaults() ConfigFile {
	return ConfigFile{
		Execution: ExecCfg{Prefer: "protocol",
			FallbackOn: []string{"403", "requires_action", "hcaptcha"}},
		Payment: PayCfg{Methods: []string{"card"}},
		Risk: RiskCfg{
			TimeOnPage: TimeOnPageCfg{
				Distribution: "lognormal", Mu: 4.32, Sigma: 0.75,
				MinSeconds: 25, MaxSeconds: 300, FixedSeconds: 75,
			},
			PastedFields: PastedFieldsCfg{Weights: map[string]int{
				"none": 20, "number": 35, "number,exp": 25,
				"number,exp,cvc": 10, "number,exp,cvc,zip": 10,
			}},
			StripeJSVersions: []string{"3704557c13"},
			DeviceIDHexLen:   6,
		},
		Retry: RetryCfg{SetupConfirm: 1, SetupConfirm402Delay: 2,
			Checkout: 2, CheckoutDelay: 3},
		Defaults: DefaultsCfg{
			Region: "sg-std", PromoCampaign: "plus-1-month-free",
			RejectNonzero: true, Timeout: 90, ALPN: "h1",
			TLSImpersonate: "chrome-latest",
		},
	}
}

// applyBuiltinFallbacks 给缺字段填默认值。
func applyBuiltinFallbacks(cf *ConfigFile) {
	def := builtinDefaults()
	if cf.Execution.Prefer == "" {
		cf.Execution.Prefer = def.Execution.Prefer
	}
	if len(cf.Execution.FallbackOn) == 0 {
		cf.Execution.FallbackOn = def.Execution.FallbackOn
	}
	if len(cf.Payment.Methods) == 0 {
		cf.Payment.Methods = def.Payment.Methods
	}
	if cf.Risk.TimeOnPage.Distribution == "" {
		cf.Risk.TimeOnPage = def.Risk.TimeOnPage
	}
	if cf.Risk.TimeOnPage.Mu == 0 {
		cf.Risk.TimeOnPage.Mu = 4.32
	}
	if cf.Risk.TimeOnPage.Sigma == 0 {
		cf.Risk.TimeOnPage.Sigma = 0.75
	}
	if cf.Risk.TimeOnPage.MinSeconds == 0 {
		cf.Risk.TimeOnPage.MinSeconds = 25
	}
	if cf.Risk.TimeOnPage.MaxSeconds == 0 {
		cf.Risk.TimeOnPage.MaxSeconds = 300
	}
	if len(cf.Risk.PastedFields.Weights) == 0 {
		cf.Risk.PastedFields.Weights = def.Risk.PastedFields.Weights
	}
	if len(cf.Risk.StripeJSVersions) == 0 {
		cf.Risk.StripeJSVersions = def.Risk.StripeJSVersions
	}
	if cf.Risk.DeviceIDHexLen <= 0 {
		cf.Risk.DeviceIDHexLen = 6
	}
	if cf.Retry.SetupConfirm <= 0 {
		cf.Retry.SetupConfirm = 1
	}
	if cf.Retry.Checkout <= 0 {
		cf.Retry.Checkout = 2
	}
	if cf.Defaults.Timeout <= 0 {
		cf.Defaults.Timeout = 90
	}
	if cf.Defaults.ALPN == "" {
		cf.Defaults.ALPN = "h1"
	}
	if cf.Defaults.TLSImpersonate == "" {
		cf.Defaults.TLSImpersonate = "chrome-latest"
	}
	if cf.Defaults.Region == "" {
		cf.Defaults.Region = "sg-std"
	}
}

// ---------------------------------------------------------------- 敏感值解析

// ResolveSecret 解析配置里的敏感值引用。
//
//	env:VAR_NAME      从环境变量读
//	file:/path/to/f   从文件读（trim 空白）
//	其它              原样返回
//
// 解析失败返回空字符串，由调用方判断是否可用。
func ResolveSecret(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(v, "env:"):
		return strings.TrimSpace(settings.NewEnv(settings.Load().EnvFile).Effective(strings.TrimPrefix(v, "env:")))
	case strings.HasPrefix(v, "file:"):
		p := strings.TrimPrefix(v, "file:")
		raw, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(raw))
	default:
		return v
	}
}

// ResolvedCard 返回解析后的卡信息。
func (cf *ConfigFile) ResolvedCard() Card {
	return Card{
		Number:   ResolveSecret(cf.Payment.Card.Number),
		ExpMonth: ResolveSecret(cf.Payment.Card.ExpMonth),
		ExpYear:  ResolveSecret(cf.Payment.Card.ExpYear),
		CVC:      ResolveSecret(cf.Payment.Card.CVC),
	}
}

// ResolvedBilling 返回解析后的账单地址。
func (cf *ConfigFile) ResolvedBilling() Billing {
	b := cf.Payment.Card.Billing
	return Billing{
		Name:       ResolveSecret(b.Name),
		Email:      ResolveSecret(b.Email),
		Line1:      ResolveSecret(b.Line1),
		Line2:      ResolveSecret(b.Line2),
		City:       ResolveSecret(b.City),
		State:      ResolveSecret(b.State),
		PostalCode: ResolveSecret(b.PostalCode),
		Country:    ResolveSecret(b.Country),
		Phone:      ResolveSecret(b.Phone),
	}
}

// CardReady 检查卡信息是否齐全。
func (cf *ConfigFile) CardReady() (bool, string) {
	c := cf.ResolvedCard()
	if c.Number == "" {
		return false, "卡号未配置（检查 PLUS_CARD_NUMBER 或配置文件）"
	}
	if c.CVC == "" {
		return false, "CVC 未配置"
	}
	if c.ExpMonth == "" || c.ExpYear == "" {
		return false, "有效期未配置"
	}
	return true, ""
}

// ---------------------------------------------------------------- 风控参数生成

// TimeOnPage 按配置的分布生成填卡停留时间（毫秒）。
//
// 对数正态分布比均匀分布更接近真实用户：
// mu=4.32 时中位数约 75 秒，多数落在 40-140 秒，长尾到几分钟。
func (cf *ConfigFile) TimeOnPage() int {
	t := cf.Risk.TimeOnPage
	switch strings.ToLower(t.Distribution) {
	case "fixed":
		return clampSeconds(t.FixedSeconds, t.MinSeconds, t.MaxSeconds) * 1000
	case "uniform":
		lo, hi := t.MinSeconds, t.MaxSeconds
		if hi <= lo {
			hi = lo + 1
		}
		return (lo + mathrand.Intn(hi-lo+1)) * 1000
	default: // lognormal
		v := math.Exp(t.Mu + t.Sigma*normalRand())
		secs := int(math.Round(v))
		return clampSeconds(secs, t.MinSeconds, t.MaxSeconds) * 1000
	}
}

// PastedFields 按权重随机选一种粘贴方案。
//
// 返回空字符串表示「全部手输」，此时不传 pasted_fields 字段。
func (cf *ConfigFile) PastedFields() string {
	w := cf.Risk.PastedFields.Weights
	if len(w) == 0 {
		return "number,exp,cvc"
	}
	// 稳定顺序：按 key 排序，避免 map 遍历随机导致权重抖动
	keys := make([]string, 0, len(w))
	total := 0
	for k, v := range w {
		if v <= 0 {
			continue
		}
		keys = append(keys, k)
		total += v
	}
	if total == 0 {
		return ""
	}
	// 简单插入排序，key 数量很少（<10）
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}

	pick := mathrand.Intn(total)
	acc := 0
	for _, k := range keys {
		acc += w[k]
		if pick < acc {
			if k == "none" {
				return ""
			}
			return k
		}
	}
	return ""
}

// StripeJSVersion 返回要用的 Stripe.js 版本号。
func (cf *ConfigFile) StripeJSVersion() string {
	if v := strings.TrimSpace(cf.Risk.StripeJSVersion); v != "" {
		return v
	}
	vs := cf.Risk.StripeJSVersions
	if len(vs) == 0 {
		return "3704557c13"
	}
	return vs[mathrand.Intn(len(vs))]
}

// PaymentUserAgent 拼出 payment_user_agent 字段。
func (cf *ConfigFile) PaymentUserAgent() string {
	v := cf.StripeJSVersion()
	return fmt.Sprintf("stripe.js/%s; stripe-js-v3/%s; card-element", v, v)
}

// DeviceIDHexLen 返回 guid/muid/sid 的 hex 后缀长度。
func (cf *ConfigFile) DeviceIDHexLen() int {
	if n := cf.Risk.DeviceIDHexLen; n > 0 {
		return n
	}
	return 6
}

// ---------------------------------------------------------------- 随机辅助

// normalRand 返回标准正态分布随机数。
//
// 用 Box-Muller 变换。种子来自 crypto/rand，避免被时间推断。
func normalRand() float64 {
	for {
		u1, u2 := secureFloat(), secureFloat()
		if u1 <= 0 {
			continue
		}
		return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
	}
}

var (
	seedOnce sync.Once
)

// secureFloat 返回 (0,1) 的随机数，种子来自 crypto/rand。
func secureFloat() float64 {
	seedOnce.Do(func() {
		var b [8]byte
		if _, err := rand.Read(b[:]); err == nil {
			mathrand.Seed(int64(binary.LittleEndian.Uint64(b[:])))
		} else {
			mathrand.Seed(time.Now().UnixNano())
		}
	})
	return mathrand.Float64()
}

func clampSeconds(v, lo, hi int) int {
	if lo <= 0 {
		lo = 1
	}
	if hi <= lo {
		hi = lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
