// Package claudehttp 用 Go 实现 Claude.ai 的「协议注册」流程（不启动浏览器）。
//
// 流程与仓库里的 register_claude_http.py 一致：
//   GET  /api/auth/login_methods?email=&source=claude
//   POST /api/auth/send_magic_link
//   （从邮箱读出 https://claude.ai/magic-link#<nonce>:<encoded_email>）
//   POST /api/auth/verify_magic_link  → sessionKey
//
// 迁到 Go 的动机：协议流程零浏览器，内存≈0、CPU≈0，并发只受代理池与站点限流约束。
package claudehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	baseURL         = "https://claude.ai"
	clientPlatform  = "web_claude_ai"
	clientSHA       = "882d9a7d43eced6a100e636e1dfdebc55764bd78"
	defaultProtocol = "1.0.0"
	defaultSeed     = "claudex-default"
)

var (
	anonNS    = uuid.MustParse("6f4a1c2e-1b3d-4e5f-8a90-0c1d2e3f4a5b")
	deviceNS  = uuid.MustParse("9d8c7b6a-5e4f-4321-9a8b-7c6d5e4f3a2b")
	profileNS = uuid.MustParse("3c2b1a09-8f7e-4d6c-b5a4-9382716f5e4d")

	magicLinkRe = regexp.MustCompile(`(?i)https://claude\.ai/magic-link#[^\s<>"']+`)
)

// ErrorKind 把失败分类，便于上层决定是否重试。
type ErrorKind string

const (
	ErrProtocol  ErrorKind = "protocol"
	ErrMailbox   ErrorKind = "mailbox"
	ErrRateLimit ErrorKind = "rate_limit"
	ErrNetwork   ErrorKind = "network"
	ErrBanned    ErrorKind = "banned"
)

// Error 带分类的协议错误。
type Error struct {
	Kind   ErrorKind
	Detail string
	Status int
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Kind, e.Detail) }

// KindOf 取错误分类。
func KindOf(err error) ErrorKind {
	var target *Error
	if errors.As(err, &target) {
		return target.Kind
	}
	return ErrProtocol
}

// BrowserProfile 是对齐 Python 侧的三套 UA。
var browserProfiles = []struct {
	UA     string
	Accept string
}{
	{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36", "en-US,en;q=0.9"},
	{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36", "en-US,en;q=0.9"},
	{"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:133.0) Gecko/20100101 Firefox/133.0", "de-DE,de;q=0.9,en;q=0.8"},
}

// Options 是一次注册的输入。
type Options struct {
	Email           string
	Seed            string // 稳定身份种子（默认用邮箱）
	ProtocolVersion string
	ProxyURL        string
	UTCOffset       int // 默认 -480（UTC+8）
	Timeout         time.Duration
	// Mailbox 负责取 magic link
	Mailbox Mailbox
}

// Result 是注册产物。
type Result struct {
	Email      string
	SessionKey string
	Cookies    []Cookie
	VerifiedAt time.Time
}

// Cookie 是导出用的最小结构。
type Cookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain"`
	Path     string `json:"path"`
	HTTPOnly bool   `json:"httpOnly"`
	Secure   bool   `json:"secure"`
}

// Client 复刻 Python 侧的 requests.Session。
type Client struct {
	http     *http.Client
	baseURL  string
	seed     string
	version  string
	cookies  map[string]string
	lastErr  error
	proxyURL string
}

// NewClient 构造协议客户端；proxyURL 为空则直连。
func NewClient(opts Options) (*Client, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	proxyURL := strings.TrimSpace(opts.ProxyURL)
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, &Error{Kind: ErrProtocol, Detail: "代理地址无效: " + err.Error()}
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	seed := strings.TrimSpace(opts.Seed)
	if seed == "" {
		seed = strings.TrimSpace(opts.Email)
	}
	if seed == "" {
		seed = defaultSeed
	}
	version := strings.TrimSpace(opts.ProtocolVersion)
	if version == "" {
		version = defaultProtocol
	}
	return &Client{
		http:     &http.Client{Transport: transport, Timeout: timeout},
		baseURL:  baseURL,
		seed:     seed,
		version:  version,
		cookies:  map[string]string{},
		proxyURL: proxyURL,
	}, nil
}

// DefaultSeed 生成稳定身份种子。
func DefaultSeed(email string) string {
	if strings.TrimSpace(email) == "" {
		return defaultSeed
	}
	return strings.TrimSpace(email)
}

// Headers 构造 anthropic 客户端头（与 Python 侧逐字段一致）。
func (c *Client) Headers() map[string]string {
	index := int(uuid.NewSHA1(profileNS, []byte(c.seed)).ID() % uint32(len(browserProfiles)))
	profile := browserProfiles[index]
	return map[string]string{
		"accept":                    "*/*",
		"content-type":              "application/json",
		"origin":                    c.baseURL,
		"referer":                   c.baseURL + "/",
		"user-agent":                profile.UA,
		"accept-language":           profile.Accept,
		"anthropic-client-platform": clientPlatform,
		"anthropic-client-version":  c.version,
		"anthropic-client-sha":      clientSHA,
		"anthropic-anonymous-id":    "claudeai.v1." + uuid.NewSHA1(anonNS, []byte(c.seed)).String(),
		"anthropic-device-id":       uuid.NewSHA1(deviceNS, []byte(c.seed)).String(),
	}
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, payload any) (map[string]any, error) {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, &Error{Kind: ErrProtocol, Detail: "请求序列化失败"}
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, &Error{Kind: ErrProtocol, Detail: err.Error()}
	}
	for key, value := range c.Headers() {
		req.Header.Set(key, value)
	}
	if len(c.cookies) > 0 {
		parts := make([]string, 0, len(c.cookies))
		for name, value := range c.cookies {
			parts = append(parts, name+"="+value)
		}
		req.Header.Set("cookie", strings.Join(parts, "; "))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &Error{Kind: ErrNetwork, Detail: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	for _, cookie := range resp.Cookies() {
		if cookie.Name != "" && cookie.Value != "" {
			c.cookies[cookie.Name] = cookie.Value
		}
	}

	switch {
	case resp.StatusCode == 429:
		return nil, &Error{Kind: ErrRateLimit, Detail: "Claude 限流", Status: 429}
	case resp.StatusCode == 403:
		return nil, &Error{Kind: ErrBanned, Detail: "Claude 拒绝访问（风控/封禁）", Status: 403}
	case resp.StatusCode >= 400:
		return nil, &Error{Kind: ErrProtocol, Detail: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160)), Status: resp.StatusCode}
	}

	var decoded map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			// 某些端点返回空体或非 JSON，视为成功但无载荷
			decoded = map[string]any{}
		}
	} else {
		decoded = map[string]any{}
	}
	return decoded, nil
}

