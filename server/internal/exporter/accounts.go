// Package exporter 用 Go 直接产出浏览器可导入的账号包，
// 替代 tools/export_accounts.py 的只读导出路径（不启动 Python）。
package exporter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ChromeCookie 是 chrome.cookies.set 兼容的最小结构。
type ChromeCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Secure   bool   `json:"secure"`
	HTTPOnly bool   `json:"httpOnly"`
	SameSite string `json:"sameSite,omitempty"`
	Expires  any    `json:"expirationDate,omitempty"`
}

// AccountEntry 是 accounts.json 里的一个账号。
type AccountEntry struct {
	ID      string         `json:"id"`
	Email   string         `json:"email,omitempty"`
	Label   string         `json:"label,omitempty"`
	Cookies []ChromeCookie `json:"cookies"`
	Source  string         `json:"source"`
}

type platformSpec struct {
	Dir        string
	KeyCookies []string
	Domains    []string
}

// 与 tools/export_accounts.py 的平台表保持一致。
var platforms = map[string]platformSpec{
	"claude": {
		Dir:        "cookies",
		KeyCookies: []string{"sessionKey"},
		Domains:    []string{".claude.ai", "claude.ai"},
	},
	"chatgpt": {
		Dir:        "cookies/chatgpt",
		KeyCookies: []string{"__Secure-next-auth.session-token", "session-token"},
		Domains:    []string{".chatgpt.com", "chatgpt.com", ".openai.com", "openai.com"},
	},
	"grok": {
		Dir:        "cookies/grok",
		KeyCookies: []string{"sso", "sso-rw", "__Secure-next-auth.session-token"},
		Domains:    []string{".grok.com", "grok.com", ".x.ai", "accounts.x.ai"},
	},
}

// Platforms 返回支持的平台名。
func Platforms() []string {
	out := make([]string, 0, len(platforms))
	for name := range platforms {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

type rawCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	Expires  any    `json:"expires"`
	HTTPOnly bool   `json:"httpOnly"`
	Secure   bool   `json:"secure"`
	SameSite string `json:"sameSite"`
}

type rawFile struct {
	Cookies    []rawCookie `json:"cookies"`
	Email      string      `json:"email"`
	SessionKey string      `json:"sessionKey"`
}

// Export 读 /data 下的 cookie 文件并组装成浏览器扩展可导入的数组。
func Export(dataDir, platform string) ([]AccountEntry, error) {
	spec, ok := platforms[strings.ToLower(strings.TrimSpace(platform))]
	if !ok {
		return nil, os.ErrInvalid
	}
	dir := filepath.Join(dataDir, spec.Dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	out := make([]AccountEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(spec.Dir, entry.Name()))
		raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		var file rawFile
		if jsonErr := json.Unmarshal(raw, &file); jsonErr != nil {
			continue
		}
		cookies := make([]ChromeCookie, 0, len(file.Cookies))
		for _, item := range file.Cookies {
			if item.Name == "" || item.Value == "" {
				continue
			}
			if !domainAllowed(item.Domain, spec.Domains) {
				continue
			}
			cookies = append(cookies, ChromeCookie{
				Name:     item.Name,
				Value:    item.Value,
				Domain:   item.Domain,
				Path:     firstNonEmpty(item.Path, "/"),
				Secure:   item.Secure,
				HTTPOnly: item.HTTPOnly,
				SameSite: mapSameSite(item.SameSite),
				Expires:  item.Expires,
			})
		}
		if len(cookies) == 0 {
			continue
		}
		if !hasKeyCookie(cookies, spec.KeyCookies) {
			continue
		}
		email := file.Email
		if email == "" {
			email = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		}
		out = append(out, AccountEntry{
			ID:      rel,
			Email:   email,
			Label:   platform + " · " + email,
			Cookies: cookies,
			Source:  rel,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func domainAllowed(domain string, allowed []string) bool {
	normalized := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	for _, candidate := range allowed {
		if strings.EqualFold(strings.TrimPrefix(candidate, "."), normalized) {
			return true
		}
		if strings.HasSuffix(normalized, "."+strings.TrimPrefix(candidate, ".")) {
			return true
		}
	}
	return false
}

func hasKeyCookie(cookies []ChromeCookie, keys []string) bool {
	if len(keys) == 0 {
		return true
	}
	for _, cookie := range cookies {
		for _, key := range keys {
			if cookie.Name == key {
				return true
			}
		}
	}
	return false
}

// Playwright 的 sameSite 值与 chrome.cookies 的取值不同，这里做映射。
func mapSameSite(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "no_restriction":
		return "no_restriction"
	case "lax":
		return "lax"
	case "strict":
		return "strict"
	default:
		return "unspecified"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
