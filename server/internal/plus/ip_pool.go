package plus

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/tiantianGPU/reg-factory/server/internal/egress"
	"time"
)

// IP 预选池
// ---------
// 为什么要有这个东西
// ----------------
// cliproxy 每换一个 sid 就换一个出口 IP，质量参差不齐：
//
//	· 有的 IP 是住宅干净出口，OpenAI 完全放行
//	· 有的 IP 被 Cloudflare 直接 403
//	· 有的 IP 上跑过太多脏活，Radar 已经盯上
//
// 之前是「取一个就用」，结果：
//
//	· 每次绑卡都可能踩到脏 IP
//	· card_declined 里混着「IP 问题」和「卡问题」，无法区分
//	· 同一个坏 IP 被反复踩，浪费时间
//
// 所以先探测一批 IP，把干净的路由记下来放进池子，用的时候直接取。
//
// 检测维度
//
//	· 出口国家是否符合阶段要求（US / SG / ...）
//	· ISP 是否机房（"hosting" 标记的直接淘汰）
//	· 延迟是否可接受
//	· 能否通过 Cloudflare（首页 200 或 403-challenge 都算通过，DNS 失败不算）
const (
	IPGood  = "good"   // 检测通过，可取用
	IPBad   = "bad"    // 淘汰
	IPInUse = "in_use" // 使用中
	IPNew   = "new"    // 刚探测，未评级
)

// IPEntry 是池子里的一个 IP 记录。

// ---------------------------------------------------------------- 方言层
//
// 这两个文件写的时候假设后端是 SQLite：
//   · 建表用 AUTOINCREMENT
//   · 占位符用 ?
// 在 PG 上 ? 不会被转成 $N，报 "syntax error at or near ORDER"。
//
// SQLDialect 由 API 层在启动时设置（见 internal/api/plus_pool.go 的 init）。
// 默认 sqlite，保持原行为不变。

var (
	SQLDialect = "sqlite" // sqlite / postgres
)

// SetDialect 由外部在连接建立后调用。
func SetDialect(d string) {
	if d == "postgres" || d == "pg" {
		SQLDialect = "postgres"
	} else {
		SQLDialect = "sqlite"
	}
}

