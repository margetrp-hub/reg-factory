// Package assets 扫描 /data 下的邮箱池与平台凭据，并提供领取账本。
package assets

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Item 是一条资产。
type Item struct {
	Ref      string `json:"ref"`      // 稳定引用（邮箱值或相对路径#行号）
	Kind     string `json:"kind"`     // emails / cookies / tokens / outlook
	Platform string `json:"platform"` // claude / chatgpt / grok / kiro / outlook / *
	Value    string `json:"value"`    // 原始内容（面板已鉴权）
	Source   string `json:"source"`   // 相对 /data 的路径
	Claimed  bool   `json:"claimed"`
}

// Scanner 读取数据目录。
type Scanner struct {
	Root string
}

// New 构造扫描器。
func New(root string) *Scanner { return &Scanner{Root: root} }

// Kinds 是支持的资产分类。
var Kinds = []string{"emails", "cookies", "tokens", "outlook"}

// isValidKind 校验分类。
func isValidKind(kind string) bool {
	for _, item := range Kinds {
		if item == kind {
			return true
		}
	}
	return false
}

// Scan 返回某分类下的全部条目（不含领取状态）。
func (s *Scanner) Scan(kind string) ([]Item, error) {
	if !isValidKind(kind) {
		return nil, errors.New("未知资产分类")
	}
	switch kind {
	case "emails":
		return s.scanLines(filepath.Join(s.Root, "emails.txt"), kind, "*")
	case "outlook":
		return s.scanOutlook()
	case "cookies":
		return s.scanTree(filepath.Join(s.Root, "cookies"), kind)
	case "tokens":
		return s.scanTree(filepath.Join(s.Root, "tokens"), kind)
	}
	return nil, nil
}

func (s *Scanner) scanLines(path, kind, platform string) ([]Item, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Item
	for _, line := range strings.Split(string(raw), "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		ref := text
		if idx := strings.Index(ref, "----"); idx > 0 {
			ref = ref[:idx] // 用邮箱地址做稳定引用，避免改密码后重复领取
		}
		out = append(out, Item{
			Ref:      kind + ":" + ref,
			Kind:     kind,
			Platform: platform,
			Value:    text,
			Source:   "emails.txt",
		})
	}
	return out, nil
}

func (s *Scanner) scanOutlook() ([]Item, error) {
	var out []Item
	for _, dir := range []string{"outlook_accounts", "_outlook_pool"} {
		base := filepath.Join(s.Root, dir)
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
				continue
			}
			path := filepath.Join(base, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			rel := dir + "/" + entry.Name()
			out = append(out, Item{
				Ref:      "outlook:" + rel,
				Kind:     "outlook",
				Platform: "outlook",
				Value:    strings.TrimSpace(string(raw)),
				Source:   rel,
			})
		}
	}
	return out, nil
}

func (s *Scanner) scanTree(base, kind string) ([]Item, error) {
	var out []Item
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		lower := strings.ToLower(entry.Name())
		if !(strings.HasSuffix(lower, ".json") || strings.HasSuffix(lower, ".txt")) {
			return nil
		}
		rel, relErr := filepath.Rel(s.Root, path)
		if relErr != nil {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		platform := platformOf(rel)
		out = append(out, Item{
			Ref:      kind + ":" + rel,
			Kind:     kind,
			Platform: platform,
			Value:    strings.TrimSpace(string(raw)),
			Source:   rel,
		})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// platformOf 从相对路径推断平台：cookies/claude/x.json -> claude
func platformOf(rel string) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 3 {
		return strings.ToLower(parts[1])
	}
	if len(parts) >= 2 {
		name := strings.ToLower(parts[len(parts)-1])
		name = strings.TrimSuffix(name, filepath.Ext(name))
		return name
	}
	return "*"
}

// Summary 返回各分类的总数。
type Summary struct {
	Kind  string `json:"kind"`
	Total int    `json:"total"`
}

// Summarize 统计全部分类。
func (s *Scanner) Summarize(claimed map[string]map[string]bool) []Summary {
	out := make([]Summary, 0, len(Kinds))
	for _, kind := range Kinds {
		items, err := s.Scan(kind)
		if err != nil {
			out = append(out, Summary{Kind: kind})
			continue
		}
		out = append(out, Summary{Kind: kind, Total: len(items)})
	}
	return out
}
