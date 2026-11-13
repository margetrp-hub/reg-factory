package runner

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
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
	case result == "refreshed":
		if account.Platform == "chatgpt" {
			current, err := assetstore.New(m.cfg.DataDir).ConfirmCredentialTask(task.ID, ref)
			if err != nil {
				status, code, message = "failed", 75, "凭据保存未确认，本次失败；可重新发起授权修复"
				failure, _ := json.Marshal(map[string]any{"ref": ref, "result": "failed", "error_schema": "account.failure.v2", "error_code": "credential_save_failed"})
				m.record(task.ID, "stdout", string(failure))
			} else {
				digest = current.ResultSHA
			}
		}
	case status == "failed" && (result == "failed" || result == "relogin_required" || result == "banned"):
		if message == "" {
			message = "上游明确拒绝或刷新前失败；查看原执行反馈后处理"
		}
	default:
		status, code, message = "failed", 75, "未取得可确认的成功结果，本次失败；可重新发起授权修复"
		failure, _ := json.Marshal(map[string]any{"ref": ref, "result": "failed", "error_schema": "account.failure.v2", "error_code": "credential_result_incomplete"})
		m.record(task.ID, "stdout", string(failure))
	}
	err = m.st.FinishAccountRefresh(task.ID, ref, status, code, message, digest)
	if err == nil && (status == "succeeded" || status == "failed") && evidence.SourceSHA != "" {
		if cleanupErr := assetstore.New(m.cfg.DataDir).ReleaseSnapshot(evidence.SourceSHA); cleanupErr != nil {
			m.record(task.ID, "system", "任务已结案，临时凭据清理失败；可在运维清理中重试")
		}
	}
	if err == nil && status == "succeeded" {
		if cleanupErr := assetstore.New(m.cfg.DataDir).AcknowledgeCredentialTask(task.ID); cleanupErr != nil {
			m.record(task.ID, "system", "凭据和任务已保存，临时提交材料等待运维清理")
		}
	}
	return status, code, message, err
}

func (m *Manager) recoverPublication(task store.Task) bool {
	ref := store.CredentialTaskRef(task)
	if ref == "" {
		return false
	}
	_, err := assetstore.New(m.cfg.DataDir).RecoverCredentialTask(task.ID, ref)
	if err != nil && !os.IsNotExist(err) {
		m.record(task.ID, "system", "本地凭据收尾未完成: "+err.Error())
	}
	return err == nil
}

func isChatGPTCredential(task store.Task) bool {
	if store.CredentialTaskRef(task) == "" {
		return false
	}
	var args []string
	if json.Unmarshal([]byte(task.ArgsJSON), &args) != nil {
		return false
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--platform" {
			return args[i+1] == "chatgpt"
		}
	}
	return false
}

// RecoverCredentials runs only before admitting work. Local publication recovery
// has no network/browser path and preserves the task ID and original receipt.
func RecoverCredentials(cfg *settings.Settings, st *store.Store) error {
	m := &Manager{cfg: cfg, st: st}
	tasks, err := st.UnfinishedCredentials()
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if !isChatGPTCredential(task) {
			continue
		}
		if !m.recoverPublication(task) {
			continue
		}
		if store.AccountRefreshRef(task) != "" {
			if _, _, _, err = m.finishRefresh(task, "refreshed", "succeeded", 0, "原保存结果已恢复"); err != nil {
				return err
			}
		} else {
			if _, err = assetstore.New(cfg.DataDir).ConfirmCredentialTask(task.ID, store.CredentialTaskRef(task)); err != nil {
				continue
			}
			if err = st.Finish(task.ID, "succeeded", 0, "原保存结果已恢复"); err != nil {
				return err
			}
			_ = assetstore.New(cfg.DataDir).AcknowledgeCredentialTask(task.ID)
		}
		_ = st.Stage(task.ID, store.StageEvent{Stage: "credential_recovery", Status: "succeeded", Action: "服务重启后恢复原结果，未重复请求上游"})
	}
	return nil
}

// Database outages delay only settlement, never repeat the external operation.
func (m *Manager) settleCredential(task store.Task, result, status string, code int, message string) (string, int, string, error) {
	if m.recoverPublication(task) {
		result, status, code, message = "refreshed", "succeeded", 0, "最新凭据已确认保存"
	}
	_ = m.st.Stage(task.ID, store.StageEvent{Stage: "credential_settlement", Status: "running", Action: "正在保存任务结果；异常时自动重试本地收尾"})
	for attempt := 0; ; attempt++ {
		var err error
		if store.AccountRefreshRef(task) != "" {
			status, code, message, err = m.finishRefresh(task, result, status, code, message)
		} else {
			if _, checkErr := assetstore.New(m.cfg.DataDir).ConfirmCredentialTask(task.ID, store.CredentialTaskRef(task)); checkErr != nil {
				if status != "failed" || result == "" {
					status, code, message = "failed", 75, "本次凭据保存未确认，可重新发起授权修复"
					failure, _ := json.Marshal(map[string]any{"ref": store.CredentialTaskRef(task), "result": "failed", "error_schema": "account.failure.v2", "error_code": "credential_save_failed"})
					m.record(task.ID, "stdout", string(failure))
				}
			} else {
				status, code, message = "succeeded", 0, "最新凭据已确认保存"
			}
			err = m.st.Finish(task.ID, status, code, message)
			if err == nil && status == "succeeded" {
				_ = assetstore.New(m.cfg.DataDir).AcknowledgeCredentialTask(task.ID)
			}
		}
		if err == nil {
			_ = m.st.Stage(task.ID, store.StageEvent{Stage: "credential_settlement", Status: status, Action: message})
			return status, code, message, nil
		}
		if attempt == 0 {
			m.record(task.ID, "system", "任务结果保存暂时失败，自动重试收尾；不重复执行上游操作")
		}
		_ = m.st.Stage(task.ID, store.StageEvent{Stage: "credential_settlement", Status: "running", Attempt: attempt + 1, Code: "CREDENTIAL_SETTLEMENT_RETRY", Action: "自动重试保存本次结果；无需重新发起"})
		delay := time.Duration(min(attempt+1, 15)) * time.Second
		select {
		case <-m.stop:
			return status, code, message, errors.New("credential_settlement_interrupted")
		case <-time.After(delay):
		}
	}
}
