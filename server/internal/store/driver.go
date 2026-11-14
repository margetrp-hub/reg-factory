package store

// 数据库驱动抽象
// ============
//
// 原来只有一个 SQLite，代码里到处是 sql.Open("sqlite", ...)。
// 现在要能同时支持 PostgreSQL 和 SQLite，且业务 SQL 一行不改。
//
// 做法
// ----
//  1. SQL 统一用 ? 占位符书写（原本就是，保持不动）
//  2. 走 PG 时用 rebind 在运行时把 ? 转成 $1/$2/...
//  3. 驱动、DSN、方言差异集中在 Dialect 里
//  4. 通过 DSN scheme 自动判断用哪个：
//
//     sqlite:  file:/data/regd.db?_pragma=...
//     pg:      postgres://user:pass@host:port/db
//              或 host=127.0.0.1 port=5433 dbname=regfactory user=postgres password=xxx
//
// 为什么不在 SQL 里直接写 $1
// -----------------------
// store 包有 694 行 store.go + account_library / production / inventory 等
// 十几个文件，? 占位符出现几千次。手工转换极易漏改且无法静态检查；
// 运行时 rebind 只有一处实现，可单测覆盖。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// Dialect 标识后端类型。
type Dialect string

const (
	DialectSQLite Dialect = "sqlite"
	DialectPG     Dialect = "pg"
)

// ---------------------------------------------------------------- rebind

// rebindState 是 rebind 用的缓冲池，避免每条 SQL 都新建 Builder。
var rebindPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

// Rebind 把 ? 占位符转成 PG 的 $1/$2/...
//
// 需要跳过的场景：
//   - 单引号字符串里的 ?（'a?b'）
//   - 双引号标识符里的 ?（"col?"）
//   - $ 后面跟数字的已有占位符（防止二次转换）
//   - -- 行注释和 /* */ 块注释
//
// 这些在业务 SQL 里都会出现（比如 LIKE '%?%'、注释里的问号），
// 转换错了会直接语法错误，所以状态机必须完整。
func Rebind(query string) string {
	// 没有 ? 直接返回，省一次分配
	if !strings.ContainsRune(query, '?') {
		return query
	}

	bp := rebindPool.Get().(*strings.Builder)
	b := bp
	b.Reset()
	b.Grow(len(query) + 16)

	n := 0
	i := 0
	length := len(query)

	for i < length {
		c := query[i]

		// 单引号字符串：一直吃到下一个未转义的单引号
		if c == '\'' {
			// PostgreSQL ordinary strings keep backslashes literal. Only E'...'
			// escapes a quote; otherwise ESCAPE '\' hides later placeholders.
			escapedString := i > 0 && (query[i-1] == 'E' || query[i-1] == 'e')
			b.WriteByte(c)
			i++
			for i < length {
				ch := query[i]
				b.WriteByte(ch)
				i++
				if ch == '\'' {
					// '' 是转义的单引号，继续
					if i < length && query[i] == '\'' {
						b.WriteByte(query[i])
						i++
						continue
					}
					break
				}
				if escapedString && ch == '\\' && i < length {
					b.WriteByte(query[i])
					i++
				}
			}
			continue
		}

		// 双引号标识符
		if c == '"' {
			b.WriteByte(c)
			i++
			for i < length {
				ch := query[i]
				b.WriteByte(ch)
				i++
				if ch == '"' {
					break
				}
			}
			continue
		}

		// 行注释 -- ... \n
		if c == '-' && i+1 < length && query[i+1] == '-' {
			for i < length && query[i] != '\n' {
				b.WriteByte(query[i])
				i++
			}
			continue
		}

		// 块注释 /* ... */
		if c == '/' && i+1 < length && query[i+1] == '*' {
			b.WriteString("/*")
			i += 2
			for i+1 < length && !(query[i] == '*' && query[i+1] == '/') {
				b.WriteByte(query[i])
				i++
			}
			if i+1 < length {
				b.WriteString("*/")
				i += 2
			}
			continue
		}

		// 占位符
		if c == '?' {
			n++
			b.WriteByte('$')
			fmt.Fprintf(b, "%d", n)
			i++
			continue
		}

		b.WriteByte(c)
		i++
	}

	out := b.String()
	rebindPool.Put(bp)
	return out
}

// ---------------------------------------------------------------- 连接

