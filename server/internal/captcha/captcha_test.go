package captcha

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewKeyFromEnv(t *testing.T) {
	t.Setenv("TWOCAPTCHA_API_KEY", strings.Repeat("a", 32))
	c := New("")
	if c.key != strings.Repeat("a", 32) {
		t.Fatalf("没从环境变量读到 key: %q", c.key)
	}
}

func TestNewKeyExplicit(t *testing.T) {
	t.Setenv("TWOCAPTCHA_API_KEY", "env-value")
	c := New(strings.Repeat("b", 32))
	if !strings.HasPrefix(c.key, "bbbb") {
		t.Fatalf("显式 key 应该优先，实际 %q", c.key)
	}
}

func TestKeyMask(t *testing.T) {
	c := New(strings.Repeat("c", 32))
	got := c.Key()
	if got != "cccc***cccc" {
		t.Fatalf("脱敏格式不对: %q", got)
	}
	// 长度不对时不返回
	c2 := New("short")
	if c2.Key() != "" {
		t.Fatalf("长度不为 32 应该返回空, 实际 %q", c2.Key())
	}
}

func TestUsableRejectsBadKey(t *testing.T) {
	for _, k := range []string{"", "abc", strings.Repeat("x", 31), strings.Repeat("x", 33)} {
		c := New(k)
		// New 会 fallback 到环境变量，先清掉
		if k == "" {
			t.Setenv("TWOCAPTCHA_API_KEY", "")
			c = New("")
		}
		if err := c.usable(); err == nil {
			t.Errorf("key=%q 应该被拒绝", k)
		} else if !errors.Is(err, ErrNoKey) {
			t.Errorf("key=%q 错误类型不对: %v", k, err)
		}
	}
}

func TestPendingMap(t *testing.T) {
	if !pending["CAPCHA_NOT_READY"] {
		t.Error("CAPCHA_NOT_READY 应该识别为 pending")
	}
	if !pending["CAPTCHA_NOT_READY"] {
		t.Error("CAPTCHA_NOT_READY 应该识别为 pending")
	}
	if pending["ERROR_WRONG_USER_KEY"] {
		t.Error("ERROR_* 不应该是 pending")
	}
}

func TestErrorFormat(t *testing.T) {
	e := &Error{Code: "ERROR_WRONG_USER_KEY"}
	if !strings.Contains(e.Error(), "ERROR_WRONG_USER_KEY") {
		t.Errorf("错误信息应包含 code, 实际 %q", e.Error())
	}
	e2 := &Error{Code: "ERROR_X", Msg: "详细说明"}
	if !strings.Contains(e2.Error(), "详细说明") {
		t.Errorf("错误信息应包含 Msg, 实际 %q", e2.Error())
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Error("短字符串不该被截断")
	}
	long := strings.Repeat("x", 50)
	got := truncate(long, 10)
	if len(got) > 15 || !strings.HasSuffix(got, "...") {
		t.Errorf("截断格式不对: %q", got)
	}
}

// TestBalanceLive 需要真实 key。没有就跳过。
func TestBalanceLive(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过真实调用")
	}
	key := strings.TrimSpace(os.Getenv("TWOCAPTCHA_API_KEY"))
	if len(key) != 32 {
		t.Skip("TWOCAPTCHA_API_KEY 未配置或长度不对，跳过真实调用")
	}

	c := New(key)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bal, err := c.Balance(ctx)
	if err != nil {
		t.Fatalf("查余额失败: %v", err)
	}
	t.Logf("2captcha 余额: $%.5f", bal)
	if bal < 0 {
		t.Errorf("余额不能为负: %f", bal)
	}
}

// TestCreateBadKey 验证错误路径（不带 key 一定失败）。
func TestCreateBadKey(t *testing.T) {
	c := New("")
	c.key = "" // 强制清空
	_, err := c.create(context.Background(), map[string]string{"method": "hcaptcha"})
	if err == nil {
		t.Fatal("没 key 应该报错")
	}
	if !errors.Is(err, ErrNoKey) {
		t.Fatalf("错误类型不对: %v", err)
	}
}
