// Package probe 用凭据本身验证账号是否还活着（不启浏览器）。
//
// OAuth 类账号（ChatGPT/Codex）直接用 refresh_token 调 token 端点：
// 成功 = 账号可用，且能顺带刷新 access_token；失败按错误码判 expired/banned。
package probe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const openAITokenURL = "https://auth.openai.com/oauth/token"

// Result 是验证结果。
type Result struct {
	Status       string // ok / expired / banned / unknown
	Message      string
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
}

// OAuth 用 refresh_token 验证一个 ChatGPT 账号。
func OAuth(creds map[string]any, proxyURL string, timeout time.Duration) Result {
	refresh, _ := creds["refresh_token"].(string)
	clientID, _ := creds["client_id"].(string)
	if strings.TrimSpace(refresh) == "" {
		return Result{Status: "unknown", Message: "缺少 refresh_token，无法验证"}
	}
	if strings.TrimSpace(clientID) == "" {
		clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(proxyURL) != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	client := &http.Client{Transport: transport, Timeout: timeout}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", clientID)
	form.Set("scope", "openid profile email offline_access")

	req, err := http.NewRequest(http.MethodPost, openAITokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{Status: "unknown", Message: err.Error()}
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return Result{Status: "unknown", Message: "网络错误: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)

	switch resp.StatusCode {
	case http.StatusOK:
		access, _ := payload["access_token"].(string)
		newRefresh, _ := payload["refresh_token"].(string)
		expires := 0
		if value, ok := payload["expires_in"].(float64); ok {
			expires = int(value)
		}
		return Result{
			Status: "ok", Message: "token 刷新成功",
			AccessToken: access, RefreshToken: newRefresh, ExpiresIn: expires,
		}
	case http.StatusBadRequest, http.StatusUnauthorized:
		code, _ := payload["error"].(string)
		desc, _ := payload["error_description"].(string)
		message := fmt.Sprintf("%s %s", code, desc)
		switch code {
		case "invalid_grant", "invalid_token":
			return Result{Status: "expired", Message: "refresh_token 已失效: " + strings.TrimSpace(message)}
		case "account_deactivated", "account_banned":
			return Result{Status: "banned", Message: "账号被停用: " + strings.TrimSpace(message)}
		}
		return Result{Status: "expired", Message: "验证失败: " + strings.TrimSpace(message)}
	case http.StatusForbidden:
		return Result{Status: "banned", Message: "被拒绝（403）"}
	default:
		return Result{Status: "unknown", Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
}

// ErrNoCredential 表示账号没有可用于验证的凭据。
var ErrNoCredential = errors.New("no credential")
