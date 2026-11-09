// Package api 装配 regd 的 HTTP 接口。
package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/apps/icmail"
	"github.com/tiantianGPU/reg-factory/server/internal/assets"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/auth"
	"github.com/tiantianGPU/reg-factory/server/internal/claudehttp"
	"github.com/tiantianGPU/reg-factory/server/internal/exporter"
	"github.com/tiantianGPU/reg-factory/server/internal/probe"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// Server 聚合依赖。
type Server struct {
	cfg     *settings.Settings
	env     *settings.Env
	st      *store.Store
	run     *runner.Manager
	auth    *auth.Manager
	scanner *assets.Scanner
	assets  *assetstore.Store
	version string

	schemaMu   sync.Mutex
	schemaAt   time.Time
	schemaList []runner.Script

	// 协议注册（Go 原生）并发槽，避免一次点 10 个把出口打死
	webRegisterSlot chan struct{}
	// IC 邮箱 app（同进程），协议注册可直接从这里取 magic link
	icApp *icmail.App
}

// New 构造 API。
func New(cfg *settings.Settings, env *settings.Env, st *store.Store, run *runner.Manager, a *auth.Manager, version string, icApp *icmail.App) *Server {
	return &Server{
		cfg:             cfg,
		env:             env,
		st:              st,
		run:             run,
		auth:            a,
		scanner:         assets.New(cfg.DataDir),
		assets:          assetstore.New(cfg.DataDir),
		version:         version,
		webRegisterSlot: make(chan struct{}, 3),
		icApp:           icApp,
	}
}

// Register 注册全部路由。
func (s *Server) Register(r *gin.Engine) {
	r.Use(s.auth.Middleware())
	if s.icApp != nil {
		group := r.Group("/api/apps/icmail", func(c *gin.Context) { c.Set("icmailAuthenticated", true); c.Next() })
		s.icApp.Register(group)
		s.icApp.RegisterPublic(r.Group("/api/apps/icmail/public"))
	}

	r.GET("/api/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true, "service": "regd", "version": s.version})
	})
	r.GET("/api/memory", func(c *gin.Context) {
		c.JSON(http.StatusOK, s.run.Memory())
	})
	r.POST("/api/login", s.auth.Login)
	r.POST("/api/logout", s.auth.Logout)
	r.GET("/api/session", s.auth.Session)

	r.GET("/api/system", s.system)
	r.GET("/api/scripts", s.scripts)

	r.POST("/api/tasks", s.createTask)
	r.GET("/api/tasks", s.listTasks)
	r.GET("/api/tasks/:id", s.getTask)
	r.POST("/api/tasks/:id/stop", s.stopTask)
	r.POST("/api/tasks/:id/production/:action", s.productionControl)
	r.POST("/api/tasks/stop-all", s.stopAll)
	r.GET("/api/tasks/:id/logs", s.taskLogs)
	r.GET("/api/tasks/:id/stream", s.taskStream)
	r.GET("/api/logs", s.taskLogs)
	r.GET("/api/logs/stream", s.taskStream)

	r.GET("/api/assets/export", s.assetExport)
	r.GET("/api/assets/tree", s.assetTree)
	r.GET("/api/assets/results", s.assetResults)
	r.GET("/api/assets", s.assetSummary)
	r.GET("/api/assets/:kind", s.assetList)
	r.POST("/api/assets/:kind/claim", s.assetClaim)
	r.POST("/api/assets/claims/reset", s.claimReset)

	r.GET("/api/config", s.configGet)
	r.PUT("/api/config", s.configPut)
	r.GET("/api/config/raw", s.configRaw)
	r.PUT("/api/config/raw", s.configRawPut)

	r.GET("/api/export/accounts", s.exportAccounts)
	r.POST("/api/registry/claude-http", s.claudeHTTPRegister)
	r.POST("/api/outlook/authorize", s.outlookAuthorize)
	r.POST("/api/accounts/import", s.importAccounts)
	r.POST("/api/accounts/change-password", s.changePasswords)
	r.POST("/api/accounts/convert", s.convertAccounts)
	r.GET("/api/accounts/detail", s.accountDetail)
	r.GET("/api/accounts/summary", s.accountSummary)
	r.GET("/api/accounts", s.listAccounts)
	r.POST("/api/accounts/sync", s.syncAccountsHandler)
	r.POST("/api/accounts/liveness", s.accountLiveness)
	r.POST("/api/accounts/refresh", s.accountRefresh)
	r.GET("/api/accounts/refresh/:id", s.getAccountRefresh)
	r.POST("/api/accounts/refresh/:id/reconcile", s.reconcileAccountRefresh)
	r.POST("/api/accounts/refresh/:id/resolve", s.resolveAccountRefresh)
	r.POST("/api/accounts/checks", s.createCredentialCheck)
	r.GET("/api/accounts/checks", s.listCredentialChecks)
	r.GET("/api/accounts/checks/:id", s.getCredentialCheck)
	r.GET("/api/audit", s.audit)
	r.GET("/api/proxy/check", s.proxyCheck)
}

// ---------------------------------------------------------------- 系统

func (s *Server) system(c *gin.Context) {
	mem := s.run.Memory()
	diskTotal, diskFree := diskUsage(s.cfg.DataDir)
	stats, _ := s.st.Stats()
	cores, quota, cpuPct := s.run.CPU()
	round1 := func(v float64) float64 { return math.Round(v*10) / 10 }

	c.JSON(http.StatusOK, gin.H{
		"version":       s.version,
		"uptime_sec":    int64(s.run.Uptime().Seconds()),
		"data_dir":      s.cfg.DataDir,
		"env_file":      s.cfg.EnvFile,
		"addr":          s.cfg.Addr,
		"python":        s.cfg.Python,
		"max_parallel":  s.cfg.MaxParallel,
		"running":       s.run.RunningCount(),
		"queued":        stats["queued"],
		"slots_used":    s.run.Weight(),
		"slots_total":   s.cfg.MaxParallel,
		"cpu_cores":     round1(cores),
		"cpu_quota":     round1(quota),
		"cpu_pct":       round1(cpuPct),
		"guarded":       s.run.Guarded(),
		"guard_pct":     s.cfg.GuardPct,
		"memory":        mem,
		"disk_total_gb": float64(diskTotal) / (1 << 30),
		"disk_free_gb":  float64(diskFree) / (1 << 30),
		"task_stats":    stats,
		"server_time":   time.Now().Format(time.RFC3339),
	})
}

