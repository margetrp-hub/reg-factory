package runner

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"aishop-next/execution-go/nodeassets"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// Verify the interpreter, exported SDK and actual durable journal, without
// touching any account or upstream. Cache only a short readiness observation.
func (m *Manager) CentralRuntimeCheck() error {
	m.centralCheckMu.Lock()
	defer m.centralCheckMu.Unlock()
	if !m.centralCheckedAt.IsZero() && time.Since(m.centralCheckedAt) < 30*time.Second {
		return m.centralCheckErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.cfg.Python, filepath.Join(m.cfg.AppRoot, "tools", "central_asset.py"), "--check-config")
	cmd.Dir = m.cfg.AppRoot
	raw, err := cmd.Output()
	var result map[string]any
	if err != nil || json.Unmarshal(raw, &result) != nil || result["configured"] != true {
		m.centralCheckErr = errors.New("ASSET_RUNTIME_UNAVAILABLE")
	} else {
		m.centralCheckErr = nil
	}
	m.centralCheckedAt = time.Now()
	return m.centralCheckErr
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
		argv, cwd, env, e := m.buildArgv(task)
		if e != nil {
			return e
		}
		raw, _ := json.Marshal(receipt)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], "--ack", string(raw))...)
		cmd.Dir = cwd
		cmd.Env = env
		if e = cmd.Run(); e != nil {
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
	if !m.beginCentral(task.ID) {
		return nil
	}
	defer m.endCentral(task.ID)
	if p, e := m.st.CentralReceipt(task.ID); e == nil && (p["state"] == "completed" || p["state"] == "failed" || p["state"] == "closed") {
		return m.settleCentral(task, p)
	}
	argv, cwd, env, err := m.buildArgv(task)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	started := "started"
	if task.StartedAt == "" {
		started = "unstarted"
	}
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], "--replay", "--interrupted", started)...)
	cmd.Dir = cwd
	cmd.Env = env
	raw, err := cmd.Output()
	if err != nil {
		return errors.New("ASSET_RESULT_DELIVERY_PENDING")
	}
	var value map[string]any
	if len(raw) > 16384 || json.Unmarshal(raw, &value) != nil {
		return errors.New("ASSET_RESULT_DELIVERY_PENDING")
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
