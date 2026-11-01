package icmail

// Store is the IC 邮箱 app's own SQLite database. It lives next to regd's data
// (same /data volume) but in a separate file so the app can be dropped or
// rebuilt without touching reg-factory's own tables.
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
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row is absent.
var ErrNotFound = errors.New("not found")

// DB wraps the sqlite handle with the queries the app needs.
type DB struct {
	db   *sql.DB
	path string
	mu   sync.Mutex // writers serialise; readers are concurrent (WAL)
}

// OpenDB prepares the app database.
func OpenDB(path string) (*DB, error) {
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
	d := &DB{db: handle, path: path}
	if err := d.ensureSchema(); err != nil {
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
    UNIQUE(mailbox, uid)
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
	_, err := d.db.Exec(schema)
	return err
}

// --- messages --------------------------------------------------------------

// Message is the read model used by both the app UI and the API.
type Message struct {
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
	d.mu.Lock()
	defer d.mu.Unlock()

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

	res, err := d.db.Exec(`
INSERT INTO mail_messages(mailbox, uid, dedupe_key, alias_hme, alias_key, source_id, from_addr,
  to_header, subject, sent_at, sort_ts, message_id, tags, primary_tag, score, snippet, body_text, indexed_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(dedupe_key) DO NOTHING`,
		rec.Mailbox, rec.UID, dedupe, nullStr(rec.AliasHME), nullStr(aliasKey), sourceID, rec.FromAddr,
		rec.ToHeader, rec.Subject, rec.SentAt, sortTS, rec.MessageID, string(tagsJSON),
		rec.PrimaryTag, rec.Score, rec.Snippet, rec.BodyText, now)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return false, nil
	}
	if aliasKey != "" {
		risk := 0
		if rec.PrimaryTag == "banned" || rec.PrimaryTag == "risk" {
			risk = 1
		}
		if _, err := d.db.Exec(`
INSERT INTO mail_alias_counters(alias_key, total, risk_hits, last_ts, updated_at)
VALUES(?,1,?,?,?)
ON CONFLICT(alias_key) DO UPDATE SET
  total = mail_alias_counters.total + 1,
  risk_hits = mail_alias_counters.risk_hits + excluded.risk_hits,
  last_ts = MAX(COALESCE(mail_alias_counters.last_ts, 0), excluded.last_ts),
  updated_at = excluded.updated_at`, aliasKey, risk, sortTS, now); err != nil {
			return true, err
		}
	}
	return true, nil
}

