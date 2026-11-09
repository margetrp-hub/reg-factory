// Package runner 负责任务队列、进程执行、日志广播与内存闸门。
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// ErrBusy 表示内存水位过高，暂不启动新任务。
var ErrBusy = errors.New("memory_guard")

// MemInfo 是 cgroup 内存快照。
type MemInfo struct {
	CurrentMB int64   `json:"current_mb"`
	MaxMB     int64   `json:"max_mb"`
	UsedPct   float64 `json:"used_pct"`
}

// Manager 管理所有 worker 进程。
type Manager struct {
	cfg *settings.Settings
	st  *store.Store

	mu    sync.Mutex
	procs map[string]*proc
	slots int
	// weight 是当前占用的“浏览器槽位”总数。任务可用 --concurrency 一次占多槽，
	// 避免“全局 6 并发 × 任务内 3 并发”互相超卖把内存打爆。
	weight int
	wake   chan struct{}
	stop   chan struct{}

	cpuMu   sync.Mutex
	lastCPU cpuSample

	startedAt time.Time
}

type cpuSample struct {
	at    time.Time
	usage uint64
}

type proc struct {
	cmd      *exec.Cmd
	taskID   string
	stopping bool
}

// New 构造管理器并启动调度循环。
func New(cfg *settings.Settings, st *store.Store) *Manager {
	m := &Manager{
		cfg:       cfg,
		st:        st,
		procs:     map[string]*proc{},
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		startedAt: time.Now(),
	}
	go m.dispatch()
	return m
}

// Uptime 返回运行时长。
func (m *Manager) Uptime() time.Duration { return time.Since(m.startedAt) }

// Wake 触发一次调度。
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// RunningCount 返回在跑任务数。
func (m *Manager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.procs)
}

// Capacity 返回并发上限。
func (m *Manager) Capacity() int { return m.cfg.MaxParallel }

// Memory 读取 cgroup 水位。
func (m *Manager) Memory() MemInfo {
	read := func(name string) int64 {
		raw, err := os.ReadFile(filepath.Join(m.cfg.CgroupRoot, name))
		if err != nil {
			return 0
		}
		value := strings.TrimSpace(string(raw))
		if value == "max" {
			return 0
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	current := read("memory.current")
	maximum := read("memory.max")
	info := MemInfo{CurrentMB: current / (1 << 20), MaxMB: maximum / (1 << 20)}
	if maximum > 0 {
		info.UsedPct = round1(float64(current) * 100 / float64(maximum))
	}
	return info
}

// Guarded 判断当前是否因内存拒绝新任务。
func (m *Manager) Guarded() bool {
	info := m.Memory()
	return info.MaxMB > 0 && info.UsedPct >= m.cfg.GuardPct
}

// ---------------------------------------------------------------- 订阅

func (m *Manager) record(taskID, stream, line string) {
	if err := m.st.AppendEvent(taskID, stream, line); err != nil {
		log.Printf("[runner] task=%s log persistence failed: %v", taskID, err)
	}
}

// ---------------------------------------------------------------- 调度

func (m *Manager) dispatch() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-m.wake:
		case <-ticker.C:
		}
		if m.Guarded() {
			continue
		}
		for {
			m.mu.Lock()
			used := m.weight
			m.mu.Unlock()
			if used >= m.cfg.MaxParallel {
				break
			}
			next, err := m.nextQueued()
			if err != nil || next == nil {
				break
			}
			w := m.taskWeight(*next)
			if w > m.cfg.MaxParallel-used {
				// 剩余槽位不够装这个任务：留给后面更小的任务，避免队头阻塞过久
				if w > 1 && used > 0 {
					break
				}
				w = m.cfg.MaxParallel - used
				if w < 1 {
					break
				}
			}
			if err := m.launch(*next, w); err != nil {
				m.record(next.ID, "system", "启动失败: "+err.Error())
				if ref := store.AccountRefreshRef(*next); ref != "" {
					_ = m.st.FinishAccountRefresh(next.ID, ref, "failed", -1, err.Error(), "")
				} else {
					_ = m.st.Finish(next.ID, "failed", -1, err.Error())
				}
				continue
			}
		}
	}
}

