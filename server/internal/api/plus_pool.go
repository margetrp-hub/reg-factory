// Package api — Plus 池能力的 HTTP 端点。
//
// 2026-11-13：把 internal/plus 里三份写完但零调用的代码接进 API：
//
//	ip_pool.go  527 行  IP 预选池（探活 + 纯净度评分 + 取还）
//	pool.go     302 行  账号池（取号 / 归还 / 退役 / 晋升）
//	warmup.go   271 行  预热（home / auth 探活 + CF clearance 吸收）
//
// 之前它们的导出符号外部引用全是 0，能力闲置。
package api

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/plus"
)

// ---------------------------------------------------------------- 工具

func atoiOr(raw string, def int) int {
	if v, err := strconv.Atoi(raw); err == nil {
		return v
	}
	return def
}

func stageOf(raw string) plus.Stage {
	switch plus.Stage(raw) {
	case plus.StageBind:
		return plus.StageBind
	case plus.StagePrepare:
		return plus.StagePrepare
	default:
		return plus.StageLink
	}
}

// maskIPProxy 脱敏代理地址里的凭据。
func maskIPProxy(raw string) string {
	return plus.MaskProxy(raw)
}

// initPoolSchema 在服务启动时调用一次：
//
//	· 告诉 plus 包当前后端方言（决定 ? 还是 $N）
//	· 建 ip_pool 表 + 给 accounts 加 pool_* 字段
//
// 之前这些从没被调用过，所以：
//
//	· ip_pool 表不存在 → GET /api/plus/ip-pool 报 "no such table"
//	· accounts 没有 pool_state → GET /api/plus/pool 报 "column does not exist"
func (s *Server) initPoolSchema() {
	plus.SetDialect(s.st.DialectName())

	db := s.st.DB()
	if err := plus.EnsureIPPoolSchema(db); err != nil {
		log.Printf("[plus-pool] 建 ip_pool 失败: %v", err)
	}
	if err := plus.EnsurePoolSchema(db); err != nil {
		log.Printf("[plus-pool] 加 accounts.pool_* 失败: %v", err)
	}
}

// ---------------------------------------------------------------- IP 预选池

