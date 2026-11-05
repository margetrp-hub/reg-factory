package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// Arg 是任务参数定义（由仓库根目录 schema/scripts.py 导出）。
type Arg struct {
	Flag       string            `json:"flag"`
	Type       string            `json:"type"`
	Default    any               `json:"default"`
	Help       string            `json:"help"`
	Choices    []string          `json:"choices,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Positional bool              `json:"positional,omitempty"`
}

// Script 是一个可执行的注册任务。
type Script struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	File     string `json:"file"`
	Desc     string `json:"desc"`
	Platform string `json:"platform"`
	Category string `json:"category"`
	Warning  string `json:"warning"`
	Args     []Arg  `json:"args"`
}

// LoadScripts 读取导出的 schema。
func LoadScripts(path string) ([]Script, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []Script
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("解析任务 schema 失败: %w", err)
	}
	return list, nil
}

// FindScript 按 ID 查找。
func FindScript(list []Script, id string) (Script, bool) {
	for _, item := range list {
		if item.ID == id {
			return item, true
		}
	}
	return Script{}, false
}

// BuildArgv 按 schema 校验参数并拼出命令行（等价于 Python 侧 _build_cmd）。
func BuildArgv(cfg *settings.Settings, task store.Task) ([]string, error) {
	list, err := LoadScripts(filepath.Join(cfg.AppRoot, "deploy", "scripts.schema.json"))
	if err != nil {
		// 兼容被放在数据目录的情况
		list, err = LoadScripts(filepath.Join(cfg.DataDir, "scripts.schema.json"))
		if err != nil {
			return nil, err
		}
	}
	script, ok := FindScript(list, task.Script)
	if !ok {
		return nil, fmt.Errorf("未知脚本: %s", task.Script)
	}
	path := filepath.Join(cfg.AppRoot, script.File)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("脚本不存在: %s", script.File)
	}

	var values map[string]any
	if strings.TrimSpace(task.ArgsJSON) != "" {
		if err := json.Unmarshal([]byte(task.ArgsJSON), &values); err != nil {
			return nil, fmt.Errorf("参数 JSON 无效: %w", err)
		}
	}

	argv := []string{cfg.Python, "-u", path}
	var positional []string
	for _, arg := range script.Args {
		raw, present := values[arg.Flag]
		if !present || raw == nil {
			continue
		}
		if arg.Positional {
			if text := scalar(raw); text != "" {
				positional = append(positional, text)
			}
			continue
		}
		switch arg.Type {
		case "bool":
			if truthy(raw) {
				argv = append(argv, arg.Flag)
			}
		case "multi":
			items := toStrings(raw)
			if len(items) > 0 {
				argv = append(argv, arg.Flag)
				argv = append(argv, items...)
			}
		default:
			text := scalar(raw)
			if text == "" {
				continue
			}
			if len(arg.Choices) > 0 && !contains(arg.Choices, text) {
				return nil, fmt.Errorf("%s 取值 %q 不在允许范围", arg.Flag, text)
			}
			argv = append(argv, arg.Flag, text)
		}
	}
	argv = append(argv, positional...)

	for _, item := range argv {
		if strings.ContainsRune(item, 0) {
			return nil, fmt.Errorf("参数包含非法字符")
		}
	}
	return argv, nil
}

// NormalizeArgs 把前端/API 提交的参数按 schema 补默认值并裁剪未知键。
// 同时接受带前缀的 "--flag" 与裸名 "flag"（curl/脚本调用更自然）。
func NormalizeArgs(list []Script, scriptID string, values map[string]any) (map[string]any, error) {
	script, ok := FindScript(list, scriptID)
	if !ok {
		return nil, fmt.Errorf("未知脚本: %s", scriptID)
	}
	allowed := map[string]Arg{}
	for _, arg := range script.Args {
		allowed[arg.Flag] = arg
	}
	out := map[string]any{}
	for key, value := range values {
		if _, exists := allowed[key]; exists {
			out[key] = value
			continue
		}
		if _, exists := allowed["--"+key]; exists {
			out["--"+key] = value
			continue
		}
		if _, exists := allowed["-"+key]; exists {
			out["-"+key] = value
		}
		// 其余静默丢弃，避免任意字符串被拼进命令行
	}
	return out, nil
}

// SortedChoices 保证前端下拉顺序稳定。
func (a Arg) SortedChoices() []string {
	out := append([]string(nil), a.Choices...)
	sort.Strings(out)
	return out
}

func scalar(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		}
	case float64:
		return typed != 0
	}
	return false
}

func toStrings(value any) []string {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := scalar(item); text != "" {
				out = append(out, text)
			}
		}
		return out
	case []string:
		return typed
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{typed}
	}
	return nil
}

func contains(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}
