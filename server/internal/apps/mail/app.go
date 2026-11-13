// Package mail owns SG's local mail sources, address pool and message index.
// Production uses local PostgreSQL. IMAP, POP3 and Graph feed the same store;
// temporary HTTP providers use the execution adapter in common/temp_email.py.
// Public code lookup and authenticated management have separate routes.
package mail

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	storepkg "github.com/tiantianGPU/reg-factory/server/internal/store"
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
	IMAPHost      string
	IMAPPort      int
	IMAPUser      string
	IMAPPass      string
	IMAPFolder    string
	IMAPInterval  int
	IMAPHeartbeat int
}

// App is the mounted instance.
type App struct {
	Cfg Config
	DB  *DB

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	changed chan struct{}
	runners map[int64]*managedRunner
	dial    func(context.Context, Source) (mailClient, error)
}

// NewConfigFromEnv resolves the canonical MAIL_* deployment configuration.
// Old keys are renamed once by deploy/upgrade-mail-config.py before cutover.
func NewConfigFromEnv() Config {
	get := func(key, def string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return def
	}
	return Config{
		DBPath:             get("MAIL_DB", ""),
		AdminKey:           get("MAIL_ADMIN_KEY", ""),
		TurnstileSiteKey:   get("MAIL_TURNSTILE_SITE_KEY", ""),
		TurnstileSecret:    get("MAIL_TURNSTILE_SECRET_KEY", ""),
		ExpectedHost:       get("MAIL_TURNSTILE_EXPECTED_HOSTNAME", ""),
		TurnstileVerifyURL: get("MAIL_TURNSTILE_VERIFY_URL", ""),
		RetentionDays:      atoiDefault(get("MAIL_RETENTION_DAYS", "90"), 90),
		BootstrapLimit:     300,
		FetchChunk:         40,
		IMAPHost:           get("MAIL_IMAP_HOST", ""),
		IMAPPort:           atoiDefault(get("MAIL_IMAP_PORT", "993"), 993),
		IMAPUser:           get("MAIL_IMAP_USER", ""),
		IMAPPass:           get("MAIL_IMAP_PASSWORD", ""),
		IMAPFolder:         get("MAIL_IMAP_FOLDER", "INBOX"),
		IMAPInterval:       atoiDefault(get("MAIL_IMAP_INTERVAL", "10"), 10),
		IMAPHeartbeat:      atoiDefault(get("MAIL_IMAP_HEARTBEAT", "60"), 60),
	}
}

// New opens the database and returns the app.
func New(cfg Config) (*App, error) {
	db, err := OpenDB(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	app := &App{Cfg: cfg, DB: db, changed: make(chan struct{}, 1), runners: map[int64]*managedRunner{}}
	if err := app.seedSource(); err != nil {
		db.Close()
		return nil, err
	}
	return app, nil
}

// Close releases resources.
func (a *App) Close() error { a.Stop(); return a.DB.Close() }

// Start launches the mail ingestion loop (no-op until a source is configured).
func (a *App) Start(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	a.done = make(chan struct{})
	done := a.done
	go func() { defer close(done); a.runWorker(runCtx) }()
}

// Stop terminates the ingestion loop.
func (a *App) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		// Workers use this mutex only for supervisor snapshots; release it
		// while joining, and do not allow another Start until they finish.
		done := a.done
		a.mu.Unlock()
		<-done
		a.mu.Lock()
		if a.done == done {
			a.cancel = nil
		}
	}
}

// --- HTTP ------------------------------------------------------------------

// Register mounts the app routes on the given gin group (/api/apps/mail).
func (a *App) Register(rg *gin.RouterGroup) {
	rg.GET("/pool", a.requireAdmin(), a.handlePool)
	rg.GET("/workflow/mailbox", a.requireAdmin(), a.handleWorkflowMailbox)
	rg.GET("/workflow/candidates", a.requireAdmin(), a.handleWorkflowCandidates)
	rg.POST("/pool/import", a.requireAdmin(), a.handlePoolImport)
	rg.POST("/pool/mailbox", a.requireAdmin(), a.handleMailbox)
	rg.GET("/pool/availability", a.requireAdmin(), a.handleAvailability)
	rg.POST("/pool/platform-state", a.requireAdmin(), a.handlePlatformState)
	rg.POST("/pool/parent-state", a.requireAdmin(), a.handleParentState)
	rg.GET("/sync", a.requireAdmin(), a.handleSyncRuns)
	rg.GET("/health", a.requireAdmin(), a.handleHealth)
	rg.GET("/messages", a.requireAdmin(), a.handleMessages)
	rg.GET("/stats", a.requireAdmin(), a.handleStats)
	rg.GET("/sources", a.requireAdmin(), a.handleSources)
	// 管理端查码：面板/协议注册用，不需要 Turnstile（外层已有登录保护）
	rg.GET("/code", a.requireAdmin(), a.handleAdminCode)
	rg.POST("/sources", a.requireAdmin(), a.handleUpsertSource)
	rg.POST("/sources/:id/toggle", a.requireAdmin(), a.handleSourceToggle)
	rg.DELETE("/sources/:id", a.requireAdmin(), a.handleSourceDelete)
	rg.POST("/sync", a.requireAdmin(), a.handleSync)
	rg.POST("/prune", a.requireAdmin(), a.handlePrune)
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
		if c.GetBool("mailAuthenticated") || a.Cfg.AdminKey == "" {
			c.Next()
			return
		}
		if subtle.ConstantTimeCompare([]byte(c.GetHeader("X-Admin-Key")), []byte(a.Cfg.AdminKey)) != 1 {
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
	okJSON(c, gin.H{"status": "ok", "app": "mail", "stats": stats})
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
		a.codeError(c, err)
		return
	}
	okJSON(c, result)
}

