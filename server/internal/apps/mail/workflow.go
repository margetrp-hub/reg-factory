package mail

import (
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

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
	var source int64
	var state string
	var enabled bool
	err = a.DB.ex.QueryRowContext(c.Request.Context(), `SELECT p.source_id, s.enabled, COALESCE(a.state,'active')
FROM mail_pool p JOIN mail_sources s ON s.id=p.source_id
LEFT JOIN mail_alias_state a ON a.alias_hme=p.email WHERE p.email=?`, email).Scan(&source, &enabled, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		failJSON(c, 500, "MAIL_READ_FAILED", "内置邮箱读取失败，请重试")
		return
	}
	if err != nil || !enabled || state != "active" {
		failJSON(c, 409, "MAILBOX_UNAVAILABLE", "邮箱未登记、地址已暂停或收件来源不可用，请在邮箱池检查原地址与来源")
		return
	}
	var latest int64
	err = a.DB.ex.QueryRowContext(c.Request.Context(), `SELECT COALESCE(MAX(id),0) FROM mail_messages
WHERE source_id=? AND id IN (SELECT message_id FROM mail_recipients WHERE email=?)`, source, email).Scan(&latest)
	if err != nil {
		failJSON(c, 500, "MAIL_READ_FAILED", "内置邮箱读取失败，请重试")
		return
	}
	messages := []Message{}
	if c.Query("cursor") != "" || after > 0 {
		messages, _, err = a.DB.messagePage(c.Request.Context(), email, "", source, 100, 0, cursor, after)
		if err != nil {
			failJSON(c, 500, "MAIL_READ_FAILED", "内置邮箱读取失败，请重试")
			return
		}
	}
	okJSON(c, gin.H{"email": email, "source_id": source, "cursor": latest, "messages": messages})
}
