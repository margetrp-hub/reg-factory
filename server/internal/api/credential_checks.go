package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/probe"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

type credentialCheckRequest struct {
	RequestID string   `json:"request_id"`
	Platform  string   `json:"platform"`
	Refs      []string `json:"refs"`
	ParentID  string   `json:"parent_id,omitempty"`
}

func (s *Server) createCredentialCheck(c *gin.Context) {
	var req credentialCheckRequest
	if err := c.ShouldBindJSON(&req); err != nil || !taskRequestID.MatchString(req.RequestID) || req.Platform == "" || len(req.Refs) < 1 || len(req.Refs) > 100 {
		c.JSON(400, gin.H{"error": "invalid_check_request", "detail": "需要平台、请求 ID 和 1–100 个账号"})
		return
	}
	// Exclude transport identity from the immutable request body.
	args, _ := json.Marshal(struct {
		Platform string   `json:"platform"`
		Refs     []string `json:"refs"`
		ParentID string   `json:"parent_id,omitempty"`
	}{req.Platform, req.Refs, req.ParentID})
	actor, _ := c.Get("actor")
	task := store.Task{ID: "request-" + req.RequestID, Script: store.CredentialCheckScript, Title: fmt.Sprintf("凭据检查 · %d 个账号", len(req.Refs)), ArgsJSON: string(args), Actor: toString(actor)}
	if s.replayTask(c, task) {
		return
	}
	if req.ParentID != "" {
		parent, err := s.st.GetTask(req.ParentID)
		var parentArgs credentialCheckRequest
		if err != nil || parent.Script != store.CredentialCheckScript || json.Unmarshal([]byte(parent.ArgsJSON), &parentArgs) != nil || parentArgs.Platform != req.Platform {
			c.JSON(400, gin.H{"error": "invalid_parent_check"})
			return
		}
		if parent.Status == "queued" || parent.Status == "running" || parent.Status == "stopping" {
			c.JSON(409, gin.H{"error": "parent_check_active", "detail": "原批次仍在执行，请先等待或停止"})
			return
		}
		allowed := map[string]bool{}
		for _, ref := range parentArgs.Refs {
			allowed[ref] = true
		}
		for _, ref := range req.Refs {
			if !allowed[ref] {
				c.JSON(400, gin.H{"error": "ref_not_in_parent"})
				return
			}
		}
	}
	accounts := make([]store.Account, 0, len(req.Refs))
	seen := map[string]bool{}
	for _, ref := range req.Refs {
		account, err := s.st.GetAccount(ref)
		if err != nil || account.Platform != req.Platform || seen[ref] {
			c.JSON(400, gin.H{"error": "invalid_check_account", "detail": "账号不存在、重复或平台不匹配"})
			return
		}
		seen[ref] = true
		accounts = append(accounts, account)
	}
	if err := s.st.CreateCredentialCheck(task, accounts); err != nil {
		if s.replayTask(c, task) {
			return
		}
		if err.Error() == "check_queue_full" {
			c.JSON(429, gin.H{"error": "check_queue_full", "detail": "检查队列已满，请稍后再试"})
			return
		}
		c.JSON(503, gin.H{"error": "check_acceptance_unavailable"})
		return
	}
	s.st.Audit(task.Actor, "account.check", task.ID, c.ClientIP())
	c.JSON(http.StatusAccepted, gin.H{"id": task.ID, "task_id": task.ID, "status": "queued"})
}

func (s *Server) listCredentialChecks(c *gin.Context) {
	platform := c.Query("platform")
	action := c.DefaultQuery("action", "check")
	if platform == "" || (action != "check" && action != "refresh") {
		c.JSON(400, gin.H{"error": "invalid_check_scope"})
		return
	}
	page, err := s.st.QueryTasks(store.TaskQuery{CredentialPlatform: platform, CredentialAction: action, Status: c.Query("status"), Limit: 25, Offset: atoi(c.Query("offset"), 0)})
	if err != nil {
		c.JSON(503, gin.H{"error": "check_history_unavailable"})
		return
	}
	ids := make([]string, 0, len(page.Tasks))
	for _, task := range page.Tasks {
		ids = append(ids, task.ID)
	}
	counts, err := s.st.CredentialCheckCounts(ids)
	if err != nil {
		c.JSON(503, gin.H{"error": "check_history_unavailable"})
		return
	}
	c.JSON(200, gin.H{"page": page, "counts": counts})
}

func (s *Server) getCredentialCheck(c *gin.Context) {
	task, err := s.st.GetTask(c.Param("id"))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && task.Script != store.CredentialCheckScript) {
		c.JSON(404, gin.H{"error": "check_not_found"})
		return
	}
	if err != nil {
		c.JSON(503, gin.H{"error": "check_unavailable"})
		return
	}
	items, err := s.st.CredentialCheckItems(task.ID)
	if err != nil {
		c.JSON(503, gin.H{"error": "check_items_unavailable"})
		return
	}
	c.JSON(200, gin.H{"task": task, "items": items})
}