// taskWeight 从任务参数里读 --concurrency，作为占用的槽位数。
func (m *Manager) taskWeight(t store.Task) int {
	w := 1
	if json.Valid([]byte(t.ArgsJSON)) {
		var args map[string]any
		if err := json.Unmarshal([]byte(t.ArgsJSON), &args); err == nil {
			for _, key := range []string{"--concurrency", "concurrency"} {
				raw, ok := args[key]
				if !ok {
					continue
				}
				switch value := raw.(type) {
				case float64:
					if value >= 1 {
						w = int(value)
					}
				case string:
					if n, convErr := strconv.Atoi(value); convErr == nil && n >= 1 {
						w = n
					}
				}
				break
			}
		}
	}
	if w > m.cfg.MaxParallel {
		w = m.cfg.MaxParallel
	}
	return w
}

// Weight 返回当前占用的槽位数。
func (m *Manager) Weight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.weight
}

// taskTimeout 读取 REG_FACTORY_TASK_TIMEOUT（秒），0 表示不限制。
func (m *Manager) taskTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("REG_FACTORY_TASK_TIMEOUT"))
	if raw == "" {
		return time.Hour
	}
	if n, err := strconv.Atoi(raw); err == nil {
		if n <= 0 {
			return 0
		}
		return time.Duration(n) * time.Second
	}
	return time.Hour
}

// CPU 返回容器瞬时 CPU 核数、配额与占用百分比（基于 cgroup v2 两次采样）。
func (m *Manager) CPU() (cores float64, quota float64, pct float64) {
	readUsage := func() uint64 {
		raw, err := os.ReadFile(filepath.Join(m.cfg.CgroupRoot, "cpu.stat"))
		if err != nil {
			return 0
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "usage_usec") {
				fields := strings.Fields(line)
				if len(fields) == 2 {
					if v, convErr := strconv.ParseUint(fields[1], 10, 64); convErr == nil {
						return v
					}
				}
			}
		}
		return 0
	}
	readQuota := func() float64 {
		raw, err := os.ReadFile(filepath.Join(m.cfg.CgroupRoot, "cpu.max"))
		if err != nil {
			return 0
		}
		fields := strings.Fields(string(raw))
		if len(fields) != 2 || fields[0] == "max" {
			return 0
		}
		q, err1 := strconv.ParseFloat(fields[0], 64)
		p, err2 := strconv.ParseFloat(fields[1], 64)
		if err1 != nil || err2 != nil || p == 0 {
			return 0
		}
		return q / p
	}

	usage := readUsage()
	now := time.Now()

	m.cpuMu.Lock()
	prev := m.lastCPU
	m.lastCPU = cpuSample{at: now, usage: usage}
	m.cpuMu.Unlock()

	quota = readQuota()
	if prev.at.IsZero() || prev.usage == 0 || usage < prev.usage {
		return 0, quota, 0
	}
	elapsed := now.Sub(prev.at).Seconds()
	if elapsed <= 0 {
		return 0, quota, 0
	}
	cores = float64(usage-prev.usage) / 1e6 / elapsed
	if quota > 0 {
		pct = cores / quota * 100
		if pct > 100 {
			pct = 100
		}
	}
	return cores, quota, pct
}

func (m *Manager) nextQueued() (*store.Task, error) {
	return m.st.NextScriptTask()
}

// ---------------------------------------------------------------- 启动/停止

// LaunchArgs 描述一次启动所需的命令行与元信息。
type LaunchArgs struct {
	Argv []string
	Cwd  string
	Env  []string
}

// Submit 写入任务并触发调度；返回任务 ID。
func (m *Manager) Submit(task store.Task) error {
	if err := m.st.CreateTask(task); err != nil {
		return err
	}
	_ = m.st.Stage(task.ID, store.StageEvent{Stage: "queue", Status: "queued", Action: "等待并发槽位；内存保护时保持排队"})
	m.Wake()
	return nil
}

