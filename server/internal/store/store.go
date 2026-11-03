// Package store 提供 SQLite 持久化：任务、日志事件、资产领取、审计。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Task 是一条任务记录。
type Task struct {
	ID         string `json:"id"`
	Script     string `json:"script"`
	Title      string `json:"title"`
	ArgsJSON   string `json:"args_json"`
	Status     string `json:"status"` // queued|running|succeeded|failed|stopped|interrupted
	PID        int    `json:"pid"`
	CreatedAt  string `json:"created_at"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	ExitCode   int    `json:"exit_code"`
	Error      string `json:"error"`
	Actor      string `json:"actor"`
}

// Event 是一行日志。
type Event struct {
	ID     int64  `json:"id"`
	TaskID string `json:"task_id"`
	TS     string `json:"ts"`
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

// Claim 是资产领取记录。
type Claim struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	TS   string `json:"ts"`
	Note string `json:"note"`
}

// Audit 是一条操作审计。
type Audit struct {
	ID     int64  `json:"id"`
	TS     string `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Detail string `json:"detail"`
	IP     string `json:"ip"`
}

// Store 包装数据库句柄。
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
  id TEXT PRIMARY KEY,
  script TEXT NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  args_json TEXT NOT NULL DEFAULT '{}',
  status TEXT NOT NULL,
  pid INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT '',
  finished_at TEXT NOT NULL DEFAULT '',
  exit_code INTEGER NOT NULL DEFAULT -1,
  error TEXT NOT NULL DEFAULT '',
  actor TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tasks_created ON tasks(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);

CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL,
  ts TEXT NOT NULL,
  stream TEXT NOT NULL DEFAULT 'stdout',
  line TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_task ON events(task_id, id);

CREATE TABLE IF NOT EXISTS claims (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  ref TEXT NOT NULL,
  ts TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_claims_kind ON claims(kind, ref);

CREATE TABLE IF NOT EXISTS accounts (
  ref TEXT PRIMARY KEY,
  platform TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT '',
  identity TEXT NOT NULL DEFAULT '',
  secret_ref TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'unknown',
  health_checked_at TEXT NOT NULL DEFAULT '',
  health_message TEXT NOT NULL DEFAULT '',
  claimed INTEGER NOT NULL DEFAULT 0,
  claimed_at TEXT NOT NULL DEFAULT '',
  refresh_state TEXT NOT NULL DEFAULT '',
  refresh_at TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_accounts_kind ON accounts(kind, platform, status);

CREATE TABLE IF NOT EXISTS work_operations (
  operation_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  action TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  actor TEXT NOT NULL DEFAULT '',
  action TEXT NOT NULL,
  detail TEXT NOT NULL DEFAULT '',
  ip TEXT NOT NULL DEFAULT ''
);
`

// Open 打开/初始化数据库。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // modernc sqlite 单写更稳
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().Format(time.RFC3339) }

// ---------------------------------------------------------------- 任务

// CreateTask 插入一条 queued 任务。
func (s *Store) CreateTask(t Task) error {
	if t.CreatedAt == "" {
		t.CreatedAt = now()
	}
	_, err := s.db.Exec(
		`INSERT INTO tasks(id, script, title, args_json, status, pid, created_at, actor)
		 VALUES(?,?,?,?,?,?,?,?)`,
		t.ID, t.Script, t.Title, t.ArgsJSON, "queued", 0, t.CreatedAt, t.Actor,
	)
	return err
}

// MarkRunning 记录启动成功。
func (s *Store) MarkRunning(id string, pid int) error {
	_, err := s.db.Exec(
		`UPDATE tasks SET status='running', pid=?, started_at=? WHERE id=?`,
		pid, now(), id,
	)
	return err
}

// Finish 记录结束状态。
func (s *Store) Finish(id, status string, exitCode int, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE tasks SET status=?, exit_code=?, finished_at=?, error=?, pid=0 WHERE id=?`,
		status, exitCode, now(), errMsg, id,
	)
	return err
}

// GetTask 取单条。
func (s *Store) GetTask(id string) (Task, error) {
	var t Task
	err := s.db.QueryRow(
		`SELECT id, script, title, args_json, status, pid, created_at, started_at,
		        finished_at, exit_code, error, actor FROM tasks WHERE id=?`, id,
	).Scan(&t.ID, &t.Script, &t.Title, &t.ArgsJSON, &t.Status, &t.PID, &t.CreatedAt,
		&t.StartedAt, &t.FinishedAt, &t.ExitCode, &t.Error, &t.Actor)
	return t, err
}