// rb 把 ? 按顺序换成 $1..$N。SQLite 后端原样返回。
func rb(query string) string {
	if SQLDialect != "postgres" {
		return query
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(query); i++ {
		c := query[i]
		if c == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// isDupColumn 判断「列已存在」。两种后端的措辞不同。
func isDupColumn(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "duplicate column") ||
		strings.Contains(m, "already exists") ||
		strings.Contains(m, "42701")
}

type IPEntry struct {
	ID        int64  `json:"id"`
	Stage     string `json:"stage"`  // link / prepare / bind
	Proxy     string `json:"proxy"`  // 完整代理地址（含凭据，接口层会脱敏）
	Egress    string `json:"egress"` // 实际出口国家
	Want      string `json:"want"`   // 期望国家
	IP        string `json:"ip"`
	ISP       string `json:"isp"`
	City      string `json:"city"`
	LatencyMs int64  `json:"latency_ms"`
	CFOK      bool   `json:"cf_ok"`     // 能否过 Cloudflare
	HomeCode  int    `json:"home_code"` // 首页 HTTP 码
	State     string `json:"state"`
	CheckedAt string `json:"checked_at"`
	Reason    string `json:"reason"`
	FailCount int    `json:"fail_count"` // 被使用后失败次数

	// 纯净度评估（不落库，每次探测现算）
	Purity *egress.PurityScore `json:"purity,omitempty"`
}

// IPPoolConfig 是探测参数。
type IPPoolConfig struct {
	Stage       Stage                               // 为哪个阶段探测
	Want        string                              // 期望出口国家
	ProxyTmpl   string                              // 代理模板，{sid} 会被替换
	Count       int                                 // 探测多少个
	Acquire     func(sid string) (ProxySpec, error) // 可选：从 provider 拿一个候选
	MinDelayMs  int64                               // 低于这个延迟才入库（0=不限制）
	CFPath      string                              // 用来测 CF 的路径，默认 /
	Concurrency int                                 // 并发探测数
}

// createTableSQL 按方言返回建表语句。
// SQLite 用 AUTOINCREMENT，PG 用 BIGSERIAL。
func createTableSQL() string {
	if SQLDialect == "postgres" {
		return `
CREATE TABLE IF NOT EXISTS ip_pool (
    id          BIGSERIAL PRIMARY KEY,
    stage       TEXT NOT NULL DEFAULT '',
    proxy       TEXT NOT NULL,
    egress      TEXT NOT NULL DEFAULT '',
    want        TEXT NOT NULL DEFAULT '',
    ip          TEXT NOT NULL DEFAULT '',
    isp         TEXT NOT NULL DEFAULT '',
    city        TEXT NOT NULL DEFAULT '',
    latency_ms  BIGINT NOT NULL DEFAULT 0,
    cf_ok       INTEGER NOT NULL DEFAULT 0,
    home_code   INTEGER NOT NULL DEFAULT 0,
    state       TEXT NOT NULL DEFAULT 'new',
    checked_at  TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    fail_count  INTEGER NOT NULL DEFAULT 0
)`
	}
	return `
CREATE TABLE IF NOT EXISTS ip_pool (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    stage       TEXT NOT NULL DEFAULT '',
    proxy       TEXT NOT NULL,
    egress      TEXT NOT NULL DEFAULT '',
    want        TEXT NOT NULL DEFAULT '',
    ip          TEXT NOT NULL DEFAULT '',
    isp         TEXT NOT NULL DEFAULT '',
    city        TEXT NOT NULL DEFAULT '',
    latency_ms  INTEGER NOT NULL DEFAULT 0,
    cf_ok       INTEGER NOT NULL DEFAULT 0,
    home_code   INTEGER NOT NULL DEFAULT 0,
    state       TEXT NOT NULL DEFAULT 'new',
    checked_at  TEXT NOT NULL DEFAULT '',
    reason      TEXT NOT NULL DEFAULT '',
    fail_count  INTEGER NOT NULL DEFAULT 0
)`
}

// EnsureIPPoolSchema 建 IP 池表（幂等）。
func EnsureIPPoolSchema(db *sql.DB) error {
	_, err := db.Exec(createTableSQL())
	if err != nil {
		return fmt.Errorf("建 ip_pool: %w", err)
	}
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_ip_pool_pick
	                ON ip_pool(stage, state, want, id)`)
	_, _ = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_ip_pool_unique
	                ON ip_pool(stage, proxy)`)
	return nil
}

// ProbeOneIP 探测一个候选 IP，返回评级结果。不写库。
func ProbeOneIP(
	ctx context.Context,
	proxyURL string,
	want string,
	cfg Config,
) *IPEntry {
	e := &IPEntry{Proxy: proxyURL, Want: strings.ToUpper(want), State: IPNew}

	if _, err := ParseProxyURL(proxyURL); err != nil {
		e.State = IPBad
		e.Reason = "代理地址无效: " + err.Error()
		return e
	}

	cfg.Proxy = proxyURL
	sess := NewSessionWithConfig("", "", cfg)

	started := time.Now()

	// 步骤 1：出口探测
	info, err := sess.ProbeEgress()
	e.LatencyMs = time.Since(started).Milliseconds()
	if err != nil {
		e.State = IPBad
		e.Reason = "出口探测失败: " + truncate(err.Error(), 80)
		return e
	}
	e.IP = info.IP
	e.Egress = info.Country
	e.ISP = info.ISP
	e.City = info.City
	e.CheckedAt = time.Now().Format(time.RFC3339)

	// 判据 1：国家匹配
	if e.Want != "" && e.Egress != e.Want {
		e.State = IPBad
		e.Reason = fmt.Sprintf("出口 %s ≠ 期望 %s", e.Egress, e.Want)
		return e
	}

	// 判据 2：机房 IP 淘汰
	if isDatacenterISP(e.ISP) {
		e.State = IPBad
		e.Reason = "机房 IP: " + e.ISP
		return e
	}

	// 步骤 2：Cloudflare 可达性
	warm, werr := sess.Warmup(ctx, cfg)
	if warm != nil {
		e.HomeCode = warm.HomeStatus
		e.CFOK = warm.OK
	}
	if werr != nil {
		e.State = IPBad
		e.Reason = "CF 预热失败: " + truncate(werr.Error(), 70)
		return e
	}
	if !e.CFOK {
		e.State = IPBad
		e.Reason = fmt.Sprintf("CF 不通过（首页 %d）", e.HomeCode)
		return e
	}

	// 判据 3：延迟
	if warm != nil && warm.LatencyMs > 0 {
		e.LatencyMs = warm.LatencyMs
	}

	// 判据 4：纯净度（最关键）
	//
	//「能过 CF」只说明没被 CF 拉黑，不代表干净。
	// 数据中心、代理出口、黑名单上的 IP 都能过 CF，但 Radar 一看就知道。
	e.Purity = egress.EvaluatePurity(ctx, info)
	if !e.Purity.Pass("B") {
		e.State = IPBad
		e.Reason = "纯净度不足：" + e.Purity.Note() +
			" | " + strings.Join(e.Purity.Reasons, "; ")
		return e
	}

	e.State = IPGood
	cookies := 0
	if warm != nil {
		cookies = warm.Cookies
	}
	e.Reason = fmt.Sprintf("通过：%s %s/%s %dms cookies=%d | %s",
		e.Egress, e.City, shortISP(e.ISP), e.LatencyMs, cookies, e.Purity.Note())
	return e
}

// ScreeningIPScore 计算一个 IP 的「干净度」评分，用来排序取用。
//
// 分数越高越优先：
//   - 住宅 ISP +30
//   - 有 cf_clearance cookie +20（说明 CF 认得这个 IP）
//   - 延迟低 +20
//   - 历史失败次数扣分
func (e *IPEntry) Score() int {
	// 纯净度是主权重：没有纯净度评估的 IP 直接 0 分
	if e.Purity == nil {
		return 0
	}
	s := e.Purity.Total

	// 延迟作为微调
	switch {
	case e.LatencyMs <= 800:
		s += 10
	case e.LatencyMs <= 1500:
		s += 5
	case e.LatencyMs > 3000:
		s -= 10
	}
	// 历史失败倒扣
	s -= e.FailCount * 15

	if s < 0 {
		s = 0
	}
	return s
}

// ---------------------------------------------------------------- 批量探测

// ScreenIPPool 探测一批 IP 并写库。
//
// 并发探测，每个探测独立会话。只有 state=good 的会留在池子里待用。
func ScreenIPPool(
	ctx context.Context,
	db *sql.DB,
	pcfg IPPoolConfig,
	base Config,
	logger func(string),
) ([]*IPEntry, error) {
	if logger == nil {
		logger = func(string) {}
	}
	if pcfg.Count <= 0 {
		pcfg.Count = 10
	}
	if pcfg.Concurrency <= 0 {
		pcfg.Concurrency = 3
	}

	// 生成候选
	candidates := make([]string, 0, pcfg.Count)
	for i := 0; i < pcfg.Count; i++ {
		if pcfg.Acquire != nil {
			// 从 provider 拿一个（带随机 sid）
			spec, err := pcfg.Acquire("")
			if err != nil {
				logger(fmt.Sprintf("[%d/%d] 取候选失败: %v", i+1, pcfg.Count, err))
				continue
			}
			candidates = append(candidates, spec.URL)
		} else if pcfg.ProxyTmpl != "" {
			// 模板替换：{sid} → 随机 8 位
			sid := randomSID(8)
			candidates = append(candidates, strings.ReplaceAll(pcfg.ProxyTmpl, "{sid}", sid))
		} else {
			return nil, fmt.Errorf("需要 IPPoolConfig.Acquire 或 ProxyTmpl")
		}
	}

	logger(fmt.Sprintf("开始探测 %d 个候选（并发 %d）", len(candidates), pcfg.Concurrency))

	results := make([]*IPEntry, len(candidates))
	var wg sync.WaitGroup
	sem := make(chan struct{}, pcfg.Concurrency)

	for i, cand := range candidates {
		wg.Add(1)
		go func(idx int, proxyURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			e := ProbeOneIP(ctx, proxyURL, pcfg.Want, base)
			e.Stage = string(pcfg.Stage)
			results[idx] = e

			mark := "✗"
			if e.State == IPGood {
				mark = "★"
			}
			logger(fmt.Sprintf("%s [%d/%d] %s  %s %s  %dms  %s",
				mark, idx+1, len(candidates), shortIP(e.IP), e.Egress,
				shortISP(e.ISP), e.LatencyMs, truncate(e.Reason, 50)))
		}(i, cand)
	}
	wg.Wait()

	// 写库
	if db != nil {
		good, bad := 0, 0
		for _, e := range results {
			if e == nil {
				continue
			}
			_, err := db.Exec(
				`INSERT INTO ip_pool
				   (stage, proxy, egress, want, ip, isp, city, latency_ms,
				    cf_ok, home_code, state, checked_at, reason)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
				 ON CONFLICT(stage, proxy) DO UPDATE SET
				   egress=excluded.egress, ip=excluded.ip, isp=excluded.isp,
				   city=excluded.city, latency_ms=excluded.latency_ms,
				   cf_ok=excluded.cf_ok, home_code=excluded.home_code,
				   state=excluded.state, checked_at=excluded.checked_at,
				   reason=excluded.reason`,
				e.Stage, e.Proxy, e.Egress, e.Want, e.IP, e.ISP, e.City,
				e.LatencyMs, boolToInt(e.CFOK), e.HomeCode, e.State,
				e.CheckedAt, e.Reason)
			if err != nil {
				logger("写库失败: " + err.Error())
				continue
			}
			if e.State == IPGood {
				good++
			} else {
				bad++
			}
		}
		logger(fmt.Sprintf("入库完成：good=%d bad=%d", good, bad))
	}

	return results, nil
}

// ---------------------------------------------------------------- 取用

// IPTake 从池里取一个最好的 IP，标记 in_use。
//
// 排序：先按评分降序（住宅 > 机房、低延迟、少失败），再按 id 升序。
// SQLite 没有自定义排序函数，所以取一批出来在 Go 侧排序。
func IPTake(db *sql.DB, stage Stage, want string) (*IPEntry, error) {
	q := `SELECT id, stage, proxy, egress, want, ip, isp, city, latency_ms,
	             cf_ok, home_code, state, checked_at, reason, fail_count
	      FROM ip_pool
	      WHERE stage = ? AND state = 'good'`
	args := []any{string(stage)}
	if want != "" {
		q += ` AND want = ?`
		args = append(args, strings.ToUpper(want))
	}
	q += ` LIMIT 200`

	rows, err := db.Query(rb(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cands []*IPEntry
	for rows.Next() {
		var e IPEntry
		var cf int
		if err := rows.Scan(&e.ID, &e.Stage, &e.Proxy, &e.Egress, &e.Want,
			&e.IP, &e.ISP, &e.City, &e.LatencyMs, &cf, &e.HomeCode,
			&e.State, &e.CheckedAt, &e.Reason, &e.FailCount); err != nil {
			continue
		}
		e.CFOK = cf != 0
		cands = append(cands, &e)
	}
	if len(cands) == 0 {
		return nil, nil
	}

	// 挑最高分
	best := cands[0]
	for _, c := range cands[1:] {
		if c.Score() > best.Score() {
			best = c
		}
	}

	// 标记 in_use（乐观锁：state 必须还是 good）
	res, err := db.Exec(
		`UPDATE ip_pool SET state='in_use' WHERE id=? AND state='good'`, best.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 被别人抢了，重来一次
		return IPTake(db, stage, want)
	}
	best.State = IPInUse
	return best, nil
}

// IPRelease 用完放回池子。
//
// success=false 时累加 fail_count；达到阈值（3）直接淘汰。
func IPRelease(db *sql.DB, id int64, success bool, reason string) error {
	if success {
		_, err := db.Exec(
			`UPDATE ip_pool SET state='good', reason=? WHERE id=?`, reason, id)
		return err
	}
	// 失败：累加计数，超阈值淘汰
	_, err := db.Exec(
		`UPDATE ip_pool SET fail_count=fail_count+1, reason=?,
		   state=CASE WHEN fail_count+1 >= 3 THEN 'bad' ELSE 'good' END
		 WHERE id=?`, reason, id)
	return err
}

// IPPoolStats 返回各状态的分布。
func IPPoolStats(db *sql.DB, stage Stage) (map[string]int, error) {
	q := `SELECT state, COUNT(*) n FROM ip_pool`
	args := []any{}
	if stage != "" {
		q += ` WHERE stage = ?`
		args = append(args, string(stage))
	}
	q += ` GROUP BY state`

	rows, err := db.Query(rb(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if rows.Scan(&st, &n) == nil {
			out[st] = n
		}
	}
	return out, nil
}

// IPPoolList 按状态列出 IP。
func IPPoolList(db *sql.DB, stage Stage, state string, limit int) ([]*IPEntry, error) {
	q := `SELECT id, stage, proxy, egress, want, ip, isp, city, latency_ms,
	             cf_ok, home_code, state, checked_at, reason, fail_count
	      FROM ip_pool WHERE 1=1`
	args := []any{}
	if stage != "" {
		q += ` AND stage = ?`
		args = append(args, string(stage))
	}
	if state != "" {
		q += ` AND state = ?`
		args = append(args, state)
	}
	q += ` ORDER BY id DESC`
	if limit > 0 {
		q += fmt.Sprintf(` LIMIT %d`, limit)
	}

	rows, err := db.Query(rb(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*IPEntry
	for rows.Next() {
		var e IPEntry
		var cf int
		if rows.Scan(&e.ID, &e.Stage, &e.Proxy, &e.Egress, &e.Want, &e.IP,
			&e.ISP, &e.City, &e.LatencyMs, &cf, &e.HomeCode, &e.State,
			&e.CheckedAt, &e.Reason, &e.FailCount) == nil {
			e.CFOK = cf != 0
			out = append(out, &e)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- 辅助

// isDatacenterISP 转发到 egress。
func isDatacenterISP(isp string) bool {
	return egress.IsDatacenterISP(isp)
}

func shortISP(s string) string {
	if len(s) > 28 {
		return s[:28]
	}
	return s
}

func shortIP(s string) string {
	if len(s) < 10 {
		return s
	}
	// 保留前两段，后面打码
	parts := strings.Split(s, ".")
	if len(parts) == 4 {
		return parts[0] + "." + parts[1] + ".x.x"
	}
	return s[:8] + "..."
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func randomSID(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[seededIntn(len(chars))]
	}
	return string(b)
}

// MaskProxy 把代理地址里的密码打码，供接口返回。
func MaskProxy(raw string) string {
	return ProxySpec{URL: raw}.String()
}
