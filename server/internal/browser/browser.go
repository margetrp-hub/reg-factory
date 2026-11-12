// Package browser — Go 原生浏览器驱动（Level 3 反检测）。
//
// 定位
// ----
// 见 伪装强度四级阶梯.md：
//
//	Level 1  裸浏览器                              最弱
//	Level 2  + JS 层注入伪装                       中
//	Level 3  + CDP 消息层干预（patchright 级别）   强    ← 这个包
//	Level 4  + 引擎层 C++ 补丁（camoufox）        最强   ← 只有 Python
//
// 这个包做到 Level 3：用 go-rod 驱动 Chromium，但绕开所有会泄漏的 CDP 命令。
//
// 三条铁律（违反任何一条就掉到 Level 1/2）
// ----------------------------------------
//
//	① 启动参数必须有 --disable-blink-features=AutomationControlled
//	   这是让 Chrome 内部就不设置 navigator.webdriver 的唯一办法。
//	   用 JS 覆盖 navigator.webdriver 是 Level 2，能被 toString() 识破。
//
//	② 不用 page.Eval() / page.MustEval()
//	   这两个内部会发 Runtime.enable，而 Runtime.enable 会让页面拿到
//	   console.debug 的调用栈，是最经典的 CDP 泄漏。
//	   改用 page.Call(ctx, "", "Runtime.evaluate", params) 直连。
//
//	③ 注入脚本用 Page.addScriptToEvaluateOnNewDocument + worldName
//	   worldName 让它跑在 ISOLATED world，页面检测不到注入痕迹。
//
// 用法
// ----
//
//	b, err := browser.Launch(ctx, browser.Config{
//	    Proxy:    "http://user:pass@host:3010",
//	    Headless: true,
//	})
//	if err != nil { ... }
//	defer b.Close()
//
//	b.Navigate(ctx, "https://signup.live.com/signup")
//	b.Fill(ctx, "#email", "john.smith88@outlook.com")
//	b.Click(ctx, "#nextButton")
//
// 局限
// ----
// 只能过「看 TLS 指纹 / 看 CDP 痕迹」的风控。
// Turnstile / DataDome / PerimeterX 需要真实浏览器执行 JS，这里过不去。
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/stealth"
)

// DefaultChromiumPath is the architecture-independent entry installed by Docker.
const DefaultChromiumPath = "/usr/local/bin/chromium"

// ErrNoBrowser 表示找不到浏览器二进制。
var ErrNoBrowser = errors.New("browser: 找不到 Chromium，检查 Config.ExecutablePath 或 REG_FACTORY_BROWSER_PATH")

// Config 是启动参数。
type Config struct {
	ExecutablePath string
	Proxy          string
	Headless       bool
	UserDataDir    string
	ExtraArgs      []string
	Language       string
	Timeout        time.Duration

	// StealthJS 覆盖默认的 stealth 脚本（默认用 go-rod/stealth）。
	StealthJS string
	// MainWorld 为 true 时同时注入主 world（默认只注 ISOLATED）。
	MainWorld bool
}

// Browser 是 Level 3 反检测的浏览器会话。
type Browser struct {
	rod  *rod.Browser
	page *rod.Page
	cfg  Config

	tmpDir string
}

// Launch 启动浏览器。
func Launch(ctx context.Context, cfg Config) (*Browser, error) {
	path := cfg.ExecutablePath
	if path == "" {
		path = os.Getenv("REG_FACTORY_BROWSER_PATH")
	}
	if path == "" {
		path = DefaultChromiumPath
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNoBrowser, path)
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Language == "" {
		cfg.Language = "en-US"
	}

	userDataDir := cfg.UserDataDir
	tmpDir := ""
	if userDataDir == "" {
		d, err := os.MkdirTemp("", "regbrowser-*")
		if err != nil {
			return nil, fmt.Errorf("browser: 建临时 profile 失败: %w", err)
		}
		userDataDir = d
		tmpDir = d
	}

	cleanup := func() {
		if tmpDir != "" {
			_ = os.RemoveAll(tmpDir)
		}
	}

	// ---- 启动参数：铁律 ① ----
	l := launcher.New().
		Bin(path).
		UserDataDir(userDataDir).
		// 容器里 leakless 常失败（需要 ptrace 权限）
		Leakless(false).
		NoSandbox(true).
		Headless(cfg.Headless).
		// 这行是 Level 3 的核心
		Set("disable-blink-features", "AutomationControlled").
		Set("disable-dev-shm-usage").
		Set("no-first-run").
		Set("no-default-browser-check").
		Set("disable-infobars", "").
		Set("lang", cfg.Language)

	if cfg.Proxy != "" {
		l = l.Proxy(cfg.Proxy)
	}
	for _, a := range cfg.ExtraArgs {
		a = strings.TrimPrefix(a, "--")
		if a == "" {
			continue
		}
		if k, v, ok := strings.Cut(a, "="); ok {
			l = l.Set(flags.Flag(k), v)
		} else {
			l = l.Set(flags.Flag(a))
		}
	}

	u, err := l.Launch()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("browser: 启动失败: %w", err)
	}

	br := rod.New().ControlURL(u)
	if err := br.Connect(); err != nil {
		cleanup()
		return nil, fmt.Errorf("browser: 连接 CDP 失败: %w", err)
	}

	page, err := br.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		_ = br.Close()
		cleanup()
		return nil, fmt.Errorf("browser: 建页面失败: %w", err)
	}

	b := &Browser{rod: br, page: page, cfg: cfg, tmpDir: tmpDir}

	if err := b.injectStealth(ctx); err != nil {
		b.Close()
		return nil, err
	}
	return b, nil
}