func (s *Server) schemaPath() string {
	primary := filepath.Join(s.cfg.AppRoot, "deploy", "scripts.schema.json")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	return filepath.Join(s.cfg.DataDir, "scripts.schema.json")
}

func (s *Server) loadScripts() ([]runner.Script, error) {
	path := s.schemaPath()
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if !s.schemaAt.IsZero() && info.ModTime().Equal(s.schemaAt) && s.schemaList != nil {
		return s.schemaList, nil
	}
	list, err := runner.LoadScripts(path)
	if err != nil {
		return nil, err
	}
	s.schemaList = list
	s.schemaAt = info.ModTime()
	return list, nil
}

func (s *Server) scripts(c *gin.Context) {
	list, err := s.loadScripts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "schema_unavailable", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"scripts": list, "count": len(list)})
}

// ---------------------------------------------------------------- 任务

type createTaskRequest struct {
	Script    string         `json:"script"`
	Args      map[string]any `json:"args"`
	RequestID string         `json:"request_id"`
}

var taskRequestID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func (s *Server) createTask(c *gin.Context) {
	var req createTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": err.Error()})
		return
	}
	if req.RequestID != "" && !taskRequestID.MatchString(req.RequestID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_request_id"})
		return
	}
	list, err := s.loadScripts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "schema_unavailable", "detail": err.Error()})
		return
	}
	normalized, err := runner.NormalizeArgs(list, req.Script, req.Args)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_script", "detail": err.Error()})
		return
	}

	title := req.Script
	if script, ok := runner.FindScript(list, req.Script); ok {
		title = script.Title
	}
	argsJSON, _ := json.Marshal(normalized)
	actor, _ := c.Get("actor")
	task := store.Task{
		ID:       "t" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Script:   req.Script,
		Title:    title,
		ArgsJSON: string(argsJSON),
		Actor:    toString(actor),
	}
	if req.RequestID != "" {
		task.ID = "request-" + req.RequestID
		if s.replayTask(c, task) {
			return
		}
	}
	if s.run.Guarded() {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "memory_guard", "detail": "内存保护中，暂不接新任务；已受理的请求仍可查询"})
		return
	}
	if err := s.run.Submit(task); err != nil {
		// The primary key serializes concurrent submissions. A response lost
		// after insertion must resolve to the original task, even after restart.
		if req.RequestID != "" && s.replayTask(c, task) {
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "submit_failed", "detail": err.Error()})
		return
	}
	s.st.Audit(task.Actor, "task.create", task.Script+" "+string(argsJSON), c.ClientIP())
	log.Printf("[task] enqueue %s script=%s slots=%d/%d",
		task.ID, task.Script, s.run.Weight(), s.cfg.MaxParallel)
	c.JSON(http.StatusAccepted, gin.H{
		"id":     task.ID,
		"status": "queued",
		"title":  title,
		"args":   normalized,
		"slots":  s.run.Weight(),
	})
}

func (s *Server) replayTask(c *gin.Context, wanted store.Task) bool {
	old, err := s.st.GetTask(wanted.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "task_read_unavailable"})
		return true
	}
	if old.Actor != wanted.Actor || old.Script != wanted.Script || old.ArgsJSON != wanted.ArgsJSON {
		c.JSON(http.StatusConflict, gin.H{"error": "task_request_conflict", "detail": "原请求已受理，不能用同一请求更换参数"})
		return true
	}
	c.JSON(http.StatusOK, gin.H{"id": old.ID, "task_id": old.ID, "status": old.Status, "title": old.Title, "replayed": true})
	return true
}

func (s *Server) listTasks(c *gin.Context) {
	status := c.Query("status")
	limit := atoi(c.DefaultQuery("limit", "100"), 100)
	offset := atoi(c.DefaultQuery("offset", "0"), -1)
	scripts := c.QueryArray("script")
	if limit < 1 || limit > 500 || offset < 0 || offset > 10000000 || len(c.Query("q")) > 128 || len(scripts) > 50 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "task_query_invalid"})
		return
	}
	page, err := s.st.QueryTasks(store.TaskQuery{Status: status, Search: c.Query("q"), Scripts: scripts, Limit: limit, Offset: offset})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, page)
}

func (s *Server) getTask(c *gin.Context) {
	task, err := s.st.GetTask(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
		return
	}
	c.JSON(http.StatusOK, task)
}

func (s *Server) stopTask(c *gin.Context) {
	id := c.Param("id")
	if err := s.run.Stop(id); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "stop_failed", "detail": err.Error()})
		return
	}
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "task.stop", id, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) productionControl(c *gin.Context) {
	id := strings.TrimPrefix(c.Param("id"), "work-")
	if !strings.HasPrefix(c.Param("id"), "work-") || (c.Param("action") != "continue" && c.Param("action") != "stop") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "PRODUCTION_CONTROL_INVALID"})
		return
	}
	var err error
	if c.Param("action") == "stop" {
		original, readErr := s.st.ProductionAdmission(id)
		if readErr != nil {
			c.JSON(404, gin.H{"error": "PRODUCTION_NOT_FOUND"})
			return
		}
		err = s.st.StopProduction(id, original.RequestSHA, time.Now().Unix())
	} else {
		err = s.st.ControlProduction(id, c.Param("action"))
	}
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "production."+c.Param("action"), id, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) stopAll(c *gin.Context) {
	ids := s.run.StopAll()
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "task.stop_all", strings.Join(ids, ","), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true, "stopped": ids})
}

func (s *Server) taskLogs(c *gin.Context) {
	id := c.Param("id")
	if id != "" {
		if _, err := s.st.GetTask(id); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
			return
		}
	}
	if c.Query("download") == "1" {
		c.Header("Content-Disposition", `attachment; filename="runtime.log"`)
		c.Header("Content-Type", "text/plain; charset=utf-8")
		var cursor int64
		for {
			events, err := s.st.EventsAfter(id, cursor, 1000)
			if err != nil {
				c.String(http.StatusInternalServerError, "log read failed")
				return
			}
			for _, event := range events {
				if _, err := fmt.Fprintf(c.Writer, "%s [%s] [%s] %s\n", event.TS, event.TaskID, event.Stream, event.Line); err != nil {
					return
				}
				cursor = event.ID
			}
			if len(events) < 1000 || c.Request.Context().Err() != nil {
				return
			}
		}
	}
	limit := atoi(c.DefaultQuery("tail", strconv.Itoa(s.cfg.LogTailMax)), s.cfg.LogTailMax)
	events, err := s.st.TailEvents(id, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events, "count": len(events)})
}

