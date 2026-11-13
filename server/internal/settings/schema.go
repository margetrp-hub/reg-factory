package settings

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type ConfigField struct {
	Key          string              `json:"key"`
	Label        string              `json:"label"`
	Help         string              `json:"help"`
	Type         string              `json:"type"`
	Choices      []string            `json:"choices,omitempty"`
	ChoiceLabels map[string]string   `json:"choice_labels,omitempty"`
	When         map[string][]string `json:"when,omitempty"`
	Default      any                 `json:"default,omitempty"`
	Secret       bool                `json:"secret"`
	Apply        string              `json:"apply,omitempty"`
	ReadOnly     bool                `json:"readonly,omitempty"`
	Advanced     bool                `json:"advanced,omitempty"`
	Generate     bool                `json:"generate,omitempty"`
	Area         string              `json:"area,omitempty"`
	EmptyDefault bool                `json:"empty_default,omitempty"`
}
type ConfigGroup struct {
	Group string        `json:"group"`
	Area  string        `json:"area"`
	Items []ConfigField `json:"items"`
}
type ConfigEntry struct {
	ConfigField
	Value           string `json:"value"`
	Line            int    `json:"line"`
	EffectiveValue  string `json:"effective_value"`
	EffectiveSource string `json:"effective_source"`
	EffectiveSet    bool   `json:"effective_set"`
	Overridden      bool   `json:"overridden"`
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
		group := configSection(entry.Key, fallback(entry.Key))
		groups[group] = append(groups[group], ConfigEntry{ConfigField: ConfigField{Key: entry.Key, Secret: entry.Secret || Secret(entry.Key), Area: configArea(entry.Key)}, Value: entry.Value, Line: entry.Line})
	}
	for name, list := range groups {
		for i := range list {
			entry := &list[i]
			if DeploymentOnly(entry.Key) || entry.ReadOnly {
				entry.ReadOnly, entry.Apply = true, "deployment"
			} else if entry.Apply == "restart" {
				entry.Apply = "live"
			}
			if strings.HasPrefix(entry.Key, "ICMAIL_IMAP_") || strings.HasPrefix(entry.Key, "MAIL_IMAP_") {
				entry.Apply = "seed"
				entry.ReadOnly = true
				entry.Help = "仅在空来源库首次启动时预置收件来源；已有邮箱的登录资料请在邮箱池的收件来源中修改。"
			}
			value, source := entry.Value, "file"
			if deployed, exists := DeploymentValue(entry.Key); exists && (entry.Line == 0 || entry.ReadOnly) {
				value, source = deployed, "deployment"
			} else if entry.Line == 0 {
				value, source = "", "unset"
				if entry.Default != nil {
					value, source = fmt.Sprint(entry.Default), "default"
				}
			}
			if entry.EmptyDefault {
				value = strings.TrimSpace(value)
				if value == "" && entry.Default != nil {
					value, source = fmt.Sprint(entry.Default), "default"
				}
			}
			entry.EffectiveSource, entry.EffectiveSet = source, value != ""
			if !entry.Secret {
				entry.EffectiveValue = value
				if entry.Line == 0 || entry.ReadOnly {
					entry.Value = value
				}
			}
		}
		groups[name] = list
	}
	return groups
}

