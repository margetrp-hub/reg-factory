package settings

import "testing"

func TestConfigMergePreservesMissingConfiguredAndUnrecognizedFields(t *testing.T) {
	schema := []ConfigGroup{{Group: "network", Items: []ConfigField{{Key: "PROXY_MODE", Default: "direct", Choices: []string{"direct", "clash"}}, {Key: "API_KEY", Secret: true}}}}
	entries := []Entry{{Key: "PROXY_MODE", Value: "clash", Line: 2}, {Key: "PROXY_MODE", Value: "ignored", Line: 3}, {Key: "CUSTOM_LOCAL_SETTING", Value: "retained", Line: 4}}
	merged := MergeConfig(schema, entries, func(string) string { return "other" })
	if len(merged["network"]) != 2 || len(merged["other"]) != 1 {
		t.Fatalf("fields lost: %+v", merged)
	}
	if merged["network"][0].Value != "clash" || merged["network"][0].Line != 2 {
		t.Fatal("first saved setting lost")
	}
	if merged["network"][1].Value != "" || merged["network"][1].Line != 0 || !merged["network"][1].Secret {
		t.Fatal("missing field became configured or secret lost")
	}
	if merged["other"][0].Value != "retained" {
		t.Fatal("custom setting lost")
	}
}

func TestUnknownProviderSettingsRemainInMaterials(t *testing.T) {
	entries := []Entry{{Key: "ICMAIL_CUSTOM_HOST", Value: "fixture", Line: 1}, {Key: "REG_FACTORY_PROXY_URL", Value: "fixture", Line: 2}, {Key: "SMS_NEW_TOKEN", Value: "fixture", Line: 3}}
	groups := MergeConfig(nil, entries, func(string) string { return "other" })
	for group, area := range map[string]string{"邮箱 · IC 邮箱池": "mail", "网络 · 住宅代理": "proxy", "短信 · 通用与重试": "sms"} {
		if len(groups[group]) != 1 || groups[group][0].Area != area {
			t.Fatal("material setting disappeared", group)
		}
	}
	if !groups["短信 · 通用与重试"][0].Secret {
		t.Fatal("unknown token must stay masked")
	}
}

func TestUnknownProviderOptionsJoinTheirExistingObject(t *testing.T) {
	schema := []ConfigGroup{{Group: "浏览器 · RoxyBrowser", Area: "browser", Items: []ConfigField{{Key: "ROXY_API"}}}, {Group: "对接 · SUB2API", Area: "integration", Items: []ConfigField{{Key: "SUB2API_URL"}}}}
	entries := []Entry{{Key: "ROXY_API", Value: "fixture", Line: 1}, {Key: "ROXY_PROXY_TOKEN", Value: "private", Line: 2}, {Key: "SUB2API_PROXY_TOKEN", Value: "private", Line: 3}, {Key: "ROXY_PROXY_TOKEN", Value: "ignored", Line: 4}}
	groups := MergeConfig(schema, entries, func(string) string { return "网络出口" })
	for group, area := range map[string]string{"浏览器 · RoxyBrowser": "browser", "对接 · SUB2API": "integration"} {
		if len(groups[group]) != 2 || groups[group][1].Area != area || !groups[group][1].Secret || groups[group][1].Value != "private" {
			t.Fatalf("provider option split or modified: %+v", groups[group])
		}
	}
}
