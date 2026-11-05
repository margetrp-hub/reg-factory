// Package icmail is the "IC 邮箱" app of reg-factory.
//
// It owns its own SQLite file next to regd's data, indexes a mailbox over IMAP
// (IDLE first, heartbeat as fallback) and exposes a small HTTP surface under
// /api/apps/icmail. The public page only ever returns a one-time code; the admin
// routes expose counts, sources and the message index.
package icmail

import (
	"context"
	"strconv"
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
		RetentionDays:      atoiDefault(get("ICMAIL_RETENTION_DAYS", "90"), 90),
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
	// 管理端查码：面板/协议注册用，不需要 Turnstile（外层已有登录保护）
	rg.GET("/code", a.requireAdmin(), a.handleAdminCode)
	rg.GET("/aliases", a.requireAdmin(), a.handleAliases)
	rg.POST("/aliases/state", a.requireAdmin(), a.handleAliasState)
	rg.POST("/sources", a.requireAdmin(), a.handleUpsertSource)
	rg.POST("/sources/:id/toggle", a.requireAdmin(), a.handleSourceToggle)
	rg.DELETE("/sources/:id", a.requireAdmin(), a.handleSourceDelete)
	rg.POST("/sync", a.requireAdmin(), a.handleSync)
	rg.GET("/prune", a.requireAdmin(), a.handlePrune)
	rg.GET("/config", a.requireAdmin(), a.handleAdminConfig)
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

// --- 管理端：别名 / 源 / 同步 / 清理 / 配置 ---------------------------------

func (a *App) handleAliases(c *gin.Context) {
	list, err := a.DB.ListAliases(c.Request.Context())
	if err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, gin.H{"aliases": list, "count": len(list)})
}

func (a *App) handleAliasState(c *gin.Context) {
	var body struct {
		HME   string `json:"hme"`
		State string `json:"state"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.HME) == "" {
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "hme is required")
		return
	}
	state := strings.ToLower(strings.TrimSpace(body.State))
	switch state {
	case "active", "paused", "blocked":
	default:
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "state must be active/paused/blocked")
		return
	}
	if err := a.DB.SetAliasState(c.Request.Context(), strings.ToLower(strings.TrimSpace(body.HME)), state); err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, gin.H{"hme": body.HME, "state": state})
}

func (a *App) handleUpsertSource(c *gin.Context) {
	var src Source
	if err := c.ShouldBindJSON(&src); err != nil || strings.TrimSpace(src.Name) == "" || strings.TrimSpace(src.Host) == "" {
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "name and host are required")
		return
	}
	if src.Port == 0 {
		src.Port = 993
	}
	if strings.TrimSpace(src.Folder) == "" {
		src.Folder = "INBOX"
	}
	// 编辑时密码留空 = 保留原密码（避免前端回填明文）
	if strings.TrimSpace(src.Password) == "" {
		if existing, listErr := a.DB.ListSources(false); listErr == nil {
			for _, item := range existing {
				if item.Name == src.Name && item.Password != "" {
					src.Password = item.Password
					break
				}
			}
		}
	}
	saved, err := a.DB.UpsertSource(src)
	if err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, gin.H{"source": gin.H{
		"id": saved.ID, "name": saved.Name, "host": saved.Host, "port": saved.Port,
		"username": saved.Username, "folder": saved.Folder, "enabled": saved.Enabled,
	}})
}

func (a *App) handleSourceToggle(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "invalid id")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "enabled is required")
		return
	}
	if err := a.DB.SetSourceEnabled(c.Request.Context(), id, body.Enabled); err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, gin.H{"id": id, "enabled": body.Enabled})
}

func (a *App) handleSourceDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "invalid id")
		return
	}
	if err := a.DB.DeleteSource(c.Request.Context(), id); err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, gin.H{"id": id, "deleted": true})
}

func (a *App) handleSync(c *gin.Context) {
	var body struct {
		SourceID int64  `json:"source_id"`
		Alias    string `json:"alias"`
	}
	_ = c.ShouldBindJSON(&body)
	if err := a.DB.RequestSync(c.Request.Context(), body.SourceID, body.Alias); err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, gin.H{"queued": true, "source_id": body.SourceID, "alias": body.Alias})
}

func (a *App) handlePrune(c *gin.Context) {
	days := atoiDefault(c.Query("days"), a.Cfg.RetentionDays)
	if days <= 0 {
		days = 90
	}
	if err := a.DB.PruneOlderThan(days); err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, gin.H{"prunedOlderThanDays": days})
}

// handleAdminConfig 暴露可调配置（密码脱敏），供工作台展示与排障。
func (a *App) handleAdminConfig(c *gin.Context) {
	sources, _ := a.DB.ListSources(false)
	out := make([]gin.H, 0, len(sources))
	for _, s := range sources {
		out = append(out, gin.H{
			"id": s.ID, "name": s.Name, "host": s.Host, "port": s.Port,
			"username": s.Username, "folder": s.Folder, "enabled": s.Enabled,
			"idle": s.Idle, "intervalSeconds": s.IntervalSeconds, "heartbeatSeconds": s.HeartbeatSeconds,
			"lastUID": s.LastUID, "passwordSet": strings.TrimSpace(s.Password) != "",
		})
	}
	okJSON(c, gin.H{
		"dbPath":         a.Cfg.DBPath,
		"retentionDays":  a.Cfg.RetentionDays,
		"bootstrapLimit": a.Cfg.BootstrapLimit,
		"fetchChunk":     a.Cfg.FetchChunk,
		"adminKeySet":    a.Cfg.AdminKey != "",
		"turnstile": gin.H{
			"siteKey":          a.Cfg.TurnstileSiteKey,
			"secretSet":        a.Cfg.TurnstileSecret != "",
			"expectedHostname": a.Cfg.ExpectedHost,
			"verifyURL":        a.Cfg.TurnstileVerifyURL,
		},
		"imap": gin.H{
			"hostEnvSet":   a.Cfg.IMAPHost != "",
			"userEnvSet":   a.Cfg.IMAPUser != "",
			"passEnvSet":   a.Cfg.IMAPPass != "",
			"folder":       a.Cfg.IMAPFolder,
			"intervalSec":  a.Cfg.IMAPInterval,
			"heartbeatSec": a.Cfg.IMAPHeartbeat,
		},
		"sources": out,
	})
}

// handleAdminCode 管理端按邮箱查验证码（不带 Turnstile，靠外层登录保护）。
func (a *App) handleAdminCode(c *gin.Context) {
	email := strings.ToLower(strings.TrimSpace(c.Query("email")))
	if !looksLikeEmail(email) {
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "valid email is required")
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