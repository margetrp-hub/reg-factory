package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	storepkg "github.com/tiantianGPU/reg-factory/server/internal/store"
)

// Pool addresses exist independently of messages. Pausing or blocking an
// address stops code lookup, but never discards its mail or source history.
type Mailbox struct {
	Email         string  `json:"email"`
	SourceID      int64   `json:"sourceId"`
	SourceName    string  `json:"sourceName"`
	SourceEnabled bool    `json:"sourceEnabled"`
	State         string  `json:"state"`
	Origin        string  `json:"origin"`
	Total         int     `json:"total"`
	RiskHits      int     `json:"riskHits"`
	LastAt        float64 `json:"lastAt"`
	LastTag       string  `json:"lastTag"`
	Manual        bool    `json:"manual"`
	UpdatedAt     float64 `json:"updatedAt"`

	// ---- 新增：多 provider / 母号级联 ----
	Provider       string `json:"provider,omitempty"`
	Role           string `json:"role,omitempty"`
	DBGroup        string `json:"dbGroup,omitempty"`
	Transport      string `json:"transport,omitempty"`
	CredentialMode string `json:"credentialMode,omitempty"`
	ParentID       string `json:"parentId,omitempty"`
	ParentState    string `json:"parentState,omitempty"`
	LastError      string `json:"lastError,omitempty"`
	LastErrorCode  string `json:"lastErrorCode,omitempty"`
	FailCount      int    `json:"failCount"`
	MessageCount   int    `json:"messageCount"`
}

func (a *App) handlePool(c *gin.Context) {
	items, total, err := a.DB.PoolPage(c.Request.Context(), c.Query("q"), c.Query("state"), int64(atoiDefault(c.Query("source_id"), 0)), atoiDefault(c.Query("limit"), 25), atoiDefault(c.Query("offset"), 0))
	if err != nil {
		failJSON(c, 500, "POOL_READ_FAILED", "邮箱池读取失败")
		return
	}
	okJSON(c, gin.H{"items": items, "total": total})
}

func (a *App) handlePoolImport(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 384000)
	var body struct {
		SourceID int64    `json:"source_id"`
		Emails   []string `json:"emails"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		failJSON(c, 400, "BAD_REQUEST", "请逐行填写邮箱地址")
		return
	}
	count, err := a.DB.ImportMailboxes(c.Request.Context(), body.SourceID, body.Emails)
	if err != nil {
		failJSON(c, 400, "POOL_IMPORT_FAILED", err.Error())
		return
	}
	a.notifySources()
	okJSON(c, gin.H{"added": count})
}

func (a *App) handleMailbox(c *gin.Context) {
	var body struct {
		Email    string `json:"email"`
		SourceID int64  `json:"source_id"`
		State    string `json:"state"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		failJSON(c, 400, "BAD_REQUEST", "邮箱配置无效")
		return
	}
	if err := a.DB.SetMailbox(c.Request.Context(), strings.ToLower(strings.TrimSpace(body.Email)), body.SourceID, body.State); err != nil {
		failJSON(c, 409, "MAILBOX_NOT_SAVED", err.Error())
		return
	}
	a.notifySources()
	okJSON(c, gin.H{"saved": true})
}

func (a *App) handleSyncRuns(c *gin.Context) {
	items, total, err := a.DB.SyncPage(c.Request.Context(), c.Query("request_id"), atoiDefault(c.Query("limit"), 25), atoiDefault(c.Query("offset"), 0))
	if err != nil {
		failJSON(c, 500, "SYNC_READ_FAILED", "同步记录读取失败")
		return
	}
	okJSON(c, gin.H{"items": items, "total": total})
}

