// Package settings 负责进程级配置与 /data/.env 的读写。
package settings

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Settings 是 regd 进程启动时确定的配置。
type Settings struct {
	DataDir         string // 运行数据根，默认 /data
	EnvFile         string // .env 路径，默认 $DataDir/.env
	AppRoot         string // Python 代码根，默认 /app
	DBPath          string // SQLite 路径，默认 $DataDir/regd.db
	Addr            string // 监听地址，默认 127.0.0.1:8800
	Python          string // Python 解释器
	PanelUser       string
	PanelPass       string
	AllowAnon       bool
	MaxParallel     int
	CgroupRoot      string
	GuardPct        float64
	LogTailMax      int
	NodeID          string
	NodeKey         string
	NodePlatform    string
	NodeReceiptKey  string
	NodeBusinessKey string
	NodeArtifactKey string
	ICloudBotURL    string
	ICloudBotKey    string
	TZ              string
}

// Load 读取环境变量；缺省值适配容器部署。
func Load() *Settings {
	dataDir := firstNonEmpty(os.Getenv("REG_FACTORY_DATA_DIR"), "/data")
	s := &Settings{
		DataDir:         dataDir,
		EnvFile:         firstNonEmpty(os.Getenv("REG_FACTORY_ENV_FILE"), filepath.Join(dataDir, ".env")),
		AppRoot:         firstNonEmpty(os.Getenv("REGD_APP_ROOT"), "/app"),
		DBPath:          firstNonEmpty(os.Getenv("REGD_DB"), filepath.Join(dataDir, "regd.db")),
		Addr:            firstNonEmpty(os.Getenv("REGD_ADDR"), "127.0.0.1:8800"),
		Python:          firstNonEmpty(os.Getenv("REGD_PYTHON"), "python3"),
		PanelUser:       firstNonEmpty(os.Getenv("REG_FACTORY_PANEL_USER"), "admin"),
		PanelPass:       os.Getenv("REG_FACTORY_PANEL_PASSWORD"),
		AllowAnon:       truthy(os.Getenv("REG_FACTORY_ALLOW_ANONYMOUS")),
		MaxParallel:     atoiDefault(os.Getenv("REG_FACTORY_MAX_CONCURRENCY"), 3),
		CgroupRoot:      firstNonEmpty(os.Getenv("REG_FACTORY_CGROUP_ROOT"), "/sys/fs/cgroup"),
		GuardPct:        floatDefault(os.Getenv("REG_FACTORY_MEMORY_GUARD_PCT"), 85),
		LogTailMax:      atoiDefault(os.Getenv("REGD_LOG_TAIL"), 2000),
		NodeID:          os.Getenv("REG_FACTORY_NODE_ID"),
		NodeKey:         os.Getenv("REG_FACTORY_NODE_KEY"),
		NodePlatform:    firstNonEmpty(os.Getenv("REG_FACTORY_NODE_PLATFORM"), "claude"),
		NodeReceiptKey:  strings.TrimSpace(os.Getenv("REG_FACTORY_NODE_RECEIPT_KEY")),
		NodeBusinessKey: strings.TrimSpace(os.Getenv("REG_FACTORY_NODE_BUSINESS_KEY")),
		NodeArtifactKey: strings.TrimSpace(os.Getenv("REG_FACTORY_NODE_ARTIFACT_KEY")),
		ICloudBotURL:    firstNonEmpty(os.Getenv("ICLOUD_BOT_URL"), os.Getenv("ICLOUD_MAIL_API_BASE")),
		ICloudBotKey:    firstNonEmpty(os.Getenv("ICLOUD_BOT_KEY"), os.Getenv("ICLOUD_MAIL_API_KEY")),
		TZ:              firstNonEmpty(os.Getenv("TZ"), "Asia/Singapore"),
	}
	if s.MaxParallel < 1 {
		s.MaxParallel = 1
	}
	if s.MaxParallel > 32 {
		s.MaxParallel = 32
	}
	return s
}

// Validate 在缺密码时拒绝启动（与 Python 侧闸门一致，fail closed）。
func (s *Settings) Validate() error {
	if s.PanelPass == "" && !s.AllowAnon {
		return errors.New("未设置 REG_FACTORY_PANEL_PASSWORD（内网自用请显式设 REG_FACTORY_ALLOW_ANONYMOUS=1）")
	}
	if s.PanelPass != "" && len(s.PanelPass) < 8 {
		return errors.New("REG_FACTORY_PANEL_PASSWORD 太短（至少 8 位）")
	}
	return nil
}