// plusIPPoolList GET /api/plus/ip-pool?stage=link&state=good&limit=100
//
// 返回池子。Proxy 字段脱敏。
func (s *Server) plusIPPoolList(c *gin.Context) {
	stage := stageOf(c.DefaultQuery("stage", "link"))
	state := c.DefaultQuery("state", "")
	limit := atoiOr(c.DefaultQuery("limit", "100"), 100)

	rows, err := plus.IPPoolList(s.st.DB(), stage, state, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	out := make([]gin.H, 0, len(rows))
	for _, e := range rows {
		out = append(out, gin.H{
			"id":         e.ID,
			"stage":      e.Stage,
			"proxy":      maskIPProxy(e.Proxy),
			"egress":     e.Egress,
			"want":       e.Want,
			"ip":         e.IP,
			"isp":        e.ISP,
			"city":       e.City,
			"latency_ms": e.LatencyMs,
			"cf_ok":      e.CFOK,
			"home_code":  e.HomeCode,
			"state":      e.State,
			"checked_at": e.CheckedAt,
			"reason":     e.Reason,
			"fail_count": e.FailCount,
			"purity":     e.Purity,
		})
	}

	stats, _ := plus.IPPoolStats(s.st.DB(), stage)

	c.JSON(http.StatusOK, gin.H{
		"stage": string(stage),
		"count": len(out),
		"stats": stats,
		"items": out,
	})
}

// plusIPPoolScreen POST /api/plus/ip-pool/screen
//
// body:
//
//	{
//	  "proxy_tmpl": "http://user-region-US-sid-{sid}-t-10:pass@host:3010",
//	  "stage": "link",
//	  "want": "US",
//	  "count": 10,
//	  "min_delay_ms": 0,
//	  "concurrency": 5,
//	  "cf_path": "/"
//	}
//
// 探测一批 IP，通过 MinDelayMs 且能过 CF 的入库，返回结果（含纯净度分级）。
func (s *Server) plusIPPoolScreen(c *gin.Context) {
	var body struct {
		ProxyTmpl   string `json:"proxy_tmpl"`
		Stage       string `json:"stage"`
		Want        string `json:"want"`
		Count       int    `json:"count"`
		MinDelayMs  int64  `json:"min_delay_ms"`
		Concurrency int    `json:"concurrency"`
		CFPath      string `json:"cf_path"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body 解析失败: " + err.Error()})
		return
	}
	if body.ProxyTmpl == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "proxy_tmpl 必填，例如 http://user:pass@host:3010 或含 {sid} 的模板"})
		return
	}
	if body.Count <= 0 {
		body.Count = 10
	}
	if body.Concurrency <= 0 {
		body.Concurrency = 5
	}
	if body.CFPath == "" {
		body.CFPath = "/"
	}

	// 用现有配置做 base（TLS 指纹 / 超时等）
	base := plus.DefaultConfig()

	pcfg := plus.IPPoolConfig{
		Stage:       stageOf(body.Stage),
		Want:        body.Want,
		ProxyTmpl:   body.ProxyTmpl,
		Count:       body.Count,
		MinDelayMs:  body.MinDelayMs,
		Concurrency: body.Concurrency,
		CFPath:      body.CFPath,
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()

	logs := make([]string, 0, body.Count)
	rows, err := plus.ScreenIPPool(ctx, s.st.DB(), pcfg, base, func(m string) {
		logs = append(logs, m)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": err.Error(),
			"logs":  logs,
		})
		return
	}

	out := make([]gin.H, 0, len(rows))
	grades := map[string]int{}
	for _, e := range rows {
		grade := ""
		if e.Purity != nil {
			grade = e.Purity.Grade
			grades[grade]++
		}
		out = append(out, gin.H{
			"id":         e.ID,
			"proxy":      maskIPProxy(e.Proxy),
			"ip":         e.IP,
			"isp":        e.ISP,
			"city":       e.City,
			"egress":     e.Egress,
			"latency_ms": e.LatencyMs,
			"cf_ok":      e.CFOK,
			"state":      e.State,
			"purity":     e.Purity,
			"grade":      grade,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"count":  len(out),
		"grades": grades,
		"items":  out,
		"logs":   logs,
	})
}

// plusIPPoolTake POST /api/plus/ip-pool/take
//
// body: {"stage": "link", "want": "US"}
// 取一个可用的 IP（标记成 in_use）。
func (s *Server) plusIPPoolTake(c *gin.Context) {
	var body struct {
		Stage string `json:"stage"`
		Want  string `json:"want"`
	}
	_ = c.ShouldBindJSON(&body)

	e, err := plus.IPTake(s.st.DB(), stageOf(body.Stage), body.Want)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":         e.ID,
		"stage":      e.Stage,
		"proxy":      maskIPProxy(e.Proxy),
		"ip":         e.IP,
		"isp":        e.ISP,
		"city":       e.City,
		"egress":     e.Egress,
		"latency_ms": e.LatencyMs,
		"cf_ok":      e.CFOK,
		"purity":     e.Purity,
	})
}

// plusIPPoolRelease POST /api/plus/ip-pool/release
//
// body: {"id": 12, "success": true, "reason": ""}
// 归还。success=false 会累加 fail_count。
func (s *Server) plusIPPoolRelease(c *gin.Context) {
	var body struct {
		ID      int64  `json:"id"`
		Success bool   `json:"success"`
		Reason  string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.ID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id 必填"})
		return
	}
	if err := plus.IPRelease(s.st.DB(), body.ID, body.Success, body.Reason); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------- 账号池

// plusPoolList GET /api/plus/pool?state=&limit=100
func (s *Server) plusPoolList(c *gin.Context) {
	state := c.DefaultQuery("state", "")
	limit := atoiOr(c.DefaultQuery("limit", "100"), 100)

	rows, err := plus.PoolList(s.st.DB(), state, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	stats, _ := plus.PoolStats(s.st.DB())
	c.JSON(http.StatusOK, gin.H{
		"count": len(rows),
		"stats": stats,
		"items": rows,
	})
}

// plusPoolTake POST /api/plus/pool/take
func (s *Server) plusPoolTake(c *gin.Context) {
	e, err := plus.PoolTake(s.st.DB())
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, e)
}

// plusPoolRelease POST /api/plus/pool/release  {"ref":"...","reason":""}
func (s *Server) plusPoolRelease(c *gin.Context) {
	var body struct {
		Ref    string `json:"ref"`
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Ref == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ref 必填"})
		return
	}
	if err := plus.PoolRelease(s.st.DB(), body.Ref, body.Reason); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---------------------------------------------------------------- 预热

// plusWarmup POST /api/plus/warmup
//
// body:
//
//	{
//	  "token": "...",          // ChatGPT access_token
//	  "account_id": "...",
//	  "proxy": "user:pass@host:port",
//	  "stage": "link"
//	}
//
// 走一遍 home / auth 探活并吸收 cookie，返回 WarmupResult。
// 这是「预热机制」的 HTTP 入口 —— 原来 warmup.go 的 271 行一行都没被调用。
func (s *Server) plusWarmup(c *gin.Context) {
	var body struct {
		Token     string `json:"token"`
		AccountID string `json:"account_id"`
		Proxy     string `json:"proxy"`
		Stage     string `json:"stage"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body 解析失败: " + err.Error()})
		return
	}
	if body.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "token 必填"})
		return
	}

	cfg := plus.DefaultConfig()
	cfg.Proxy = body.Proxy

	sess := plus.NewSessionWithConfig(body.Token, body.AccountID, cfg)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()

	res, err := sess.Warmup(ctx, cfg)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":           res.OK,
		"proxy":        res.Proxy,
		"egress":       res.Egress,
		"ip":           res.IP,
		"isp":          res.ISP,
		"home_status":  res.HomeStatus,
		"auth_status":  res.AuthStatus,
		"cookies":      res.Cookies,
		"cf_clearance": res.CfClearance,
		"latency_ms":   res.LatencyMs,
		"message":      res.Message,
		"stage":        body.Stage,
	})
}