// ListTasks 按时间倒序列出任务。
func (s *Store) ListTasks(status string, limit int) ([]Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	q := `SELECT id, script, title, args_json, status, pid, created_at, started_at,
	             finished_at, exit_code, error, actor FROM tasks`
	if strings.TrimSpace(status) != "" {
		rows, err = s.db.Query(q+` WHERE status=? ORDER BY created_at DESC LIMIT ?`, status, limit)
	} else {
		rows, err = s.db.Query(q+` ORDER BY created_at DESC LIMIT ?`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Task, 0, limit)
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Script, &t.Title, &t.ArgsJSON, &t.Status, &t.PID,
			&t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.ExitCode, &t.Error, &t.Actor); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RecoverInterrupted 把上次进程崩溃遗留的 running 标记为 interrupted。
func (s *Store) RecoverInterrupted() (int64, error) {
	res, err := s.db.Exec(
		`UPDATE tasks SET status='interrupted', error='regd restarted', finished_at=?, pid=0
		 WHERE status IN ('running','queued')`, now(),
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------- 事件

// AppendEvent 写一行日志。
func (s *Store) AppendEvent(taskID, stream, line string) error {
	_, err := s.db.Exec(
		`INSERT INTO events(task_id, ts, stream, line) VALUES(?,?,?,?)`,
		taskID, now(), stream, line,
	)
	return err
}

// TailEvents 取最近 N 行（按 id 升序返回，便于前端直接追加）。
func (s *Store) TailEvents(taskID string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 20000 {
		limit = 2000
	}
	rows, err := s.db.Query(
		`SELECT id, task_id, ts, stream, line FROM events
		 WHERE task_id=? ORDER BY id DESC LIMIT ?`, taskID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Event, 0, limit)
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TaskID, &e.TS, &e.Stream, &e.Line); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	// 反转为升序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- 领取

// ClaimAsset 记录领取；返回是否首次（同一 kind+ref 不重复）。
func (s *Store) ClaimAsset(kind, ref, note string) (bool, error) {
	var exists int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM claims WHERE kind=? AND ref=?`, kind, ref).Scan(&exists)
	if err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil
	}
	_, err = s.db.Exec(`INSERT INTO claims(kind, ref, ts, note) VALUES(?,?,?,?)`, kind, ref, now(), note)
	return err == nil, err
}

// ClaimedRefs 返回某类已领取的引用集合。
func (s *Store) ClaimedRefs(kind string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT ref FROM claims WHERE kind=?`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, err
		}
		out[ref] = true
	}
	return out, rows.Err()
}

// ResetClaims 清空某类领取记录。
func (s *Store) ResetClaims(kind string) (int64, error) {
	var res sql.Result
	var err error
	if kind == "" {
		res, err = s.db.Exec(`DELETE FROM claims`)
	} else {
		res, err = s.db.Exec(`DELETE FROM claims WHERE kind=?`, kind)
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------- 审计

// Audit 追加一条操作记录。
func (s *Store) Audit(actor, action, detail, ip string) {
	_, _ = s.db.Exec(
		`INSERT INTO audit(ts, actor, action, detail, ip) VALUES(?,?,?,?,?)`,
		now(), actor, action, detail, ip,
	)
}

// ListAudit 倒序列出审计。
func (s *Store) ListAudit(limit int) ([]Audit, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT id, ts, actor, action, detail, ip FROM audit ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Audit, 0, limit)
	for rows.Next() {
		var a Audit
		if err := rows.Scan(&a.ID, &a.TS, &a.Actor, &a.Action, &a.Detail, &a.IP); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Stats 返回概览计数。
func (s *Store) Stats() (map[string]int, error) {
	out := map[string]int{}
	rows, err := s.db.Query(`SELECT status, COUNT(1) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

// ReserveOperation 幂等登记一个 Work operation_id；返回已存在的 task（若有）。
func (s *Store) ReserveOperation(operationID, taskID, action string) (string, bool, error) {
	var existing string
	err := s.db.QueryRow(`SELECT task_id FROM work_operations WHERE operation_id=?`, operationID).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	_, err = s.db.Exec(
		`INSERT INTO work_operations(operation_id, task_id, action, created_at) VALUES(?,?,?,?)`,
		operationID, taskID, action, now(),
	)
	if err != nil {
		// 并发下可能已被别的请求插入
		if again := s.db.QueryRow(`SELECT task_id FROM work_operations WHERE operation_id=?`, operationID).Scan(&existing); again == nil {
			return existing, false, nil
		}
		return "", false, err
	}
	return taskID, true, nil
}

// AccountSeed 是从数据目录扫出来的一条资产，用于同步进 accounts 表。
type AccountSeed struct {
	Ref       string
	Kind      string
	Platform  string
	Identity  string
	SecretRef string
}

// Account 是账号主数据（对齐 account-node 的 identity/secret/business/health 分组语义）。
type Account struct {
	Ref             string `json:"ref"`
	Platform        string `json:"platform"`
	Kind            string `json:"kind"`
	Identity        string `json:"identity"`
	SecretRef       string `json:"secret_ref"`
	Status          string `json:"status"`
	HealthCheckedAt string `json:"health_checked_at"`
	HealthMessage   string `json:"health_message"`
	Claimed         bool   `json:"claimed"`
	ClaimedAt       string `json:"claimed_at"`
	RefreshState    string `json:"refresh_state"`
	RefreshAt       string `json:"refresh_at"`
	Note            string `json:"note"`
	UpdatedAt       string `json:"updated_at"`
}

// SyncAccounts 把扫描结果 upsert 进 accounts（保留已有健康/领取/刷新状态）。
func (s *Store) SyncAccounts(seeds []AccountSeed) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO accounts(ref, platform, kind, identity, secret_ref, status, created_at, updated_at)
		VALUES(?,?,?,?,?,'unknown',?,?)
		ON CONFLICT(ref) DO UPDATE SET
		  platform=excluded.platform, kind=excluded.kind, identity=excluded.identity,
		  secret_ref=excluded.secret_ref, updated_at=excluded.updated_at`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	ts := now()
	count := 0
	for _, seed := range seeds {
		if seed.Ref == "" {
			continue
		}
		if _, err := stmt.Exec(seed.Ref, seed.Platform, seed.Kind, seed.Identity, seed.SecretRef, ts, ts); err != nil {
			return count, err
		}
		count++
	}
	// 源文件已删除的条目一并清掉（本次未被 upsert 的 updated_at 仍为旧值）
	if _, err := tx.Exec(`DELETE FROM accounts WHERE updated_at < ?`, ts); err != nil {
		return count, err
	}
	return count, tx.Commit()
}

// ListAccounts 按条件列出账号。
func (s *Store) ListAccounts(kind, platform, status string, limit int) ([]Account, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT ref, platform, kind, identity, secret_ref, status, health_checked_at, health_message,
	             claimed, claimed_at, refresh_state, refresh_at, note, updated_at FROM accounts WHERE 1=1`
	var args []any
	if kind != "" {
		q += " AND kind=?"
		args = append(args, kind)
	}
	if platform != "" {
		q += " AND platform=?"
		args = append(args, platform)
	}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	q += " ORDER BY updated_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Account, 0, limit)
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.Ref, &a.Platform, &a.Kind, &a.Identity, &a.SecretRef, &a.Status,
			&a.HealthCheckedAt, &a.HealthMessage, &a.Claimed, &a.ClaimedAt,
			&a.RefreshState, &a.RefreshAt, &a.Note, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount 取单条。
func (s *Store) GetAccount(ref string) (Account, error) {
	var a Account
	// 账号引用证明需要 oauth/join/liveness 与 version：这些是 Team 判定源号
	// 是否可派单的唯一依据，漏查会让 reference 里的 liveness 恒为空，
	// Team 只能把每个候选号判成 pending，生产单永远起不来。
	err := s.db.QueryRow(`SELECT ref, platform, kind, identity, secret_ref, status, health_checked_at,
		health_message, claimed, claimed_at, refresh_state, refresh_at, note, version,
		oauth_status, join_status, liveness_status, liveness_checked_at, updated_at
		FROM accounts WHERE ref=?`, ref).Scan(&a.Ref, &a.Platform, &a.Kind, &a.Identity, &a.SecretRef,
		&a.Status, &a.HealthCheckedAt, &a.HealthMessage, &a.Claimed, &a.ClaimedAt,
		&a.RefreshState, &a.RefreshAt, &a.Note, &a.Version,
		&a.OAuthStatus, &a.JoinStatus, &a.LivenessStatus, &a.LivenessCheckedAt, &a.UpdatedAt)
	return a, err
}

// MarkHealth 写入测活结果。
func (s *Store) MarkHealth(ref, status, message string) error {
	_, err := s.db.Exec(`UPDATE accounts SET status=?, health_checked_at=?, health_message=?, updated_at=? WHERE ref=?`,
		status, now(), message, now(), ref)
	return err
}

// MarkRefresh 写入刷新/修复结果。
func (s *Store) MarkRefresh(ref, state, message string) error {
	_, err := s.db.Exec(`UPDATE accounts SET refresh_state=?, refresh_at=?, health_message=?, updated_at=? WHERE ref=?`,
		state, now(), message, now(), ref)
	return err
}

// MarkClaimed 标记账号已被下游领取（领取后禁止自动修复）。
func (s *Store) MarkClaimed(ref string) error {
	_, err := s.db.Exec(`UPDATE accounts SET claimed=1, claimed_at=?, updated_at=? WHERE ref=?`, now(), now(), ref)
	return err
}

// DueForLiveness 取最久未测活的账号（供定时巡检）。
func (s *Store) DueForLiveness(platform string, limit int) ([]Account, error) {
	if limit <= 0 {
		limit = 5
	}
	q := `SELECT ref, platform, kind, identity, secret_ref, status, health_checked_at, health_message,
	             claimed, claimed_at, refresh_state, refresh_at, note, updated_at
	      FROM accounts WHERE claimed=0`
	var args []any
	if platform != "" {
		q += " AND platform=?"
		args = append(args, platform)
	}
	q += " ORDER BY (health_checked_at = '') DESC, health_checked_at ASC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.Ref, &a.Platform, &a.Kind, &a.Identity, &a.SecretRef, &a.Status,
			&a.HealthCheckedAt, &a.HealthMessage, &a.Claimed, &a.ClaimedAt,
			&a.RefreshState, &a.RefreshAt, &a.Note, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