// LoadEnvFile 把 .env 注入进程环境，已存在的变量优先（与 Python config._load_dotenv 一致）。
func LoadEnvFile(path string) int {
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()

	count := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		// 默认「进程环境优先」：Dockerfile / compose 设的值不被 .env 覆盖。
		// 但少数 key 是「部署配置」而不是「镜像默认值」，必须允许 .env 覆盖，
		// 否则面板改不动 —— 比如 REGD_DB 在 Dockerfile 里是 /data/regd.db，
		// 想切 PostgreSQL 就只能改镜像。
		if _, exists := os.LookupEnv(key); exists && !envFileOverrides[key] {
			continue
		}
		os.Setenv(key, strings.Trim(strings.TrimSpace(value), "\"'"))
		fileEnvironment.Lock()
		fileEnvironment.keys[key] = true
		fileEnvironment.Unlock()
		count++
	}
	return count
}

// envFileOverrides 是允许 /data/.env 覆盖进程环境的 key。
//
// 这些 key 在 Dockerfile 里被设成固定值（如 REGD_DB=/data/regd.db），
// 但它们本质是部署配置，必须能在面板改：
//
//	· REGD_DB    主库位置，切 SQLite ↔ PostgreSQL
//	· MAIL_DB    邮件库位置
//	· REGD_ADDR  监听地址
var envFileOverrides = map[string]bool{
	"REGD_DB":   true,
	"MAIL_DB":   true,
	"REGD_ADDR": true,
}

var fileEnvironment = struct {
	sync.RWMutex
	keys map[string]bool
}{keys: map[string]bool{}}

// DeploymentValue excludes the process snapshot injected from the env file.
func DeploymentValue(key string) (string, bool) {
	fileEnvironment.RLock()
	defer fileEnvironment.RUnlock()
	if fileEnvironment.keys[key] {
		return "", false
	}
	return os.LookupEnv(key)
}

// WorkerEnvironment freezes the latest file settings when a worker starts.
// Deployment environment overrides still win. Running workers are unchanged.
func WorkerEnvironment(path string) ([]string, error) {
	entries, err := parseFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	fileEnvironment.RLock()
	for _, item := range os.Environ() {
		key, value, _ := strings.Cut(item, "=")
		if !fileEnvironment.keys[key] {
			values[key] = value
		}
	}
	fileEnvironment.RUnlock()
	for _, entry := range entries {
		if _, exists := values[entry.Key]; !exists {
			values[entry.Key] = entry.Value
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}

// ---------------------------------------------------------------- .env 读写

// Entry 是 .env 的一行键值（保留原始注释用）。
type Entry struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret"`
	Line   int    `json:"line"`
}

var secretHints = []string{
	"KEY", "TOKEN", "SECRET", "PASSWORD", "PASS", "COOKIE", "CREDENTIAL", "PRIVATE",
}

// Secret 判断一个键是否属于敏感项（前端默认打码）。
func Secret(key string) bool {
	upper := strings.ToUpper(key)
	for _, hint := range secretHints {
		if strings.Contains(upper, hint) {
			return true
		}
	}
	return false
}

// Env 是 .env 的线程安全视图。
type Env struct {
	path string
	mu   sync.RWMutex
}

// NewEnv 绑定文件。
func NewEnv(path string) *Env { return &Env{path: path} }

// Path 返回文件路径。
func (e *Env) Path() string { return e.path }

// Parse 解析当前文件；不存在时返回空。
func (e *Env) Parse() ([]Entry, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return parseFile(e.path)
}

func parseFile(path string) ([]Entry, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()

	var out []Entry
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		out = append(out, Entry{Key: key, Value: value, Secret: Secret(key), Line: line})
	}
	return out, scanner.Err()
}

// Get 按首次出现取值（与 Python 侧 config._load_dotenv 语义一致）。
func (e *Env) Get(key string) string {
	entries, _ := e.Parse()
	for _, item := range entries {
		if item.Key == key {
			return item.Value
		}
	}
	return ""
}

// Update 就地替换已存在的键，不存在的追加到末尾；返回实际变更的键。
func (e *Env) Update(updates map[string]string) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	original, err := os.ReadFile(e.path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(original), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}

	seen := map[string]bool{}
	var changed []string
	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value, want := updates[key]
		if !want || seen[key] {
			continue
		}
		lines[i] = fmt.Sprintf("%s=%s", key, value)
		seen[key] = true
		changed = append(changed, key)
	}
	// 追加新键，保持稳定顺序便于 diff
	var pending []string
	for key := range updates {
		if !seen[key] {
			pending = append(pending, key)
		}
	}
	sort.Strings(pending)
	for _, key := range pending {
		lines = append(lines, fmt.Sprintf("%s=%s", key, updates[key]))
		changed = append(changed, key)
	}

	content := strings.Join(lines, "\n") + "\n"
	tmp := e.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(e.path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, e.path); err != nil {
		return nil, err
	}
	return changed, nil
}

// ---------------------------------------------------------------- 小工具

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func atoiDefault(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return n
}

func floatDefault(value string, fallback float64) float64 {
	if value == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return fallback
	}
	return f
}