// StartCredentialChecks owns one low-rate worker. Acceptance is durable and
// independent of an HTTP request or a browser's lifetime. No browser slots or
// OAuth refresh tokens are consumed by AT checks.
func (s *Server) StartCredentialChecks(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			task, err := s.st.ClaimCredentialCheck()
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				log.Printf("[checks] cannot claim batch: %v", err)
				continue
			}
			s.executeCredentialCheck(ctx, task, s.checkCredential)
		}
	}()
	return done
}

func (s *Server) executeCredentialCheck(ctx context.Context, task store.Task, check func(context.Context, string, store.CredentialCheckItem) store.CredentialCheckItem) {
	status, message := "failed", "检查未完成；可重测未确认项"
	defer func() {
		if err := s.st.FinishCredentialCheck(task.ID, status, message); err != nil {
			log.Printf("[checks] finish %s failed: %v", task.ID, err)
		}
	}()
	items, err := s.st.CredentialCheckItems(task.ID)
	if err != nil {
		return
	}
	for _, item := range items {
		if ctx.Err() != nil {
			status, message = "interrupted", "服务停止，保留已返回结果；可重测未确认项"
			return
		}
		started, err := s.st.StartCredentialCheckItem(task.ID, item.Ordinal)
		if err != nil {
			return
		}
		if !started {
			status, message = "stopped", "检查已停止，保留已返回结果"
			return
		}
		item = check(ctx, task.ID, item)
		if err = s.st.CompleteCredentialCheckItem(task.ID, item); err != nil {
			return
		}
		if ctx.Err() != nil {
			status, message = "interrupted", "服务停止，保留已返回结果；可重测未确认项"
			return
		}
		if item.Status == "unknown" {
			status, message = "unknown", "本项结果未确认，后续检查已停止；可重测未知与未执行项"
			return
		}
		// Avoid bursts across accounts even when upstream answers immediately.
		timer := time.NewTimer(300 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			status, message = "interrupted", "服务停止，保留已返回结果"
			return
		case <-timer.C:
		}
	}
	status, message = "succeeded", ""
}

func (s *Server) checkCredential(ctx context.Context, batchID string, item store.CredentialCheckItem) store.CredentialCheckItem {
	item.Status, item.Message = "unknown", "凭据不可读；未使用 RT"
	account, err := s.st.GetAccount(item.Ref)
	if err != nil {
		return item
	}
	if account.Platform == "chatgpt" {
		asset, err := s.assets.Get(item.Ref)
		if err != nil || asset.Platform != "chatgpt" {
			return item
		}
		proxy := s.env.Get("REG_FACTORY_PRODUCTION_PROXY")
		if value, exists := settings.DeploymentValue("REG_FACTORY_PRODUCTION_PROXY"); exists {
			proxy = value
		}
		result := probe.OAuthContext(ctx, asset.Payload, proxy, 30*time.Second)
		item.Status, item.Message = result.Status, result.Message
		return item
	}
	// Keep other platforms' existing report lookup. Do not call --refresh-scan:
	// some scanners exchange tokens, so that is a separately invoked operation.
	argv, _ := json.Marshal([]string{"liveness", "--ref", item.Ref, "--platform", account.Platform})
	child := store.Task{ID: fmt.Sprintf("%s-item-%d", batchID, item.Ordinal), Script: "asset_ops", Title: "读取检查报告 · " + item.Identity, ArgsJSON: string(argv), Actor: "panel"}
	item.Mode, item.ChildTaskID = "report", child.ID
	if err := s.run.Submit(child); err != nil {
		item.Message = "报告读取任务未受理"
		return item
	}
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			item.Message = "报告读取未确认，可查看原任务；未刷新凭据"
			return item
		case <-ticker.C:
		}
		current, err := s.st.GetTask(child.ID)
		if err != nil {
			return item
		}
		if current.Status == "queued" || current.Status == "running" {
			continue
		}
		events, err := s.st.TailEvents(child.ID, 40)
		if err != nil {
			return item
		}
		for i := len(events) - 1; i >= 0; i-- {
			var result struct {
				Ref       string `json:"ref"`
				Status    string `json:"status"`
				Message   string `json:"message"`
				CheckedAt string `json:"checked_at"`
			}
			if json.Unmarshal([]byte(events[i].Line), &result) == nil && result.Ref == item.Ref && validCheckStatus(result.Status) {
				item.Status, item.Message, item.CheckedAt = result.Status, "历史报告："+result.Message, result.CheckedAt
				return item
			}
		}
		item.Message = "报告未返回可核对结果，请查看原任务"
		return item
	}
}

func validCheckStatus(status string) bool {
	switch status {
	case "ok", "needs_refresh", "banned", "invalid", "expired", "unknown", "relogin_required":
		return true
	}
	return false
}
