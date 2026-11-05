package claudehttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHeadersAreStablePerSeed(t *testing.T) {
	client, err := NewClient(Options{Email: "a@example.com"})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	first := client.Headers()
	second := client.Headers()
	if first["anthropic-device-id"] == "" || first["anthropic-anonymous-id"] == "" {
		t.Fatalf("缺少身份头: %v", first)
	}
	if first["anthropic-device-id"] != second["anthropic-device-id"] {
		t.Fatalf("同 seed 的 device id 应稳定")
	}
	if first["anthropic-client-platform"] != "web_claude_ai" {
		t.Fatalf("platform 头不对: %v", first["anthropic-client-platform"])
	}
	if !strings.HasPrefix(first["anthropic-anonymous-id"], "claudeai.v1.") {
		t.Fatalf("anonymous id 前缀不对: %v", first["anthropic-anonymous-id"])
	}

	other, _ := NewClient(Options{Email: "b@example.com"})
	if other.Headers()["anthropic-device-id"] == first["anthropic-device-id"] {
		t.Fatalf("不同 seed 应得到不同 device id")
	}
}

func TestExtractMagicLink(t *testing.T) {
	text := `点击登录 https://claude.ai/magic-link#abc123%3Adef%3D 立即进入`
	got := ExtractMagicLink(text)
	if got == "" || !strings.HasPrefix(got, "https://claude.ai/magic-link#") {
		t.Fatalf("未提取到链接: %q", got)
	}
	if ExtractMagicLink("nothing here") != "" {
		t.Fatalf("无链接时应返回空")
	}
}

func TestVerifyMagicLinkParsesFragment(t *testing.T) {
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/send_magic_link":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case "/api/auth/verify_magic_link":
			_ = json.NewDecoder(r.Body).Decode(&seen)
			http.SetCookie(w, &http.Cookie{Name: "sessionKey", Value: "sk-test-123", Path: "/"})
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer server.Close()

	client, err := NewClient(Options{Email: "a@example.com", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	if _, err := client.SendMagicLink(ctx, "a@example.com", -480); err != nil {
		t.Fatalf("send: %v", err)
	}
	link := "https://claude.ai/magic-link#nonce-abc:encoded%40example.com"
	if _, key, err := client.VerifyMagicLink(ctx, link); err != nil || key != "sk-test-123" {
		t.Fatalf("verify: key=%q err=%v", key, err)
	}
	credentials, _ := seen["credentials"].(map[string]any)
	if credentials["nonce"] != "nonce-abc" {
		t.Fatalf("nonce 解析错误: %v", seen)
	}
	if credentials["encoded_email_address"] != "encoded@example.com" {
		t.Fatalf("encoded_email 未解码: %v", credentials["encoded_email_address"])
	}
}

func TestRegisterWithStaticMailbox(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/verify_magic_link" {
			http.SetCookie(w, &http.Cookie{Name: "sessionKey", Value: "sk-static", Path: "/"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	result, err := Register(context.Background(), Options{
		Email:   "a@example.com",
		BaseURL: server.URL,
		Timeout: 5 * time.Second,
		Mailbox: &StaticMailbox{Addr: "a@example.com", Link: "https://claude.ai/magic-link#n:e"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if result.SessionKey != "sk-static" {
		t.Fatalf("sessionKey=%q", result.SessionKey)
	}
}
