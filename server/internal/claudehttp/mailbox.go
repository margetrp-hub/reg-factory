package claudehttp

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

// Mailbox 负责把 Claude 发来的 magic link 交给注册流程。
type Mailbox interface {
	Address() string
	WaitForMagicLink(ctx context.Context, timeout time.Duration) (string, error)
}

// OutlookMailbox 用 Graph refresh_token 读收件箱（不启动浏览器）。
type OutlookMailbox struct {
	email        string
	refreshToken string
	clientID     string
	http         *http.Client
}

// NewOutlookMailbox 构造 Graph 邮箱读取器。
func NewOutlookMailbox(email, refreshToken, clientID string) *OutlookMailbox {
	if strings.TrimSpace(clientID) == "" {
		clientID = "9e5f94bc-e8a4-4e73-b8be-63364c29d753"
	}
	return &OutlookMailbox{
		email:        strings.TrimSpace(email),
		refreshToken: strings.TrimSpace(refreshToken),
		clientID:     clientID,
		http:         &http.Client{Timeout: 30 * time.Second},
	}
}

// Address 返回邮箱地址。
func (m *OutlookMailbox) Address() string { return m.email }

func (m *OutlookMailbox) accessToken(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("client_id", m.clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", m.refreshToken)
	form.Set("scope", "https://graph.microsoft.com/Mail.Read offline_access")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://login.microsoftonline.com/consumers/oauth2/v2.0/token",
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", &Error{Kind: ErrMailbox, Detail: err.Error()}
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := m.http.Do(req)
	if err != nil {
		return "", &Error{Kind: ErrNetwork, Detail: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return "", &Error{Kind: ErrMailbox, Detail: fmt.Sprintf("Graph token HTTP %d: %s", resp.StatusCode, truncate(string(raw), 120))}
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.AccessToken == "" {
		return "", &Error{Kind: ErrMailbox, Detail: "Graph token 响应缺少 access_token"}
	}
	return payload.AccessToken, nil
}

// WaitForMagicLink 轮询收件箱找 Claude 的 magic link。
func (m *OutlookMailbox) WaitForMagicLink(ctx context.Context, timeout time.Duration) (string, error) {
	if m.refreshToken == "" {
		return "", &Error{Kind: ErrMailbox, Detail: "缺少 Graph refresh_token，无法取码"}
	}
	token, err := m.accessToken(ctx)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	interval := 5 * time.Second
	for {
		link, err := m.latestMagicLink(ctx, token)
		if err != nil {
			return "", err
		}
		if link != "" {
			return link, nil
		}
		if time.Now().After(deadline) {
			return "", &Error{Kind: ErrMailbox, Detail: "等待 Claude 邮件超时"}
		}
		select {
		case <-ctx.Done():
			return "", &Error{Kind: ErrMailbox, Detail: "已取消"}
		case <-time.After(interval):
		}
	}
}

func (m *OutlookMailbox) latestMagicLink(ctx context.Context, token string) (string, error) {
	endpoint := "https://graph.microsoft.com/v1.0/me/messages" +
		"?$top=15&$select=subject,body,receivedDateTime&$orderby=receivedDateTime%20desc"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", &Error{Kind: ErrMailbox, Detail: err.Error()}
	}
	req.Header.Set("authorization", "Bearer "+token)
	resp, err := m.http.Do(req)
	if err != nil {
		return "", &Error{Kind: ErrNetwork, Detail: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return "", &Error{Kind: ErrMailbox, Detail: "Graph token 失效（401），需要重新授权"}
	}
	if resp.StatusCode != http.StatusOK {
		return "", &Error{Kind: ErrMailbox, Detail: fmt.Sprintf("Graph 邮件 HTTP %d", resp.StatusCode)}
	}
	var payload struct {
		Value []struct {
			Subject string `json:"subject"`
			Body    struct {
				Content string `json:"content"`
			} `json:"body"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", &Error{Kind: ErrMailbox, Detail: "Graph 响应解析失败"}
	}
	for _, message := range payload.Value {
		text := message.Subject + "\n" + message.Body.Content
		if link := ExtractMagicLink(text); link != "" {
			return link, nil
		}
	}
	return "", nil
}

// StaticMailbox 供测试或人工粘贴链接使用。
type StaticMailbox struct {
	Addr string
	Link string
}

// Address 返回地址。
func (m *StaticMailbox) Address() string { return m.Addr }

// WaitForMagicLink 直接返回预置链接。
func (m *StaticMailbox) WaitForMagicLink(ctx context.Context, timeout time.Duration) (string, error) {
	if m.Link == "" {
		return "", &Error{Kind: ErrMailbox, Detail: "未提供 magic link"}
	}
	return m.Link, nil
}