// Query returns messages newest first.
func (d *DB) Query(alias, tag string, limit int) ([]Message, error) {
	clauses := []string{}
	args := []any{}
	if alias != "" {
		clauses = append(clauses, "alias_key = ?")
		args = append(args, strings.ToLower(strings.TrimSpace(alias)))
	}
	if tag != "" {
		clauses = append(clauses, "primary_tag = ?")
		args = append(args, tag)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	args = append(args, limit)
	rows, err := d.db.Query(
		"SELECT uid, subject, from_addr, to_header, sent_at, snippet, body_text, tags, primary_tag, alias_hme"+
			" FROM mail_messages"+where+" ORDER BY sort_ts DESC, id DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var (
			m       Message
			sentAt  sql.NullFloat64
			tagsRaw string
			aliasDB sql.NullString
		)
		if err := rows.Scan(&m.UID, &m.Subject, &m.From, &m.To, &sentAt, &m.Snippet, &m.BodyText,
			&tagsRaw, &m.PrimaryTag, &aliasDB); err != nil {
			return nil, err
		}
		if sentAt.Valid {
			v := sentAt.Float64
			m.SentAt = &v
			m.Date = time.Unix(int64(v), 0).UTC().Format(time.RFC1123Z)
		}
		if aliasDB.Valid {
			m.Alias = aliasDB.String
		}
		_ = json.Unmarshal([]byte(tagsRaw), &m.Tags)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Totals reports indexed counts.
func (d *DB) Totals() (int, uint32, error) {
	var total int
	var last sql.NullInt64
	if err := d.db.QueryRow("SELECT COUNT(*), MAX(CAST(uid AS INTEGER)) FROM mail_messages").Scan(&total, &last); err != nil {
		return 0, 0, err
	}
	if !last.Valid {
		return total, 0, nil
	}
	return total, uint32(last.Int64), nil
}

// CountsByTag returns the tag histogram.
func (d *DB) CountsByTag() (map[string]int, error) {
	rows, err := d.db.Query("SELECT primary_tag, COUNT(*) FROM mail_messages GROUP BY primary_tag ORDER BY 2 DESC")
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

// Source is one IMAP account + folder.
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
}

// Addr is the host:port dial target.
func (s Source) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// UpsertSource seeds (or updates) one IMAP account by name.
func (d *DB) UpsertSource(src Source) (Source, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := float64(time.Now().Unix())
	var id int64
	err := d.db.QueryRow("SELECT id FROM mail_sources WHERE name = ?", src.Name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := d.db.Exec(`
INSERT INTO mail_sources(name, host, port, username, password, folder, enabled, idle,
  interval_seconds, heartbeat_seconds, created_at, updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			src.Name, src.Host, defaultInt(src.Port, 993), src.Username, src.Password,
			defaultStr(src.Folder, "INBOX"), boolInt(src.Enabled), boolInt(src.Idle),
			defaultInt(src.IntervalSeconds, 10), defaultInt(src.HeartbeatSeconds, 60), now, now)
		if err != nil {
			return Source{}, err
		}
		id, _ = res.LastInsertId()
	} else if err != nil {
		return Source{}, err
	} else {
		if _, err := d.db.Exec(`
UPDATE mail_sources SET host=?, port=?, username=?, password=?, folder=?, enabled=?, idle=?,
  interval_seconds=?, heartbeat_seconds=?, updated_at=? WHERE id=?`,
			src.Host, defaultInt(src.Port, 993), src.Username, src.Password,
			defaultStr(src.Folder, "INBOX"), boolInt(src.Enabled), boolInt(src.Idle),
			defaultInt(src.IntervalSeconds, 10), defaultInt(src.HeartbeatSeconds, 60), now, id); err != nil {
			return Source{}, err
		}
	}
	return d.getSource(id)
}

// ListSources returns enabled sources.
func (d *DB) ListSources(onlyEnabled bool) ([]Source, error) {
	q := "SELECT id, name, host, port, username, password, folder, enabled, idle, interval_seconds," +
		" heartbeat_seconds, last_uid, last_ok_at, last_idle_at, last_error FROM mail_sources"
	if onlyEnabled {
		q += " WHERE enabled = 1"
	}
	q += " ORDER BY id"
	rows, err := d.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var (
			s          Source
			enabled    int
			idle       int
			lastOK     sql.NullFloat64
			lastIdle   sql.NullFloat64
			lastErr    sql.NullString
		)
		if err := rows.Scan(&s.ID, &s.Name, &s.Host, &s.Port, &s.Username, &s.Password, &s.Folder,
			&enabled, &idle, &s.IntervalSeconds, &s.HeartbeatSeconds, &s.LastUID, &lastOK, &lastIdle, &lastErr); err != nil {
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
	err := d.db.QueryRow(`SELECT id, name, host, port, username, password, folder, enabled, idle,
  interval_seconds, heartbeat_seconds, last_uid, last_ok_at, last_idle_at, last_error
  FROM mail_sources WHERE id = ?`, id).Scan(&s.ID, &s.Name, &s.Host, &s.Port, &s.Username, &s.Password,
		&s.Folder, &enabled, &idle, &s.IntervalSeconds, &s.HeartbeatSeconds, &s.LastUID, &lastOK, &lastIdle, &lastErr)
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
func (d *DB) Cursor(id int64, lastUID *uint32, lastOK *time.Time, lastErr string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
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
	args = append(args, id)
	_, err := d.db.Exec("UPDATE mail_sources SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	return err
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
	if _, err := d.db.Exec("DELETE FROM mail_messages WHERE sort_ts < ?", cutoff); err != nil {
		return err
	}
	_, err := d.db.Exec(`DELETE FROM mail_alias_counters;
INSERT INTO mail_alias_counters(alias_key, total, risk_hits, last_ts, updated_at)
SELECT alias_key, COUNT(*), SUM(CASE WHEN primary_tag IN ('banned','risk') THEN 1 ELSE 0 END),
       MAX(sort_ts), strftime('%s','now')
FROM mail_messages WHERE alias_key IS NOT NULL AND alias_key != '' GROUP BY alias_key`)
	return err
}