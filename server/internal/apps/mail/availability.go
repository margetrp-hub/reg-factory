package mail

// Mailbox health, administrative pause and restrictions on a target platform
// are independent facts. Registration consumes their projection in workflow.go;
// successful login can update one platform without reviving a disabled mailbox.
import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	BanUnknown     = "unknown"
	BanRegistered  = "registered"
	BanBanned      = "banned"
	BanRateLimited = "rate_limited"
	BanBlocked     = "blocked"
)

type PlatformBan struct {
	Email        string  `json:"email"`
	Platform     string  `json:"platform"`
	State        string  `json:"state"`
	Reason       string  `json:"reason"`
	EvidenceType string  `json:"evidence_type"`
	EvidenceRef  string  `json:"evidence_ref"`
	UpdatedAt    float64 `json:"updated_at"`
}

func (d *DB) MarkPlatformBan(ctx context.Context, ban PlatformBan) error {
	ban.Email = strings.ToLower(strings.TrimSpace(ban.Email))
	ban.Platform = strings.ToLower(strings.TrimSpace(ban.Platform))
	if !looksLikeEmail(ban.Email) || ban.Platform == "" {
		return errors.New("请指定邮箱与目标平台")
	}
	switch ban.State {
	case BanUnknown, BanRegistered, BanBanned, BanRateLimited, BanBlocked:
	default:
		return errors.New("请选择有效的平台状态")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := float64(time.Now().Unix())
	var bannedAt any
	if ban.State == BanBanned {
		bannedAt = now
	}
	result, err := d.ex.ExecContext(ctx, `INSERT INTO mail_platform_bans
 (email,platform,state,reason,evidence_type,evidence_ref,banned_at,checked_at,updated_at)
 SELECT email,?,?,?,?,?,?,?,? FROM mail_pool p WHERE email=?
 AND NOT EXISTS(SELECT 1 FROM mail_transferred t WHERE t.email=p.email)
 ON CONFLICT(email,platform) DO UPDATE SET state=excluded.state,reason=excluded.reason,
 evidence_type=excluded.evidence_type,evidence_ref=excluded.evidence_ref,
 banned_at=excluded.banned_at,checked_at=excluded.checked_at,updated_at=excluded.updated_at`,
		ban.Platform, ban.State, ban.Reason, ban.EvidenceType, ban.EvidenceRef, bannedAt, now, now, ban.Email)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrNotFound
	}
	return err
}

func (d *DB) BansForEmail(email string) ([]PlatformBan, error) {
	rows, err := d.ex.Query(`SELECT email,platform,state,reason,evidence_type,evidence_ref,updated_at
 FROM mail_platform_bans WHERE email=? ORDER BY platform`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []PlatformBan{}
	for rows.Next() {
		var b PlatformBan
		if err = rows.Scan(&b.Email, &b.Platform, &b.State, &b.Reason, &b.EvidenceType, &b.EvidenceRef, &b.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, b)
	}
	return items, rows.Err()
}

// A parent restriction means the mail provider disabled the actual parent
// mailbox. It never represents a ban on ChatGPT/Claude/etc. Recovery only clears
// this parent fact; child pauses, health failures and platform bans stay intact.
func (d *DB) SetParentState(ctx context.Context, parent, state string) error {
	parent = strings.ToLower(strings.TrimSpace(parent))
	if parent == "" || (state != "active" && state != "banned") {
		return errors.New("请指定邮箱母号及可用状态")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	result, err := d.ex.ExecContext(ctx, `UPDATE mail_pool SET parent_state=?,ban_scope=''
 WHERE (parent_id=? OR email=?) AND NOT EXISTS(SELECT 1 FROM mail_transferred t WHERE t.email=mail_pool.email)`, state, parent, parent)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		return ErrNotFound
	}
	return err
}

func (a *App) handleAvailability(c *gin.Context) {
	email := strings.ToLower(strings.TrimSpace(c.Query("email")))
	var parent, state string
	err := a.DB.ex.QueryRowContext(c.Request.Context(), `SELECT parent_id,parent_state FROM mail_pool p
 WHERE email=? AND NOT EXISTS(SELECT 1 FROM mail_transferred t WHERE t.email=p.email)`, email).Scan(&parent, &state)
	if errors.Is(err, sql.ErrNoRows) {
		failJSON(c, 404, "MAILBOX_NOT_FOUND", "邮箱不在本节点地址池中")
		return
	}
	if err != nil {
		failJSON(c, 500, "MAIL_READ_FAILED", "邮箱状态读取失败")
		return
	}
	bans, err := a.DB.BansForEmail(email)
	if err != nil {
		failJSON(c, 500, "MAIL_READ_FAILED", "平台状态读取失败")
		return
	}
	okJSON(c, gin.H{"platforms": bans, "parentId": parent, "parentState": state})
}

func (a *App) handlePlatformState(c *gin.Context) {
	var body PlatformBan
	if err := c.ShouldBindJSON(&body); err != nil {
		failJSON(c, 400, "BAD_REQUEST", "邮箱状态格式无效")
		return
	}
	body.EvidenceType, body.EvidenceRef = "manual", ""
	if err := a.DB.MarkPlatformBan(c.Request.Context(), body); err != nil {
		failJSON(c, 409, "MAILBOX_NOT_SAVED", err.Error())
		return
	}
	okJSON(c, gin.H{"saved": true})
}

func (a *App) handleParentState(c *gin.Context) {
	var body struct {
		ParentID string `json:"parent_id"`
		State    string `json:"state"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		failJSON(c, 400, "BAD_REQUEST", "邮箱母号状态格式无效")
		return
	}
	if err := a.DB.SetParentState(c.Request.Context(), body.ParentID, body.State); err != nil {
		failJSON(c, 409, "MAILBOX_NOT_SAVED", err.Error())
		return
	}
	okJSON(c, gin.H{"saved": true})
}
