package runner

import (
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func (m *Manager) finishRefresh(task store.Task, result, status string, code int, message string) (string, int, string, error) {
	ref := store.AccountRefreshRef(task)
	evidence, err := m.st.RefreshEvidence(task.ID)
	if err != nil {
		return status, code, message, err
	}
	account, err := m.st.GetAccount(ref)
	if err != nil {
		return status, code, message, err
	}
	digest := ""
	switch {
	case status == "succeeded" && result == "refreshed":
		if account.Platform == "chatgpt" {
			current, _, err := assetstore.New(m.cfg.DataDir).RefreshedOAuth(ref, evidence.SourceSHA)
			if err != nil {
				status, code, message = "failed", 75, "凭据保存未确认，本次失败；可重新发起授权修复"
			} else {
				digest = current.ResultSHA
			}
		}
	case status == "failed" && (result == "failed" || result == "relogin_required" || result == "banned"):
		message = "上游明确拒绝或刷新前失败；查看原执行反馈后处理"
	default:
		status, code, message = "failed", 75, "未取得可确认的成功结果，本次失败；可重新发起授权修复"
	}
	err = m.st.FinishAccountRefresh(task.ID, ref, status, code, message, digest)
	if err == nil && (status == "succeeded" || status == "failed") && evidence.SourceSHA != "" {
		if cleanupErr := assetstore.New(m.cfg.DataDir).ReleaseSnapshot(evidence.SourceSHA); cleanupErr != nil {
			m.record(task.ID, "system", "任务已结案，临时凭据清理失败；可在运维清理中重试")
		}
	}
	return status, code, message, err
}
