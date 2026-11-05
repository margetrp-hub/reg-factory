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
  updated_at TEXT NOT NULL,
  version INTEGER NOT NULL DEFAULT 1,
  password_state TEXT NOT NULL DEFAULT '',
  password_changed_at TEXT NOT NULL DEFAULT '',
  password_message TEXT NOT NULL DEFAULT '',
  oauth_status TEXT NOT NULL DEFAULT '',
  join_status TEXT NOT NULL DEFAULT '',
  liveness_status TEXT NOT NULL DEFAULT '',
  liveness_checked_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_accounts_kind ON accounts(kind, platform, status);

CREATE TABLE IF NOT EXISTS account_credentials (
  ref TEXT PRIMARY KEY,
  password TEXT NOT NULL DEFAULT '',
  totp TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS work_operations (
  operation_id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  action TEXT NOT NULL DEFAULT '',
  input_resource_version INTEGER NOT NULL DEFAULT 0,
  result_resource_version INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  completed_at TEXT NOT NULL DEFAULT ''
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
	st := &Store{db: db}
	// Existing SG databases predate the operation result columns. Migrate them
	// in place so a retry after a lost receipt cannot advance an account twice.
	st.ensureColumns("work_operations", map[string]string{
		"input_resource_version":  "INTEGER NOT NULL DEFAULT 0",
		"result_resource_version": "INTEGER NOT NULL DEFAULT 0",
		"completed_at":            "TEXT NOT NULL DEFAULT ''",
	})
	return st, nil
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
// inputVersion 是这次操作看到的账号版本，作为刷新结果的幂等基线。
func (s *Store) ReserveOperation(operationID, taskID, action string, inputVersion ...int) (string, bool, error) {
	version := 0
	if len(inputVersion) > 0 && inputVersion[0] > 0 {
		version = inputVersion[0]
	}
	var existing string
	err := s.db.QueryRow(`SELECT task_id FROM work_operations WHERE operation_id=?`, operationID).Scan(&existing)
	if err == nil {
		return existing, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	_, err = s.db.Exec(
		`INSERT INTO work_operations(operation_id, task_id, action, input_resource_version, created_at) VALUES(?,?,?,?,?)`,
		operationID, taskID, action, version, now(),
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

// CommitOperationResult records the durable version produced by a successful
// account refresh/authorization. The operation row and account version are
// updated in one transaction, so repeated receipt polls return the same
// version without rotating the account again.
func (s *Store) CommitOperationResult(operationID, action, accountRef string, expectedVersion int) (int, error) {
	if operationID == "" || accountRef == "" {
		return 0, fmt.Errorf("operation result scope missing")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var saved, input int
	err = tx.QueryRow(`SELECT result_resource_version, input_resource_version FROM work_operations WHERE operation_id=?`, operationID).Scan(&saved, &input)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	operationFound := err == nil
	if err == nil && saved > 0 {
		return saved, nil
	}
	if expectedVersion <= 0 {
		expectedVersion = input
	}
	var current int
	if err = tx.QueryRow(`SELECT version FROM accounts WHERE ref=?`, accountRef).Scan(&current); err != nil {
		return 0, err
	}
	if current < 1 {
		current = 1
	}
	if expectedVersion > 0 && current != expectedVersion && current != expectedVersion+1 {
		return 0, fmt.Errorf("account version changed: got %d want %d", current, expectedVersion)
	}
	resultVersion := current
	if current == expectedVersion || expectedVersion <= 0 {
		resultVersion = current + 1
		updated, updateErr := tx.Exec(`UPDATE accounts SET version=?,updated_at=? WHERE ref=? AND version=?`, resultVersion, now(), accountRef, current)
		if updateErr != nil {
			return 0, updateErr
		}
		if count, _ := updated.RowsAffected(); count != 1 {
			return 0, fmt.Errorf("account version update lost")
		}
	}
	if operationFound {
		if _, err = tx.Exec(`UPDATE work_operations SET result_resource_version=?,completed_at=? WHERE operation_id=?`, resultVersion, now(), operationID); err != nil {
			return 0, err
		}
	} else if _, err = tx.Exec(`INSERT INTO work_operations(operation_id,task_id,action,input_resource_version,result_resource_version,created_at,completed_at) VALUES(?,?,?,?,?,?,?)`,
		operationID, "", action, expectedVersion, resultVersion, now(), now()); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return resultVersion, nil
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

	// Team 参考协议需要的判定字段（源号是否可派单）
	Version           int    `json:"version"`
	OAuthStatus       string `json:"oauth_status"`
	JoinStatus        string `json:"join_status"`
	LivenessStatus    string `json:"liveness_status"`
	LivenessCheckedAt string `json:"liveness_checked_at"`

	PasswordState     string `json:"password_state"`
	PasswordChangedAt string `json:"password_changed_at"`
	PasswordMessage   string `json:"password_message"`
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
	// 源文件已删除的条目一并清掉（本次未被 upsert 的 updated_at 仍为旧值）；
	// 人工导入（imported/creds）不参与文件同步清理。
	if _, err := tx.Exec(`DELETE FROM accounts WHERE updated_at < ? AND kind NOT IN ('imported','creds')`, ts); err != nil {
		return count, err
	}
	return count, tx.Commit()
}

// ensureColumns 按需补列（SQLite 没有 ADD COLUMN IF NOT EXISTS）。
func (s *Store) ensureColumns(table string, cols map[string]string) {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if scanErr := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); scanErr == nil {
			have[name] = true
		}
	}
	rows.Close()
	for name, def := range cols {
		if have[name] {
			continue
		}
		_, _ = s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + name + " " + def)
	}
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

// UpsertAccounts 只插入/更新账号，不清理未出现的条目（人工导入用）。
func (s *Store) UpsertAccounts(seeds []AccountSeed) (int, error) {
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
	return count, tx.Commit()
}

// LibraryAccounts 分页返回账号库投影（Team 参考协议用），跨平台不过滤 NodePlatform。
func (s *Store) LibraryAccounts(query, status, platform string, page, pageSize int) ([]Account, int, error) {
	where := []string{"1=1"}
	args := []any{}
	if q := strings.TrimSpace(query); q != "" {
		where = append(where, "(identity LIKE ? OR ref LIKE ?)")
		like := "%" + q + "%"
		args = append(args, like, like)
	}
	if st := strings.TrimSpace(status); st != "" {
		where = append(where, "status = ?")
		args = append(args, st)
	}
	if p := strings.TrimSpace(platform); p != "" {
		where = append(where, "platform = ?")
		args = append(args, p)
	}
	cond := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM accounts WHERE "+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 200 {
		pageSize = 50
	}
	offset := (page - 1) * pageSize

	rows, err := s.db.Query(`SELECT ref, platform, kind, identity, secret_ref, status, health_checked_at,
		health_message, claimed, claimed_at, refresh_state, refresh_at, note, version,
		oauth_status, join_status, liveness_status, liveness_checked_at, updated_at
		FROM accounts WHERE `+cond+` ORDER BY updated_at DESC LIMIT ? OFFSET ?`,
		append(args, pageSize, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]Account, 0, pageSize)
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.Ref, &a.Platform, &a.Kind, &a.Identity, &a.SecretRef, &a.Status,
			&a.HealthCheckedAt, &a.HealthMessage, &a.Claimed, &a.ClaimedAt,
			&a.RefreshState, &a.RefreshAt, &a.Note, &a.Version,
			&a.OAuthStatus, &a.JoinStatus, &a.LivenessStatus, &a.LivenessCheckedAt, &a.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// MarkPassword 记录改密码结果（能力化：账号状态的一部分）。
func (s *Store) MarkPassword(ref, state, message string) error {
	_, err := s.db.Exec(`UPDATE accounts SET password_state=?, password_changed_at=?, password_message=?, updated_at=? WHERE ref=?`,
		state, now(), message, now(), ref)
	return err
}

// SaveCredential 保存账号的登录凭据（改密码能力需要旧密码 + TOTP）。
func (s *Store) SaveCredential(ref, password, totp string) error {
	if password == "" && totp == "" {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO account_credentials(ref, password, totp, updated_at)
		VALUES(?,?,?,?)
		ON CONFLICT(ref) DO UPDATE SET
		  password=CASE WHEN excluded.password != '' THEN excluded.password ELSE password END,
		  totp=CASE WHEN excluded.totp != '' THEN excluded.totp ELSE totp END,
		  updated_at=excluded.updated_at`, ref, password, totp, now())
	return err
}

// GetCredential 读取账号凭据。
func (s *Store) GetCredential(ref string) (password, totp string, err error) {
	err = s.db.QueryRow(`SELECT password, totp FROM account_credentials WHERE ref=?`, ref).Scan(&password, &totp)
	return password, totp, err
}

// OperationTask 由 operation_id 反查本地 task_id（Team 凭 operation 取任务用）。
func (s *Store) OperationTask(operationID string) (string, error) {
	var taskID string
	err := s.db.QueryRow(`SELECT task_id FROM work_operations WHERE operation_id=?`, operationID).Scan(&taskID)
	return taskID, err
}
