package mail

// Store owns the local mail_sources/mail_pool/message tables in MAIL_DB.
// Deployed nodes use their local PostgreSQL; the SQLite opener remains for
// isolated fixtures and explicitly read historical data, never a PG fallback.
//
// Two schema decisions matter at a few thousand mailboxes:
//   - alias_key / sort_ts are materialised columns; LOWER(x) = ? or
//     ORDER BY COALESCE(a,b) cannot use an index and cost ~200ms per lookup at
//     200k rows in the original implementation.
//   - counters are pre-aggregated so the list view never runs a GROUP BY.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	storepkg "github.com/tiantianGPU/reg-factory/server/internal/store"
)

// ErrNotFound is returned when a row is absent.
var ErrNotFound = errors.New("not found")

// DB owns mailbox queries and serializes source configuration with result writes.
type DB struct {
	dialect storepkg.Dialect
	ex      *storepkg.Execer
	exRO    *storepkg.Execer

	db   *sql.DB
	path string
	mu   sync.Mutex // source updates and ingestion writes share the same ownership check
}

// OpenDB creates the mail schema in the configured local database. Runtime
// configuration accepts PostgreSQL only; the file opener serves isolated tests.
func OpenDB(path string) (*DB, error) {
	if storepkg.DialectOf(path) == storepkg.DialectPG {
		return openPGDB(path)
	}
	return openSQLiteDB(path)
}

// openPGDB 打开 PG 后端的邮件库。
func openPGDB(dsn string) (*DB, error) {
	handle, d, err := storepkg.OpenDSN(dsn)
	if err != nil {
		return nil, err
	}
	db := &DB{db: handle, path: dsn, dialect: d, ex: storepkg.NewExecer(handle, d)}
	db.exRO = db.ex
	if err := db.ensureSchema(); err != nil {
		handle.Close()
		return nil, err
	}
	// Fresh PostgreSQL installs initialize these tables through the same owner.
	if err := db.ensurePool(); err != nil {
		handle.Close()
		return nil, err
	}
	if err := db.ensureSync(); err != nil {
		handle.Close()
		return nil, err
	}
	if err := db.migrate(); err != nil {
		handle.Close()
		return nil, err
	}
	return db, nil
}

// openSQLiteDB 是原来的 SQLite 路径。
func openSQLiteDB(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(8000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)", path)
	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	handle.SetMaxOpenConns(8)
	handle.SetMaxIdleConns(4)
	if err := handle.Ping(); err != nil {
		handle.Close()
		return nil, err
	}
	d := &DB{db: handle, path: path, dialect: storepkg.DialectSQLite,
		ex: storepkg.NewExecer(handle, storepkg.DialectSQLite)}
	d.exRO = d.ex // 邮件库单连接，读写共用执行器
	if err := d.ensureSchema(); err != nil {
		handle.Close()
		return nil, err
	}
	if err := d.ensurePool(); err != nil {
		handle.Close()
		return nil, err
	}
	if err := d.ensureSync(); err != nil {
		handle.Close()
		return nil, err
	}
	// migrate 必须放在所有建表之后：它 ALTER 的 mail_pool 由 ensurePool 创建
	if err := d.migrate(); err != nil {
		handle.Close()
		return nil, err
	}
	return d, nil
}

// Close releases the handle.
func (d *DB) Close() error { return d.db.Close() }

// Path is the database file.
func (d *DB) Path() string { return d.path }