// DSN 判断一个连接串指向哪种后端。
//
//	postgres://...   postgresql://...   host=... port=...    → PG
//	file:...         /path/to.db         :memory:            → SQLite
func DialectOf(dsn string) Dialect {
	low := strings.ToLower(strings.TrimSpace(dsn))
	switch {
	case strings.HasPrefix(low, "postgres://"),
		strings.HasPrefix(low, "postgresql://"),
		strings.Contains(low, "dbname="),
		strings.Contains(low, "host=") && strings.Contains(low, "user="):
		return DialectPG
	}
	return DialectSQLite
}

// OpenDSN 按 DSN 打开连接。
//
// 返回值里的 *sql.DB 对两种后端都是 standard database/sql，
// 调用方不用区分。
func OpenDSN(dsn string) (*sql.DB, Dialect, error) {
	d := DialectOf(dsn)
	switch d {
	case DialectPG:
		db, err := openPG(dsn)
		return db, d, err
	default:
		db, err := openSQLite(dsn)
		return db, d, err
	}
}

// OpenDSNReadOnly 打开只读连接。
//
// SQLite 用 file:...+mode=ro 实现真正的只读；
// PG 用 default_transaction_read_only=on，防止误写。
func OpenDSNReadOnly(dsn string) (*sql.DB, Dialect, error) {
	d := DialectOf(dsn)
	switch d {
	case DialectPG:
		db, err := openPGReadOnly(dsn)
		return db, d, err
	default:
		db, err := openSQLiteReadOnly(dsn)
		return db, d, err
	}
}

// ---------------------------------------------------------------- 查询执行

// Execer 是可 rebind 的执行接口。
//
// store 包里所有 d.ex.Exec / Query / QueryRow 都换成经这个包，
// 由 dialect 决定是否 rebind。
type Execer struct {
	db      *sql.DB
	dialect Dialect
}

// NewExecer 包装一个连接。
func NewExecer(db *sql.DB, d Dialect) *Execer {
	return &Execer{db: db, dialect: d}
}

// DB 返回底层连接。
func (e *Execer) DB() *sql.DB { return e.db }

func (e *Execer) PrepareTx(tx *sql.Tx, query string) (*sql.Stmt, error) {
	return tx.Prepare(e.prepare(query))
}

func (e *Execer) ExecTxContext(ctx context.Context, tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	return tx.ExecContext(ctx, e.prepare(query), args...)
}

func (e *Execer) QueryTxContext(ctx context.Context, tx *sql.Tx, query string, args ...any) (*sql.Rows, error) {
	return tx.QueryContext(ctx, e.prepare(query), args...)
}

// Dialect 返回方言。
func (e *Execer) Dialect() Dialect { return e.dialect }

// prepare 按方言决定是否 rebind。
func (e *Execer) prepare(query string) string {
	if e.dialect == DialectPG {
		return Rebind(query)
	}
	return query
}

// Exec 执行。
//
// PG 的 Exec 不支持多语句（SQLite 支持）。建表 / 建索引这类 DDL
// 经常用 `;` 连写，所以在 PG 分支自动拆分逐条执行。
//
// 只拆「无参数」的多语句：带参数时拆分会打乱占位符顺序，
// 那种情况按单语句处理，让 PG 自己报错更安全。
func (e *Execer) Exec(query string, args ...any) (sql.Result, error) {
	q := e.prepare(query)
	if e.dialect == DialectPG && len(args) == 0 && hasMultipleStatements(q) {
		return e.execMulti(context.Background(), q)
	}
	return e.db.Exec(q, args...)
}

// ExecContext 带 context 的执行。
func (e *Execer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	q := e.prepare(query)
	if e.dialect == DialectPG && len(args) == 0 && hasMultipleStatements(q) {
		return e.execMulti(ctx, q)
	}
	return e.db.ExecContext(ctx, q, args...)
}

// hasMultipleStatements 判断 SQL 里是否有多条语句。
//
// 注意不能用「最后一个分号后是否还有内容」来判断：
//
//	"CREATE TABLE a(); CREATE TABLE b();"   ← 两条，但最后分号后为空
//
// 正确做法是数「顶层有效分号」的数量：数到第一个就返回 true。
func hasMultipleStatements(query string) bool {
	const (
		stNormal = iota
		stStr
		stLineComment
		stBlockComment
	)
	state := stNormal

	for i := 0; i < len(query); i++ {
		c := query[i]

		switch state {
		case stStr:
			if c == '\\' && i+1 < len(query) {
				i++
				continue
			}
			if c == '\'' {
				if i+1 < len(query) && query[i+1] == '\'' {
					i++
					continue
				}
				state = stNormal
			}
			continue
		case stLineComment:
			if c == '\n' {
				state = stNormal
			}
			continue
		case stBlockComment:
			if c == '*' && i+1 < len(query) && query[i+1] == '/' {
				i++
				state = stNormal
			}
			continue
		}

		switch c {
		case '\'':
			state = stStr
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				i++
				state = stLineComment
			}
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				i++
				state = stBlockComment
			}
		case ';':
			// 找到一个顶层分号。若其后还有非空内容 → 多语句
			if strings.TrimSpace(query[i+1:]) != "" {
				return true
			}
			// 末尾分号，不算多语句
			return false
		}
	}
	return false
}

