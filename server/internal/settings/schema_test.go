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
	for i, area := range []string{"mail", "proxy", "sms"} {
		if groups["other"][i].Area != area {
			t.Fatal("material setting disappeared", area)
		}
	}
	if !groups["other"][2].Secret {
		t.Fatal("unknown token must stay masked")
	}
}
