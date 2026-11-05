package nodeserver

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// nodeTicket 是节点侧任务票据：用 operation_id（每次 attempt 唯一）。
func nodeTicket(verified *Verified) string {
	if v, _ := verified.Payload["operation_id"].(string); v != "" {
		return v
	}
	id, _ := verified.Payload["task_id"].(string)
	return id
}

// writeAccepted 按 Work 的 Submit 契约回应。
//
// Work 校验 id 是 UUID、origin=="work"，且 action/subject_id/subject_kind/
// resource_version 与下发时逐字段一致；只回 {accepted,...} 会被判
// WORK_NODE_ACK_INVALID，任务永远停在 submitting。
func (h *Handler) writeAccepted(c *gin.Context, verified *Verified) {
	c.JSON(http.StatusOK, gin.H{
		"id":               nodeTicket(verified),
		"origin":           "work",
		"action":           verified.Action,
		"subject_id":       verified.Payload["subject_id"],
		"subject_kind":     verified.Payload["subject_kind"],
		"resource_version": verified.Payload["resource_version"],
	})
}

// taskByTicket 用票据反查本地任务。
func (h *Handler) taskByTicket(ticket string) (store.Task, error) {
	if local, err := h.st.OperationTask(ticket); err == nil && local != "" {
		if task, err := h.st.GetTask(local); err == nil {
			return task, nil
		}
	}
	if task, err := h.st.GetTask(ticket); err == nil {
		return task, nil
	}
	return h.st.GetTask("work-" + ticket)
}

// workArtifact 交付这次执行产生的凭据工件。
//
// Work 拿到回执后按 result_sha256 来这里取材料：密文里既有 scope 字段也有
// 完整 result（含 document），Core 解密后逐字段比对回执，所以两处的
// result_sha256 必须由同一份完整结果算出 —— 包括 exitCode。
func (h *Handler) workArtifact(c *gin.Context) {
	ticket := c.Param("task_id")
	var doc Document
	if err := c.ShouldBindJSON(&doc); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WORK_REQUEST_INVALID"})
		return
	}
	verified, err := VerifyRead(doc, h.cfg.Key, h.cfg.NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	if nodeTicket(verified) != ticket {
		writeErr(c, perr("WORK_RESULT_SCOPE_MISMATCH", http.StatusForbidden))
		return
	}
	task, err := h.taskByTicket(ticket)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "NOT_FOUND", "task_id": ticket})
		return
	}
	if task.Status != "succeeded" && task.Status != "completed" {
		c.JSON(http.StatusConflict, gin.H{"error": "WORK_ARTIFACT_UNAVAILABLE", "status": task.Status})
		return
	}
	full := h.fullResult(verified, "completed", task.ExitCode)
	if full["document"] == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "WORK_ARTIFACT_UNAVAILABLE"})
		return
	}
	rawFull, err := Canonical(full)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WORK_REQUEST_INVALID"})
		return
	}
	sum := sha256.Sum256(rawFull)
	payload := map[string]any{
		"node_id":          verified.Payload["node_id"],
		"issuer":           verified.Payload["issuer"],
		"operation_id":     verified.Payload["operation_id"],
		"task_id":          verified.Payload["task_id"],
		"attempt_id":       verified.Payload["attempt_id"],
		"fence":            verified.Payload["fence"],
		"subject_id":       verified.Payload["subject_id"],
		"resource_version": verified.Payload["resource_version"],
		"command_sha256":   verified.Payload["command_sha256"],
		"schema":           "account-node.team-artifact.v1",
		"node_task_id":     ticket,
		"result_sha256":    hex.EncodeToString(sum[:]),
		"result":           full,
	}
	raw, err := Canonical(payload)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WORK_REQUEST_INVALID"})
		return
	}
	cipher, err := fernetEncrypt(h.cfg.ArtifactKey, raw)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "TEAM_ARTIFACT_KEY_UNCONFIGURED"})
		return
	}
	log.Printf("[node] artifact delivered ticket=%s", ticket)
	c.JSON(http.StatusOK, gin.H{
		"schema":       "account-node.encrypted-artifact.v1",
		"node_task_id": ticket,
		"ciphertext":   cipher,
	})
}
