package mail

// 邮箱状态的两层
// ============
//
// 第一层：邮箱自身能不能收信（mail_pool.state）
//
//	ready        可以收
//	in_use       正在被某个流程用
//	auth_failed  凭证失效（refresh_token 过期 / 密码错 / JWT 过期）
//	exhausted    配额用完（IC 有上限）
//	retired      人工淘汰
//
// 第二层：邮箱在某个平台上的状态（mail_platform_bans）
//
//	unknown      还没在这个平台用过
//	registered   注册成功，正在用
//	banned       被这个平台封
//	rate_limited 被限流（可恢复）
//	blocked      平台直接拒收这个域名/地址（换地址，不是封号）
//
// 第三层：母号级联（mail_pool.parent_id）
//
//	IC 地址挂在父 Apple ID 下。母号被封时，其名下所有地址一起废。
//	这是唯一会「一炸全炸」的关系，必须显式处理。
//
// 关键约束：这三层互相独立。
// 一个 Outlook 在 OpenAI 被封，在 Claude 还能用 —— 不能因为一个平台封了就 retire 整个邮箱。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 平台封禁状态
const (
	BanUnknown     = "unknown"
	BanRegistered  = "registered"
	BanBanned      = "banned"
	BanRateLimited = "rate_limited"
	BanBlocked     = "blocked"
)

// 判定依据
const (
	EvidenceMail   = "mail"   // mail_messages.id
	EvidenceLogin  = "login"  // accounts.ref
	EvidenceAPI    = "api"    // 平台接口直接返回
	EvidenceManual = "manual" // 人工
)

