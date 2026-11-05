package exporter

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCookieFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestExportClaude(t *testing.T) {
	root := t.TempDir()
	writeCookieFile(t, filepath.Join(root, "cookies"), "full_abc.json", `{
	  "email": "user@example.com",
	  "sessionKey": "sk-1",
	  "cookies": [
	    {"name":"sessionKey","value":"sk-1","domain":".claude.ai","path":"/","httpOnly":true,"secure":true,"sameSite":"lax"},
	    {"name":"other","value":"x","domain":".example.com","path":"/"}
	  ]
	}`)

	entries, err := Export(root, "claude")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(entries))
	}
	entry := entries[0]
	if entry.Email != "user@example.com" {
		t.Fatalf("email=%q", entry.Email)
	}
	if len(entry.Cookies) != 1 || entry.Cookies[0].Name != "sessionKey" {
		t.Fatalf("应只保留 claude 域名下的 sessionKey，得到 %+v", entry.Cookies)
	}
	if entry.Cookies[0].SameSite != "lax" || !entry.Cookies[0].HTTPOnly {
		t.Fatalf("cookie 属性映射异常: %+v", entry.Cookies[0])
	}
}

func TestExportSkipsMissingKeyCookie(t *testing.T) {
	root := t.TempDir()
	writeCookieFile(t, filepath.Join(root, "cookies"), "full_empty.json", `{
	  "email": "a@example.com",
	  "cookies": [{"name":"other","value":"x","domain":".claude.ai"}]
	}`)
	entries, err := Export(root, "claude")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("没有 sessionKey 的账号应被跳过，得到 %d", len(entries))
	}
}

func TestExportUnknownPlatform(t *testing.T) {
	if _, err := Export(t.TempDir(), "nope"); err == nil {
		t.Fatalf("未知平台应报错")
	}
	if len(Platforms()) != 3 {
		t.Fatalf("平台数=%d", len(Platforms()))
	}
}
