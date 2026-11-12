package plus

import "testing"

func TestRegionPresets(t *testing.T) {
	if len(RegionPresets) == 0 {
		t.Fatal("预设不能为空")
	}
	ids := map[string]bool{}
	for _, r := range RegionPresets {
		if r.ID == "" || r.Label == "" || r.Country == "" || r.Currency == "" {
			t.Errorf("预设字段不完整: %+v", r)
		}
		if ids[r.ID] {
			t.Errorf("ID 重复: %s", r.ID)
		}
		ids[r.ID] = true
		if len(r.Country) != 2 {
			t.Errorf("%s 国家码应为 2 位: %q", r.ID, r.Country)
		}
		if len(r.Currency) != 3 {
			t.Errorf("%s 币种码应为 3 位: %q", r.ID, r.Currency)
		}
	}
}

func TestFindRegion(t *testing.T) {
	r, ok := FindRegion("ph-low")
	if !ok {
		t.Fatal("ph-low 应存在")
	}
	if r.Country != "PH" || r.Currency != "PHP" || r.Egress != "PH" {
		t.Errorf("ph-low 字段不对: %+v", r)
	}

	// 大小写不敏感
	if _, ok := FindRegion("PH-LOW"); !ok {
		t.Error("应大小写不敏感")
	}
	if _, ok := FindRegion("nonexistent"); ok {
		t.Error("不存在的 ID 应返回 false")
	}
}

func TestRegionToConfig(t *testing.T) {
	r, _ := FindRegion("ph-low")
	base := Config{
		PromoCampaign: "plus-1-month-free",
		RejectNonzero: true,
	}
	cfg := r.ToConfig(base)
	if cfg.Country != "PH" || cfg.Currency != "PHP" {
		t.Errorf("建单国家币种不对: %+v", cfg)
	}
	if cfg.BindCountry != "PH" {
		t.Errorf("bind 未指定时应沿用建单国家: %q", cfg.BindCountry)
	}
	// 显式指定绑卡国家时不覆盖
	base2 := Config{BindCountry: "US", BindCurrency: "USD"}
	cfg2 := r.ToConfig(base2)
	if cfg2.BindCountry != "US" {
		t.Errorf("显式 bind 不应被覆盖: %q", cfg2.BindCountry)
	}
}

func TestEgressMismatch(t *testing.T) {
	ph, _ := FindRegion("ph-low")

	if msg := ph.EgressMismatch("PH"); msg != "" {
		t.Errorf("出口 PH 应匹配，实际: %s", msg)
	}
	if msg := ph.EgressMismatch("ph"); msg != "" {
		t.Errorf("应大小写不敏感")
	}
	if msg := ph.EgressMismatch("SG"); msg == "" {
		t.Error("出口 SG 应报不匹配")
	}
	if msg := ph.EgressMismatch(""); msg == "" {
		t.Error("出口未知应报错")
	}
}

func TestRegionsCopy(t *testing.T) {
	got := Regions()
	got[0].Label = "被篡改"
	if RegionPresets[0].Label == "被篡改" {
		t.Error("Regions() 应返回副本")
	}
}