func (s *Server) taskStream(c *gin.Context) {
	id := c.Param("id")
	if id != "" {
		if _, err := s.st.GetTask(id); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
			return
		}
	}
	cursorText := c.GetHeader("Last-Event-ID")
	if cursorText == "" {
		cursorText = c.Query("after")
	}
	cursor, err := strconv.ParseInt(cursorText, 10, 64)
	if cursorText != "" && (err != nil || cursor < 0) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_log_cursor"})
		return
	}
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "stream_unsupported"})
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// SQLite is the single source for history and live output. Reconnection
	// resumes by event ID, so there is no replay gap or slow-subscriber loss.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	initial := cursorText == ""
	lastPing := time.Now()
	for {
		var task store.Task
		if id != "" {
			task, err = s.st.GetTask(id)
			if err != nil {
				writeSSE(c, "stream_error", gin.H{"error": "任务状态读取失败，正在重连"})
				return
			}
		}
		for {
			var events []store.Event
			wasInitial := initial
			if initial {
				events, err = s.st.TailEvents(id, 500)
				initial = false
			} else {
				events, err = s.st.EventsAfter(id, cursor, 1000)
			}
			if err != nil {
				writeSSE(c, "stream_error", gin.H{"error": "日志读取失败，正在重连"})
				return
			}
			for _, event := range events {
				fmt.Fprintf(c.Writer, "id: %d\n", event.ID)
				writeSSE(c, "log", event)
				cursor = event.ID
			}
			flusher.Flush()
			if wasInitial || len(events) < 1000 || c.Request.Context().Err() != nil {
				break
			}
		}
		if id != "" && task.Status != "running" && task.Status != "queued" && task.Status != "stopping" && task.Status != "unknown" {
			writeSSE(c, "done", gin.H{"task": id, "status": task.Status, "exit_code": task.ExitCode, "error": task.Error})
			flusher.Flush()
			return
		}
		if time.Since(lastPing) >= 15*time.Second {
			fmt.Fprint(c.Writer, ": ping\n\n")
			flusher.Flush()
			lastPing = time.Now()
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func writeSSE(c *gin.Context, event string, payload any) {
	raw, _ := json.Marshal(payload)
	fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, raw)
}

// ---------------------------------------------------------------- 资产

// assetExport 导出「下游可直接导入」的标准凭据 JSON。
// chatgpt/oauth -> SUB2API / CPA / Codex 认的字段集；不带资产库元数据。
func (s *Server) assetExport(c *gin.Context) {
	platform := strings.TrimSpace(c.DefaultQuery("platform", "chatgpt"))
	kind := strings.TrimSpace(c.DefaultQuery("kind", ""))

	list, err := s.assets.List()
	if ref := c.Query("ref"); ref != "" {
		var selected assetstore.Asset
		if digest := c.Query("result_sha256"); digest != "" {
			selected, err = s.assets.Result(ref, digest)
		} else {
			selected, err = s.assets.Get(ref)
		}
		list = []assetstore.Asset{selected}
	}
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "RESULT_SELECTION_REQUIRED", "detail": err.Error()})
		return
	}

	out := make([]map[string]any, 0, len(list))
	for _, asset := range list {
		if platform != "" && asset.Platform != platform {
			continue
		}
		if kind != "" && asset.Kind != kind {
			continue
		}
		payload := asset.Payload
		if payload == nil {
			continue
		}
		switch asset.Kind {
		case "oauth", "team", "free":
			record, err := assetstore.OAuthDocument(payload, asset.Email)
			if err != nil {
				c.JSON(http.StatusConflict, gin.H{"error": "RESULT_SELECTION_REQUIRED", "ref": asset.Ref, "detail": err.Error()})
				return
			}
			record["source_ref"], record["result_sha256"] = asset.Ref, asset.ResultSHA
			out = append(out, record)
		case "imported":
			// 邮箱 + 密码 + 2FA（登录用，不是 token）
			out = append(out, map[string]any{
				"email":      firstNonEmptyStr(str(payload["email"]), asset.Email),
				"password":   str(payload["password"]),
				"totp":       str(payload["totp"]),
				"platform":   asset.Platform,
				"source_ref": asset.Ref,
			})
		}
	}

	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"platform": platform, "kind": kind, "count": len(out), "accounts": out,
	})
}

func (s *Server) assetResults(c *gin.Context) {
	ref := strings.TrimSpace(c.Query("ref"))
	items, err := s.assets.History(ref)
	if err != nil {
		c.JSON(503, gin.H{"error": "result_history_unavailable"})
		return
	}
	if ref == "" {
		query := strings.ToLower(strings.TrimSpace(c.Query("query")))
		selected := []assetstore.Asset{}
		seen := map[string]bool{}
		for _, item := range items {
			if item.Platform != "chatgpt" || (item.Kind != "oauth" && item.Kind != "team" && item.Kind != "free") ||
				seen[item.Ref] || !strings.Contains(strings.ToLower(item.Ref+" "+item.Email), query) {
				continue
			}
			seen[item.Ref] = true
			selected = append(selected, item)
			if len(selected) == 100 {
				break
			}
		}
		items = selected
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"ref": ref, "items": items})
}

func str(value any) string {
	text, _ := value.(string)
	return text
}

// assetTree 返回统一资产库的「平台 → 类型」统计与明细。
func (s *Server) assetTree(c *gin.Context) {
	list, err := s.assets.List()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	type node struct {
		Platform string             `json:"platform"`
		Kind     string             `json:"kind"`
		Count    int                `json:"count"`
		Items    []assetstore.Asset `json:"items"`
	}
	buckets := map[string]*node{}
	for _, asset := range list {
		key := asset.Platform + "/" + asset.Kind
		if buckets[key] == nil {
			buckets[key] = &node{Platform: asset.Platform, Kind: asset.Kind}
		}
		buckets[key].Count++
		if len(buckets[key].Items) < 50 {
			// 凭据内容不进列表响应（token 只在专用导出/取用接口出现）
			asset.Payload = nil
			buckets[key].Items = append(buckets[key].Items, asset)
		}
	}
	out := make([]*node, 0, len(buckets))
	for _, item := range buckets {
		out = append(out, item)
	}
	c.JSON(http.StatusOK, gin.H{
		"root": s.assets.Root(), "total": len(list), "groups": out,
	})
}

