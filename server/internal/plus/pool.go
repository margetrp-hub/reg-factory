package plus

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 预选池状态机
// ------------
// 账号从进入系统到被使用，状态流转是：
//
//	candidate   刚入库，还没做过能力检测
//	    ↓ 跑 sweep（warmup + 出口 + 提链）
//	preselected 检测通过，可以取用          ← 只有这里能被取走
//	    ↓ 被任务取走
//	in_use      使用中，其他任务不能再取
//	    ↓ 用完
//	retired     淘汰（检测失败 / 用完即废 / 手动下架）
//
// 为什么要有 preselected 这一层
// ------------------------
// 之前的做法是「要用的时候才去测」，结果是：
//
//	· 每次取号都要花 10 秒提链才知道能不能用
//	· 批量任务跑到一半才发现一半号是废的
//	· 风控号码被反复试探，加速报废
//
// 预选池把「检测」和「使用」解耦：检测一次性做完，使用只管从池子里拿。
const (
	PoolCandidate   = "candidate"   // 候选，未检测
	PoolPreselected = "preselected" // 预选通过，可取用
	PoolInUse       = "in_use"      // 使用中
	PoolRetired     = "retired"     // 淘汰
)

// PoolEntry 是池子里的一个条目。

type PoolEntry struct {
	Ref       string `json:"ref"`
	Identity  string `json:"identity"`
	AccountID string `json:"account_id"`
	Plan      string `json:"plan"`
	PoolState string `json:"pool_state"`
	AtStatus  string `json:"at_status"`
	CheckedAt string `json:"checked_at"`
	Reason    string `json:"reason"`
	// 检测时的出口信息
	Egress string `json:"egress"`
	IP     string `json:"ip"`
	ISP    string `json:"isp"`
	// 建单能力
	CheckoutOK bool   `json:"checkout_ok"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
}

// EnsurePoolSchema 建表 / 加字段（幂等）。
func EnsurePoolSchema(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE accounts ADD COLUMN pool_state TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN pool_checked_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN pool_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN pool_egress TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN pool_ip TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN pool_amount TEXT NOT NULL DEFAULT ''`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			// 字段已存在时 SQLite 报 "duplicate column name"，忽略
			if !isDupColumn(err) {
				return fmt.Errorf("加字段: %w", err)
			}
		}
	}
	// 索引：按池子状态取号是最热的查询
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_accounts_pool
	                ON accounts(platform, pool_state)`)
	return nil
}

// PoolStats 返回池子各状态的分布。
func PoolStats(db *sql.DB) (map[string]int, error) {
	rows, err := db.Query(
		`SELECT COALESCE(NULLIF(pool_state,''),'unclassified') AS st, COUNT(*) n
		 FROM accounts WHERE platform='chatgpt' GROUP BY st`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err == nil {
			out[st] = n
		}
	}
	return out, nil
}

// PoolList 按状态列出池子条目。
func PoolList(db *sql.DB, state string, limit int) ([]PoolEntry, error) {
	q := `SELECT ref, identity, COALESCE(pool_state,''), COALESCE(status,''),
	             COALESCE(pool_checked_at,''), COALESCE(pool_reason,''),
	             COALESCE(pool_egress,''), COALESCE(pool_ip,''),
	             COALESCE(pool_amount,'')
	      FROM accounts WHERE platform='chatgpt'`
	args := []any{}
	if state != "" {
		q += ` AND pool_state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY pool_checked_at DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}

	rows, err := db.Query(rb(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PoolEntry
	for rows.Next() {
		var e PoolEntry
		if err := rows.Scan(&e.Ref, &e.Identity, &e.PoolState, &e.AtStatus,
			&e.CheckedAt, &e.Reason, &e.Egress, &e.IP, &e.Amount); err != nil {
			return nil, err
		}
		e.CheckoutOK = IsZero(e.Amount)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PoolTake 从预选池取一个账号，标记为 in_use。
//
// 用 SQLite 的 UPDATE ... RETURNING 保证原子性：
// 两个并发任务同时取号时不会拿到同一个。
//
// 返回 nil 表示池子空了。
func PoolTake(db *sql.DB) (*PoolEntry, error) {
	q := `UPDATE accounts
	      SET pool_state = 'in_use',
	          updated_at = ?
	      WHERE ref = (
	          SELECT ref FROM accounts
	          WHERE platform='chatgpt'
	            AND pool_state = 'preselected' AND claimed=0
	          ORDER BY pool_checked_at ASC
	          LIMIT 1
	      ) AND pool_state='preselected' AND claimed=0
	      RETURNING ref, identity, pool_state, status, pool_checked_at,
	                pool_reason, pool_egress, pool_ip, pool_amount`

	now := time.Now().Format(time.RFC3339)
	rows, err := db.Query(rb(q), now)
	if err != nil {
		return nil, fmt.Errorf("取号: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, nil // 池子空
	}
	var e PoolEntry
	if err := rows.Scan(&e.Ref, &e.Identity, &e.PoolState, &e.AtStatus,
		&e.CheckedAt, &e.Reason, &e.Egress, &e.IP, &e.Amount); err != nil {
		return nil, err
	}
	e.CheckoutOK = IsZero(e.Amount)
	return &e, nil
}

// PoolRelease 把 in_use 的号放回预选池（用完但还能继续用）。
func PoolRelease(db *sql.DB, ref string, reason string) error {
	_, err := db.Exec(
		rb(`UPDATE accounts SET pool_state='preselected', pool_reason=?, updated_at=?
		 WHERE ref=? AND pool_state='in_use'`),
		reason, time.Now().Format(time.RFC3339), ref)
	return err
}

// PoolRetire 淘汰一个号。
func PoolRetire(db *sql.DB, ref string, reason string) error {
	_, err := db.Exec(
		rb(`UPDATE accounts SET pool_state='retired', pool_reason=?, updated_at=?
		 WHERE ref=?`),
		reason, time.Now().Format(time.RFC3339), ref)
	return err
}

// PoolPromote 检测通过，把号提升到预选池。
func PoolPromote(db *sql.DB, ref string, e PoolEntry) error {
	_, err := db.Exec(
		rb(`UPDATE accounts
		 SET pool_state='preselected', pool_checked_at=?, pool_reason=?,
		     pool_egress=?, pool_ip=?, pool_amount=?, updated_at=?
		 WHERE ref=?`),
		time.Now().Format(time.RFC3339), e.Reason,
		e.Egress, e.IP, e.Amount, time.Now().Format(time.RFC3339), ref)
	return err
}

// ---------------------------------------------------------------- 检测并入池

// ScreenResult 是一个账号的完整检测结果。
type ScreenResult struct {
	Email      string `json:"email"`
	Ref        string `json:"ref"`
	Verdict    string `json:"verdict"` // preselected / retired
	Reason     string `json:"reason"`
	WarmOK     bool   `json:"warm_ok"`
	Egress     string `json:"egress"`
	IP         string `json:"ip"`
	ISP        string `json:"isp"`
	Cookies    int    `json:"cookies"`
	CheckoutOK bool   `json:"checkout_ok"`
	Amount     string `json:"amount"`
	Currency   string `json:"currency"`
	CheckoutID string `json:"checkout_id"`
	DelayMs    int64  `json:"delay_ms"`
}

// Screen 对一个账号做完整检测：预热 → 出口 → 提链。
//
// 三步全过才进预选池。
//
// 参数
//
//	token, accountID  账号凭据
//	stages            阶段配置（用 link 阶段的出口）
//	base              基础配置
//
// 返回的 ScreenResult 里 Verdict 是 "preselected" 或 "retired"。
func (s *Session) Screen(
	ctx context.Context,
	token, accountID string,
	stages StagesCfg,
	base Config,
) *ScreenResult {
	res := &ScreenResult{}

	// 步骤 1：预热（内含出口探测）
	warm, err := s.WarmupStage(ctx, StageLink, stages, base)
	if warm != nil {
		res.WarmOK = warm.OK
		res.Egress = warm.Egress
		res.IP = warm.IP
		res.ISP = warm.ISP
		res.Cookies = warm.Cookies
	}
	if err != nil {
		res.Verdict = PoolRetired
		res.Reason = "预热失败: " + err.Error()
		return res
	}
	if !warm.OK {
		res.Verdict = PoolRetired
		res.Reason = "预热不通过: " + warm.Message
		return res
	}

	// 步骤 2：提链（验证能建 0 元单）
	co, err := s.CreateCheckout(ctx, base)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "unusual activity"):
			res.Reason = "风控: unusual activity"
		case strings.Contains(msg, "deactivated_workspace"):
			res.Reason = "工作区停用"
		case strings.Contains(msg, "token_revoked"):
			res.Reason = "AT 失效"
		default:
			res.Reason = "提链失败: " + truncate(msg, 80)
		}
		res.Verdict = PoolRetired
		return res
	}

	res.CheckoutID = co.CheckoutID
	res.Amount = co.Amount
	res.Currency = co.Currency

	// 步骤 3：判断是否 0 元
	if !IsZero(co.Amount) {
		res.CheckoutOK = false
		res.Verdict = PoolRetired
		res.Reason = fmt.Sprintf("金额非 0（%s %s），Team 号或促销不可用", co.Amount, co.Currency)
		return res
	}

	res.CheckoutOK = true
	res.Verdict = PoolPreselected
	res.Reason = fmt.Sprintf("预热通过，出口 %s，0 元单 %s %s",
		res.Egress, co.Amount, co.Currency)
	return res
}
