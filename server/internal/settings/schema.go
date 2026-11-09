package settings

import (
	"encoding/json"
	"os"
	"strings"
)

type ConfigField struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Help     string   `json:"help"`
	Type     string   `json:"type"`
	Choices  []string `json:"choices,omitempty"`
	Default  any      `json:"default,omitempty"`
	Secret   bool     `json:"secret"`
	Apply    string   `json:"apply,omitempty"`
	ReadOnly bool     `json:"readonly,omitempty"`
	Area     string   `json:"area,omitempty"`
}
type ConfigGroup struct {
	Group string        `json:"group"`
	Area  string        `json:"area"`
	Items []ConfigField `json:"items"`
}
type ConfigEntry struct {
	ConfigField
	Value string `json:"value"`
	Line  int    `json:"line"`
}

func LoadConfigSchema(path string) ([]ConfigGroup, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var groups []ConfigGroup
	err = json.Unmarshal(raw, &groups)
	return groups, err
}

// MergeConfig preserves the first configured value and every unknown key. An
// absent setting remains unset; a schema default is guidance, never a write.
func MergeConfig(schema []ConfigGroup, entries []Entry, fallback func(string) string) map[string][]ConfigEntry {
	actual := map[string]Entry{}
	for _, entry := range entries {
		if _, ok := actual[entry.Key]; !ok {
			actual[entry.Key] = entry
		}
	}
	groups := map[string][]ConfigEntry{}
	seen := map[string]bool{}
	for _, group := range schema {
		for _, field := range group.Items {
			if seen[field.Key] {
				continue
			}
			seen[field.Key] = true
			entry := actual[field.Key]
			field.Secret = field.Secret || Secret(field.Key)
			field.Area = group.Area
			groups[group.Group] = append(groups[group.Group], ConfigEntry{ConfigField: field, Value: entry.Value, Line: entry.Line})
		}
	}
	for _, entry := range entries {
		if seen[entry.Key] {
			continue
		}
		seen[entry.Key] = true
		group := fallback(entry.Key)
		groups[group] = append(groups[group], ConfigEntry{ConfigField: ConfigField{Key: entry.Key, Secret: entry.Secret || Secret(entry.Key), Area: configArea(entry.Key)}, Value: entry.Value, Line: entry.Line})
	}
	return groups
}

// Preserve provider-specific keys outside the catalog in their material area.
func configArea(key string) string {
	upper := strings.ToUpper(key)
	for _, category := range []struct {
		area     string
		prefixes []string
	}{
		{"proxy", []string{"PROXY", "CLASH", "REG_FACTORY_PROXY", "REG_FACTORY_PRODUCTION_PROXY"}},
		{"mail", []string{"ICMAIL", "ICLOUD", "YYDS", "GPTMAIL", "CFMAIL", "MOEMAIL", "REMAIL", "CUSTOM_MAIL", "OUTLOOK", "CHATGPT_EMAIL", "CHATGPT_VERIFICATION"}},
		{"sms", []string{"SMS", "HERO", "CODEX_PHONE", "CODEX_SMS", "CUSTOM_SMS"}},
		{"captcha", []string{"CAPSOLVER", "EZCAPTCHA", "YESCAPTCHA", "VISION", "VOTE", "GEMMA"}},
		{"browser", []string{"BROWSER", "BITBROWSER", "ADSPOWER", "CHROMIUM", "CLOAK"}},
		{"node", []string{"REG_FACTORY_NODE_"}},
	} {
		for _, prefix := range category.prefixes {
			if strings.HasPrefix(upper, prefix) {
				return category.area
			}
		}
	}
	return "custom"
}
