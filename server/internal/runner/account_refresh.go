package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
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
	case status == "succeeded" && result == "refreshed":
		if account.Platform == "chatgpt" {
			current, err := assetstore.New(m.cfg.DataDir).ConfirmCredentialTask(task.ID, ref)
			if err != nil {
				status, code, message = "failed", 75, "凭据保存未确认，本次失败；可重新发起授权修复"
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
	// A complete receipt/current pair is already durable. Do not make a
	// confirmed result depend on starting another Python process.
	if _, err := assetstore.New(m.cfg.DataDir).ConfirmCredentialTask(task.ID, ref); err == nil {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.cfg.Python, filepath.Join(m.cfg.AppRoot, "tools", "asset_ops.py"), "recover-publication", "--ref", ref)
	cmd.Dir = m.cfg.AppRoot
	env, err := settings.WorkerEnvironment(m.cfg.EnvFile)
	if err != nil {
		return false
	}
	cmd.Env = append(env, "REG_FACTORY_DATA_DIR="+m.cfg.DataDir, "REG_FACTORY_RUN_ID="+task.ID, "PYTHONIOENCODING=utf-8")
	raw, err := cmd.Output()
	var result struct {
		Ref    string `json:"ref"`
		Result string `json:"result"`
	}
	return err == nil && json.Unmarshal(raw, &result) == nil && result.Ref == ref && result.Result == "refreshed"
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