func (m *Manager) launch(task store.Task, weight int) error {
	argv, cwd, env, err := m.buildArgv(task)
	if err != nil {
		return err
	}
	if weight < 1 {
		weight = 1
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.SysProcAttr = procAttr()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	refreshRef := store.AccountRefreshRef(task)
	credentialRef := store.CredentialTaskRef(task)
	sourceSHA := ""
	if refreshRef != "" {
		account, err := m.st.GetAccount(refreshRef)
		if err != nil {
			return errors.New("刷新账号不可读")
		}
		if account.Platform == "chatgpt" {
			asset, err := assetstore.New(m.cfg.DataDir).Snapshot(refreshRef, account.Identity)
			if err != nil {
				return errors.New("原凭据不可读，未执行 RT 刷新")
			}
			sourceSHA = asset.ResultSHA
		}
	}
	// Serialize launch and stop so a queued cancellation cannot race a spawn.
	m.mu.Lock()
	if refreshRef != "" {
		if err := m.st.BeginAccountRefresh(task.ID, refreshRef, sourceSHA); err != nil {
			m.mu.Unlock()
			return err
		}
	}
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return err
	}

	m.procs[task.ID] = &proc{cmd: cmd, taskID: task.ID}
	m.weight += weight
	m.mu.Unlock()
	_ = m.st.MarkRunning(task.ID, cmd.Process.Pid)
	started := time.Now()
	_ = m.st.Stage(task.ID, store.StageEvent{Stage: "execution", Status: "running"})
	m.record(task.ID, "system", fmt.Sprintf("任务启动 script=%s pid=%d", task.Script, cmd.Process.Pid))

	// 任务超时保护：注册流程卡住时释放槽位，避免队头阻塞
	var timer *time.Timer
	if timeout := m.taskTimeout(); timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			m.record(task.ID, "system", fmt.Sprintf("[regd] 任务超过 %s 未结束，强制终止", timeout))
			_ = killGroup(cmd.Process.Pid, 5*time.Second)
		})
	}

	var wg sync.WaitGroup
	var refreshResult string // written only by stdout pump; read after wg.Wait
	pump := func(reader io.Reader, stream string) {
		defer wg.Done()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
		for scanner.Scan() {
			text := scanner.Text()
			if credentialRef != "" && stream == "stdout" {
				var result struct {
					Ref    string `json:"ref"`
					Result string `json:"result"`
				}
				if json.Unmarshal([]byte(text), &result) == nil && result.Ref == credentialRef {
					refreshResult = result.Result
				}
			}
			m.record(task.ID, stream, text)
		}
		if err := scanner.Err(); err != nil {
			m.record(task.ID, "system", "读取输出失败: "+err.Error())
		}
	}
	wg.Add(2)
	go pump(stdout, "stdout")
	go pump(stderr, "stderr")

	go func() {
		wg.Wait()
		if timer != nil {
			timer.Stop()
		}
		err := cmd.Wait()
		m.mu.Lock()
		entry := m.procs[task.ID]
		stopping := entry != nil && entry.stopping
		delete(m.procs, task.ID)
		m.weight -= weight
		if m.weight < 0 {
			m.weight = 0
		}
		m.mu.Unlock()

		code := 0
		status := "succeeded"
		message := ""
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else {
				code = -1
				message = err.Error()
			}
			if stopping {
				status = "stopped"
			} else if code == 75 {
				status = "unknown"
				message = "执行结果未知；在 Work 核对原任务，不重复消耗凭证"
			} else {
				status = "failed"
			}
		}
		if refreshRef != "" {
			var finishErr error
			status, code, message, finishErr = m.finishRefresh(task, refreshResult, status, code, message)
			if finishErr != nil {
				m.record(task.ID, "system", "刷新收尾未确认；保留原任务，不重复交换 RT")
				m.Wake()
				return
			}
		} else {
			if credentialRef != "" && (status == "unknown" || (status != "succeeded" && refreshResult == "")) {
				status, code, message = "failed", 75, "未取得可确认的成功结果，本次失败；可重新发起授权修复"
			}
			_ = m.st.Finish(task.ID, status, code, message)
		}
		m.record(task.ID, "system", fmt.Sprintf("[regd] 任务结束 status=%s exit=%d", status, code))
		_ = m.st.Stage(task.ID, store.StageEvent{Stage: "execution", Status: status, DurationMS: time.Since(started).Milliseconds(), Code: message})
		m.afterFinish(task, status, code)
		m.Wake()
	}()
	return nil
}

// Stop 停止单个任务（进程组 SIGTERM，5 秒后 SIGKILL）。
func (m *Manager) Stop(taskID string) error {
	if task, err := m.st.GetTask(taskID); err == nil && task.Script == store.CredentialCheckScript {
		return m.st.StopCredentialCheck(taskID)
	}
	if task, err := m.st.GetTask(taskID); err == nil && task.Script == "production.team_json" {
		return errors.New("生产任务由 Work 管理，请在生产中心取消或核对原任务")
	}
	m.mu.Lock()
	entry := m.procs[taskID]
	if entry != nil {
		entry.stopping = true
	}
	if entry == nil {
		defer m.mu.Unlock()
		// 还没启动的排队任务：直接标记停止
		if task, err := m.st.GetTask(taskID); err == nil && task.Status == "queued" {
			m.record(taskID, "system", "任务在启动前已停止")
			if ref := store.AccountRefreshRef(task); ref != "" {
				return m.st.FinishAccountRefresh(taskID, ref, "stopped", -1, "启动前已停止，未执行 RT 刷新", "")
			}
			return m.st.Finish(taskID, "stopped", -1, "cancelled before start")
		}
		return errors.New("任务不在运行中")
	}
	m.mu.Unlock()
	return killGroup(entry.cmd.Process.Pid, 5*time.Second)
}

