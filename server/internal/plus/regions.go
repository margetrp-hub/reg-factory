package plus

import "strings"

// Region 是一个建单区域预设。
//
// 设计意图
//   - UI 上呈现为下拉选项，用户选「菲律宾（低价区）」而不是手填 PH/PHP
//   - Egress 把「国家/出口匹配」这个隐含约束显式化：
//     选了 PH 预设，就必须有 PH 出口，否则提前报错，不用等 OpenAI 返回 400
//   - 价格档位差异写在 Note 里，便于运维判断哪个划算
type Region struct {
	ID       string `json:"id"`       // 稳定标识，UI 用它做 value
	Label    string `json:"label"`    // 显示名
	Country  string `json:"country"`  // 建单国家（ISO 3166-1 alpha-2）
	Currency string `json:"currency"` // 建单币种（ISO 4217）
	Egress   string `json:"egress"`   // 需要的出口地区
	Note     string `json:"note"`     // 价格档位说明
	Enabled  bool   `json:"enabled"`  // UI 上是否可选
}

// RegionPresets 是内置区域预设。
//
// 价格依据：本轮实测与 SG 现有 standalone_config.json。
// 新增区域只需往这里加一条，逻辑代码不用改。
var RegionPresets = []Region{
	{
		ID: "ph-low", Label: "菲律宾（低价区）",
		Country: "PH", Currency: "PHP", Egress: "PH",
		Note:    "Plus 低价区，需 PH 出口才能建单",
		Enabled: true,
	},
	{
		ID: "th-low", Label: "泰国（低价区）",
		Country: "TH", Currency: "THB", Egress: "TH",
		Note:    "Plus 699 THB/月，需 TH 出口",
		Enabled: true,
	},
	{
		ID: "sg-std", Label: "新加坡（标准）",
		Country: "SG", Currency: "SGD", Egress: "SG",
		Note:    "Plus 30.00 SGD/月（含 9% GST），SG 出口可直连",
		Enabled: true,
	},
	{
		ID: "us-retail", Label: "美国（零售）",
		Country: "US", Currency: "USD", Egress: "US",
		Note:    "Plus 20.00 USD/月，需 US 出口",
		Enabled: true,
	},
}

// FindRegion 按 ID 查预设。
func FindRegion(id string) (Region, bool) {
	target := strings.ToLower(strings.TrimSpace(id))
	for _, r := range RegionPresets {
		if strings.EqualFold(r.ID, target) {
			return r, true
		}
	}
	return Region{}, false
}

// enabledRegions 返回 UI 上可选的预设。
func enabledRegions() []Region {
	out := make([]Region, 0, len(RegionPresets))
	for _, r := range RegionPresets {
		if r.Enabled {
			out = append(out, r)
		}
	}
	return out
}

// ToConfig 把区域预设落到 Config 上。
//
// bindCountry 为空时沿用建单国家；绑卡国家通常可以和建单国家不同，
// 由调用方显式指定（例如 PH 建单 + US 绑卡）。
func (r Region) ToConfig(base Config) Config {
	cfg := base
	cfg.Country = r.Country
	cfg.Currency = r.Currency
	if strings.TrimSpace(cfg.BindCountry) == "" {
		cfg.BindCountry = r.Country
	}
	if strings.TrimSpace(cfg.BindCurrency) == "" {
		cfg.BindCurrency = r.Currency
	}
	return cfg
}

// EgressMismatch 判断实际出口地区与预设要求是否不符。
//
// 返回空字符串表示匹配；否则返回可直接展示给用户的错误说明。
//
// actualCountry 是从 ipinfo 之类服务查到的出口国家。
func (r Region) EgressMismatch(actualCountry string) string {
	actual := strings.ToUpper(strings.TrimSpace(actualCountry))
	if actual == "" {
		return "出口地区未知，无法校验国家匹配"
	}
	want := strings.ToUpper(strings.TrimSpace(r.Egress))
	if want == "" || actual == want {
		return ""
	}
	return "区域「" + r.Label + "」要求 " + want + " 出口，当前出口是 " + actual +
		"，OpenAI 会返回 Billing country must match request country"
}

// Regions 返回所有预设的副本，供 UI 拉取。
func Regions() []Region {
	src := enabledRegions()
	out := make([]Region, len(src))
	copy(out, src)
	return out
}