// LoginMethods 查询该邮箱支持的登录方式。
func (c *Client) LoginMethods(ctx context.Context, email string) (map[string]any, error) {
	query := url.Values{}
	query.Set("email", email)
	query.Set("source", "claude")
	return c.doJSON(ctx, http.MethodGet, "/api/auth/login_methods", query, nil)
}

// SendMagicLink 发送登录链接邮件。
func (c *Client) SendMagicLink(ctx context.Context, email string, utcOffset int) (map[string]any, error) {
	payload := map[string]any{
		"utc_offset":     utcOffset,
		"email_address":  email,
		"login_intent":   nil,
		"locale":         "en-US",
		"return_to":      nil,
		"source":         "claude",
	}
	return c.doJSON(ctx, http.MethodPost, "/api/auth/send_magic_link", nil, payload)
}

// VerifyMagicLink 用邮件里的链接换 sessionKey。
func (c *Client) VerifyMagicLink(ctx context.Context, magicLink string) (map[string]any, string, error) {
	parsed, err := url.Parse(magicLink)
	if err != nil || parsed.Fragment == "" {
		return nil, "", &Error{Kind: ErrProtocol, Detail: "magic link 缺少 fragment"}
	}
	fragment, err := url.PathUnescape(parsed.Fragment)
	if err != nil {
		fragment = parsed.Fragment
	}
	nonce, encodedEmail, found := strings.Cut(fragment, ":")
	if !found || nonce == "" || encodedEmail == "" {
		return nil, "", &Error{Kind: ErrProtocol, Detail: "magic link fragment 格式不符"}
	}
	payload := map[string]any{
		"credentials": map[string]any{
			"method":                "nonce",
			"nonce":                 nonce,
			"encoded_email_address": encodedEmail,
		},
		"locale": "en-US",
		"source": "claude",
	}
	decoded, err := c.doJSON(ctx, http.MethodPost, "/api/auth/verify_magic_link", nil, payload)
	if err != nil {
		return nil, "", err
	}
	sessionKey := c.cookies["sessionKey"]
	if sessionKey == "" {
		sessionKey = findSessionKey(decoded)
	}
	if sessionKey == "" {
		return decoded, "", &Error{Kind: ErrProtocol, Detail: "验证成功但没有 sessionKey"}
	}
	c.cookies["sessionKey"] = sessionKey
	return decoded, sessionKey, nil
}

// Cookies 导出当前会话 cookie。
func (c *Client) Cookies() []Cookie {
	out := make([]Cookie, 0, len(c.cookies))
	for name, value := range c.cookies {
		out = append(out, Cookie{
			Name: name, Value: value, Domain: ".claude.ai", Path: "/",
			HTTPOnly: name == "sessionKey", Secure: true,
		})
	}
	return out
}

func findSessionKey(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			lower := strings.ToLower(key)
			if (lower == "sessionkey" || lower == "session_key") && nested != nil {
				if text, ok := nested.(string); ok && text != "" {
					return text
				}
			}
			if found := findSessionKey(nested); found != "" {
				return found
			}
		}
	case []any:
		for _, nested := range typed {
			if found := findSessionKey(nested); found != "" {
				return found
			}
		}
	}
	return ""
}

// ExtractMagicLink 从任意文本中提取 magic link。
func ExtractMagicLink(text string) string {
	if match := magicLinkRe.FindString(text); match != "" {
		return match
	}
	return ""
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