// splitStatements 按顶层分号拆分。
//
// 状态机必须正确跳过字符串和注释里的分号，否则
// `WHERE state NOT IN ('failed','cancelled')` 这种会被拆断。
func splitStatements(query string) []string {
	var out []string
	var cur strings.Builder

	const (
		stNormal = iota
		stStr
		stLineComment
		stBlockComment
	)
	state := stNormal

	for i := 0; i < len(query); i++ {
		c := query[i]

		switch state {
		case stStr:
			cur.WriteByte(c)
			if c == '\\' && i+1 < len(query) {
				// 反斜杠转义
				cur.WriteByte(query[i+1])
				i++
				continue
			}
			if c == '\'' {
				// '' 是转义的单引号，字符串继续
				if i+1 < len(query) && query[i+1] == '\'' {
					cur.WriteByte(query[i+1])
					i++
					continue
				}
				state = stNormal
			}
			continue

		case stLineComment:
			cur.WriteByte(c)
			if c == '\n' {
				state = stNormal
			}
			continue

		case stBlockComment:
			cur.WriteByte(c)
			if c == '*' && i+1 < len(query) && query[i+1] == '/' {
				cur.WriteByte(query[i+1])
				i++
				state = stNormal
			}
			continue
		}

		// stNormal
		switch c {
		case '\'':
			// 写入开引号后进入字符串状态；下一轮从引号后一个字符开始判断
			cur.WriteByte(c)
			state = stStr
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				cur.WriteByte(c)
				cur.WriteByte(query[i+1])
				i++
				state = stLineComment
				continue
			}
			cur.WriteByte(c)
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				cur.WriteByte(c)
				cur.WriteByte(query[i+1])
				i++
				state = stBlockComment
				continue
			}
			cur.WriteByte(c)
		case ';':
			s := strings.TrimSpace(cur.String())
			if s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}

	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// execMulti 逐条执行多条语句，返回最后一条的结果。
func (e *Execer) execMulti(ctx context.Context, query string) (sql.Result, error) {
	stmts := splitStatements(query)
	var last sql.Result
	for i, s := range stmts {
		if strings.TrimSpace(s) == "" {
			continue
		}
		r, err := e.db.ExecContext(ctx, s)
		if err != nil {
			return nil, fmt.Errorf("多语句第 %d/%d 条失败: %w", i+1, len(stmts), err)
		}
		last = r
	}
	return last, nil
}

// QueryTx 在事务内执行查询，按方言 rebind。
func (e *Execer) QueryTx(tx *sql.Tx, query string, args ...any) (*sql.Rows, error) {
	q := e.prepare(query)
	rows, err := tx.Query(q, args...)
	if err != nil {
		head := q
		if len(head) > 300 {
			head = head[:300] + "..."
		}
		return nil, fmt.Errorf("%w\n  SQL: %s", err, head)
	}
	return rows, nil
}

// Query 查询。
//
// 出错时把实际执行的 SQL 带进错误里 —— PG 的报错只说 "syntax error at
// or near ..."，不知道是哪条语句，排查成本极高。
func (e *Execer) Query(query string, args ...any) (*sql.Rows, error) {
	q := e.prepare(query)
	rows, err := e.db.Query(q, args...)
	if err != nil {
		return nil, wrapQueryErr(err, q)
	}
	return rows, nil
}

// wrapQueryErr 把 SQL 片段附在错误信息后面。
func wrapQueryErr(err error, sqlText string) error {
	if err == nil {
		return nil
	}
	// 只保留前 300 字符，避免日志爆掉
	head := sqlText
	if len(head) > 300 {
		head = head[:300] + "..."
	}
	return fmt.Errorf("%w\n  SQL: %s", err, head)
}

// QueryContext 带 context 的查询。
func (e *Execer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return e.db.QueryContext(ctx, e.prepare(query), args...)
}

// QueryRow 单行查询。
func (e *Execer) QueryRow(query string, args ...any) *sql.Row {
	return e.db.QueryRow(e.prepare(query), args...)
}