// PlatformBan 是一条平台封禁记录。
type PlatformBan struct {
	Email        string     `json:"email"`
	Platform     string     `json:"platform"`
	State        string     `json:"state"`
	Reason       string     `json:"reason"`
	EvidenceType string     `json:"evidence_type"`
	EvidenceRef  string     `json:"evidence_ref"`
	BannedAt     *time.Time `json:"banned_at,omitempty"`
	CheckedAt    *time.Time `json:"checked_at,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// MarkPlatformBan 记录某个邮箱在某个平台的状态。
//
// 这是最常用的入口：
//   - 收到封禁通知邮件 → EvidenceMail + mail_messages.id
//   - 登录返回 banned   → EvidenceLogin + accounts.ref
//   - 注册被拒          → EvidenceAPI
//
// 注意：**只影响这个平台**，不改 mail_pool.state。
func (d *DB) MarkPlatformBan(ctx context.Context, ban PlatformBan) error {
	if ban.Email == "" || ban.Platform == "" {
		return fmt.Errorf("MarkPlatformBan: email 和 platform 必填")
	}
	now := float64(time.Now().Unix())

	var bannedAt any
	if ban.State == BanBanned {
		bannedAt = now
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	_, err := d.ex.ExecContext(ctx, `
INSERT INTO mail_platform_bans
  (email, platform, state, reason, evidence_type, evidence_ref, banned_at, checked_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?)
ON CONFLICT(email, platform) DO UPDATE SET
  state         = excluded.state,
  reason        = excluded.reason,
  evidence_type = excluded.evidence_type,
  evidence_ref  = excluded.evidence_ref,
  banned_at     = CASE WHEN excluded.state='banned' AND banned_at IS NULL
                       THEN excluded.banned_at ELSE banned_at END,
  checked_at    = excluded.checked_at,
  updated_at    = excluded.updated_at`,
		ban.Email, ban.Platform, ban.State, ban.Reason,
		ban.EvidenceType, ban.EvidenceRef, bannedAt, now, now)
	if err != nil {
		return fmt.Errorf("MarkPlatformBan: %w", err)
	}
	return nil
}

// ClearPlatformBan 解除某个平台的封禁（申诉成功 / 限流恢复）。
func (d *DB) ClearPlatformBan(ctx context.Context, email, platform string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.ex.ExecContext(ctx,
		`UPDATE mail_platform_bans SET state=?, reason='', updated_at=?
		 WHERE email=? AND platform=?`,
		BanRegistered, float64(time.Now().Unix()), email, platform)
	return err
}

// BansForEmail 查一个邮箱在所有平台的状态。
func (d *DB) BansForEmail(email string) ([]PlatformBan, error) {
	rows, err := d.ex.Query(`
SELECT email, platform, state, reason, evidence_type, evidence_ref,
       banned_at, checked_at, updated_at
FROM mail_platform_bans WHERE email = ? ORDER BY platform`, email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBans(rows)
}

// BansForPlatform 查某个平台被封的所有邮箱。
func (d *DB) BansForPlatform(platform string) ([]PlatformBan, error) {
	rows, err := d.ex.Query(`
SELECT email, platform, state, reason, evidence_type, evidence_ref,
       banned_at, checked_at, updated_at
FROM mail_platform_bans WHERE platform = ? ORDER BY updated_at DESC`, platform)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBans(rows)
}

func scanBans(rows *sql.Rows) ([]PlatformBan, error) {
	var out []PlatformBan
	for rows.Next() {
		var (
			b        PlatformBan
			bannedAt sql.NullFloat64
			checked  sql.NullFloat64
			updated  float64
		)
		if err := rows.Scan(&b.Email, &b.Platform, &b.State, &b.Reason,
			&b.EvidenceType, &b.EvidenceRef, &bannedAt, &checked, &updated); err != nil {
			return nil, err
		}
		if bannedAt.Valid {
			t := time.Unix(int64(bannedAt.Float64), 0)
			b.BannedAt = &t
		}
		if checked.Valid {
			t := time.Unix(int64(checked.Float64), 0)
			b.CheckedAt = &t
		}
		b.UpdatedAt = time.Unix(int64(updated), 0)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- 母号级联

// BanParent 封一个母号，并把它名下所有子地址一起标记。
//
// 场景：某个 Apple ID 被 Apple 封了，它生成的几十个 IC 地址瞬间全部失效。
// 不级联的话，池子会把它们当成「可用」派出去，然后注册全线失败。
//
// 参数
//
//	parentID  母号标识（IC 用 Apple ID 邮箱）
//	platform  为空表示整个邮箱不可用（不是平台封）
//
// 返回受影响的子地址数量。
func (d *DB) BanParent(ctx context.Context, parentID, reason, platform string) (int, error) {
	if strings.TrimSpace(parentID) == "" {
		return 0, fmt.Errorf("BanParent: parentID 必填")
	}
	now := float64(time.Now().Unix())

	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 1. 找出所有子地址
	rows, err := d.ex.QueryTxContext(ctx, tx,
		`SELECT email FROM mail_pool WHERE parent_id = ?`, parentID)
	if err != nil {
		return 0, fmt.Errorf("查子地址: %w", err)
	}
	var children []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err == nil {
			children = append(children, e)
		}
	}
	rows.Close()

	if len(children) == 0 {
		// 母号本身可能也在池里
		children = []string{parentID}
	}

	// 2. 母号状态
	if _, err := d.ex.ExecTxContext(ctx, tx,
		`UPDATE mail_pool SET parent_state='banned', ban_scope=?, last_error=?, updated_at=?
		 WHERE email = ? OR parent_id = ?`,
		platform, reason, now, parentID, parentID); err != nil {
		return 0, fmt.Errorf("标记母号: %w", err)
	}

	// 3. 没有平台时，整个邮箱 retire；有平台时只记平台封禁
	if platform == "" {
		if _, err := d.ex.ExecTxContext(ctx, tx,
			`UPDATE mail_pool SET state='retired', last_error=?, last_error_code='parent_banned', updated_at=?
			 WHERE parent_id = ?`,
			reason, now, parentID); err != nil {
			return 0, fmt.Errorf("级联 retire: %w", err)
		}
	} else {
		for _, e := range children {
			if _, err := d.ex.ExecTxContext(ctx, tx, `
INSERT INTO mail_platform_bans
  (email, platform, state, reason, evidence_type, evidence_ref, banned_at, checked_at, updated_at)
VALUES (?,?,'banned',?,'parent',?,?,?,?)
ON CONFLICT(email, platform) DO UPDATE SET
  state=excluded.state, reason=excluded.reason,
  evidence_type=excluded.evidence_type, evidence_ref=excluded.evidence_ref,
  banned_at=COALESCE(banned_at, excluded.banned_at),
  updated_at=excluded.updated_at`,
				e, platform, reason, parentID, now, now, now); err != nil {
				return 0, fmt.Errorf("级联平台封禁 %s: %w", e, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(children), nil
}

// ---------------------------------------------------------------- 可用性判定

// Usable 判断一个邮箱当前能不能用于某个平台。
//
// 三层都要过：
//  1. 邮箱自身 state = ready
//  2. 这个平台没有 banned / blocked
//  3. 母号没被封
//
// 返回 (可用, 原因)。
func (d *DB) Usable(ctx context.Context, email, platform string) (bool, string) {
	var (
		state       string
		parentState string
	)
	err := d.ex.QueryRowContext(ctx,
		`SELECT COALESCE(state,'ready'), COALESCE(parent_state,'')
		 FROM mail_pool WHERE email = ?`, email).Scan(&state, &parentState)
	if err == sql.ErrNoRows {
		return false, "不在池中"
	}
	if err != nil {
		return false, err.Error()
	}

	// 第 1 层
	switch state {
	case "ready", "in_use":
	default:
		return false, "邮箱状态 " + state
	}

	// 第 3 层（母号）
	if parentState == "banned" {
		return false, "母号已封"
	}

	// 第 2 层（平台）
	if platform != "" {
		var banState string
		err := d.ex.QueryRowContext(ctx,
			`SELECT state FROM mail_platform_bans WHERE email=? AND platform=?`,
			email, platform).Scan(&banState)
		if err != nil && err != sql.ErrNoRows {
			return false, err.Error()
		}
		switch banState {
		case BanBanned, BanBlocked:
			return false, "平台封禁 " + banState
		case BanRateLimited:
			// 限流是可恢复的，但仍要提示调用方
			return false, "平台限流中"
		}
	}

	return true, ""
}

// ---------------------------------------------------------------- 取号

// TakeForPlatform 原子地从「可用邮箱池」取一个邮箱。
//
// 这是取号的唯一入口。筛选条件全部可空：
//
//	TakeFilter{Platform:"chatgpt"}                        任意可用
//	TakeFilter{Platform:"chatgpt", Provider:"outlook"}    只要 Outlook
//	TakeFilter{Platform:"claude", Group:"cloudmail"}      只要自建
//	TakeFilter{Platform:"chatgpt", Role:"alias"}          只要别名（IC）
//	TakeFilter{Platform:"chatgpt", ExcludeRoles:["hub"]}  排除收件总线
//
// 三层过滤都在 SQL 里做，一次查询搞定：
//  1. 邮箱自身 state 可用
//  2. 该平台未被封
//  3. 母号未封
//
// 排序：最久没用的先上（轮转），失败少的优先，最后按 email 保证稳定。
func (d *DB) TakeForPlatform(ctx context.Context, f TakeFilter) (*Mailbox, error) {
	now := float64(time.Now().Unix())

	d.mu.Lock()
	defer d.mu.Unlock()

	var where []string
	var args []any

	// 三层状态过滤
	where = append(where, "m.state = 'ready'")
	where = append(where, "COALESCE(m.parent_state,'') <> 'banned'")
	where = append(where,
		"(b.state IS NULL OR b.state IN ('unknown','registered'))")

	// 平台
	args = append(args, f.Platform)

	// 可选的分类过滤
	if f.Provider != "" {
		where = append(where, "COALESCE(m.provider,'') = ?")
		args = append(args, f.Provider)
	}
	if f.Group != "" {
		where = append(where, "COALESCE(NULLIF(m.db_group,''), m.provider, '') = ?")
		args = append(args, f.Group)
	}
	if f.Role != "" {
		where = append(where, "COALESCE(NULLIF(m.role,''),'address') = ?")
		args = append(args, f.Role)
	}
	for _, r := range f.ExcludeRoles {
		where = append(where, "COALESCE(NULLIF(m.role,''),'address') <> ?")
		args = append(args, r)
	}
	if f.RequireParent {
		where = append(where, "COALESCE(m.parent_id,'') <> ''")
	}

	q := `
UPDATE mail_pool
SET state='in_use', assigned_at=?, last_used_at=?
WHERE email = (
    SELECT m.email FROM mail_pool m
    LEFT JOIN mail_platform_bans b
           ON m.email = b.email AND b.platform = ?
    WHERE ` + strings.Join(where, "\n      AND ") + `
    ORDER BY m.last_used_at ASC NULLS FIRST, m.fail_count ASC, m.email
    LIMIT 1
)
RETURNING email, COALESCE(source_id,0), COALESCE(provider,''),
          COALESCE(parent_id,''), COALESCE(role,'address'),
          COALESCE(db_group,''), COALESCE(transport,''),
          COALESCE(credential_mode,'')`

	// 参数顺序：SET 的两个时间 → JOIN 的 platform → WHERE 的其余
	qargs := []any{now, now}
	qargs = append(qargs, f.Platform)
	qargs = append(qargs, args[1:]...) // args[0] 是 platform，已在 JOIN 用过

	var (
		email     string
		sourceID  int64
		provider  string
		parentID  string
		role      string
		dbGroup   string
		transport string
		credMode  string
	)
	err := d.ex.QueryRowContext(ctx, q, qargs...).Scan(
		&email, &sourceID, &provider, &parentID, &role,
		&dbGroup, &transport, &credMode)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("TakeForPlatform(%s): %w", f.Describe(), err)
	}

	if dbGroup == "" {
		dbGroup = GroupOf(provider)
	}

	return &Mailbox{
		Email:          email,
		SourceID:       sourceID,
		Provider:       provider,
		ParentID:       parentID,
		Role:           role,
		DBGroup:        dbGroup,
		Transport:      transport,
		CredentialMode: credMode,
		State:          "in_use",
	}, nil
}

// TakeAny 是最简形式：只排除某个平台的封禁。
func (d *DB) TakeAny(ctx context.Context, platform string) (*Mailbox, error) {
	return d.TakeForPlatform(ctx, TakeFilter{Platform: platform})
}

// ReleaseWithResult 用完放回，并按结果决定后续状态。
//
// outcome:
//
//	ok        放回 ready
//	auth      凭证失效 → auth_failed
//	platform  该平台被封 → 记平台封禁，邮箱本身放回 ready
//	banned    邮箱本身被封 → retired
//	retry     可重试错误 → 放回 ready，fail_count++
func (d *DB) ReleaseWithResult(ctx context.Context, email, platform, outcome, reason, evidenceRef string) error {
	now := float64(time.Now().Unix())

	d.mu.Lock()
	defer d.mu.Unlock()

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	switch outcome {
	case "ok":
		if _, err := d.ex.ExecTxContext(ctx, tx,
			`UPDATE mail_pool SET state='ready', last_error='', last_error_code='', updated_at=?
			 WHERE email=?`, now, email); err != nil {
			return err
		}

	case "auth":
		if _, err := d.ex.ExecTxContext(ctx, tx,
			`UPDATE mail_pool SET state='auth_failed', last_error=?, last_error_code='auth_failed', updated_at=?
			 WHERE email=?`, reason, now, email); err != nil {
			return err
		}

	case "banned":
		if _, err := d.ex.ExecTxContext(ctx, tx,
			`UPDATE mail_pool SET state='retired', last_error=?, last_error_code='banned', updated_at=?
			 WHERE email=?`, reason, now, email); err != nil {
			return err
		}

	case "platform":
		// 只记平台封禁，邮箱放回池子 —— 它在别的平台还能用
		if _, err := d.ex.ExecTxContext(ctx, tx,
			`UPDATE mail_pool SET state='ready', updated_at=? WHERE email=?`, now, email); err != nil {
			return err
		}
		if platform != "" {
			if _, err := d.ex.ExecTxContext(ctx, tx, `
INSERT INTO mail_platform_bans
  (email, platform, state, reason, evidence_type, evidence_ref, banned_at, updated_at)
VALUES (?,?,'banned',?,'login',?,?,?)
ON CONFLICT(email, platform) DO UPDATE SET
  state=excluded.state, reason=excluded.reason, updated_at=excluded.updated_at`,
				email, platform, reason, evidenceRef, now, now); err != nil {
				return err
			}
		}

	case "retry":
		if _, err := d.ex.ExecTxContext(ctx, tx, `
UPDATE mail_pool SET
  state = CASE WHEN fail_count + 1 >= 3 THEN 'retired' ELSE 'ready' END,
  fail_count = fail_count + 1,
  last_error = ?, last_error_code = ?, updated_at = ?
WHERE email = ?`, reason, "retry", now, email); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ---------------------------------------------------------------- 统计

// MailStats 是邮箱池的总览，给面板用。
type MailStats struct {
	Total       int            `json:"total"`
	ByState     map[string]int `json:"by_state"`
	ByProvider  map[string]int `json:"by_provider"`
	ByPlatform  map[string]int `json:"by_platform_ban"`
	BannedTotal int            `json:"banned_total"`
	ParentBans  int            `json:"parent_bans"`
}

// MailPoolStats 汇总池子状态。
func (d *DB) MailPoolStats(ctx context.Context) (*MailStats, error) {
	st := &MailStats{
		ByState:    map[string]int{},
		ByProvider: map[string]int{},
		ByPlatform: map[string]int{},
	}

	rows, err := d.ex.QueryContext(ctx,
		`SELECT COALESCE(state,'ready'), COUNT(*) FROM mail_pool GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var s string
			var n int
			if rows.Scan(&s, &n) == nil {
				st.ByState[s] = n
				st.Total += n
			}
		}
		rows.Close()
	}

	rows, err = d.ex.QueryContext(ctx,
		`SELECT COALESCE(NULLIF(provider,''),'?'), COUNT(*) FROM mail_pool GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var s string
			var n int
			if rows.Scan(&s, &n) == nil {
				st.ByProvider[s] = n
			}
		}
		rows.Close()
	}

	rows, err = d.ex.QueryContext(ctx,
		`SELECT platform, COUNT(*) FROM mail_platform_bans
		 WHERE state IN ('banned','blocked') GROUP BY platform`)
	if err == nil {
		for rows.Next() {
			var s string
			var n int
			if rows.Scan(&s, &n) == nil {
				st.ByPlatform[s] = n
				st.BannedTotal += n
			}
		}
		rows.Close()
	}

	_ = d.ex.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mail_pool WHERE parent_state='banned'`).Scan(&st.ParentBans)

	return st, nil
}
