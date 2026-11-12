package plus

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

const (
	// StripeBootstrapRoute 是拿每账号 Stripe publishable key 的端点
	StripeBootstrapRoute = "/backend-api/payments/stripe_client_bootstrap"
	// StripeBootstrapURL 完整地址
	StripeBootstrapURL = AppBase + StripeBootstrapRoute
)

// StripeBootstrap 拉取该账号的 Stripe publishable key。
//
// 对应 Python 的 _stripe_client_bootstrap。
//
// 为什么不能从 checkout 响应里取 key：
// 账号按 Stripe merchant 分片，checkout 出来的 key 可能与账号实际所属的
// merchant 不一致，用它创建 PaymentMethod 再提交到 SetupIntent 会被拒。
// 所以必须走这个账号级的 bootstrap 端点。
//
// 注意：这个 GET 不能带 Content-Type: application/json，
// Python 侧用 json_content=False 显式去掉了。
func (s *Session) StripeBootstrap(ctx context.Context, cfg Config) (string, error) {
	if s.AccountID == "" {
		return "", fmt.Errorf("stripe client bootstrap: missing account id")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	u, err := url.Parse(StripeBootstrapURL)
	if err != nil {
		return "", fmt.Errorf("解析 bootstrap URL: %w", err)
	}
	q := u.Query()
	q.Set("account_id", s.AccountID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("构造请求: %w", err)
	}
	for k, v := range s.bootstrapHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读响应: %w", err)
	}

	s.captureUpdates(resp)

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("stripe client bootstrap: HTTP %d: %s",
			resp.StatusCode, truncate(string(rawBody), 300))
	}

	var parsed map[string]any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return "", fmt.Errorf("解析响应: %w", err)
	}

	key := findPublishableKey(parsed)
	if !strings.HasPrefix(key, "pk_live_") && !strings.HasPrefix(key, "pk_test_") {
		return "", fmt.Errorf("stripe client bootstrap: missing publishable key (body: %s)",
			truncate(string(rawBody), 200))
	}
	return key, nil
}

// bootstrapHeaders 组装 bootstrap 请求头。
//
// 与 headers() 的差别：不带 Content-Type（GET 请求），
// 但保留 x-openai-target-path / route。
func (s *Session) bootstrapHeaders() map[string]string {
	h := s.headers(StripeBootstrapRoute)
	delete(h, "Content-Type")
	return h
}

// findPublishableKey 在响应里递归找 publishable_key。
//
// 响应可能是 {"publishable_key": "..."}，也可能嵌在更深层。
func findPublishableKey(m map[string]any) string {
	// 直接命中
	if v := pickString(m, "publishable_key", "publishableKey"); v != "" {
		return v
	}
	// 一层嵌套
	for _, key := range []string{"stripe", "settings", "payment", "data", "result"} {
		if sub, ok := m[key].(map[string]any); ok {
			if v := pickString(sub, "publishable_key", "publishableKey"); v != "" {
				return v
			}
		}
	}
	// 深度搜索（最多 3 层）
	return deepFindKey(m, "publishable_key", 3)
}

// deepFindKey 递归找指定 key 的字符串值。
func deepFindKey(v any, want string, depth int) string {
	if depth <= 0 {
		return ""
	}
	switch node := v.(type) {
	case map[string]any:
		if s, ok := node[want].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		for _, child := range node {
			if got := deepFindKey(child, want, depth-1); got != "" {
				return got
			}
		}
	case []any:
		for _, child := range node {
			if got := deepFindKey(child, want, depth-1); got != "" {
				return got
			}
		}
	}
	return ""
}