// QueryRowContext 带 context 的单行查询。
func (e *Execer) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return e.db.QueryRowContext(ctx, e.prepare(query), args...)
}

// ExecTx 在事务内执行一条或多条语句，按方言处理。
//
// 事务内的语句和普通 Exec 一样需要两件事：
//  1. rebind（? → $N）
//  2. PG 下多语句要拆开逐条执行
//
// 直接用 tx.Exec 会绕过这两步，在 PG 上必然失败。
func (e *Execer) ExecTx(tx *sql.Tx, query string) (sql.Result, error) {
	q := e.prepare(query)
	if e.dialect == DialectPG && hasMultipleStatements(q) {
		stmts := splitStatements(q)
		var last sql.Result
		for _, s := range stmts {
			if strings.TrimSpace(s) == "" {
				continue
			}
			r, err := tx.Exec(s)
			if err != nil {
				return nil, fmt.Errorf("事务内多语句失败: %w", err)
			}
			last = r
		}
		return last, nil
	}
	return tx.Exec(q)
}

// ExecTxArgs 在事务内执行带参数的单条语句。
func (e *Execer) ExecTxArgs(tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	return tx.Exec(e.prepare(query), args...)
}

// QueryRowTx 在事务内执行单行查询。
func (e *Execer) QueryRowTx(tx *sql.Tx, query string, args ...any) *sql.Row {
	return tx.QueryRow(e.prepare(query), args...)
}

// ExecEach 把多语句拆开逐条执行，出错时报出具体是哪条。
//
// 与 execMulti 的区别：execMulti 只在 PG 下走拆分（SQLite 原生支持多语句）；
// ExecEach 无条件拆分，两种后端行为一致，便于定位问题。
//
// 建表 / 建索引这类 DDL 应该用 ExecEach —— 它们本来就该一条一条执行，
// 拆开之后错误位置明确，也不依赖驱动对多语句的支持程度。
func (e *Execer) ExecEach(query string) error {
	stmts := splitStatements(e.prepare(query))
	for i, s := range stmts {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, err := e.db.Exec(s); err != nil {
			head := s
			if len(head) > 100 {
				head = head[:100] + "..."
			}
			return fmt.Errorf("第 %d/%d 条失败 (%s): %w", i+1, len(stmts), head, err)
		}
	}
	return nil
}

// ExecEachTx 是 ExecEach 的事务版。
func (e *Execer) ExecEachTx(tx *sql.Tx, query string) error {
	stmts := splitStatements(e.prepare(query))
	for i, s := range stmts {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, err := tx.Exec(s); err != nil {
			head := s
			if len(head) > 100 {
				head = head[:100] + "..."
			}
			return fmt.Errorf("事务内第 %d/%d 条失败 (%s): %w", i+1, len(stmts), head, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- 方言差异

// UpsertIgnore 生成「插入，冲突时忽略」的语句。
//
//	SQLite:  INSERT OR IGNORE INTO ...
//	PG:      INSERT INTO ... ON CONFLICT DO NOTHING
func UpsertIgnore(d Dialect, insertSQL string) string {
	switch d {
	case DialectPG:
		s := strings.TrimSpace(insertSQL)
		if strings.HasSuffix(strings.ToUpper(s), "DO NOTHING") {
			return insertSQL
		}
		return insertSQL + " ON CONFLICT DO NOTHING"
	}
	// SQLite：把 INSERT INTO 换成 INSERT OR IGNORE INTO
	return strings.Replace(insertSQL, "INSERT INTO", "INSERT OR IGNORE INTO", 1)
}

// LastInsertID 拿自增主键。
//
// SQLite 支持 LastInsertId，PG 要 RETURNING。这里统一用 PG 风格的
// RETURNING id（SQLite 3.35+ 也支持），所以两边都能用。
//
// 调用方应改写为：
//
//	var id int64
//	err := ex.QueryRow(`INSERT INTO t(...) VALUES(...) RETURNING id`, args...).Scan(&id)
func LastInsertIDHint(d Dialect) string {
	return "RETURNING id"
}

// NowSeconds 返回当前 unix 秒的 SQL 表达式。
//
// 业务代码大多直接传 float64(time.Now().Unix())，这个只在少数
// 需要数据库计时的地方用。
func NowSeconds(d Dialect) string {
	switch d {
	case DialectPG:
		return "EXTRACT(EPOCH FROM NOW())::double precision"
	}
	return "strftime('%s','now')"
}
