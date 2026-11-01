package claudehttp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var unsafeNameRe = regexp.MustCompile(`[^a-zA-Z0-9@._-]+`)

// Register 执行完整协议注册并落盘 sessionKey。
func Register(ctx context.Context, opts Options) (*Result, error) {
	email := strings.TrimSpace(opts.Email)
	if email == "" {
		return nil, &Error{Kind: ErrProtocol, Detail: "缺少邮箱"}
	}
	if opts.Mailbox == nil {
		return nil, &Error{Kind: ErrMailbox, Detail: "缺少邮箱读取器"}
	}
	if opts.UTCOffset == 0 {
		opts.UTCOffset = -480
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 120 * time.Second
	}

	client, err := NewClient(opts)
	if err != nil {
		return nil, err
	}

	// 1) 登录方式探测：拿不到不影响主流程（不同账号可能返回不同结构）
	_, _ = client.LoginMethods(ctx, email)

	// 2) 发送 magic link
	if _, err := client.SendMagicLink(ctx, email, opts.UTCOffset); err != nil {
		return nil, err
	}

	// 3) 等邮件
	link, err := opts.Mailbox.WaitForMagicLink(ctx, opts.Timeout)
	if err != nil {
		return nil, err
	}

	// 4) 用 nonce 换 sessionKey
	_, sessionKey, err := client.VerifyMagicLink(ctx, link)
	if err != nil {
		return nil, err
	}

	return &Result{
		Email:      email,
		SessionKey: sessionKey,
		Cookies:    client.Cookies(),
		VerifiedAt: time.Now(),
	}, nil
}

// SaveSession 把会话写到 cookies/claude/<email>.json。
func SaveSession(dataDir string, result *Result) (string, error) {
	dir := filepath.Join(dataDir, "cookies", "claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := unsafeNameRe.ReplaceAllString(result.Email, "_")
	if name == "" {
		name = "session"
	}
	path := filepath.Join(dir, name+".json")

	payload := map[string]any{
		"email":        result.Email,
		"sessionKey":   result.SessionKey,
		"cookies":      result.Cookies,
		"verified_at":  result.VerifiedAt.UTC().Format(time.RFC3339),
		"source":       "claude-http-go",
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(temp, path); err != nil {
		return "", err
	}
	return path, nil
}

// SessionKeyOf 从保存的文件里读 sessionKey（供测活复用）。
func SessionKeyOf(dataDir, email string) string {
	name := unsafeNameRe.ReplaceAllString(email, "_")
	path := filepath.Join(dataDir, "cookies", "claude", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var payload struct {
		SessionKey string `json:"sessionKey"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return payload.SessionKey
}
