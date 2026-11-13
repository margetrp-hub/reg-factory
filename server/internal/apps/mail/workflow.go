package mail

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Registration candidates use the same availability facts as the actual read.
// A failed mailbox must not prevent choosing another usable address.
func (a *App) handleWorkflowCandidates(c *gin.Context) {
	platform := strings.ToLower(strings.TrimSpace(c.Query("platform")))
	if platform == "" {
		failJSON(c, 400, "PLATFORM_REQUIRED", "请选择目标平台")
		return
	}
	offset := atoiDefault(c.Query("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	from := ` FROM mail_pool p JOIN mail_sources s ON s.id=p.source_id
 LEFT JOIN mail_alias_state a ON a.alias_hme=p.email
 LEFT JOIN mail_platform_bans b ON b.email=p.email AND b.platform=?
 WHERE s.enabled=1 AND COALESCE(s.last_error,'')='' AND (` + mailboxStateSQL + `)='active'
 AND p.origin<>'discovered' AND p.role NOT IN ('hub','parent')
 AND COALESCE(b.state,'unknown') NOT IN ('banned','blocked','rate_limited')
 AND NOT EXISTS(SELECT 1 FROM mail_transferred t WHERE t.email=p.email)`
	var total int
	if err := a.DB.ex.QueryRowContext(c.Request.Context(), "SELECT count(*)"+from, platform).Scan(&total); err != nil {
		failJSON(c, 500, "MAIL_READ_FAILED", "邮箱候选读取失败")
		return
	}
	rows, err := a.DB.ex.QueryContext(c.Request.Context(), "SELECT p.email,p.source_id,p.origin"+from+" ORDER BY p.email LIMIT 100 OFFSET CAST(? AS BIGINT)", platform, offset)
	if err != nil {
		failJSON(c, 500, "MAIL_READ_FAILED", "邮箱候选读取失败")
		return
	}
	defer rows.Close()
	items := []gin.H{}
	for rows.Next() {
		var email, origin string
		var source int64
		if err = rows.Scan(&email, &source, &origin); err != nil {
			failJSON(c, 500, "MAIL_READ_FAILED", "邮箱候选读取失败")
			return
		}
		items = append(items, gin.H{"email": email, "sourceId": source, "sourceEnabled": true, "origin": origin})
	}
	if rows.Err() != nil {
		failJSON(c, 500, "MAIL_READ_FAILED", "邮箱候选读取失败")
		return
	}
	okJSON(c, gin.H{"items": items, "total": total})
}

// Worker reads use the panel's existing authentication, never the public OTP API.
// An exact pool identity and its current source/state are checked on every poll.
func (a *App) handleWorkflowMailbox(c *gin.Context) {
	email := strings.ToLower(strings.TrimSpace(c.Query("email")))
	if !looksLikeEmail(email) {
		failJSON(c, 400, "BAD_MAILBOX", "请指定原账号的完整邮箱地址")
		return
	}
	after, err := strconv.ParseFloat(c.DefaultQuery("after", "0"), 64)
	if err != nil || math.IsNaN(after) || math.IsInf(after, 0) || after < 0 {
		failJSON(c, 400, "BAD_TIME", "取信起始时间无效")
		return
	}
	cursor, err := strconv.ParseInt(c.DefaultQuery("cursor", "0"), 10, 64)
	if err != nil || cursor < 0 {
		failJSON(c, 400, "BAD_CURSOR", "取信消息编号无效")
		return
	}
	var messageCursor *int64
	if c.Query("cursor") != "" || after > 0 {
		messageCursor = &cursor
	}
	snapshot, err := a.DB.WorkflowMailbox(c.Request.Context(), email, strings.ToLower(strings.TrimSpace(c.Query("platform"))), messageCursor, after)
	if err != nil {
		var unavailable *WorkflowUnavailable
		if errors.As(err, &unavailable) {
			failJSON(c, 409, unavailable.Code, unavailable.Error())
		} else {
			failJSON(c, 500, "MAIL_READ_FAILED", "内置邮箱读取失败，请重试")
		}
		return
	}
	okJSON(c, snapshot)
}

type WorkflowSnapshot struct {
	Email    string    `json:"email"`
	SourceID int64     `json:"source_id"`
	Cursor   int64     `json:"cursor"`
	Messages []Message `json:"messages"`
}

type WorkflowUnavailable struct {
	Code    string
	Message string
}

func (e *WorkflowUnavailable) Error() string { return e.Message }

// Python workers and in-process protocol registration use this same projection.
// A nil cursor captures the watermark before sending mail; later reads only
// include messages after that watermark and the current attempt's start time.
func (d *DB) WorkflowMailbox(ctx context.Context, email, platform string, cursor *int64, after float64) (WorkflowSnapshot, error) {
	snapshot := WorkflowSnapshot{Email: email, Messages: []Message{}}
	var source int64
	var state string
	var enabled bool
	var platformState string
	var sourceError string
	err := d.ex.QueryRowContext(ctx, `SELECT p.source_id, s.enabled, `+mailboxStateSQL+`,COALESCE(b.state,'unknown'),COALESCE(s.last_error,'')
FROM mail_pool p JOIN mail_sources s ON s.id=p.source_id
LEFT JOIN mail_alias_state a ON a.alias_hme=p.email
LEFT JOIN mail_platform_bans b ON b.email=p.email AND b.platform=?
WHERE p.email=? AND NOT EXISTS(SELECT 1 FROM mail_transferred t WHERE t.email=p.email)`, platform, email).Scan(&source, &enabled, &state, &platformState, &sourceError)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return snapshot, err
	}
	if err != nil || !enabled || state != "active" {
		return snapshot, &WorkflowUnavailable{"MAILBOX_UNAVAILABLE", "邮箱未登记、地址已暂停或收件来源不可用，请在邮箱池检查原地址与来源"}
	}
	if platformState == BanBanned || platformState == BanBlocked || platformState == BanRateLimited {
		return snapshot, &WorkflowUnavailable{"MAILBOX_PLATFORM_UNAVAILABLE", "此邮箱在所选平台不可用；其他平台的邮箱状态保持独立"}
	}
	if sourceError != "" {
		return snapshot, &WorkflowUnavailable{"MAIL_SOURCE_UNAVAILABLE", "收件来源连接失败，请检查邮箱登录与收信设置"}
	}
	snapshot.SourceID = source
	err = d.ex.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM mail_messages
WHERE source_id=? AND id IN (SELECT message_id FROM mail_recipients WHERE email=?)`, source, email).Scan(&snapshot.Cursor)
	if err != nil {
		return snapshot, err
	}
	if cursor != nil {
		snapshot.Messages, _, err = d.messagePage(ctx, email, "", source, 100, 0, *cursor, after)
	}
	return snapshot, err
}
