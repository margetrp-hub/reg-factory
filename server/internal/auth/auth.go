// Package auth 提供面板鉴权：Cookie 会话、Basic、Bearer/X-API-Key 与失败限流。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/settings"
)

// CookieName 是会话 Cookie 名。
const CookieName = "rf_sess"

// Manager 持有鉴权配置与限流状态。
type Manager struct {
	cfg   *settings.Settings
	token string
	basic string

	mu    sync.Mutex
	fails map[string][]time.Time

	sessionTTL time.Duration
	maxFails   int
	window     time.Duration
}

// New 构造鉴权管理器。
func New(cfg *settings.Settings) *Manager {
	mac := hmac.New(sha256.New, []byte("reg-factory/regd/"+cfg.PanelPass))
	mac.Write([]byte(cfg.PanelUser))
	token := hex.EncodeToString(mac.Sum(nil))

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.PanelUser+":"+cfg.PanelPass))
	return &Manager{
		cfg:        cfg,
		token:      token,
		basic:      basic,
		fails:      map[string][]time.Time{},
		sessionTTL: 7 * 24 * time.Hour,
		maxFails:   10,
		window:     5 * time.Minute,
	}
}

// Token 返回当前会话令牌（供内部诊断）。
func (m *Manager) Token() string { return m.token }

func (m *Manager) authorized(c *gin.Context) bool {
	if m.cfg.AllowAnon {
		return true
	}
	if cookie, err := c.Cookie(CookieName); err == nil && hmac.Equal([]byte(cookie), []byte(m.token)) {
		c.Set("actor", m.cfg.PanelUser)
		return true
	}
	header := c.GetHeader("Authorization")
	if header != "" {
		if strings.HasPrefix(header, "Basic ") && hmac.Equal([]byte(header), []byte(m.basic)) {
			c.Set("actor", m.cfg.PanelUser)
			return true
		}
		if strings.HasPrefix(strings.ToLower(header), "bearer ") {
			value := strings.TrimSpace(header[7:])
			if hmac.Equal([]byte(value), []byte(m.cfg.PanelPass)) {
				c.Set("actor", "api-key")
				return true
			}
		}
	}
	if key := c.GetHeader("X-API-Key"); key != "" && hmac.Equal([]byte(key), []byte(m.cfg.PanelPass)) {
		c.Set("actor", "api-key")
		return true
	}
	return false
}

// RouteKind 决定未认证时的响应形式。
func RouteKind(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/"):
		return "api"
	default:
		return "page"
	}
}

// Middleware 鉴权中间件。
func (m *Manager) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		switch path {
		case "/api/health", "/api/login", "/api/memory":
			c.Next()
			return
		}
		// IC 邮箱 app 的对外面：只回验证码，无需登录。
		// 其余 /api/apps/mail/* 仍走下面的鉴权。
		if strings.HasPrefix(path, "/api/apps/mail/public/") {
			c.Next()
			return
		}
		if m.authorized(c) {
			c.Next()
			return
		}
		// 只有 API 需要服务端拦截；页面与静态资源放行，
		// 由前端 Shell 调 /api/session 收到 401 后跳登录页（SPA 标准做法，
		// 也避免 /login 自身被重定向成死循环）。
		if strings.HasPrefix(path, "/api/") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":  "unauthorized",
				"detail": "需要登录：POST /api/login",
			})
			return
		}
		c.Next()
	}
}

// rateLimited 判断某 IP 是否超过失败上限。
func (m *Manager) rateLimited(ip string) bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	bucket := m.fails[ip][:0]
	for _, ts := range m.fails[ip] {
		if now.Sub(ts) < m.window {
			bucket = append(bucket, ts)
		}
	}
	m.fails[ip] = bucket
	return len(bucket) >= m.maxFails
}

func (m *Manager) recordFail(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fails[ip] = append(m.fails[ip], time.Now())
	if len(m.fails) > 4096 {
		now := time.Now()
		for key, values := range m.fails {
			var keep []time.Time
			for _, ts := range values {
				if now.Sub(ts) < m.window {
					keep = append(keep, ts)
				}
			}
			if len(keep) == 0 {
				delete(m.fails, key)
			} else {
				m.fails[key] = keep
			}
		}
	}
}

// LoginRequest 是登录请求体。
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login 处理登录并下发 Cookie。
func (m *Manager) Login(c *gin.Context) {
	ip := c.ClientIP()
	if m.rateLimited(ip) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too_many_attempts", "detail": "尝试过于频繁，请稍后再试"})
		return
	}
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return
	}
	okUser := hmac.Equal([]byte(strings.TrimSpace(req.Username)), []byte(m.cfg.PanelUser))
	okPass := hmac.Equal([]byte(req.Password), []byte(m.cfg.PanelPass))
	if !okUser || !okPass {
		m.recordFail(ip)
		log.Printf("[auth] login failed ip=%s", ip)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials", "detail": "账号或密码不正确"})
		return
	}
	secure := c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(CookieName, m.token, int(m.sessionTTL.Seconds()), "/", "", secure, true)
	c.JSON(http.StatusOK, gin.H{"ok": true, "user": m.cfg.PanelUser})
}

// Logout 清除 Cookie。
func (m *Manager) Logout(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(CookieName, "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// Session 返回当前登录态。
func (m *Manager) Session(c *gin.Context) {
	actor, _ := c.Get("actor")
	c.JSON(http.StatusOK, gin.H{
		"ok":    true,
		"user":  m.cfg.PanelUser,
		"actor": toStr(actor),
		"anon":  m.cfg.AllowAnon,
	})
}

func toStr(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
