// Package api 装配 regd 的 HTTP 接口。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/assets"
	"github.com/tiantianGPU/reg-factory/server/internal/auth"
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
	version string

	schemaMu   sync.Mutex
	schemaAt   time.Time
	schemaList []runner.Script
}

// New 构造 API。
func New(cfg *settings.Settings, env *settings.Env, st *store.Store, run *runner.Manager, a *auth.Manager, version string) *Server {
	return &Server{
		cfg:     cfg,
		env:     env,
		st:      st,
		run:     run,
		auth:    a,
		scanner: assets.New(cfg.DataDir),
		version: version,
	}
}

// Register 注册全部路由。
func (s *Server) Register(r *gin.Engine) {
	r.Use(s.auth.Middleware())

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
	r.POST("/api/tasks/stop-all", s.stopAll)
	r.GET("/api/tasks/:id/logs", s.taskLogs)
	r.GET("/api/tasks/:id/stream", s.taskStream)

	r.GET("/api/assets", s.assetSummary)
	r.GET("/api/assets/:kind", s.assetList)
	r.POST("/api/assets/:kind/claim", s.assetClaim)
	r.POST("/api/assets/claims/reset", s.claimReset)

	r.GET("/api/config", s.configGet)
	r.PUT("/api/config", s.configPut)
	r.GET("/api/config/raw", s.configRaw)
	r.PUT("/api/config/raw", s.configRawPut)

	r.POST("/api/outlook/authorize", s.outlookAuthorize)
	r.GET("/api/accounts", s.listAccounts)
	r.POST("/api/accounts/sync", s.syncAccountsHandler)
	r.POST("/api/accounts/liveness", s.accountLiveness)
	r.POST("/api/accounts/refresh", s.accountRefresh)
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
	Script string         `json:"script"`
	Args   map[string]any `json:"args"`
}

func (s *Server) createTask(c *gin.Context) {
	if s.run.Guarded() {
		mem := s.run.Memory()
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":  "memory_guard",
			"detail": fmt.Sprintf("容器内存已用 %.1f%%（%dMB/%dMB），超过 %.0f%% 阈值，暂不接新任务",
				mem.UsedPct, mem.CurrentMB, mem.MaxMB, s.cfg.GuardPct),
			"memory": mem,
		})
		return
	}
	var req createTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body", "detail": err.Error()})
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
	if err := s.run.Submit(task); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "submit_failed", "detail": err.Error()})
		return
	}
	s.st.Audit(task.Actor, "task.create", task.Script+" "+string(argsJSON), c.ClientIP())
	log.Printf("[task] enqueue %s script=%s slots=%d/%d args=%s",
		task.ID, task.Script, s.run.Weight(), s.cfg.MaxParallel, string(argsJSON))
	c.JSON(http.StatusAccepted, gin.H{
		"id":     task.ID,
		"status": "queued",
		"title":  title,
		"args":   normalized,
		"slots":  s.run.Weight(),
	})
}

func (s *Server) listTasks(c *gin.Context) {
	status := c.Query("status")
	limit := atoi(c.DefaultQuery("limit", "100"), 100)
	tasks, err := s.st.ListTasks(status, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks, "count": len(tasks)})
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

func (s *Server) stopAll(c *gin.Context) {
	ids := s.run.StopAll()
	actor, _ := c.Get("actor")
	s.st.Audit(toString(actor), "task.stop_all", strings.Join(ids, ","), c.ClientIP())
	c.JSON(http.StatusOK, gin.H{"ok": true, "stopped": ids})
}

func (s *Server) taskLogs(c *gin.Context) {
	id := c.Param("id")
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
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "stream_unsupported"})
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	if events, err := s.st.TailEvents(id, 200); err == nil {
		for _, event := range events {
			writeSSE(c, "log", gin.H{"ts": event.TS, "stream": event.Stream, "line": event.Line})
		}
		flusher.Flush()
	}

	lines, cancel := s.run.Subscribe(id)
	defer cancel()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	done := c.Request.Context().Done()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			fmt.Fprint(c.Writer, ": ping\n\n")
			flusher.Flush()
		case line, open := <-lines:
			if !open {
				writeSSE(c, "done", gin.H{"task": id})
				flusher.Flush()
				return
			}
			writeSSE(c, "log", gin.H{"ts": line.TS, "stream": line.Stream, "line": line.Line})
			flusher.Flush()
		}
		if task, err := s.st.GetTask(id); err == nil {
			if task.Status != "running" && task.Status != "queued" {
				writeSSE(c, "done", gin.H{"task": id, "status": task.Status, "exit_code": task.ExitCode})
				flusher.Flush()
				return
			}
		}
	}
}

func writeSSE(c *gin.Context, event string, payload any) {
	raw, _ := json.Marshal(payload)
	fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, raw)
}

// ---------------------------------------------------------------- 资产

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

// ---------------------------------------------------------------- 账号主数据

// SeedAccounts 把数据目录里的资产同步进 accounts 表（保留健康/领取状态）。
func (s *Server) SeedAccounts() (int, error) {
	seeds := make([]store.AccountSeed, 0, 256)
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

func (s *Server) listAccounts(c *gin.Context) {
	list, err := s.st.ListAccounts(c.Query("kind"), c.Query("platform"), c.Query("status"),
		atoi(c.DefaultQuery("limit", "200"), 200))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": list, "count": len(list)})
}

type accountOpRequest struct {
	Ref         string `json:"ref"`
	Platform    string `json:"platform"`
	RefreshScan bool   `json:"refresh_scan"`
	AllowRelogin bool  `json:"allow_relogin"`
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
	// 治理：已领取/已售账号禁止自动修复
	if account, err := s.st.GetAccount(req.Ref); err == nil && account.Claimed {
		c.JSON(http.StatusConflict, gin.H{
			"error": "ACCOUNT_CLAIMED", "detail": "该账号已被下游领取，禁止自动刷新",
		})
		return
	}
	flags := []string{}
	if req.AllowRelogin {
		flags = append(flags, "--allow-relogin")
	}
	id, err := s.submitAssetOp("refresh", req.Ref, req.Platform, flags...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"ok": true, "task_id": id})
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
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, platform := range platforms {
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
	groups := map[string][]settings.Entry{}
	for _, entry := range entries {
		name := groupOf(entry.Key)
		groups[name] = append(groups[name], entry)
	}
	c.JSON(http.StatusOK, gin.H{
		"path":   s.env.Path(),
		"groups": groups,
		"count":  len(entries),
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