func (d *DB) ensurePool() error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var definition string
	if definition, err = d.ex.BodyTextLen(); err != nil {
		return err
	}
	// The old global folder+UID constraint rejected a second source's INBOX.
	// Copy all columns and IDs in one transaction; message contents stay intact.
	if strings.Contains(definition, "UNIQUE(mailbox, uid)") {
		definition = strings.Replace(definition, "mail_messages", "mail_messages_pool_v2", 1)
		definition = strings.Replace(definition, "UNIQUE(mailbox, uid)", "UNIQUE(source_id, mailbox, uid)", 1)
		if _, err = tx.Exec(definition); err != nil {
			return err
		}
		if _, err = d.ex.ExecTx(tx, `INSERT INTO mail_messages_pool_v2 SELECT * FROM mail_messages;
DROP TABLE mail_messages;
ALTER TABLE mail_messages_pool_v2 RENAME TO mail_messages;`); err != nil {
			return err
		}
	}
	if _, err = d.ex.ExecTx(tx, `
CREATE UNIQUE INDEX IF NOT EXISTS ux_icmail_dedupe ON mail_messages(dedupe_key);
CREATE INDEX IF NOT EXISTS idx_icmail_alias ON mail_messages(alias_key, sort_ts DESC);
CREATE INDEX IF NOT EXISTS idx_icmail_tag ON mail_messages(primary_tag, sort_ts DESC);
CREATE INDEX IF NOT EXISTS idx_icmail_sort ON mail_messages(sort_ts DESC);
CREATE TABLE IF NOT EXISTS mail_pool (
 email TEXT PRIMARY KEY, source_id INTEGER, origin TEXT NOT NULL DEFAULT 'imported', created_at REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mail_pool_source ON mail_pool(source_id,email);
CREATE TABLE IF NOT EXISTS mail_recipients (
 message_id INTEGER NOT NULL, email TEXT NOT NULL, PRIMARY KEY(message_id,email)
);
CREATE INDEX IF NOT EXISTS idx_mail_recipient_email ON mail_recipients(email,message_id);
CREATE TABLE IF NOT EXISTS mail_pool_meta (name TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS mail_transferred(email TEXT PRIMARY KEY);
`); err != nil {
		return err
	}
	var ready int
	if err = d.ex.QueryRowTx(tx, "SELECT count(*) FROM mail_pool_meta WHERE name='alias-state-v1'").Scan(&ready); err != nil {
		return err
	}
	if ready == 0 {
		// Keep the latest state and its evidence when old addresses differ only
		// in case/whitespace. Resolve ties conservatively, then use indexed keys.
		if _, err = d.ex.ExecTx(tx, `INSERT INTO mail_alias_state(alias_hme,state,manual,hits,last_tag,last_hit_at,updated_at)
SELECT email,state,manual,hits,last_tag,last_hit_at,updated_at FROM (
 SELECT lower(trim(alias_hme)) AS email,state,manual,hits,last_tag,last_hit_at,updated_at,
 row_number() OVER (PARTITION BY lower(trim(alias_hme)) ORDER BY updated_at DESC,manual DESC,
 CASE state WHEN 'blocked' THEN 2 WHEN 'paused' THEN 1 ELSE 0 END DESC,alias_hme) AS rank
 FROM mail_alias_state
) WHERE rank=1
ON CONFLICT(alias_hme) DO UPDATE SET state=excluded.state,manual=excluded.manual,hits=excluded.hits,
 last_tag=excluded.last_tag,last_hit_at=excluded.last_hit_at,updated_at=excluded.updated_at;
DELETE FROM mail_alias_state WHERE alias_hme != lower(trim(alias_hme));
INSERT INTO mail_pool_meta(name) VALUES('alias-state-v1');`); err != nil {
			return err
		}
	}
	if err = d.ex.QueryRowTx(tx, "SELECT count(*) FROM mail_pool_meta WHERE name='recipients-v1'").Scan(&ready); err != nil {
		return err
	}
	if ready == 0 {
		if _, err = d.ex.ExecTx(tx, storepkg.UpsertIgnore(d.dialect, `INSERT INTO mail_pool(email,origin,created_at)
SELECT lower(alias_hme),'manual',updated_at FROM mail_alias_state WHERE 1=1`)); err != nil {
			return err
		}
		rows, err := d.ex.QueryTx(tx, "SELECT id, source_id, COALESCE(to_header,''), COALESCE(alias_hme,'') FROM mail_messages")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, source int64
			var header, alias string
			if err = rows.Scan(&id, &source, &header, &alias); err != nil {
				rows.Close()
				return err
			}
			if err = indexRecipients(d, tx, id, source, extractAddresses(header+","+alias)); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO mail_pool_meta(name) VALUES('recipients-v1')"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func indexRecipients(d *DB, tx *sql.Tx, messageID, sourceID int64, recipients []string) error {
	for _, address := range recipients {
		email := strings.ToLower(strings.TrimSpace(address))
		if !looksLikeEmail(email) {
			continue
		}
		insRecip := storepkg.UpsertIgnore(d.dialect, "INSERT INTO mail_recipients(message_id,email) VALUES(?,?)")
		if _, err := d.ex.ExecTxArgs(tx, insRecip, messageID, email); err != nil {
			return err
		}
		insPool := storepkg.UpsertIgnore(d.dialect, `INSERT INTO mail_pool(email,source_id,origin,created_at) VALUES(?,?,'discovered',?)`)
		if _, err := d.ex.ExecTxArgs(tx, insPool, email, sourceID, float64(time.Now().Unix())); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) ImportMailboxes(ctx context.Context, sourceID int64, addresses []string) (int, error) {
	if len(addresses) == 0 || len(addresses) > 1000 {
		return 0, errors.New("每次导入 1–1000 个邮箱地址")
	}
	normalized := map[string]bool{}
	for _, email := range addresses {
		email = strings.ToLower(strings.TrimSpace(email))
		if !looksLikeEmail(email) {
			return 0, errors.New("包含无效邮箱，请逐行填写邮箱地址")
		}
		normalized[email] = true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if sourceID > 0 {
		var id int64
		if err = d.ex.QueryRowTx(tx, "SELECT id FROM mail_sources WHERE id=?", sourceID).Scan(&id); err != nil {
			return 0, errors.New("收件来源不存在")
		}
	}
	count := 0
	for email := range normalized {
		res, err := d.ex.ExecTxArgs(tx, `INSERT INTO mail_pool(email,source_id,origin,created_at) VALUES(?,NULLIF(?,0),'imported',?)
ON CONFLICT(email) DO UPDATE SET origin='imported',source_id=COALESCE(mail_pool.source_id,excluded.source_id)
WHERE mail_pool.origin='discovered' AND (excluded.source_id IS NULL OR mail_pool.source_id IS NULL OR mail_pool.source_id=excluded.source_id)`, email, sourceID, float64(time.Now().Unix()))
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		count += int(n)
	}
	return count, tx.Commit()
}

func (d *DB) SetMailbox(ctx context.Context, email string, sourceID int64, state string) error {
	if !looksLikeEmail(email) || (state != "active" && state != "paused" && state != "blocked") {
		return errors.New("邮箱或状态无效")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if sourceID > 0 {
		var id int64
		if err = d.ex.QueryRowTx(tx, "SELECT id FROM mail_sources WHERE id=?", sourceID).Scan(&id); err != nil {
			return errors.New("收件来源不存在")
		}
	}
	res, err := d.ex.ExecTxArgs(tx, "UPDATE mail_pool SET source_id=NULLIF(?,0) WHERE email=?", sourceID, email)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	_, err = d.ex.ExecTxArgs(tx, `INSERT INTO mail_alias_state(alias_hme,state,manual,updated_at) VALUES(?,?,1,?)
ON CONFLICT(alias_hme) DO UPDATE SET state=excluded.state,manual=1,updated_at=excluded.updated_at`, email, state, float64(time.Now().Unix()))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) PoolPage(ctx context.Context, q, state string, sourceID int64, limit, offset int) ([]Mailbox, int, error) {
	if limit < 1 || limit > 100 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	where := " WHERE NOT EXISTS(SELECT 1 FROM mail_transferred t WHERE t.email=p.email)"
	args := []any{}
	if q != "" {
		if d.dialect == storepkg.DialectPG {
			where += " AND strpos(p.email,?)>0"
		} else {
			where += " AND instr(p.email,?)>0"
		}
		args = append(args, strings.ToLower(strings.TrimSpace(q)))
	}
	if state != "" {
		where += " AND COALESCE(a.state,'active')=?"
		args = append(args, state)
	}
	if sourceID > 0 {
		where += " AND p.source_id=?"
		args = append(args, sourceID)
	}
	from := ` FROM mail_pool p LEFT JOIN mail_sources s ON s.id=p.source_id LEFT JOIN mail_alias_state a ON a.alias_hme=p.email`
	var total int
	if err := d.ex.QueryRowContext(ctx, "SELECT count(*)"+from+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.ex.QueryContext(ctx, `SELECT p.email,COALESCE(p.source_id,0),COALESCE(s.name,''),COALESCE(s.enabled,0),COALESCE(a.state,'active'),p.origin,
(SELECT count(*) FROM mail_recipients r WHERE r.email=p.email),
(SELECT count(*) FROM mail_recipients r JOIN mail_messages m ON m.id=r.message_id WHERE r.email=p.email AND m.primary_tag IN ('risk','banned')),
COALESCE((SELECT max(m.sort_ts) FROM mail_recipients r JOIN mail_messages m ON m.id=r.message_id WHERE r.email=p.email),0),
COALESCE((SELECT m.primary_tag FROM mail_recipients r JOIN mail_messages m ON m.id=r.message_id WHERE r.email=p.email ORDER BY m.sort_ts DESC,m.id DESC LIMIT 1),''),
COALESCE(a.manual,0),COALESCE(a.updated_at,p.created_at),p.provider,p.role,COALESCE(NULLIF(p.transport,''),s.transport,''),p.parent_id`+from+where+" ORDER BY p.email LIMIT CAST(? AS BIGINT) OFFSET CAST(? AS BIGINT)", append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Mailbox{}
	for rows.Next() {
		var m Mailbox
		if err = rows.Scan(&m.Email, &m.SourceID, &m.SourceName, &m.SourceEnabled, &m.State, &m.Origin, &m.Total, &m.RiskHits, &m.LastAt, &m.LastTag, &m.Manual, &m.UpdatedAt, &m.Provider, &m.Role, &m.Transport, &m.ParentID); err != nil {
			return nil, 0, err
		}
		items = append(items, m)
	}
	return items, total, rows.Err()
}

var ErrMailboxUnavailable = errors.New("邮箱已暂停或阻止，暂不提供取码")

func (d *DB) MailboxAvailable(email string) error {
	var moved int
	if err := d.ex.QueryRow(`SELECT count(*) FROM mail_transferred WHERE email=?`, strings.ToLower(strings.TrimSpace(email))).Scan(&moved); err != nil {
		return err
	}
	if moved > 0 {
		return ErrMailboxUnavailable
	}
	var state string
	err := d.ex.QueryRow(`SELECT state FROM mail_alias_state WHERE alias_hme=?`, strings.ToLower(strings.TrimSpace(email))).Scan(&state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if state == "blocked" || state == "paused" {
		return ErrMailboxUnavailable
	}
	return nil
}

func (d *DB) SourceAliases(id int64) ([]string, error) {
	rows, err := d.ex.Query("SELECT email FROM mail_pool WHERE source_id=?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var email string
		if err = rows.Scan(&email); err != nil {
			return nil, err
		}
		out = append(out, email)
	}
	return out, rows.Err()
}

func (d *DB) MessagePage(ctx context.Context, alias, tag string, sourceID int64, limit, offset int) ([]Message, int, error) {
	return d.messagePage(ctx, alias, tag, sourceID, limit, offset, 0, 0)
}

func (d *DB) messagePage(ctx context.Context, alias, tag string, sourceID int64, limit, offset int, cursor int64, after float64) ([]Message, int, error) {
	where := " WHERE 1=1"
	args := []any{}
	if cursor > 0 {
		where += " AND id>?"
		args = append(args, cursor)
	}
	if after > 0 {
		where += " AND sort_ts>=?"
		args = append(args, after)
	}
	if alias != "" {
		where += " AND id IN (SELECT message_id FROM mail_recipients WHERE email=?)"
		args = append(args, strings.ToLower(strings.TrimSpace(alias)))
	}
	if tag != "" {
		where += " AND primary_tag=?"
		args = append(args, tag)
	}
	if sourceID > 0 {
		where += " AND source_id=?"
		args = append(args, sourceID)
	}
	if limit <= 0 || limit > 500 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := d.ex.QueryRowContext(ctx, "SELECT count(*) FROM mail_messages"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.ex.QueryContext(ctx, "SELECT id,source_id,uid,subject,from_addr,to_header,sort_ts,snippet,body_text,tags,primary_tag,COALESCE(alias_hme,'') FROM mail_messages"+where+" ORDER BY sort_ts DESC,id DESC LIMIT CAST(? AS BIGINT) OFFSET CAST(? AS BIGINT)", append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var ts sql.NullFloat64
		var tags string
		if err = rows.Scan(&m.ID, &m.SourceID, &m.UID, &m.Subject, &m.From, &m.To, &ts, &m.Snippet, &m.BodyText, &tags, &m.PrimaryTag, &m.Alias); err != nil {
			return nil, 0, err
		}
		if ts.Valid {
			v := ts.Float64
			m.SentAt = &v
			m.Date = time.Unix(int64(v), 0).UTC().Format(time.RFC1123Z)
		}
		_ = json.Unmarshal([]byte(tags), &m.Tags)
		out = append(out, m)
	}
	return out, total, rows.Err()
}