// injectStealth 注入反检测脚本（铁律 ②③）。
func (b *Browser) injectStealth(ctx context.Context) error {
	js := b.cfg.StealthJS
	if js == "" {
		js = stealth.JS
	}

	// 铁律 ③：ISOLATED world 注入
	_, err := proto.PageAddScriptToEvaluateOnNewDocument{
		Source:    js,
		WorldName: "__stealth_isolated",
	}.Call(b.page.Context(ctx))
	if err != nil {
		return fmt.Errorf("browser: 注入 stealth 失败: %w", err)
	}

	// 可选：主 world 也注一份
	if b.cfg.MainWorld {
		_, _ = proto.PageAddScriptToEvaluateOnNewDocument{Source: js}.Call(b.page.Context(ctx))
	}
	return nil
}

// ---------------------------------------------------------------- CDP 直连

// Eval 执行 JS 并返回原始 JSON。
func (b *Browser) Eval(ctx context.Context, js string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	res, err := proto.RuntimeEvaluate{
		Expression:    js,
		ReturnByValue: true,
		AwaitPromise:  true,
	}.Call(b.page.Context(ctx))
	if err != nil {
		return nil, fmt.Errorf("browser: 执行 JS 失败: %w", err)
	}
	if res.ExceptionDetails != nil {
		return nil, fmt.Errorf("browser: JS 抛异常: %s", res.ExceptionDetails.Text)
	}
	// gson.JSON 的零值是 null，MarshalJSON 会正确输出
	return res.Result.Value.MarshalJSON()
}

// EvalString 执行 JS 并把结果当字符串返回。
func (b *Browser) EvalString(ctx context.Context, js string) (string, error) {
	raw, err := b.Eval(ctx, js)
	if err != nil {
		return "", err
	}
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return string(raw), nil
	}
	return s, nil
}

// EvalBool 执行 JS 并把结果当布尔返回。
func (b *Browser) EvalBool(ctx context.Context, js string) (bool, error) {
	raw, err := b.Eval(ctx, js)
	if err != nil {
		return false, err
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, err
	}
	return v, nil
}

// ---------------------------------------------------------------- 页面操作

// Navigate 导航。
func (b *Browser) Navigate(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	if err := b.page.Context(ctx).Navigate(url); err != nil {
		return fmt.Errorf("browser: 导航到 %s 失败: %w", url, err)
	}
	_ = b.page.Context(ctx).WaitLoad()
	return nil
}

// Fill 填表单（逐字符 + 随机间隔）。
func (b *Browser) Fill(ctx context.Context, selector, value string) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	el, err := b.page.Context(ctx).Element(selector)
	if err != nil {
		return fmt.Errorf("browser: 找不到 %s: %w", selector, err)
	}
	if err := el.ScrollIntoView(); err != nil {
		_ = err
	}
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("browser: 点击 %s 失败: %w", selector, err)
	}
	// 清空
	_ = el.SelectAllText()
	_ = b.page.Keyboard.Press(input.Backspace)

	// 逐字符
	for _, ch := range value {
		if err := b.page.Keyboard.Type(input.Key(ch)); err != nil {
			return fmt.Errorf("browser: 输入字符失败: %w", err)
		}
		time.Sleep(time.Duration(25+randInt(70)) * time.Millisecond)
	}
	return nil
}

// Input 快速填表单（一次性，不逐字符）。
func (b *Browser) Input(ctx context.Context, selector, value string) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	el, err := b.page.Context(ctx).Element(selector)
	if err != nil {
		return fmt.Errorf("browser: 找不到 %s: %w", selector, err)
	}
	return el.Input(value)
}

// Click 点击元素。
func (b *Browser) Click(ctx context.Context, selector string) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	el, err := b.page.Context(ctx).Element(selector)
	if err != nil {
		return fmt.Errorf("browser: 找不到 %s: %w", selector, err)
	}
	if err := el.ScrollIntoView(); err != nil {
		_ = err
	}
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("browser: 点击 %s 失败: %w", selector, err)
	}
	time.Sleep(time.Duration(100+randInt(150)) * time.Millisecond)
	return nil
}

