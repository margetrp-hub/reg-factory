// Package icmail is the "IC 邮箱" app of reg-factory.
//
// It owns its own SQLite file next to regd's data, indexes a mailbox over IMAP
// (IDLE first, heartbeat as fallback) and exposes a small HTTP surface under
// /api/apps/icmail. The public page only ever returns a one-time code; the admin
// routes expose counts, sources and the message index.
package icmail

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Config is the app's configuration, resolved from the environment once at boot.
type Config struct {
	DBPath             string
	AdminKey           string
	TurnstileSiteKey   string
	TurnstileSecret    string
	ExpectedHost       string
	TurnstileVerifyURL string
	RetentionDays      int
	BootstrapLimit     int
	FetchChunk         int

	// IMAP source (seeded into mail_sources on first boot)
	IMAPHost     string
	IMAPPort     int
	IMAPUser     string
	IMAPPass     string
	IMAPFolder   string
	IMAPInterval int
	IMAPHeartbeat int
}

// App is the mounted instance.
type App struct {
	Cfg Config
	DB  *DB

	mu     sync.Mutex
	cancel context.CancelFunc
}

// NewConfigFromEnv resolves configuration; dbPath defaults to <dataDir>/icmail.db.
func NewConfigFromEnv(dataDir string) Config {
	get := func(key, def string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}
	dbPath := get("ICMAIL_DB", "")
	if dbPath == "" {
		dbPath = filepath.Join(dataDir, "icmail.db")
	}
	return Config{
		DBPath:             dbPath,
		AdminKey:           get("ICMAIL_ADMIN_KEY", get("HME_ADMIN_KEY", "")),
		TurnstileSiteKey:   get("ICMAIL_TURNSTILE_SITE_KEY", get("TURNSTILE_SITE_KEY", "")),
		TurnstileSecret:    get("ICMAIL_TURNSTILE_SECRET_KEY", get("TURNSTILE_SECRET_KEY", "")),
		ExpectedHost:       get("ICMAIL_TURNSTILE_EXPECTED_HOSTNAME", get("TURNSTILE_EXPECTED_HOSTNAME", "")),
		TurnstileVerifyURL: get("ICMAIL_TURNSTILE_VERIFY_URL", ""),
		BootstrapLimit:     300,
		FetchChunk:         40,
		IMAPHost:           get("ICMAIL_IMAP_HOST", get("MAIL_IMAP_HOST", "")),
		IMAPPort:           atoiDefault(get("ICMAIL_IMAP_PORT", get("MAIL_IMAP_PORT", "993")), 993),
		IMAPUser:           get("ICMAIL_IMAP_USER", get("MAIL_IMAP_USER", "")),
		IMAPPass:           get("ICMAIL_IMAP_PASSWORD", get("MAIL_IMAP_PASSWORD", "")),
		IMAPFolder:         get("ICMAIL_IMAP_FOLDER", "INBOX"),
		IMAPInterval:       atoiDefault(get("ICMAIL_IMAP_INTERVAL", "10"), 10),
		IMAPHeartbeat:      atoiDefault(get("ICMAIL_IMAP_HEARTBEAT", "60"), 60),
	}
}

// New opens the database and returns the app.
func New(cfg Config) (*App, error) {
	db, err := OpenDB(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	app := &App{Cfg: cfg, DB: db}
	app.seedSource()
	return app, nil
}

// Close releases resources.
func (a *App) Close() error { return a.DB.Close() }

// Start launches the mail ingestion loop (no-op until a source is configured).
func (a *App) Start(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	go a.runWorker(runCtx)
}

// Stop terminates the ingestion loop.
func (a *App) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
}

// --- HTTP ------------------------------------------------------------------

// Register mounts the app routes on the given gin group (/api/apps/icmail).
func (a *App) Register(rg *gin.RouterGroup) {
	rg.GET("/health", a.handleHealth)
	rg.GET("/messages", a.requireAdmin(), a.handleMessages)
	rg.GET("/stats", a.requireAdmin(), a.handleStats)
	rg.GET("/sources", a.requireAdmin(), a.handleSources)
}

// RegisterPublic mounts the unauthenticated surface: the widget config and the
// one thing the public page is allowed to ask for — the code.
func (a *App) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/config", a.handleConfig)
	rg.POST("/code", a.handleCode)
}

func (a *App) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 空密钥 = 交给外层（regd 的登录中间件）保护；独立部署时才需要自带钥匙。
		if a.Cfg.AdminKey == "" {
			c.Next()
			return
		}
		if c.GetHeader("X-Admin-Key") != a.Cfg.AdminKey {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"ok": false, "error": gin.H{"code": "UNAUTHORIZED", "message": "invalid admin key"}})
			return
		}
		c.Next()
	}
}