// --- 管理端：别名 / 源 / 同步 / 清理 / 配置 ---------------------------------

func (a *App) handleUpsertSource(c *gin.Context) {
	var src Source
	if err := c.ShouldBindJSON(&src); err != nil {
		failJSON(c, http.StatusBadRequest, "BAD_REQUEST", "收件来源格式无效")
		return
	}
	if src.ID < 0 || src.IntervalSeconds < 0 || src.HeartbeatSeconds < 0 {
		failJSON(c, http.StatusBadRequest, "BAD_SOURCE", "请检查服务器、端口和同步间隔")
		return
	}
	saved, err := a.DB.UpsertSource(src)
	if err != nil {
		failJSON(c, http.StatusConflict, "SOURCE_NOT_SAVED", err.Error())
		return
	}
	a.notifySources()
	okJSON(c, gin.H{"source": gin.H{
		"id": saved.ID, "name": saved.Name, "host": saved.Host, "port": saved.Port,
		"username": saved.Username, "folder": saved.Folder, "enabled": saved.Enabled,
		"transport": saved.Transport, "credentialMode": saved.CredentialMode,
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
	a.notifySources()
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
	a.notifySources()
}

func (a *App) handleSync(c *gin.Context) {
	var body struct {
		SourceID  int64  `json:"source_id"`
		Alias     string `json:"alias"`
		RequestID string `json:"request_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		failJSON(c, 400, "BAD_REQUEST", "需要同步来源和请求编号")
		return
	}
	items, err := a.DB.QueueSync(c.Request.Context(), body.SourceID, strings.ToLower(strings.TrimSpace(body.Alias)), body.RequestID)
	if err != nil {
		failJSON(c, http.StatusConflict, "SYNC_NOT_QUEUED", err.Error())
		return
	}
	a.notifySources()
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "data": gin.H{"queued": true, "runs": items, "requestId": body.RequestID}})
}

func (a *App) handlePrune(c *gin.Context) {
	days := a.Cfg.RetentionDays
	if days <= 0 {
		days = 90
	}
	if value, present := c.GetQuery("days"); present {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 3650 {
			failJSON(c, http.StatusBadRequest, "INVALID_RETENTION", "保留时间必须为 1–3650 天")
			return
		}
		days = parsed
	}
	if days > 3650 {
		failJSON(c, http.StatusBadRequest, "INVALID_RETENTION", "保留时间必须为 1–3650 天")
		return
	}
	if err := a.DB.PruneOlderThan(days); err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	okJSON(c, gin.H{"prunedOlderThanDays": days})
}

// handleAdminConfig 暴露可调配置（密码脱敏），供工作台展示与排障。
func (a *App) handleAdminConfig(c *gin.Context) {
	dbPath := a.Cfg.DBPath
	if a.DB.dialect == storepkg.DialectPG {
		// A DSN contains credentials; it is not a displayable filesystem path.
		dbPath = "PostgreSQL"
	}
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
		"dbPath":         dbPath,
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
		a.codeError(c, err)
		return
	}
	okJSON(c, result)
}

func (a *App) codeError(c *gin.Context, err error) {
	if errors.Is(err, ErrMailboxUnavailable) {
		failJSON(c, http.StatusConflict, "MAILBOX_UNAVAILABLE", "邮箱已暂停或阻止，请先在邮箱池恢复")
		return
	}
	failJSON(c, http.StatusInternalServerError, "CODE_READ_FAILED", "取码失败，请稍后重试")
}

func (a *App) handleMessages(c *gin.Context) {
	rows, total, err := a.DB.MessagePage(c.Request.Context(), c.Query("alias"), c.Query("tag"), int64(atoiDefault(c.Query("source_id"), 0)), atoiDefault(c.Query("limit"), 25), atoiDefault(c.Query("offset"), 0))
	if err != nil {
		failJSON(c, http.StatusInternalServerError, "DB_ERROR", err.Error())
		return
	}
	if rows == nil {
		rows = []Message{}
	}
	okJSON(c, gin.H{"messages": rows, "count": len(rows), "total": total})
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
			"lastUID":   s.LastUID, "lastOKAt": s.LastOKAt, "intervalSeconds": s.IntervalSeconds, "heartbeatSeconds": s.HeartbeatSeconds,
			"passwordSet": s.Password != "", "runtime": a.sourceRuntime(s),
			"transport": s.Transport, "credentialMode": s.CredentialMode,
			"clientID": s.ClientID, "refreshTokenSet": s.RefreshToken != "",
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
func (a *App) seedSource() error {
	if a.Cfg.IMAPHost == "" && a.Cfg.IMAPUser == "" {
		return nil
	}
	existing, err := a.DB.ListSources(false)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	_, err = a.DB.UpsertSource(Source{
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
	return err
}