// Text 拿元素文本。
func (b *Browser) Text(ctx context.Context, selector string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	el, err := b.page.Context(ctx).Element(selector)
	if err != nil {
		return "", fmt.Errorf("browser: 找不到 %s: %w", selector, err)
	}
	return el.Text()
}

// Attr 拿元素属性。
func (b *Browser) Attr(ctx context.Context, selector, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	el, err := b.page.Context(ctx).Element(selector)
	if err != nil {
		return "", err
	}
	v, err := el.Attribute(name)
	if err != nil || v == nil {
		return "", err
	}
	return *v, nil
}

// WaitVisible 等元素出现。
func (b *Browser) WaitVisible(ctx context.Context, selector string) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	_, err := b.page.Context(ctx).Element(selector)
	return err
}

// Exists 元素是否存在（不等待）。
func (b *Browser) Exists(ctx context.Context, selector string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	_, err := b.page.Context(ctx).Element(selector)
	return err == nil
}

// ---------------------------------------------------------------- 采集

// Screenshot 截图到文件。
func (b *Browser) Screenshot(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	res, err := proto.PageCaptureScreenshot{Format: "png"}.Call(b.page.Context(ctx))
	if err != nil {
		return fmt.Errorf("browser: 截图失败: %w", err)
	}
	bin := res.Data
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, bin, 0o644)
}

// Cookie 是简化后的 cookie。
type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

// Cookies 拿全部 cookie。
func (b *Browser) Cookies(ctx context.Context) ([]Cookie, error) {
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()

	res, err := proto.NetworkGetAllCookies{}.Call(b.page.Context(ctx))
	if err != nil {
		return nil, fmt.Errorf("browser: 取 cookie 失败: %w", err)
	}
	out := make([]Cookie, 0, len(res.Cookies))
	for _, c := range res.Cookies {
		out = append(out, Cookie{
			Name: c.Name, Value: c.Value,
			Domain: c.Domain, Path: c.Path,
		})
	}
	return out, nil
}

// Cookie 按名取 cookie 值。
func (b *Browser) Cookie(ctx context.Context, name string) (string, bool, error) {
	cs, err := b.Cookies(ctx)
	if err != nil {
		return "", false, err
	}
	for _, c := range cs {
		if c.Name == name {
			return c.Value, true, nil
		}
	}
	return "", false, nil
}

// CookieHeader 把 cookie 拼成请求头格式。
func (b *Browser) CookieHeader(ctx context.Context) (string, error) {
	cs, err := b.Cookies(ctx)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; "), nil
}

// URL 当前地址。
func (b *Browser) URL(ctx context.Context) (string, error) {
	return b.EvalString(ctx, "document.location.href")
}

// Title 当前标题。
func (b *Browser) Title(ctx context.Context) (string, error) {
	return b.EvalString(ctx, "document.title")
}

// Fingerprint 采集常见指纹，用于自检。
func (b *Browser) Fingerprint(ctx context.Context) (map[string]any, error) {
	js := `JSON.stringify({
		ua: navigator.userAgent,
		webdriver: navigator.webdriver,
		platform: navigator.platform,
		cores: navigator.hardwareConcurrency,
		memory: navigator.deviceMemory,
		languages: navigator.languages,
		timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
		plugins: navigator.plugins.length,
		chrome: typeof window.chrome,
		permissions: typeof navigator.permissions,
		webgl: (() => {
			try {
				const c = document.createElement('canvas');
				const gl = c.getContext('webgl');
				const d = gl.getExtension('WEBGL_debug_renderer_info');
				return gl.getParameter(d.UNMASKED_RENDERER_WEBGL);
			} catch(e) { return 'err'; }
		})(),
	})`
	s, err := b.EvalString(ctx, js)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// ---------------------------------------------------------------- 暴露底层

// Page 暴露底层 rod.Page（只读用途）。
func (b *Browser) Page() *rod.Page { return b.page }

// Rod 暴露底层 rod.Browser。
func (b *Browser) Rod() *rod.Browser { return b.rod }

// ---------------------------------------------------------------- 关闭

// Close 关闭并清理临时 profile。
func (b *Browser) Close() error {
	var first error
	if b.rod != nil {
		if err := b.rod.Close(); err != nil {
			first = err
		}
	}
	if b.tmpDir != "" {
		time.Sleep(300 * time.Millisecond)
		if err := os.RemoveAll(b.tmpDir); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// ---------------------------------------------------------------- 辅助

func randInt(n int) int {
	if n <= 0 {
		return 0
	}
	return int(time.Now().UnixNano() % int64(n))
}