// StopAll 停止全部任务。
func (m *Manager) StopAll() []string {
	_ = m.st.StopCredentialChecks()
	m.mu.Lock()
	ids := make([]string, 0, len(m.procs))
	for id, entry := range m.procs {
		entry.stopping = true
		ids = append(ids, id)
	}
	_ = m.st.StopQueuedScripts()
	m.mu.Unlock()
	for _, id := range ids {
		m.mu.Lock()
		entry := m.procs[id]
		m.mu.Unlock()
		if entry != nil {
			_ = killGroup(entry.cmd.Process.Pid, 5*time.Second)
		}
	}
	return ids
}

func killGroup(pid int, grace time.Duration) error {
	if pid <= 0 {
		return nil
	}
	terminate(pid)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	forceKill(pid)
	return nil
}

func round1(value float64) float64 {
	return float64(int64(value*10+0.5)) / 10
}

// ---------------------------------------------------------------- 环境

func (m *Manager) buildArgv(task store.Task) ([]string, string, []string, error) {
	baseEnv, envErr := settings.WorkerEnvironment(m.cfg.EnvFile)
	if envErr != nil {
		return nil, "", nil, fmt.Errorf("任务配置不可读: %w", envErr)
	}
	// raw 任务：ArgsJSON 直接是参数数组，脚本为 tools/<script>.py
	if task.Script == "asset_ops" || task.Script == "change_password" || task.Script == "import_free_json" {
		var argvTail []string
		if err := json.Unmarshal([]byte(task.ArgsJSON), &argvTail); err != nil {
			return nil, "", nil, fmt.Errorf("raw 参数无效: %w", err)
		}
		script := filepath.Join(m.cfg.AppRoot, "tools", task.Script+".py")
		if _, err := os.Stat(script); err != nil {
			return nil, "", nil, fmt.Errorf("%s.py 不存在", task.Script)
		}
		argv := append([]string{m.cfg.Python, "-u", script}, argvTail...)
		env := append(baseEnv,
			"PYTHONUNBUFFERED=1", "PYTHONIOENCODING=utf-8",
			"REG_FACTORY_DATA_DIR="+m.cfg.DataDir,
			"REG_FACTORY_WEBUI_TASK=1", "REG_FACTORY_RUN_ID="+task.ID,
		)
		return argv, m.cfg.DataDir, env, nil
	}

	argv, err := BuildArgv(m.cfg, task)
	if err != nil {
		return nil, "", nil, err
	}
	env := baseEnv
	env = append(env,
		"PYTHONUNBUFFERED=1",
		"PYTHONIOENCODING=utf-8",
		"REG_FACTORY_WEBUI_TASK=1",
		"REG_FACTORY_RUN_ID="+task.ID,
		"REG_FACTORY_DATA_DIR="+m.cfg.DataDir,
	)
	cwd := m.cfg.DataDir
	return argv, cwd, env, nil
}

// Shutdown 停止全部并退出调度循环。
func (m *Manager) Shutdown(ctx context.Context) {
	select {
	case <-m.stop:
		return
	default:
	}
	close(m.stop)
	m.StopAll()
	_ = ctx
}

// afterFinish 任务收尾的副作用：改密码结果回写到账号状态。
func (m *Manager) afterFinish(task store.Task, status string, code int) {
	if task.Script != "change_password" {
		return
	}
	var argv []string
	if err := json.Unmarshal([]byte(task.ArgsJSON), &argv); err != nil {
		return
	}
	ref := ""
	email := ""
	for index, item := range argv {
		if item == "--ref" && index+1 < len(argv) {
			ref = argv[index+1]
		}
		if item == "--email" && index+1 < len(argv) {
			email = argv[index+1]
		}
	}
	if ref == "" && email != "" {
		ref = "import:" + email
	}
	if ref == "" {
		return
	}
	state := "failed"
	message := fmt.Sprintf("exit=%d", code)
	if status == "succeeded" {
		state = "changed"
		message = "密码已更新"
	}
	_ = m.st.MarkPassword(ref, state, message)
}
