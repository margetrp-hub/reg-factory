package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/probe"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func refreshNeedsResolution(task store.Task) bool {
	return task.Status == "unknown" || ((task.Status == "interrupted" || task.Status == "stopped") && task.StartedAt != "")
}

func (s *Server) getAccountRefresh(c *gin.Context) {
	task, err := s.st.GetTask(c.Param("id"))
	if err != nil || store.AccountRefreshRef(task) == "" {
		c.JSON(404, gin.H{"error": "refresh_not_found"})
		return
	}
	evidence, err := s.st.RefreshEvidence(task.ID)
	if err != nil {
		c.JSON(503, gin.H{"error": "refresh_evidence_unavailable"})
		return
	}
	account, err := s.st.GetAccount(evidence.Ref)
	owned := err != nil || account.Claimed
	pending := refreshNeedsResolution(task)
	verificationID, verificationStatus := "", ""
	if check, err := s.st.GetTask(store.RefreshVerificationID(task.ID)); err == nil {
		verificationID, verificationStatus = check.ID, check.Status
	}
	method := "refresh"
	var args []string
	_ = json.Unmarshal([]byte(task.ArgsJSON), &args)
	for _, arg := range args {
		if arg == "--allow-relogin" {
			method = "authorize"
		}
	}
	var progress *store.StageEvent
	stages, err := s.st.CredentialStages(task.ID)
	if err != nil {
		c.JSON(503, gin.H{"error": "refresh_progress_unavailable"})
		return
	}
	for _, stage := range stages {
		if stage.Stage == "credential_authorize" {
			method = "authorize"
		}
		progress = &stage
	}
	c.JSON(200, gin.H{"task": task, "evidence": evidence, "can_resolve": pending && !owned,
		"method": method, "progress": progress,
		"library_status": account.LibraryStatus, "verification_task_id": verificationID, "verification_status": verificationStatus,
		"can_reconcile": pending && !owned && account.Platform == "chatgpt" && evidence.SourceSHA != "",
		"can_retry":     !owned && (task.Status == "failed" || ((task.Status == "stopped" || task.Status == "interrupted") && task.StartedAt == ""))})
}

func (s *Server) reconcileAccountRefresh(c *gin.Context) {
	task, err := s.st.GetTask(c.Param("id"))
	if err != nil || store.AccountRefreshRef(task) == "" {
		c.JSON(404, gin.H{"error": "refresh_not_found"})
		return
	}
	evidence, err := s.st.RefreshEvidence(task.ID)
	if err != nil {
		c.JSON(503, gin.H{"error": "refresh_evidence_unavailable"})
		return
	}
	if task.Status == "succeeded" && evidence.Resolution != "" {
		s.getAccountRefresh(c)
		return
	}
	account, err := s.st.GetAccount(evidence.Ref)
	if err != nil || account.Claimed {
		c.JSON(409, gin.H{"error": "account_owned_by_core", "detail": "已领取账号在主系统原任务处理"})
		return
	}
	if !refreshNeedsResolution(task) || account.Platform != "chatgpt" {
		c.JSON(409, gin.H{"error": "refresh_not_awaiting_resolution"})
		return
	}
	current, credentials, err := s.assets.RefreshedOAuth(evidence.Ref, evidence.SourceSHA)
	if err != nil {
		c.JSON(409, gin.H{"error": "refresh_not_confirmed", "detail": err.Error()})
		return
	}
	proxy := s.env.Get("REG_FACTORY_PRODUCTION_PROXY")
	if value, exists := settings.DeploymentValue("REG_FACTORY_PRODUCTION_PROXY"); exists {
		proxy = value
	}
	result := probe.OAuthContext(c.Request.Context(), credentials, proxy, 30*time.Second)
	if result.Status != "ok" {
		c.JSON(409, gin.H{"error": "refresh_not_confirmed", "detail": result.Message + "；保留原任务，未再次交换 RT"})
		return
	}
	// Do not settle a different version if an operator replaced it during the probe.
	selected, err := s.assets.Get(evidence.Ref, account.Identity)
	if err != nil || selected.ResultSHA != current.ResultSHA {
		c.JSON(409, gin.H{"error": "credential_changed", "detail": "核对期间凭据版本变化，请重新核对"})
		return
	}
	actor, _ := c.Get("actor")
	if err = s.st.ResolveAccountRefresh(task.ID, toString(actor), "", "succeeded", current.ResultSHA); err != nil {
		c.JSON(409, gin.H{"error": err.Error()})
		return
	}
	_ = s.assets.ReleaseSnapshot(evidence.SourceSHA)
	s.getAccountRefresh(c)
}

func (s *Server) resolveAccountRefresh(c *gin.Context) {
	var req struct {
		Reason string `json:"reason"`
	}
	if c.ShouldBindJSON(&req) != nil || len(strings.TrimSpace(req.Reason)) < 4 || len(req.Reason) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "resolution_reason_required", "detail": "请填写核对证据及确认未完成的原因（4–1000 字节），不要填写凭据"})
		return
	}
	actor, _ := c.Get("actor")
	if err := s.st.ResolveAccountRefresh(c.Param("id"), toString(actor), req.Reason, "failed", ""); err != nil {
		c.JSON(409, gin.H{"error": err.Error(), "detail": "状态已变化或该账号由主系统管理，请更新记录"})
		return
	}
	if evidence, err := s.st.RefreshEvidence(c.Param("id")); err == nil && s.assets != nil {
		_ = s.assets.ReleaseSnapshot(evidence.SourceSHA)
	}
	s.getAccountRefresh(c)
}
