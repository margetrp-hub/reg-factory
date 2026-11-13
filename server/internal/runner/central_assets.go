package runner

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"aishop-next/execution-go/nodeassets"
	"github.com/tiantianGPU/reg-factory/server/internal/centralasset"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// The Go host validates its configuration and actual durable storage without
// launching Python or touching an account/upstream.
func (m *Manager) CentralRuntimeCheck() error {
	m.centralCheckMu.Lock()
	defer m.centralCheckMu.Unlock()
	env, err := settings.WorkerEnvironment(m.cfg.EnvFile)
	if err != nil {
		return err
	}
	cfg, err := centralasset.FromEnvironment(m.cfg.DataDir, env)
	if err == nil {
		err = cfg.VerifyWritable()
	}
	m.centralCheckErr = err
	m.centralCheckedAt = time.Now()
	return m.centralCheckErr
}

func (m *Manager) centralOperation(task store.Task) (*centralasset.Operation, error) {
	p, err := CentralContext(task)
	if err != nil {
		return nil, err
	}
	env, err := settings.WorkerEnvironment(m.cfg.EnvFile)
	if err != nil {
		return nil, err
	}
	// New resolves an original admitted operation against retained bindings even
	// when the current connection has been cleared or switched to another center.
	cfg, _ := centralasset.FromEnvironment(m.cfg.DataDir, env)
	op, err := centralasset.New(cfg, p)
	if err != nil {
		return nil, err
	}
	op.Restore = func(ctx context.Context, material map[string]any) (map[string]any, error) {
		m.centralMu.Lock()
		restore := m.assetRestorer
		m.centralMu.Unlock()
		if restore == nil {
			return nil, errors.New("ASSET_RESTORE_RUNTIME_UNAVAILABLE")
		}
		return restore(ctx, p, material)
	}
	op.Progress = func(stage string) {
		actions := map[string]string{"asset_claim": "领取原授权中央资料", "asset_read": "读取指定凭据版本", "asset_execution": "执行平台操作", "asset_result_save": "密封保存原执行结果", "asset_writeback": "提交原结果并核对中央回执"}
		_ = m.st.Stage(task.ID, store.StageEvent{Stage: stage, Status: "running", Action: actions[stage]})
	}
	return op, nil
}

func (m *Manager) SetAssetRestorer(restore func(context.Context, map[string]any, map[string]any) (map[string]any, error)) {
	m.centralMu.Lock()
	m.assetRestorer = restore
	m.centralMu.Unlock()
}

func CentralContext(task store.Task) (map[string]any, error) {
	var argv []string
	if task.Script != "central_asset" || json.Unmarshal([]byte(task.ArgsJSON), &argv) != nil || len(argv) != 2 || argv[0] != "--assignment" {
		return nil, errors.New("ASSET_TASK_SCOPE_INVALID")
	}
	var scope map[string]any
	if json.Unmarshal([]byte(argv[1]), &scope) != nil {
		return nil, errors.New("ASSET_TASK_SCOPE_INVALID")
	}
	for _, name := range []string{"attempt_id", "subject_id", "node_id", "operation_id", "task_id", "command_sha256"} {
		if s, ok := scope[name].(string); !ok || s == "" {
			return nil, errors.New("ASSET_TASK_SCOPE_INVALID")
		}
	}
	revision, ok := scope["resource_version"].(float64)
	params, paramsOK := scope["parameters"].(map[string]any)
	ref, refOK := params["asset_ref"].(map[string]any)
	center, centerOK := ref["center_id"].(string)
	if !ok || revision < 1 || revision > 2147483647 || math.Trunc(revision) != revision || !paramsOK || !refOK || !centerOK || center == "" {
		return nil, errors.New("ASSET_TASK_SCOPE_INVALID")
	}
	return scope, nil
}

