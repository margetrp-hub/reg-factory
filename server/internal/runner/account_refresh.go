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
				status, code, message = "unknown", 75, err.Error()
			} else {
				digest = current.ResultSHA
			}
		}
	case status == "failed" && (result == "failed" || result == "relogin_required" || result == "banned"):
		message = "上游明确拒绝或刷新前失败；查看原执行反馈后处理"
	default:
		status, code, message = "unknown", 75, "执行已结束但刷新结果未确认；请核对已保存凭据，或根据证据人工结案"
	}
	err = m.st.FinishAccountRefresh(task.ID, ref, status, code, message, digest)
	return status, code, message, err
}