func (d *DB) ensureSchema() error {
	const schema = `
CREATE TABLE IF NOT EXISTS mail_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    mailbox TEXT NOT NULL,
    uid TEXT NOT NULL,
    dedupe_key TEXT,
    alias_hme TEXT,
    alias_key TEXT,
    source_id INTEGER NOT NULL DEFAULT 1,
    from_addr TEXT NOT NULL DEFAULT '',
    to_header TEXT NOT NULL DEFAULT '',
    subject TEXT NOT NULL DEFAULT '',
    sent_at REAL,
    sort_ts REAL,
    message_id TEXT NOT NULL DEFAULT '',
    tags TEXT NOT NULL DEFAULT '[]',
    primary_tag TEXT NOT NULL DEFAULT 'other',
    score INTEGER NOT NULL DEFAULT 0,
    snippet TEXT NOT NULL DEFAULT '',
    body_text TEXT NOT NULL DEFAULT '',
    indexed_at REAL NOT NULL,
    UNIQUE(source_id, mailbox, uid)
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_icmail_dedupe ON mail_messages(dedupe_key);
CREATE INDEX IF NOT EXISTS idx_icmail_alias ON mail_messages(alias_key, sort_ts DESC);
CREATE INDEX IF NOT EXISTS idx_icmail_tag ON mail_messages(primary_tag, sort_ts DESC);
CREATE INDEX IF NOT EXISTS idx_icmail_sort ON mail_messages(sort_ts DESC);

CREATE TABLE IF NOT EXISTS mail_alias_state (
    alias_hme TEXT PRIMARY KEY,
    state TEXT NOT NULL DEFAULT 'active',
    manual INTEGER NOT NULL DEFAULT 0,
    hits INTEGER NOT NULL DEFAULT 0,
    last_tag TEXT NOT NULL DEFAULT '',
    last_hit_at REAL,
    updated_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS mail_alias_counters (
    alias_key TEXT PRIMARY KEY,
    total INTEGER NOT NULL DEFAULT 0,
    risk_hits INTEGER NOT NULL DEFAULT 0,
    last_ts REAL,
    updated_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS mail_sources (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    host TEXT NOT NULL,
    port INTEGER NOT NULL DEFAULT 993,
    username TEXT NOT NULL DEFAULT '',
    password TEXT NOT NULL DEFAULT '',
    folder TEXT NOT NULL DEFAULT 'INBOX',
    enabled INTEGER NOT NULL DEFAULT 1,
    idle INTEGER NOT NULL DEFAULT 1,
    interval_seconds INTEGER NOT NULL DEFAULT 10,
    heartbeat_seconds INTEGER NOT NULL DEFAULT 60,
    last_uid INTEGER NOT NULL DEFAULT 0,
    last_ok_at REAL,
    last_idle_at REAL,
    last_error TEXT,
    created_at REAL NOT NULL,
    updated_at REAL NOT NULL
);

CREATE TABLE IF NOT EXISTS mail_sync_requests (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id INTEGER,
    alias_key TEXT,
    state TEXT NOT NULL DEFAULT 'pending',
    requested_at REAL NOT NULL,
    started_at REAL,
    finished_at REAL,
    note TEXT
);
`
	_, err := d.ex.Exec(storepkg.SchemaDDL(schema, d.dialect))
	return err
}