// Uncatalogued provider options belong beside that provider's standard options.
// Keep an explicit prefix boundary: a provider's proxy option is still theirs.
func configSection(key, fallback string) string {
	upper := strings.ToUpper(key)
	for _, section := range []struct {
		name     string
		prefixes []string
	}{
		{"网络 · 模式与平台覆盖", []string{"OUTLOOK_PROXY", "CLAUDE_PROXY", "CHATGPT_PROXY", "GROK_PROXY", "KIRO_PROXY", "GITHUB_PROXY", "PROXY_MODE"}},
		{"网络 · 生产与凭据修复", []string{"REG_FACTORY_PRODUCTION_PROXY"}},
		{"网络 · 住宅代理", []string{"REG_FACTORY_PROXY", "REG_FACTORY_RESIDENTIAL_"}},
		{"网络 · Clash", []string{"CLASH_"}},
		{"浏览器 · 自定义 API", []string{"CUSTOM_BROWSER_API"}},
		{"浏览器 · 本机 Chrome", []string{"CUSTOM_BROWSER_PATH"}},
		{"浏览器 · BitBrowser 与内核", []string{"BITBROWSER_", "BB_CORE_"}},
		{"浏览器 · CloakBrowser", []string{"CLOAK_"}},
		{"浏览器 · RoxyBrowser", []string{"ROXY_"}},
		{"浏览器 · AdsPower", []string{"ADSPOWER_"}},
		{"浏览器 · 通用设置", []string{"REG_FACTORY_BROWSER_", "BROWSER_", "CHROMIUM_"}},
		{"邮箱 · IC 邮箱池", []string{"ICMAIL_", "MAIL_IMAP_"}},
		{"邮箱 · iCloud", []string{"ICLOUD_MAIL_"}},
		{"邮箱 · Outlook 与辅助邮箱", []string{"OUTLOOK_"}},
		{"邮箱 · 自定义邮箱", []string{"CUSTOM_MAIL_"}},
		{"邮箱 · YYDS", []string{"YYDS_"}},
		{"邮箱 · GPTMail", []string{"GPTMAIL_"}},
		{"邮箱 · MoeMail", []string{"MOEMAIL_"}},
		{"邮箱 · CFMail", []string{"CFMAIL_"}},
		{"邮箱 · Remail", []string{"REMAIL_"}},
		{"邮箱 · 收码与等待", []string{"CHATGPT_VERIFICATION_"}},
		{"短信 · SMS-Man", []string{"SMSMAN_"}},
		{"短信 · Hero SMS", []string{"HERO_SMS_"}},
		{"短信 · 通用与重试", []string{"SMS_", "CODEX_PHONE_", "CODEX_SMS_", "CODEX_ADDPHONE_", "CUSTOM_SMS_"}},
		{"验证 · CapSolver", []string{"CAPSOLVER_"}},
		{"验证 · EZCaptcha", []string{"EZCAPTCHA_"}},
		{"验证 · YesCaptcha", []string{"YESCAPTCHA_"}},
		{"验证 · Claude 识别与验证", []string{"CLAUDE_VISION_", "CLAUDE_HCAPTCHA_", "CLAUDE_CAPTCHA_"}},
		{"验证 · 视觉模型与投票", []string{"VISION_", "VOTE_", "GEMMA_"}},
		{"对接 · SUB2API", []string{"SUB2API_"}},
		{"对接 · CPA", []string{"CPA_", "CODEX_CPA_", "CODEX_AUTH_URL_SOURCE"}},
		{"对接 · chatgpt2api", []string{"CHATGPT2API_"}},
		{"对接 · webchat2api", []string{"WEBCHAT2API_"}},
		{"对接 · K12 控制台", []string{"K12_"}},
		{"运行 · 并发与资源", []string{"REG_FACTORY_MEMORY_", "REG_FACTORY_MAX_"}},
		{"运行 · 周期巡检", []string{"REG_FACTORY_LIVENESS_"}},
		{"运行 · 接口访问", []string{"REGD_", "REG_FACTORY_PANEL_"}},
		{"运行 · 数据路径", []string{"REG_FACTORY_DATA_", "REG_FACTORY_ENV_"}},
	} {
		for _, prefix := range section.prefixes {
			if strings.HasPrefix(upper, prefix) {
				return section.name
			}
		}
	}
	return fallback
}

// Preserve provider-specific keys outside the catalog in their material area.
func configArea(key string) string {
	upper := strings.ToUpper(key)
	for _, category := range []struct {
		area     string
		prefixes []string
	}{
		{"proxy", []string{"OUTLOOK_PROXY", "CLAUDE_PROXY", "CHATGPT_PROXY", "GROK_PROXY", "KIRO_PROXY", "GITHUB_PROXY"}},
		{"proxy", []string{"PROXY", "CLASH", "REG_FACTORY_PROXY", "REG_FACTORY_PRODUCTION_PROXY", "REG_FACTORY_RESIDENTIAL_"}},
		{"mail", []string{"ICMAIL", "MAIL_IMAP_", "ICLOUD", "YYDS", "GPTMAIL", "CFMAIL", "MOEMAIL", "REMAIL", "CUSTOM_MAIL", "OUTLOOK", "CHATGPT_EMAIL", "CHATGPT_VERIFICATION"}},
		{"sms", []string{"SMS", "HERO", "CODEX_PHONE", "CODEX_SMS", "CODEX_ADDPHONE", "CUSTOM_SMS"}},
		{"captcha", []string{"CAPSOLVER", "EZCAPTCHA", "YESCAPTCHA", "VISION", "VOTE", "GEMMA", "CLAUDE_VISION_", "CLAUDE_HCAPTCHA_", "CLAUDE_CAPTCHA_"}},
		{"browser", []string{"BROWSER", "BITBROWSER", "BB_CORE_", "ADSPOWER", "CHROMIUM", "CLOAK", "ROXY", "CUSTOM_BROWSER", "REG_FACTORY_BROWSER"}},
		{"node", []string{"REG_FACTORY_NODE_", "REG_FACTORY_ASSET_", "REG_FACTORY_WORK_ORIGIN"}},
		{"integration", []string{"SUB2API", "CPA_", "CHATGPT2API", "WEBCHAT2API", "K12_", "CODEX_CPA", "CODEX_AUTH_URL_SOURCE"}},
		{"runtime", []string{"REGD_", "REG_FACTORY_PANEL_", "REG_FACTORY_MEMORY_", "REG_FACTORY_MAX_", "REG_FACTORY_DATA_", "REG_FACTORY_ENV_", "REG_FACTORY_LIVENESS_"}},
	} {
		for _, prefix := range category.prefixes {
			if strings.HasPrefix(upper, prefix) {
				return category.area
			}
		}
	}
	return "custom"
}