func (s *Server) assetSummary(c *gin.Context) {
	out := make([]gin.H, 0, len(assets.Kinds))

	for _, kind := range assets.Kinds {
		items, err := s.scanner.Scan(kind)
		if err != nil {
			continue
		}
		claimed, _ := s.st.ClaimedRefs(kind)
		left := 0
		for _, item := range items {
			if !claimed[item.Ref] {
				left++
			}
		}
		out = append(out, gin.H{"kind": kind, "total": len(items), "claimed": len(items) - left, "available": left})
	}
	c.JSON(http.StatusOK, gin.H{"assets": out})
}

func (s *Server) assetList(c *gin.Context) {
	kind := c.Param("kind")
	limit := atoi(c.DefaultQuery("limit", "200"), 200)
	items, err := s.scanner.Scan(kind)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	claimed, _ := s.st.ClaimedRefs(kind)
	for index := range items {
		items[index].Claimed = claimed[items[index].Ref]
	}
	if len(items) > limit {
		items = items[:limit]
	}
	c.JSON(http.StatusOK, gin.H{"kind": kind, "items": items, "count": len(items)})
}

func (s *Server) assetClaim(c *gin.Context) {
	kind := c.Param("kind")
	items, err := s.scanner.Scan(kind)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	claimed, _ := s.st.ClaimedRefs(kind)
	actor, _ := c.Get("actor")
	for _, item := range items {
		if claimed[item.Ref] {
			continue
		}
		first, claimErr := s.st.ClaimAsset(kind, item.Ref, toString(actor))
		if claimErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": claimErr.Error()})
			return
		}
		if !first {
			continue
		}
		s.st.Audit(toString(actor), "asset.claim", kind+" "+item.Ref, c.ClientIP())
		c.JSON(http.StatusOK, gin.H{"ok": true, "item": item})
		return
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "no_available_asset", "kind": kind})
}

func (s *Server) claimReset(c *gin.Context) {
	kind := c.Query("kind")
	n, err := s.st.ResetClaims(kind)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "asset.reset", kind, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true, "removed": n})
}

// ---------------------------------------------------------------- 账号导出（Go 原生）

// exportAccounts 输出浏览器扩展可直接导入的 accounts.json 结构。
func (s *Server) exportAccounts(c *gin.Context) {
	platform := c.DefaultQuery("platform", "claude")
	entries, err := exporter.Export(s.cfg.DataDir, platform)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported_platform", "platforms": exporter.Platforms()})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"platform": platform,
		"count":    len(entries),
		"entries":  entries,
	})
}

// ---------------------------------------------------------------- 协议注册（Go 原生，无浏览器）

// icMailbox 让协议注册直接从 IC 的 SQLite 取信（同进程，零网络、零鉴权）。
type icMailbox struct {
	app   *icmail.App
	email string
}

// Address 返回邮箱地址。
func (m *icMailbox) Address() string { return m.email }