// migrate 给已有库补字段。新库由 ensureSchema 的 CREATE TABLE 覆盖。
//
// 为什么要 migrate
// --------------
// ensureSchema 用的是 CREATE TABLE IF NOT EXISTS，老库不会被重建。
// 加字段必须走 ALTER TABLE，且每列只能加一次，重复会报 duplicate column。
//
// 这次扩的是「多 provider / 多 transport」所需：
//
//	credential_mode  password / oauth_refresh / jwt / api_key / none
//	transport        imap / pop3 / graph / outlook / cloudmail / luckmail / inbucket
//	endpoint         cloudmail / luckmail 的 API 地址
//	refresh_token    OAuth 刷新令牌（Outlook）
//	client_id        OAuth 客户端 id（Outlook 用 9e5f94bc-...）
//	jwt              cloudmail 的访问令牌
//	api_key          luckmail 的 API key
//	meta             JSON，放 provider 特有字段（IC 配額 / parent_apple_id 等）
func (d *DB) migrate() error {
	alters := []string{
		`ALTER TABLE mail_sources ADD COLUMN credential_mode TEXT NOT NULL DEFAULT 'password'`,
		`ALTER TABLE mail_sources ADD COLUMN transport TEXT NOT NULL DEFAULT 'imap'`,
		`ALTER TABLE mail_sources ADD COLUMN endpoint TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_sources ADD COLUMN refresh_token TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_sources ADD COLUMN client_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_sources ADD COLUMN jwt TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_sources ADD COLUMN api_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_sources ADD COLUMN meta TEXT NOT NULL DEFAULT '{}'`,

		// 池条目也要能挂状态（现在只有 email/source_id/origin/created_at）
		`ALTER TABLE mail_pool ADD COLUMN state TEXT NOT NULL DEFAULT 'ready'`,
		`ALTER TABLE mail_pool ADD COLUMN assigned_to TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_pool ADD COLUMN assigned_at REAL`,
		`ALTER TABLE mail_pool ADD COLUMN last_checked_at REAL`,
		`ALTER TABLE mail_pool ADD COLUMN last_used_at REAL`,
		`ALTER TABLE mail_pool ADD COLUMN fail_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE mail_pool ADD COLUMN last_error TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_pool ADD COLUMN last_error_code TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_pool ADD COLUMN message_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE mail_pool ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
	}
	for _, q := range alters {
		if _, err := d.ex.Exec(q); err != nil {
			if isDuplicateColumn(err) {
				continue
			}
			return fmt.Errorf("migrate: %w", err)
		}
	}

	// 平台封禁：邮箱在某平台的状态。
	//
	// 为什么要单独一张表而不是在 mail_pool 上加一列：
	// 一个邮箱会同时出现在多个平台，每个平台各自独立判定。
	// 放在 mail_pool 上就只能记「最后一次被谁封」，信息全丢。
	//
	// evidence_type / evidence_ref 记录判定依据：
	//   mail    → mail_messages.id，收到封禁通知或退信
	//   login   → accounts.ref，登录失败 / token revoked
	//   manual  → 人工标记
	//   api     → 平台接口直接返回 banned
	_, err := d.ex.Exec(`
CREATE TABLE IF NOT EXISTS mail_platform_bans (
    email           TEXT NOT NULL,
    platform        TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'unknown',
    reason          TEXT NOT NULL DEFAULT '',
    evidence_type   TEXT NOT NULL DEFAULT '',
    evidence_ref    TEXT NOT NULL DEFAULT '',
    banned_at       REAL,
    checked_at      REAL,
    updated_at      REAL NOT NULL,
    PRIMARY KEY (email, platform)
);
CREATE INDEX IF NOT EXISTS idx_mpb_platform ON mail_platform_bans(platform, state);
CREATE INDEX IF NOT EXISTS idx_mpb_email ON mail_platform_bans(email);
`)
	if err != nil {
		return fmt.Errorf("建 mail_platform_bans: %w", err)
	}

	// 母号关系：IC 地址挂在父 Apple ID 下，母号被封要整组级联。
	alters2 := []string{
		`ALTER TABLE mail_pool ADD COLUMN parent_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_pool ADD COLUMN parent_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_pool ADD COLUMN ban_scope TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_pool ADD COLUMN role TEXT NOT NULL DEFAULT 'address'`,
		`ALTER TABLE mail_pool ADD COLUMN transport TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_pool ADD COLUMN credential_mode TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_pool ADD COLUMN meta TEXT NOT NULL DEFAULT '{}'`,

		// 邮件正文落哪个库。空表示按 provider 推导（见 GroupOf）。
		`ALTER TABLE mail_pool ADD COLUMN db_group TEXT NOT NULL DEFAULT ''`,

		// 源的独立配置：不同类邮箱的限速/批量/退避完全不同。
		// 240 个 Outlook 用 interval=10s、idle=1 会起 240 个常连接，
		// 必须能按源单独调。
		`ALTER TABLE mail_sources ADD COLUMN db_group TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE mail_sources ADD COLUMN concurrency INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE mail_sources ADD COLUMN batch_size INTEGER NOT NULL DEFAULT 50`,
		`ALTER TABLE mail_sources ADD COLUMN retry_limit INTEGER NOT NULL DEFAULT 3`,
		`ALTER TABLE mail_sources ADD COLUMN backoff_seconds INTEGER NOT NULL DEFAULT 30`,
		`ALTER TABLE mail_sources ADD COLUMN priority INTEGER NOT NULL DEFAULT 100`,
	}
	for _, q := range alters2 {
		if _, err := d.ex.Exec(q); err != nil {
			if isDuplicateColumn(err) {
				continue
			}
			return fmt.Errorf("migrate parent: %w", err)
		}
	}

	return nil
}

// --- messages --------------------------------------------------------------

// Message is the read model used by both the app UI and the API.
type Message struct {
	ID         int64    `json:"id"`
	SourceID   int64    `json:"sourceId"`
	UID        string   `json:"uid"`
	Subject    string   `json:"subject"`
	From       string   `json:"from"`
	To         string   `json:"to"`
	Date       string   `json:"date"`
	Snippet    string   `json:"snippet"`
	BodyText   string   `json:"text,omitempty"`
	Tags       []string `json:"tags"`
	PrimaryTag string   `json:"primaryTag"`
	Alias      string   `json:"alias"`
	SentAt     *float64 `json:"sentAt"`
}

// MessageRecord is the write shape for one indexed mail.
type MessageRecord struct {
	Recipients []string
	Mailbox    string
	UID        string
	SourceID   int64
	AliasHME   string
	FromAddr   string
	ToHeader   string
	Subject    string
	SentAt     *float64
	MessageID  string
	Tags       []string
	PrimaryTag string
	Score      int
	Snippet    string
	BodyText   string
}

// StoreMessage inserts one message; false means the uid was already present.
func (d *DB) StoreMessage(rec MessageRecord) (bool, error) {
	return d.storeMessage(rec, nil)
}

func (d *DB) StoreSourceMessage(src Source, rec MessageRecord) (bool, error) {
	return d.storeMessage(rec, &src)
}

func (d *DB) storeMessage(rec MessageRecord, src *Source) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if src != nil {
		if err := d.checkSource(*src); err != nil {
			return false, err
		}
	}

	aliasKey := strings.ToLower(strings.TrimSpace(rec.AliasHME))
	now := float64(time.Now().Unix())
	sortTS := now
	if rec.SentAt != nil {
		sortTS = *rec.SentAt
	}
	tagsJSON, _ := json.Marshal(rec.Tags)
	sourceID := rec.SourceID
	if sourceID == 0 {
		sourceID = 1
	}
	dedupe := fmt.Sprintf("%d:%s", sourceID, rec.UID)

	tx, err := d.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	err = d.ex.QueryRowTx(tx, `
INSERT INTO mail_messages(mailbox, uid, dedupe_key, alias_hme, alias_key, source_id, from_addr,
  to_header, subject, sent_at, sort_ts, message_id, tags, primary_tag, score, snippet, body_text, indexed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(dedupe_key) DO NOTHING RETURNING id`,
		rec.Mailbox, rec.UID, dedupe, nullStr(rec.AliasHME), nullStr(aliasKey), sourceID, rec.FromAddr,
		rec.ToHeader, rec.Subject, rec.SentAt, sortTS, rec.MessageID, string(tagsJSON),
		rec.PrimaryTag, rec.Score, rec.Snippet, rec.BodyText, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if aliasKey != "" {
		risk := 0
		if rec.PrimaryTag == "banned" || rec.PrimaryTag == "risk" {
			risk = 1
		}
		if _, err := d.ex.ExecTxArgs(tx, `
INSERT INTO mail_alias_counters(alias_key, total, risk_hits, last_ts, updated_at)
VALUES(?,1,?,?,?)
ON CONFLICT(alias_key) DO UPDATE SET
  total = mail_alias_counters.total + 1,
  risk_hits = mail_alias_counters.risk_hits + excluded.risk_hits,
  last_ts = CASE WHEN COALESCE(mail_alias_counters.last_ts,0)>excluded.last_ts THEN mail_alias_counters.last_ts ELSE excluded.last_ts END,
  updated_at = excluded.updated_at`, aliasKey, risk, sortTS, now); err != nil {
			return true, err
		}
	}
	recipients := append(append([]string{}, rec.Recipients...), extractAddresses(rec.ToHeader+","+rec.AliasHME)...)
	if err := indexRecipients(d, tx, id, sourceID, recipients); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Query returns messages newest first.
func (d *DB) Query(alias, tag string, limit int) ([]Message, error) {
	items, _, err := d.MessagePage(context.Background(), alias, tag, 0, limit, 0)
	return items, err
}

// Totals reports indexed counts.
func (d *DB) Totals() (int, uint32, error) {
	var total int
	var last sql.NullInt64
	if err := d.ex.QueryRow("SELECT COUNT(*), MAX(CAST(uid AS INTEGER)) FROM mail_messages").Scan(&total, &last); err != nil {
		return 0, 0, err
	}
	if !last.Valid {
		return total, 0, nil
	}
	return total, uint32(last.Int64), nil
}

// CountsByTag returns the tag histogram.
func (d *DB) CountsByTag() (map[string]int, error) {
	rows, err := d.ex.Query("SELECT primary_tag, COUNT(*) FROM mail_messages GROUP BY primary_tag ORDER BY 2 DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var tag string
		var n int
		if err := rows.Scan(&tag, &n); err != nil {
			return nil, err
		}
		out[tag] = n
	}
	return out, rows.Err()
}

// --- sources ---------------------------------------------------------------

// Source owns one mailbox and its selected transport; credentials never go in
// address-pool rows. The same source may receive mail for multiple aliases.
type Source struct {
	ID               int64
	Name             string
	Host             string
	Port             int
	Username         string
	Password         string
	Folder           string
	Enabled          bool
	Idle             bool
	IntervalSeconds  int
	HeartbeatSeconds int
	LastUID          uint32
	LastOKAt         *time.Time
	LastIdleAt       *time.Time
	LastError        string

	// Supported source transports are IMAP/POP3 with an app password, or
	// Microsoft Graph with ClientID + RefreshToken. Temporary API providers
	// belong to the worker catalog, not imaginary source runner implementations.
	// Endpoint/JWT/APIKey/Meta retain previously saved material for export;
	// they do not advertise additional executable source protocols.
	CredentialMode string
	Transport      string
	Endpoint       string
	RefreshToken   string
	ClientID       string
	JWT            string
	APIKey         string
	// Meta 放 provider 特有字段的 JSON
	Meta string
}

// Addr is the host:port dial target.
func (s Source) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// UpsertSource owns validation and secret-preserving saves for all transports.
func (d *DB) UpsertSource(src Source) (Source, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if moved, err := d.MailboxTransferred(src.Username); err != nil {
		return Source{}, err
	} else if moved {
		return Source{}, ErrMailboxUnavailable
	}
	now := float64(time.Now().Unix())
	id := src.ID
	if id == 0 {
		err := d.ex.QueryRow("SELECT id FROM mail_sources WHERE name=?", src.Name).Scan(&id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Source{}, err
		}
	}
	var old Source
	if id != 0 {
		var err error
		old, err = d.getSource(id)
		if err != nil {
			return Source{}, err
		}
		if src.Password == "" {
			src.Password = old.Password
		}
		if src.RefreshToken == "" {
			src.RefreshToken = old.RefreshToken
		}
		if src.ClientID == "" {
			src.ClientID = old.ClientID
		}
	}
	src.Transport = defaultStr(src.Transport, TransportIMAP)
	src.Folder = defaultStr(src.Folder, "INBOX")
	if strings.TrimSpace(src.Name) == "" {
		return Source{}, errors.New("请填写来源名称")
	}
	if src.Transport != TransportCloudmail && strings.TrimSpace(src.Username) == "" {
		return Source{}, errors.New("请填写登录账号")
	}
	switch src.Transport {
	case TransportCloudmail:
		src.CredentialMode, src.Idle = "address_jwt", false
		if strings.TrimSpace(src.Endpoint) == "" {
			return Source{}, errors.New("请填写 cloudmail 接口地址")
		}
	case TransportIMAP, TransportPOP3:
		src.CredentialMode = "password"
		if src.Host == "" || src.Password == "" || strings.ContainsAny(src.Host, " /\\\r\n") || strings.ContainsAny(src.Username+src.Password, "\r\n") {
			return Source{}, errors.New("请填写邮件服务器和应用专用密码")
		}
		if src.Port == 0 {
			if src.Transport == TransportPOP3 {
				src.Port = 995
			} else {
				src.Port = 993
			}
		}
		if src.Port < 1 || src.Port > 65535 {
			return Source{}, errors.New("邮件端口应为 1–65535")
		}
		if src.Transport == TransportPOP3 {
			src.Idle = false
		}
	case TransportGraph:
		src.CredentialMode, src.Idle = "oauth_refresh", false
		if src.ClientID == "" || src.RefreshToken == "" {
			return Source{}, errors.New("请填写 Microsoft Client ID 和邮箱 Refresh Token")
		}
	default:
		return Source{}, errors.New("请选择 IMAP、POP3 或 Microsoft Graph 收信；临时邮箱 API 在邮箱来源配置中管理")
	}
	if id == 0 {
		err := d.ex.QueryRow(`INSERT INTO mail_sources(
			name,host,port,username,password,folder,enabled,idle,
			interval_seconds,heartbeat_seconds,created_at,updated_at,
			credential_mode,transport,endpoint,refresh_token,client_id,jwt,api_key,meta
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`,
			src.Name, src.Host, defaultInt(src.Port, 993), src.Username, src.Password,
			defaultStr(src.Folder, "INBOX"), boolInt(src.Enabled), boolInt(src.Idle),
			defaultInt(src.IntervalSeconds, 10), defaultInt(src.HeartbeatSeconds, 60),
			now, now,
			defaultStr(src.CredentialMode, "password"),
			defaultStr(src.Transport, "imap"),
			src.Endpoint, src.RefreshToken, src.ClientID, src.JWT, src.APIKey,
			defaultStr(src.Meta, "{}")).Scan(&id)
		if err != nil {
			return Source{}, err
		}
	} else {
		var err error
		// OAuth application/credential changes re-authorize the same mailbox.
		// Only a changed message namespace invalidates its existing cursor.
		changed := old.Username != src.Username || old.Folder != src.Folder || old.Host != src.Host || old.Port != defaultInt(src.Port, 993) || old.Transport != src.Transport
		if changed {
			var count int
			if err = d.ex.QueryRow("SELECT count(*) FROM mail_messages WHERE source_id=?", id).Scan(&count); err != nil {
				return Source{}, err
			}
			if count > 0 {
				return Source{}, errors.New("此来源已有收信记录；更换账号、服务器或文件夹请新建来源，原记录会保留")
			}
		}
		_, err = d.ex.Exec(`UPDATE mail_sources SET
			name=?,host=?,port=?,username=?,password=?,folder=?,enabled=?,idle=?,
			interval_seconds=?,heartbeat_seconds=?,updated_at=?,
			credential_mode=?,transport=?,endpoint=?,
			refresh_token=CASE WHEN ?<>'' THEN ? ELSE refresh_token END,
			client_id=CASE WHEN ?<>'' THEN ? ELSE client_id END,
			jwt=CASE WHEN ?<>'' THEN ? ELSE jwt END,
			api_key=CASE WHEN ?<>'' THEN ? ELSE api_key END,
			meta=CASE WHEN ?<>'' AND ?<>'{}' THEN ? ELSE meta END,
			last_uid=CASE WHEN ? THEN 0 ELSE last_uid END
			WHERE id=?`,
			src.Name, src.Host, defaultInt(src.Port, 993), src.Username, src.Password,
			defaultStr(src.Folder, "INBOX"), boolInt(src.Enabled), boolInt(src.Idle),
			defaultInt(src.IntervalSeconds, 10), defaultInt(src.HeartbeatSeconds, 60),
			now,
			defaultStr(src.CredentialMode, "password"),
			defaultStr(src.Transport, "imap"),
			src.Endpoint,
			src.RefreshToken, src.RefreshToken,
			src.ClientID, src.ClientID,
			src.JWT, src.JWT,
			src.APIKey, src.APIKey,
			src.Meta, src.Meta, src.Meta,
			changed, id)
		if err != nil {
			return Source{}, err
		}
	}
	return d.getSource(id)
}

// ListSources returns enabled sources.
func (d *DB) ListSources(onlyEnabled bool) ([]Source, error) {
	q := "SELECT id, name, host, port, username, password, folder, enabled, idle, interval_seconds," +
		" heartbeat_seconds, last_uid, last_ok_at, last_idle_at, last_error, " +
		"COALESCE(credential_mode,'password'), COALESCE(transport,'imap'), COALESCE(endpoint,''), COALESCE(refresh_token,''), COALESCE(client_id,''), COALESCE(jwt,''), COALESCE(api_key,''), COALESCE(meta,'{}') FROM mail_sources"
	if onlyEnabled {
		q += " WHERE enabled = 1"
	}
	q += " ORDER BY id"
	rows, err := d.ex.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var (
			s        Source
			enabled  int
			idle     int
			lastOK   sql.NullFloat64
			lastIdle sql.NullFloat64
			lastErr  sql.NullString
		)
		if err := rows.Scan(&s.ID, &s.Name, &s.Host, &s.Port, &s.Username, &s.Password, &s.Folder,
			&enabled, &idle, &s.IntervalSeconds, &s.HeartbeatSeconds, &s.LastUID, &lastOK, &lastIdle, &lastErr,
			&s.CredentialMode, &s.Transport, &s.Endpoint, &s.RefreshToken, &s.ClientID, &s.JWT, &s.APIKey, &s.Meta); err != nil {
			return nil, err
		}
		s.Enabled = enabled == 1
		s.Idle = idle == 1
		if lastOK.Valid {
			t := time.Unix(int64(lastOK.Float64), 0)
			s.LastOKAt = &t
		}
		if lastIdle.Valid {
			t := time.Unix(int64(lastIdle.Float64), 0)
			s.LastIdleAt = &t
		}
		s.LastError = lastErr.String
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) getSource(id int64) (Source, error) {
	var (
		s        Source
		enabled  int
		idle     int
		lastOK   sql.NullFloat64
		lastIdle sql.NullFloat64
		lastErr  sql.NullString
	)
	err := d.ex.QueryRow(`SELECT id, name, host, port, username, password, folder, enabled, idle,
  interval_seconds, heartbeat_seconds, last_uid, last_ok_at, last_idle_at, last_error,
  COALESCE(credential_mode,'password'), COALESCE(transport,'imap'), COALESCE(endpoint,''), COALESCE(refresh_token,''), COALESCE(client_id,''), COALESCE(jwt,''), COALESCE(api_key,''), COALESCE(meta,'{}')
  FROM mail_sources WHERE id = ?`, id).Scan(&s.ID, &s.Name, &s.Host, &s.Port, &s.Username, &s.Password,
		&s.Folder, &enabled, &idle, &s.IntervalSeconds, &s.HeartbeatSeconds, &s.LastUID, &lastOK, &lastIdle, &lastErr,
		&s.CredentialMode, &s.Transport, &s.Endpoint, &s.RefreshToken, &s.ClientID, &s.JWT, &s.APIKey, &s.Meta)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Source{}, ErrNotFound
		}
		return Source{}, err
	}
	s.Enabled = enabled == 1
	s.Idle = idle == 1
	if lastOK.Valid {
		t := time.Unix(int64(lastOK.Float64), 0)
		s.LastOKAt = &t
	}
	if lastIdle.Valid {
		t := time.Unix(int64(lastIdle.Float64), 0)
		s.LastIdleAt = &t
	}
	s.LastError = lastErr.String
	return s, nil
}

// Cursor persists runner progress for one source.
func (d *DB) Cursor(src Source, lastUID *uint32, lastOK *time.Time, lastErr string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkSource(src); err != nil {
		return err
	}
	sets := []string{"updated_at = ?"}
	args := []any{float64(time.Now().Unix())}
	if lastUID != nil {
		sets = append(sets, "last_uid = ?")
		args = append(args, *lastUID)
	}
	if lastOK != nil {
		sets = append(sets, "last_ok_at = ?")
		args = append(args, float64(lastOK.Unix()))
	}
	if lastErr != "" {
		sets = append(sets, "last_error = ?")
		args = append(args, lastErr)
	} else if lastOK != nil {
		sets = append(sets, "last_error = NULL")
	}
	args = append(args, src.ID)
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = d.ex.ExecTxArgs(tx, "UPDATE mail_sources SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
		return err
	}
	if lastOK != nil {
		// Successful access is the recovery fact for expired mailbox login.
		// Administrative pause, parent shutdown and platform bans stay separate.
		if _, err = d.ex.ExecTxArgs(tx, `UPDATE mail_pool SET state='ready',last_error='',last_error_code=''
 WHERE source_id=? AND state='auth_failed'`, src.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var ErrSourceChanged = errors.New("收件来源已变更或停用，原连接停止写入；新配置由收件器自动接续")

// Called under the writer lock, so a configuration update cannot cross the
// final ownership check and attribute old connection data to a new mailbox.
func (d *DB) checkSource(src Source) error {
	current, err := d.getSource(src.ID)
	if errors.Is(err, ErrNotFound) {
		return ErrSourceChanged
	}
	if err != nil {
		return err
	}
	if !current.Enabled || !sameSource(current, src) {
		return ErrSourceChanged
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func nullStr(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func defaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// Stats is a small health payload for the app card.
func (d *DB) Stats(ctx context.Context) (map[string]any, error) {
	total, lastUID, err := d.Totals()
	if err != nil {
		return nil, err
	}
	tags, err := d.CountsByTag()
	if err != nil {
		return nil, err
	}
	sources, err := d.ListSources(false)
	if err != nil {
		return nil, err
	}
	_ = ctx
	return map[string]any{
		"indexed": total,
		"lastUid": lastUID,
		"byTag":   tags,
		"sources": len(sources),
	}, nil
}

// PruneOlderThan deletes messages older than the cutoff and rebuilds counters.
func (d *DB) PruneOlderThan(days int) error {
	if days <= 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := float64(time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix())
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := d.ex.ExecTxArgs(tx, "DELETE FROM mail_recipients WHERE message_id IN (SELECT id FROM mail_messages WHERE sort_ts < ?)", cutoff); err != nil {
		return err
	}
	if _, err := d.ex.ExecTxArgs(tx, "DELETE FROM mail_messages WHERE sort_ts < ?", cutoff); err != nil {
		return err
	}
	_, err = d.ex.ExecTx(tx, `DELETE FROM mail_alias_counters;
INSERT INTO mail_alias_counters(alias_key, total, risk_hits, last_ts, updated_at)
SELECT alias_key, COUNT(*), SUM(CASE WHEN primary_tag IN ('banned','risk') THEN 1 ELSE 0 END),
       MAX(sort_ts), `+storepkg.NowSeconds(d.dialect)+`
FROM mail_messages WHERE alias_key IS NOT NULL AND alias_key != '' GROUP BY alias_key`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// --- aliases / sources management -----------------------------------------

var addrInHeader = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// extractAddresses 从 To 头里取出邮箱（兼容 "Name <a@b.com>, c@d.com"）。
func extractAddresses(header string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 2)
	for _, match := range addrInHeader.FindAllString(header, -1) {
		addr := strings.ToLower(strings.TrimSpace(match))
		if addr != "" && !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	return out
}

// SetSourceEnabled 启停一个 IMAP 源。
func (d *DB) SetSourceEnabled(ctx context.Context, id int64, enabled bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if enabled {
		src, err := d.getSource(id)
		if err != nil {
			return err
		}
		if moved, err := d.MailboxTransferred(src.Username); err != nil {
			return err
		} else if moved {
			return ErrMailboxUnavailable
		}
	}
	result, err := d.ex.ExecContext(ctx, `UPDATE mail_sources SET enabled=? WHERE id=?`, boolInt(enabled), id)
	return sourceWriteResult(result, err)
}

// DeleteSource 删除一个 IMAP 源（不动已索引的消息）。
func (d *DB) DeleteSource(ctx context.Context, id int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	result, err := d.ex.ExecContext(ctx, `DELETE FROM mail_sources WHERE id=?`, id)
	return sourceWriteResult(result, err)
}

func sourceWriteResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// isDuplicateColumn 判断「列已存在」错误。
//
// SQLite:  duplicate column name: xxx
// PG:      column "xxx" of relation "yyy" already exists (SQLSTATE 42701)
//
// 两种都要认，否则迁移在第二个后端上必失败。
func isDuplicateColumn(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "42701")
}
