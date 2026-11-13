// Package probe performs read-only credential checks. AT tests never rotate RT.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const openAIUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type Result struct {
	Status    string // ok / banned / workspace_deactivated / needs_refresh / unknown
	Message   string
	retryable bool
}

// OAuth tests AT only. A 401 says nothing about RT or the account's entitlement.
func OAuth(creds map[string]any, proxyURL string, timeout time.Duration) Result {
	return OAuthContext(context.Background(), creds, proxyURL, timeout)
}

func OAuthContext(ctx context.Context, creds map[string]any, proxyURL string, timeout time.Duration) Result {
	access, _ := creds["access_token"].(string)
	if strings.TrimSpace(access) == "" {
		return Result{Status: "needs_refresh", Message: "缺少 AT，请刷新凭据后复测；未使用 RT"}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if strings.TrimSpace(proxyURL) != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil || parsed.Host == "" {
			return Result{Status: "unknown", Message: "测试出口配置无效"}
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: min(timeout, 8*time.Second), CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	return checkAccessTokenWithRetry(ctx, client, creds, []time.Duration{time.Second, 3 * time.Second})
}

func checkAccessToken(client *http.Client, creds map[string]any) Result {
	return checkAccessTokenContext(context.Background(), client, creds)
}

func checkAccessTokenWithRetry(ctx context.Context, client *http.Client, creds map[string]any, delays []time.Duration) Result {
	for attempt := 0; ; attempt++ {
		result := checkAccessTokenContext(ctx, client, creds)
		if !result.retryable || attempt >= len(delays) || ctx.Err() != nil {
			if attempt > 0 {
				result.Message = fmt.Sprintf("%s（尝试 %d 次）", result.Message, attempt+1)
			}
			return result
		}
		timer := time.NewTimer(delays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return Result{Status: "unknown", Message: "测试中断，未使用 RT；可重新测试"}
		case <-timer.C:
		}
	}
}

func checkAccessTokenContext(ctx context.Context, client *http.Client, creds map[string]any) Result {
	access, _ := creds["access_token"].(string)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openAIUsageURL, nil)
	if err != nil {
		return Result{Status: "unknown", Message: "无法建立测试请求"}
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	if account, _ := creds["account_id"].(string); account != "" {
		req.Header.Set("Chatgpt-Account-Id", account)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Status: "unknown", Message: "网络或 TLS 测试失败，可重试 AT 测试；未使用 RT", retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var payload map[string]any
	parsed := err == nil && json.Unmarshal(raw, &payload) == nil && payload != nil
	if parsed && resp.StatusCode >= 400 && resp.StatusCode < 500 {
		code, _ := payload["error"].(string)
		for _, key := range []string{"error", "detail", ""} {
			item := payload
			if key != "" {
				item, _ = payload[key].(map[string]any)
			}
			for _, field := range []string{"code", "error_code", "type"} {
				if value, _ := item[field].(string); code == "" {
					code = value
				}
			}
		}
		switch strings.ToLower(strings.TrimSpace(code)) {
		case "account_deactivated", "account_banned":
			return Result{Status: "banned", Message: "上游明确返回账号封禁/停用"}
		case "workspace_deactivated", "deactivated_workspace":
			return Result{Status: "workspace_deactivated", Message: "上游明确返回空间停用"}
		}
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if !parsed {
			return Result{Status: "unknown", Message: "上游响应不是有效测试结果", retryable: err != nil}
		}
		return Result{Status: "ok", Message: "AT 测试通过；未刷新凭据"}
	case http.StatusUnauthorized:
		return Result{Status: "needs_refresh", Message: "AT 已失效或被拒绝，请刷新后复测；账号与 RT 状态尚未判定"}
	case http.StatusForbidden:
		return Result{Status: "unknown", Message: "测试被上游拒绝（403），账号状态未确认"}
	case http.StatusTooManyRequests:
		return Result{Status: "unknown", Message: "测试被限流，请稍后重试 AT 测试"}
	default:
		return Result{Status: "unknown", Message: fmt.Sprintf("测试 HTTP %d，账号状态未确认", resp.StatusCode), retryable: resp.StatusCode >= 500}
	}
}
