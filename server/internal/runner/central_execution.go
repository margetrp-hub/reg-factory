package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/probe"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func (m *Manager) launchCentral(task store.Task, weight int) error {
	op, err := m.centralOperation(task)
	if err != nil {
		return err
	}
	env, err := settings.WorkerEnvironment(m.cfg.EnvFile)
	if err != nil {
		return err
	}
	env = append(env, "REG_FACTORY_DATA_DIR="+m.cfg.DataDir, "REG_FACTORY_RUN_ID="+task.ID,
		"REG_FACTORY_ICMAIL_ADDR="+m.cfg.Addr, "REG_FACTORY_ICMAIL_KEY="+m.cfg.PanelPass, "PYTHONIOENCODING=utf-8", "PYTHONUNBUFFERED=1")
	timeout := m.taskTimeout()
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	m.launchMu.Lock()
	select {
	case <-m.stop:
		m.launchMu.Unlock()
		cancel()
		return errors.New("服务正在停止，未启动任务")
	default:
	}
	current, err := m.st.GetTask(task.ID)
	if err == nil && current.Status != "queued" {
		err = errors.New("任务已停止，未启动")
	}
	if err == nil {
		err = m.st.MarkRunning(task.ID, 0)
	}
	if err != nil {
		m.launchMu.Unlock()
		cancel()
		return err
	}
	weight = max(1, weight)
	m.mu.Lock()
	m.procs[task.ID] = &proc{taskID: task.ID, cancel: cancel}
	m.weight += weight
	m.workers.Add(1)
	m.mu.Unlock()
	m.launchMu.Unlock()
	m.record(task.ID, "system", "Go 执行宿主已启动；领取、原结果保存及回库由本任务统一处理")
	go func() {
		defer m.workers.Done()
		defer cancel()
		defer func() {
			m.mu.Lock()
			delete(m.procs, task.ID)
			m.weight -= weight
			m.mu.Unlock()
			m.Wake()
		}()
		value, err := op.Run(ctx, func(ctx context.Context, material map[string]any) (map[string]any, error) {
			return m.executeCentral(ctx, task, env, material)
		})
		if err == nil {
			err = m.finishCentral(task, value)
		}
		if err != nil {
			m.record(task.ID, "system", "中央结果等待原任务补传；不会再次执行平台操作")
			m.record(task.ID, "system", "收尾证据: "+err.Error())
			if current, e := m.st.GetTask(task.ID); e == nil && current.Status == "running" {
				_ = m.st.Finish(task.ID, "waiting_asset", 75, "ASSET_RESULT_DELIVERY_PENDING")
			}
			m.st.DeferCentral(task.ID)
		}
	}()
	return nil
}

type boundedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1024*1024 {
		b.overflow = true
		return len(p), nil
	}
	if b.overflow {
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

func (m *Manager) executeCentral(ctx context.Context, task store.Task, env []string, material map[string]any) (map[string]any, error) {
	scope, err := CentralContext(task)
	if err != nil {
		return nil, err
	}
	if scope["action"] == "asset.liveness" {
		document, _ := material["document"].(map[string]any)
		creds, credentialErr := assetstore.OAuthDocument(document, "")
		if errors.Is(credentialErr, assetstore.ErrAmbiguous) {
			return map[string]any{"status": "failed", "document": nil, "error_code": "ASSET_DOCUMENT_AMBIGUOUS"}, nil
		}
		proxy := ""
		for _, entry := range env {
			if k, v, _ := strings.Cut(entry, "="); k == "REG_FACTORY_PRODUCTION_PROXY" {
				proxy = v
			}
		}
		// Never pass recovery material or RT to a liveness request.
		observed := probe.OAuthContext(ctx, map[string]any{"access_token": creds["access_token"], "account_id": creds["account_id"]}, proxy, 15*time.Second)
		state := map[string]string{"ok": "alive", "needs_refresh": "needs_refresh", "banned": "banned", "workspace_deactivated": "workspace_deactivated", "unknown": "unknown"}[observed.Status]
		status, code := "completed", ""
		if state == "unknown" || state == "" {
			status, code, state = "failed", "AT_CHECK_UNAVAILABLE", "unknown"
		}
		return map[string]any{"status": status, "document": nil, "error_code": code, "liveness": state, "source_status": state, "checked_at": time.Now().UTC().Format(time.RFC3339Nano)}, nil
	}
	raw, err := json.Marshal(map[string]any{"context": scope, "material": material})
	if err != nil || len(raw) > 1024*1024 {
		return nil, errors.New("ASSET_EXECUTOR_INPUT_INVALID")
	}
	cmd := exec.CommandContext(ctx, m.cfg.Python, "-u", filepath.Join(m.cfg.AppRoot, "tools", "central_asset.py"))
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.SysProcAttr = m.cfg.AppRoot, env, bytes.NewReader(raw), procAttr()
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid, 5*time.Second) }
	cmd.WaitDelay = 6 * time.Second
	var output boundedOutput
	cmd.Stdout = &output
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.New("ASSET_EXECUTOR_UNAVAILABLE")
	}
	if err = cmd.Start(); err != nil {
		return map[string]any{"status": "failed", "document": nil, "error_code": "ASSET_EXECUTOR_UNAVAILABLE"}, nil
	}
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 4096), 512*1024)
	for scanner.Scan() {
		m.record(task.ID, "stderr", scanner.Text())
	}
	if scanner.Err() != nil {
		_ = killGroup(cmd.Process.Pid, 5*time.Second)
	}
	err = cmd.Wait()
	var result map[string]any
	if output.overflow || json.Unmarshal(output.Bytes(), &result) != nil {
		return nil, errors.New("ASSET_EXECUTION_UNKNOWN")
	}
	if err != nil {
		return result, errors.New("ASSET_EXECUTION_UNKNOWN")
	}
	return result, nil
}