func okJSON(c *gin.Context, data any) {
	c.JSON(http.StatusOK, gin.H{"ok": true, "data": data, "error": nil})
}

func failJSON(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"ok": false, "data": nil, "error": gin.H{"code": code, "message": message}})
}

func (a *App) handleHealth(c *gin.Context) {
	stats, err := a.DB.Stats(c.Request.Context())
	if err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, gin.H{"status": "ok", "app": "icmail", "stats": stats})
}

func (a *App) handleConfig(c *gin.Context) {
	okJSON(c, gin.H{
		"siteKey":    a.Cfg.TurnstileSiteKey,
		"configured": strings.TrimSpace(a.Cfg.TurnstileSiteKey) != "",
	})
}

type codeRequest struct {
	Email          string `json:"email"`
	TurnstileToken string `json:"turnstileToken"`
}

func (a *App) handleCode(c *gin.Context) {
	var req codeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "invalid JSON body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !looksLikeEmail(email) {
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "valid email is required")
		return
	}
	if err := a.VerifyTurnstile(req.TurnstileToken, clientIP(c), "otp_lookup"); err != nil {
		switch err {
		case errTurnstileNotConfigured:
			failJSON(c, http.StatusServiceUnavailable, "TURNSTILE_NOT_CONFIGURED", "human verification is not configured")
		case errTurnstileUnavailable:
			failJSON(c, http.StatusBadGateway, "TURNSTILE_UNAVAILABLE", "human verification service unavailable")
		default:
			failJSON(c, http.StatusUnauthorized, "TURNSTILE_FAILED", "human verification failed")
		}
		return
	}
	result, err := a.LookupCode(email, 20)
	if err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, result)
}

func (a *App) handleMessages(c *gin.Context) {
	rows, err := a.DB.Query(c.Query("alias"), c.Query("tag"), atoiDefault(c.Query("limit"), 100))
	if err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if rows == nil {
		rows = []Message{}
	}
	okJSON(c, gin.H{"messages": rows, "count": len(rows)})
}

func (a *App) handleStats(c *gin.Context) {
	stats, err := a.DB.Stats(c.Request.Context())
	if err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, stats)
}

func (a *App) handleSources(c *gin.Context) {
	list, err := a.DB.ListSources(false)
	if err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	out := make([]gin.H, 0, len(list))
	for _, s := range list {
		out = append(out, gin.H{
			"id": s.ID, "name": s.Name, "host": s.Host, "port": s.Port, "username": s.Username,
			"folder": s.Folder, "enabled": s.Enabled, "idle": s.Idle, "lastUid": s.LastUID,
			"lastError": nullStr(s.LastError),
		})
	}
	okJSON(c, gin.H{"sources": out})
}

// --- helpers ---------------------------------------------------------------

func looksLikeEmail(v string) bool {
	if v == "" || len(v) > 320 || strings.ContainsAny(v, " \t\r\n") {
		return false
	}
	at := strings.IndexByte(v, '@')
	return at > 0 && at < len(v)-1 && strings.Count(v, "@") == 1
}

func clientIP(c *gin.Context) string {
	if v := c.GetHeader("CF-Connecting-IP"); v != "" {
		return v
	}
	if v := c.GetHeader("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	return c.ClientIP()
}

func atoiDefault(v string, def int) int {
	if v == "" {
		return def
	}
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			return def
		}
		n = n*10 + int(r-'0')
	}
	if n <= 0 {
		return def
	}
	return n
}

// seedSource makes the configured mailbox the first source. It is idempotent, so
// a panel-managed source always wins over the env seed.
func (a *App) seedSource() {
	if a.Cfg.IMAPHost == "" || a.Cfg.IMAPUser == "" {
		return
	}
	if existing, err := a.DB.ListSources(false); err == nil && len(existing) > 0 {
		return
	}
	_, _ = a.DB.UpsertSource(Source{
		Name:             "default",
		Host:             a.Cfg.IMAPHost,
		Port:             a.Cfg.IMAPPort,
		Username:         a.Cfg.IMAPUser,
		Password:         a.Cfg.IMAPPass,
		Folder:           defaultStr(a.Cfg.IMAPFolder, "INBOX"),
		Enabled:          true,
		Idle:             true,
		IntervalSeconds:  a.Cfg.IMAPInterval,
		HeartbeatSeconds: a.Cfg.IMAPHeartbeat,
	})
}