func (m *Manager) settleCentral(task store.Task, value map[string]any) error {
	p, err := CentralContext(task)
	if err != nil {
		return err
	}
	state, _ := value["state"].(string)
	if state != "pending" && state != "completed" && state != "failed" && state != "closed" && state != "unknown" {
		return errors.New("ASSET_ACK_SCOPE_INVALID")
	}
	resultID, okID := value["result_id"].(string)
	outcome, okOutcome := value["outcome"].(string)
	if !okID || !okOutcome {
		return errors.New("ASSET_ACK_SCOPE_INVALID")
	}
	if value["task_id"] != p["attempt_id"] || value["account_id"] != p["subject_id"] {
		return errors.New("ASSET_ACK_SCOPE_INVALID")
	}
	receipt, _ := value["receipt"].(map[string]any)
	if state != "pending" {
		if !(state == "failed" && value["preflight"] == true && receipt["state"] == "absent" && receipt["task_id"] == p["attempt_id"]) {
			expected := nodeassets.ResultExpectation{TaskID: p["attempt_id"].(string), AccountID: p["subject_id"].(string),
				ResultID: resultID, Outcome: outcome, ExpectedRevision: int64(p["resource_version"].(float64)),
				Refresh: p["action"] == "account.refresh" || p["action"] == "account.authorize"}
			observed, e := nodeassets.ValidateResultReceipt(receipt, expected)
			if e != nil || observed != state {
				return errors.New("ASSET_ACK_SCOPE_INVALID")
			}
		}
	}
	if err = m.st.SaveCentralReceipt(task.ID, state, value); err != nil {
		return err
	}
	_ = m.st.Stage(task.ID, store.StageEvent{Stage: "asset_writeback", Status: state, Action: "中央资料回库与原任务结果确认"})
	if state == "completed" || state == "failed" || state == "closed" {
		op, e := m.centralOperation(task)
		if e != nil {
			return e
		}
		if e = op.Acknowledge(receipt); e != nil {
			return errors.New("ASSET_JOURNAL_CLEANUP_PENDING")
		}
		return m.st.AcknowledgeCentral(task.ID)
	}
	return nil
}

// Only guard the in-flight task set; no network or process wait holds this lock.
func (m *Manager) beginCentral(id string) bool {
	m.centralMu.Lock()
	defer m.centralMu.Unlock()
	if m.centralActive[id] || len(m.centralActive) >= 3 {
		return false
	}
	if m.centralActive == nil {
		m.centralActive = map[string]bool{}
	}
	m.centralActive[id] = true
	return true
}

func (m *Manager) endCentral(id string) {
	m.centralMu.Lock()
	delete(m.centralActive, id)
	m.centralMu.Unlock()
}

func (m *Manager) finishCentral(task store.Task, value map[string]any) error {
	if !m.beginCentral(task.ID) {
		return errors.New("ASSET_RESULT_DELIVERY_PENDING")
	}
	defer m.endCentral(task.ID)
	return m.settleCentral(task, value)
}

func (m *Manager) ReplayCentral(task store.Task) error {
	// Callers may hold an old stopped/interrupted snapshot. Check the current
	// scheduler fact before creating recovery evidence for an absent result.
	current, err := m.st.GetTask(task.ID)
	if err != nil {
		return err
	}
	if current.Status == "queued" || current.Status == "running" || current.Status == "stopping" {
		return errors.New("ASSET_EXECUTION_STILL_ACTIVE")
	}
	task = current
	if !m.beginCentral(task.ID) {
		return nil
	}
	defer m.endCentral(task.ID)
	if p, e := m.st.CentralReceipt(task.ID); e == nil && (p["state"] == "completed" || p["state"] == "failed" || p["state"] == "closed") {
		return m.settleCentral(task, p)
	}
	op, err := m.centralOperation(task)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	value, err := op.Replay(ctx, task.StartedAt != "")
	if err != nil {
		return err
	}
	return m.settleCentral(task, value)
}

func (m *Manager) recoverCentral() {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-tick.C:
		}
		tasks, err := m.st.PendingCentral()
		if err != nil {
			continue
		}
		var deliveries sync.WaitGroup
		for _, task := range tasks {
			select {
			case <-m.stop:
				return
			default:
			}
			deliveries.Add(1)
			go func(task store.Task) {
				defer deliveries.Done()
				if err := m.ReplayCentral(task); err != nil {
					m.st.DeferCentral(task.ID)
				}
			}(task)
		}
		deliveries.Wait()
	}
}