// WaitForMagicLink 轮询 IC 的索引库，找主题/正文里的 Claude magic link。
func (m *icMailbox) WaitForMagicLink(ctx context.Context, timeout time.Duration) (string, error) {
	if m.app == nil || m.app.DB == nil {
		return "", &claudehttp.Error{Kind: claudehttp.ErrMailbox, Detail: "IC 邮箱未初始化"}
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := m.app.DB.MailboxAvailable(m.email); err != nil {
			return "", &claudehttp.Error{Kind: claudehttp.ErrMailbox, Detail: err.Error()}
		}
		messages, err := m.app.DB.Query(m.email, "", 10)
		if err == nil {
			for _, message := range messages {
				text := message.Subject + "\n" + message.BodyText + "\n" + message.Snippet
				if link := claudehttp.ExtractMagicLink(text); link != "" {
					return link, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", &claudehttp.Error{Kind: claudehttp.ErrMailbox, Detail: "IC 等 magic link 超时"}
		}
		select {
		case <-ctx.Done():
			return "", &claudehttp.Error{Kind: claudehttp.ErrMailbox, Detail: "已取消"}
		case <-time.After(3 * time.Second):
		}
	}
}

type claudeHTTPRequest struct {
	Email        string `json:"email"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
	Proxy        string `json:"proxy"`
	Seed         string `json:"seed"`
	MagicLink    string `json:"magic_link"`
	Mailbox      string `json:"mailbox"`
	Timeout      int    `json:"timeout"`
}

// claudeHTTPRegister 用 Go 协议流程注册 Claude（零浏览器）。
func (s *Server) claudeHTTPRegister(c *gin.Context) {
	var req claudeHTTPRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Email) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": "需要 email"})
		return
	}
	var mailbox claudehttp.Mailbox
	switch {
	case strings.TrimSpace(req.MagicLink) != "":
		mailbox = &claudehttp.StaticMailbox{Addr: req.Email, Link: strings.TrimSpace(req.MagicLink)}
	case s.icApp != nil && (strings.EqualFold(strings.TrimSpace(req.Mailbox), "icloud") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSpace(req.Email)), "@icloud.com")):
		// IC 与 regd 同进程：直接读它的 SQLite，零网络、零鉴权
		mailbox = &icMailbox{app: s.icApp, email: strings.TrimSpace(req.Email)}
	case strings.TrimSpace(req.RefreshToken) != "":
		mailbox = claudehttp.NewOutlookMailbox(req.Email, req.RefreshToken, req.ClientID)
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "mailbox_required",
			"detail": "需要 mailbox=icloud（IC 同进程取码）、refresh_token（Outlook）或 magic_link（人工粘贴）",
		})
		return
	}

	select {
	case s.webRegisterSlot <- struct{}{}:
		defer func() { <-s.webRegisterSlot }()
	default:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "busy", "detail": "协议注册并发已满，请稍后重试"})
		return
	}

	timeout := req.Timeout
	if timeout < 30 || timeout > 600 {
		timeout = 180
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeout+30)*time.Second)
	defer cancel()

	result, err := claudehttp.Register(ctx, claudehttp.Options{
		Email: req.Email, Seed: req.Seed, ProxyURL: req.Proxy,
		Timeout: time.Duration(timeout) * time.Second, Mailbox: mailbox,
	})
	if err != nil {
		kind := claudehttp.KindOf(err)
		actor, _ := c.Get("actor")
		s.st.Audit(toString(actor), "claude.protocol_failed", req.Email+" "+string(kind), c.ClientIP())
		status := http.StatusBadGateway
		if kind == claudehttp.ErrRateLimit {
			status = http.StatusTooManyRequests
		}
		c.JSON(status, gin.H{"error": string(kind), "detail": err.Error()})
		return
	}
	path, saveErr := claudehttp.SaveSession(s.cfg.DataDir, result)
	if saveErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "save_failed", "detail": saveErr.Error()})
		return
	}
	if _, syncErr := s.SeedAccounts(); syncErr != nil {
		log.Printf("[claude-http] accounts 同步失败: %v", syncErr)
	}
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "claude.protocol_ok", req.Email+" -> "+path, c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "email": result.Email,
		"session_key_len": len(result.SessionKey),
		"saved_to":        path, "proxy": req.Proxy != "",
	})
}

// ---------------------------------------------------------------- 账号主数据

// SeedAccounts 把数据目录里的资产同步进 accounts 表（保留健康/领取状态）。
func (s *Server) SeedAccounts() (int, error) {
	seeds := make([]store.AccountSeed, 0, 256)

	// 统一资产库：/data/assets/<platform>/<kind>/<ref>.json
	if stored, listErr := s.assets.List(); listErr == nil {
		for _, asset := range stored {
			seeds = append(seeds, store.AccountSeed{
				Ref: asset.Ref, Platform: asset.Platform, Kind: asset.Kind,
				Identity: asset.Email, SecretRef: "assets/" + asset.Path,
			})
		}
	}

	for _, kind := range assets.Kinds {
		items, scanErr := s.scanner.Scan(kind)
		if scanErr != nil {
			continue
		}
		for _, item := range items {
			identity := item.Value
			if idx := strings.Index(identity, "----"); idx > 0 {
				identity = identity[:idx]
			} else if idx := strings.IndexByte(identity, '\n'); idx > 0 {
				identity = identity[:idx]
			}
			if len(identity) > 160 {
				identity = identity[:160]
			}
			seeds = append(seeds, store.AccountSeed{
				Ref: item.Ref, Kind: kind, Platform: item.Platform,
				Identity: strings.TrimSpace(identity), SecretRef: item.Source,
			})
		}
	}
	return s.st.SyncAccounts(seeds)
}

func (s *Server) syncAccountsHandler(c *gin.Context) {
	n, err := s.SeedAccounts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "synced": n})
}

type importRequest struct {
	Accounts           string `json:"accounts"`
	Platform           string `json:"platform"`
	Kind               string `json:"kind"`
	AutoChangePassword bool   `json:"auto_change_password"`
	NewPassword        string `json:"new_password"`
	Concurrency        int    `json:"concurrency"`
}

// importAccounts 导入「已接码」账号（人工拿到、已经过短信验证的号）。
// 每行支持：email----password / email password / email
func (s *Server) importAccounts(c *gin.Context) {
	var req importRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Accounts) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": "需要 accounts"})
		return
	}
	platform := strings.TrimSpace(req.Platform)
	if platform == "" {
		platform = "chatgpt"
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "imported"
	}

	batch := "imp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	seeds := make([]store.AccountSeed, 0, 64)
	type parsed struct{ email, password, totp string }
	rows := make([]parsed, 0, 64)

	for _, line := range strings.Split(req.Accounts, "\n") {
		text := strings.TrimSpace(line)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.Split(text, "----")
		if len(parts) == 1 {
			parts = strings.Fields(text)
		}
		email := strings.TrimSpace(parts[0])
		if !strings.Contains(email, "@") {
			continue
		}
		password := ""
		if len(parts) > 1 {
			password = strings.TrimSpace(parts[1])
		}
		totp := ""
		if len(parts) > 2 {
			totp = strings.TrimSpace(parts[2])
		}
		seeds = append(seeds, store.AccountSeed{
			Ref: "import:" + strings.ToLower(email), Platform: platform, Kind: kind,
			Identity: email, SecretRef: batch,
		})
		rows = append(rows, parsed{email: email, password: password, totp: totp})
	}
	if len(seeds) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no_valid_rows", "detail": "没有识别到邮箱"})
		return
	}
	for _, row := range rows {
		ref := "import:" + strings.ToLower(row.email)
		_ = s.st.SaveCredential(ref, row.password, row.totp)
		_ = s.assets.Put(assetstore.Asset{
			Ref: ref, Platform: platform, Kind: kind, Email: row.email, Source: "import",
			Payload: map[string]any{"password": row.password, "totp": row.totp},
		})
	}
	imported, err := s.st.UpsertAccounts(seeds)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	taskIDs := make([]string, 0, len(rows))
	if req.AutoChangePassword && strings.TrimSpace(req.NewPassword) != "" {
		for _, row := range rows {
			argv := []string{"--platform", platform, "--email", row.email,
				"--ref", "import:" + strings.ToLower(row.email), "--new-password", req.NewPassword}
			if row.password != "" {
				argv = append(argv, "--old-password", row.password)
			}
			if row.totp != "" {
				argv = append(argv, "--totp-secret", row.totp)
			}
			raw, _ := json.Marshal(argv)
			task := store.Task{
				ID:       "pwd-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(len(taskIDs)),
				Script:   "change_password",
				Title:    "改密码 · " + row.email,
				ArgsJSON: string(raw),
				Actor:    "panel",
			}
			if err := s.run.Submit(task); err == nil {
				taskIDs = append(taskIDs, task.ID)
			}
		}
	}

	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "accounts.import", fmt.Sprintf("%s platform=%s count=%d auto_pwd=%v", batch, platform, imported, req.AutoChangePassword), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "batch": batch, "imported": imported,
		"tasks": taskIDs, "auto_change_password": len(taskIDs) > 0,
	})
}

// proxyPoolFor 从环境变量取代理池（PROXY_POOL 优先，其次 PROXY_URL）。
func (s *Server) proxyPoolFor() []string {
	value := func(key string) string {
		if configured, exists := settings.DeploymentValue(key); exists {
			return configured
		}
		return s.env.Get(key)
	}
	raw := strings.TrimSpace(value("REG_FACTORY_PROXY_POOL"))
	if raw == "" {
		raw = strings.TrimSpace(value("REG_FACTORY_PROXY"))
	}
	if raw == "" {
		raw = strings.TrimSpace(value("REG_FACTORY_PROXY_URL"))
	}
	if raw == "" {
		return nil
	}
	out := make([]string, 0, 8)
	for _, item := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

type changePasswordRequest struct {
	Refs        []string `json:"refs"`
	NewPassword string   `json:"new_password"`
	Platform    string   `json:"platform"`
}

// changePasswords 批量改密码（能力入口：账号列表多选 → 改密码）。
func (s *Server) changePasswords(c *gin.Context) {
	var req changePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Refs) == 0 || strings.TrimSpace(req.NewPassword) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": "需要 refs 与 new_password"})
		return
	}
	if len(req.Refs) > 200 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too_many", "detail": "单次最多 200 个"})
		return
	}

	queued := make([]gin.H, 0, len(req.Refs))
	skipped := make([]gin.H, 0)
	for _, refValue := range req.Refs {
		account, err := s.st.GetAccount(refValue)
		if err != nil {
			skipped = append(skipped, gin.H{"ref": refValue, "reason": "not_found"})
			continue
		}
		if account.Claimed {
			skipped = append(skipped, gin.H{"ref": refValue, "reason": "claimed"})
			continue
		}
		email := account.Identity
		if email == "" {
			email = strings.TrimPrefix(strings.TrimPrefix(refValue, "import:"), "emails:")
		}
		password, totp, credErr := s.st.GetCredential(refValue)
		if credErr == nil && password == "" && totp == "" {
			skipped = append(skipped, gin.H{"ref": refValue, "reason": "no_credential"})
			continue
		}

		platform := strings.TrimSpace(req.Platform)
		if platform == "" {
			platform = account.Platform
		}
		if platform == "" || platform == "*" {
			platform = "chatgpt"
		}
		argv := []string{"--platform", platform, "--email", email, "--ref", refValue, "--new-password", req.NewPassword}
		if password != "" {
			argv = append(argv, "--old-password", password)
		}
		if totp != "" {
			argv = append(argv, "--totp-secret", totp)
		}
		if pool := s.proxyPoolFor(); len(pool) > 0 {
			argv = append(argv, "--proxy", pool[len(queued)%len(pool)])
		}
		raw, _ := json.Marshal(argv)
		taskID := "pwd-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		task := store.Task{
			ID: taskID, Script: "change_password",
			Title: "改密码 · " + email, ArgsJSON: string(raw), Actor: "panel",
		}
		if err := s.run.Submit(task); err != nil {
			skipped = append(skipped, gin.H{"ref": refValue, "reason": "submit_failed"})
			continue
		}
		_ = s.st.MarkPassword(refValue, "pending", "已排队")
		queued = append(queued, gin.H{"ref": refValue, "task_id": taskID})
	}

	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "accounts.change_password",
		fmt.Sprintf("queued=%d skipped=%d", len(queued), len(skipped)), c.ClientIP())
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "queued": queued, "skipped": skipped})
}

type convertRequest struct {
	Refs      []string `json:"refs"`
	Platform  string   `json:"platform"`
	RequestID string   `json:"request_id"`
}

// convertAccounts 把「邮箱+密码+2FA」账号登录成标准 Free 凭据 JSON。
// 产物落 /data/assets/<platform>/free/，同时把账号状态刷成 ok，Team 即可拉到 available。
func (s *Server) convertAccounts(c *gin.Context) {
	var req convertRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Refs) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": "需要 refs"})
		return
	}
	if len(req.Refs) > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too_many", "detail": "单次最多 100 个"})
		return
	}

	if req.RequestID != "" && !taskRequestID.MatchString(req.RequestID) {
		c.JSON(400, gin.H{"error": "invalid_request_id"})
		return
	}
	actor, _ := c.Get("actor")
	queued := make([]gin.H, 0, len(req.Refs))
	skipped := make([]gin.H, 0)

	for _, refValue := range req.Refs {
		account, err := s.st.GetAccount(refValue)
		if err != nil {
			skipped = append(skipped, gin.H{"ref": refValue, "reason": "not_found"})
			continue
		}
		password, _, credErr := s.st.GetCredential(refValue)
		if credErr != nil || password == "" {
			skipped = append(skipped, gin.H{"ref": refValue, "reason": "no_password"})
			continue
		}
		if account.Platform != "chatgpt" {
			skipped = append(skipped, gin.H{"ref": refValue, "reason": "unsupported_platform"})
			continue
		}
		email := account.Identity
		if email == "" {
			email = strings.TrimPrefix(refValue, "import:")
		}

		requestID := ""
		if req.RequestID != "" {
			digest := sha256.Sum256([]byte(req.RequestID + ":" + refValue))
			requestID = fmt.Sprintf("%x", digest[:])
		}
		task, _, err := s.queueAccountRefresh(accountOpRequest{Ref: refValue, AllowRelogin: true, RequestID: requestID}, toString(actor))
		if err != nil {
			reason := "submit_failed"
			if errors.Is(err, store.ErrAccountOwned) {
				reason = "claimed"
			}
			skipped = append(skipped, gin.H{"ref": refValue, "reason": reason})
			continue
		}
		queued = append(queued, gin.H{"ref": refValue, "task_id": task.ID, "email": email, "status": task.Status})
	}

	s.run.Wake()
	s.st.Audit(toString(actor), "accounts.convert_free",
		fmt.Sprintf("queued=%d skipped=%d", len(queued), len(skipped)), c.ClientIP())
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "queued": queued, "skipped": skipped})
}

// accountDetail 返回单个账号的完整状态（抽屉用）。
func (s *Server) accountDetail(c *gin.Context) {
	ref := strings.TrimSpace(c.Query("ref"))
	if ref == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_ref"})
		return
	}
	account, err := s.st.GetAccount(ref)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "detail": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"account": account})
}

func (s *Server) listAccounts(c *gin.Context) {
	limit, offset := atoi(c.DefaultQuery("limit", "200"), 200), atoi(c.DefaultQuery("offset", "0"), -1)
	if limit < 1 || limit > 1000 || offset < 0 || offset > 10000000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_query_invalid"})
		return
	}
	list, total, err := s.st.ListAccountsPage(c.Query("kind"), c.Query("platform"), c.Query("status"), limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": list, "count": len(list), "total": total, "offset": offset, "limit": limit})
}

func (s *Server) accountSummary(c *gin.Context) {
	rows, err := s.st.AccountSummaries(c.Query("platform"))
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account_summary_unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"platforms": rows})
}

type accountOpRequest struct {
	Ref          string `json:"ref"`
	Platform     string `json:"platform"`
	RefreshScan  bool   `json:"refresh_scan"`
	AllowRelogin bool   `json:"allow_relogin"`
	RequestID    string `json:"request_id"`
}

// submitAssetOp 以 raw 任务形式跑 tools/asset_ops.py。
func (s *Server) submitAssetOp(op, ref, platform string, flags ...string) (string, error) {
	argv := []string{op, "--ref", ref}
	if platform != "" {
		argv = append(argv, "--platform", platform)
	}
	argv = append(argv, flags...)
	raw, _ := json.Marshal(argv)
	id := "op-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	task := store.Task{
		ID: id, Script: "asset_ops", Title: "面板" + op + " · " + ref,
		ArgsJSON: string(raw), Actor: "panel",
	}
	return id, s.run.Submit(task)
}

// probeOAuthAccount 只使用 AT 测试，不换取或保存凭据。
// 返回 (handled, error)：handled=true 表示已同步处理完，不再排任务。
func (s *Server) probeOAuthAccount(ref string) (bool, error) {
	asset, err := s.assets.Get(ref)
	if err != nil || asset.Platform != "chatgpt" || asset.Kind != "oauth" {
		return false, nil
	}
	proxy := s.env.Get("REG_FACTORY_PRODUCTION_PROXY")
	if value, exists := settings.DeploymentValue("REG_FACTORY_PRODUCTION_PROXY"); exists {
		proxy = value
	}
	result := probe.OAuth(asset.Payload, proxy, 30*time.Second)
	if err := s.st.MarkHealth(ref, result.Status, result.Message); err != nil {
		return true, err
	}
	return true, nil
}

func (s *Server) accountLiveness(c *gin.Context) {
	var req accountOpRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Ref == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": "需要 ref"})
		return
	}
	flags := []string{}
	if req.RefreshScan {
		flags = append(flags, "--refresh-scan")
	}
	// OAuth 类账号：直接用 token 验证，同步返回结果
	if handled, probeErr := s.probeOAuthAccount(req.Ref); handled {
		if probeErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": probeErr.Error()})
			return
		}
		account, _ := s.st.GetAccount(req.Ref)
		c.JSON(http.StatusOK, gin.H{"ok": true, "probe": "oauth", "status": account.Status, "message": account.HealthMessage})
		return
	}

	id, err := s.submitAssetOp("liveness", req.Ref, req.Platform, flags...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "task_id": id})
}

func (s *Server) accountRefresh(c *gin.Context) {
	var req accountOpRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Ref == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": "需要 ref"})
		return
	}
	if req.RequestID != "" && !taskRequestID.MatchString(req.RequestID) {
		c.JSON(400, gin.H{"error": "invalid_request_id"})
		return
	}
	actor, _ := c.Get("actor")
	task, created, err := s.queueAccountRefresh(req, toString(actor))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(404, gin.H{"error": "account_not_found"})
			return
		}
		if errors.Is(err, store.ErrAccountOwned) {
			c.JSON(409, gin.H{"error": "account_owned_by_core", "detail": "已领取账号通过主系统原任务刷新"})
			return
		}
		if err.Error() == "task_request_conflict" {
			c.JSON(409, gin.H{"error": "task_request_conflict"})
			return
		}
		c.JSON(503, gin.H{"error": "refresh_acceptance_unavailable"})
		return
	}
	if !created {
		c.JSON(200, gin.H{"ok": true, "task_id": task.ID, "status": task.Status, "replayed": true})
		return
	}
	s.run.Wake()
	s.st.Audit(task.Actor, "account.refresh", task.ID, c.ClientIP())
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "task_id": task.ID, "status": "queued"})
}

// Conversion and repair share serialization, retained evidence and replay rules.
// Only references enter task arguments; login secrets remain in the asset store.
func (s *Server) queueAccountRefresh(req accountOpRequest, actor string) (store.Task, bool, error) {
	account, err := s.st.GetAccount(req.Ref)
	if err != nil {
		return store.Task{}, false, err
	}
	argv := []string{"refresh", "--ref", req.Ref, "--platform", account.Platform}
	title := "RT 刷新 · "
	if req.AllowRelogin {
		argv = append(argv, "--allow-relogin")
		title = "凭据修复 · "
	}
	raw, _ := json.Marshal(argv)
	task := store.Task{ID: "op-" + strconv.FormatInt(time.Now().UnixNano(), 36), Script: "asset_ops", Title: title + req.Ref, ArgsJSON: string(raw), Actor: actor}
	if req.RequestID != "" {
		task.ID = "request-" + req.RequestID
	}
	id, created, err := s.st.QueueAccountRefresh(task, req.Ref)
	if err != nil {
		return store.Task{}, false, err
	}
	task, err = s.st.GetTask(id)
	return task, created, err
}

type outlookAuthRequest struct {
	Accounts    string `json:"accounts"`
	Concurrency int    `json:"concurrency"`
}

// outlookAuthorize 把粘贴的 email----password 落成临时文件并提交授权任务。
func (s *Server) outlookAuthorize(c *gin.Context) {
	var req outlookAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Accounts) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": "需要 accounts"})
		return
	}
	if len(req.Accounts) > 5_000_000 {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "too_large"})
		return
	}
	concurrency := req.Concurrency
	if concurrency < 1 {
		concurrency = 3
	}
	if concurrency > 10 {
		concurrency = 10
	}
	dir := filepath.Join(s.cfg.DataDir, "runtime", "outlook_authorize")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	handle, err := os.CreateTemp(dir, "accounts-*.txt")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	path := handle.Name()
	_, writeErr := handle.WriteString(strings.ReplaceAll(req.Accounts, "\r\n", "\n") + "\n")
	handle.Close()
	_ = os.Chmod(path, 0o600)
	if writeErr != nil {
		_ = os.Remove(path)
		c.JSON(http.StatusInternalServerError, gin.H{"error": writeErr.Error()})
		return
	}

	args, _ := json.Marshal(map[string]any{
		"--input":       path,
		"--concurrency": concurrency,
	})
	task := store.Task{
		ID:       "auth-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Script:   "unlock_outlook",
		Title:    "Outlook 批量 Graph 授权",
		ArgsJSON: string(args),
		Actor:    "panel",
	}
	if err := s.run.Submit(task); err != nil {
		_ = os.Remove(path)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "outlook.authorize", path, c.ClientIP())
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "task_id": task.ID})
}

// StartLivenessLoop 按配置周期触发号池巡检（单平台串行）。
func (s *Server) StartLivenessLoop(ctx context.Context) {
	if !envBool("REG_FACTORY_LIVENESS_ENABLED", true) {
		log.Printf("[liveness] 巡检已关闭")
		return
	}
	interval := envDuration("REG_FACTORY_LIVENESS_INTERVAL", 6*time.Hour)
	platforms := envList("REG_FACTORY_LIVENESS_PLATFORMS", []string{"claude"})
	accountConcurrency := os.Getenv("REG_FACTORY_LIVENESS_ACCOUNT_CONCURRENCY")
	if accountConcurrency == "" {
		accountConcurrency = "1"
	}
	log.Printf("[liveness] 巡检开启：每 %s，平台 %v，账号并发 %s", interval, platforms, accountConcurrency)
	submitScan := func(platform string) {
		argv := []string{"scan", "--platform", platform, "--account-concurrency", accountConcurrency}
		raw, _ := json.Marshal(argv)
		task := store.Task{
			ID:       "liveness-" + strconv.FormatInt(time.Now().UnixNano(), 36),
			Script:   "asset_ops",
			Title:    "巡检 · " + platform,
			ArgsJSON: string(raw),
			Actor:    "scheduler",
		}
		if err := s.run.Submit(task); err != nil {
			log.Printf("[liveness] 提交失败 %s: %v", platform, err)
		}
	}
	go func() {
		// 首扫：账号从未测活过就先跑一轮，避免面板一直是 unknown
		if accounts, err := s.st.ListAccounts("", "", "unknown", 1); err == nil && len(accounts) > 0 {
			log.Printf("[liveness] 检测到未测活账号，先执行首轮巡检")
			submitScan(platforms[0])
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, platform := range platforms {
					submitScan(platform)
				}
			}
		}
	}()
}

// ---------------------------------------------------------------- 配置

func (s *Server) configGet(c *gin.Context) {
	entries, err := s.env.Parse()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	schema, schemaErr := settings.LoadConfigSchema(filepath.Join(s.cfg.AppRoot, "deploy", "config.schema.json"))
	groups := settings.MergeConfig(schema, entries, groupOf)
	schemaError := ""
	if schemaErr != nil {
		schemaError = "完整配置定义暂不可读，以下仅显示已保存配置；请重试或检查安装文件。"
	}
	c.JSON(http.StatusOK, gin.H{
		"path":         s.env.Path(),
		"groups":       groups,
		"count":        len(entries),
		"schema_error": schemaError,
	})
}

type configUpdate struct {
	Updates map[string]string `json:"updates"`
}

func (s *Server) configPut(c *gin.Context) {
	var req configUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return
	}
	if len(req.Updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty_updates"})
		return
	}
	for key, value := range req.Updates {
		if !validKey(key) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_key", "detail": key})
			return
		}
		if strings.ContainsAny(value, "\r\n") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_value", "detail": key + " 含换行"})
			return
		}
	}
	changed, err := s.env.Update(req.Updates)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "config.update", strings.Join(changed, ","), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true, "changed": changed})
}

func (s *Server) configRaw(c *gin.Context) {
	raw, err := os.ReadFile(s.env.Path())
	if err != nil && !os.IsNotExist(err) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"path": s.env.Path(), "content": string(raw)})
}

type rawUpdate struct {
	Content string `json:"content"`
}

func (s *Server) configRawPut(c *gin.Context) {
	var req rawUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
		return
	}
	if len(req.Content) > 1<<20 {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "too_large"})
		return
	}
	for _, line := range strings.Split(req.Content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, _, ok := strings.Cut(trimmed, "=")
		if !ok || !validKey(strings.TrimSpace(key)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_line", "detail": trimmed})
			return
		}
	}
	temp := s.env.Path() + ".tmp"
	if err := os.WriteFile(temp, []byte(req.Content), 0o600); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := os.Rename(temp, s.env.Path()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "config.raw_update", "", c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------- 审计与代理

func (s *Server) audit(c *gin.Context) {
	list, err := s.st.ListAudit(atoi(c.DefaultQuery("limit", "200"), 200))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"audit": list})
}

func (s *Server) proxyCheck(c *gin.Context) {
	proxyURL := c.Query("proxy")
	target := c.DefaultQuery("url", "https://claude.ai")
	if proxyURL != "" && !strings.Contains(proxyURL, "://") {
		proxyURL = "http://" + proxyURL
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxyURL != "" {
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_proxy", "detail": err.Error()})
			return
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	start := time.Now()
	req, _ := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, target, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36")
	resp, err := client.Do(req)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		message := err.Error()
		if len(message) > 180 {
			message = message[:180]
		}
		c.JSON(http.StatusOK, gin.H{"ok": false, "error": message, "ms": elapsed})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	title := ""
	lower := strings.ToLower(string(body))
	if idx := strings.Index(lower, "<title>"); idx >= 0 {
		if end := strings.Index(lower[idx:], "</title>"); end > 0 {
			title = strings.TrimSpace(string(body)[idx+7 : idx+end])
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":     resp.StatusCode < 400,
		"status": resp.StatusCode,
		"ms":     elapsed,
		"title":  title,
		"proxy":  redactProxy(proxyURL),
	})
}

// ---------------------------------------------------------------- 工具

func groupOf(key string) string {
	upper := strings.ToUpper(key)
	switch {
	case strings.HasPrefix(upper, "SUB2API"), strings.HasPrefix(upper, "CPA_"), strings.HasPrefix(upper, "CODEX"):
		return "下游服务"
	case strings.Contains(upper, "PROXY"), strings.Contains(upper, "CLASH"):
		return "网络出口"
	case strings.Contains(upper, "BROWSER"), strings.Contains(upper, "FINGERPRINT"), strings.Contains(upper, "CLOAK"):
		return "浏览器"
	case strings.Contains(upper, "MAIL"), strings.Contains(upper, "YYDS"), strings.Contains(upper, "ICLOUD"), strings.Contains(upper, "SMS"):
		return "邮箱与接码"
	case strings.Contains(upper, "CAPTCHA"), strings.Contains(upper, "VISION"), strings.Contains(upper, "VOTE"):
		return "验证码"
	case strings.Contains(upper, "LIVENESS"):
		return "测活"
	case strings.Contains(upper, "REFRESH"):
		return "凭据修复"
	case strings.HasPrefix(upper, "REG_FACTORY_"):
		return "运行参数"
	}
	return "其他"
}

func validKey(key string) bool {
	if key == "" || len(key) > 120 {
		return false
	}
	for index, r := range key {
		ok := r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (index > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

func redactProxy(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "***"
	}
	return parsed.Scheme + "://***@" + parsed.Host
}

func atoi(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func toString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil && d >= time.Minute {
		return d
	}
	return fallback
}

func envList(name string, fallback []string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, item := range parts {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func firstNonEmptyStr(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
