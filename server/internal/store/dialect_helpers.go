package store

// SQLite 专属 SQL 的方言替代
// ======================
//
// 迁移到 PG 后发现 5 处硬编码了 SQLite 的系统表查询：
//
//   PRAGMA table_info(t)                         列清单
//   SELECT sql FROM sqlite_master WHERE ...      索引/表定义
//   PRAGMA database_list                         库路径
//
// 这些在 PG 里都有对等物，但语法完全不同。集中到 Dialect 上，
// 业务代码改调方法，不再直接写系统表 SQL。

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// JSONText reads a fixed top-level object key or array index. Malformed historic
// task arguments and log lines remain readable and never abort a whole page.
// column and key are source-code constants, not request input.
func (e *Execer) JSONText(column, key string) string {
	if e.dialect == DialectPG {
		return "(CASE WHEN " + column + " IS JSON THEN " + column + "::jsonb #>> '{" + key + "}' END)"
	}
	path := "$." + key
	if _, err := strconv.Atoi(key); err == nil {
		path = "$[" + key + "]"
	}
	return "(CASE WHEN json_valid(" + column + ") THEN json_extract(" + column + ",'" + path + "') END)"
}

// SQLite serializes writers; PostgreSQL needs the same exclusion for each
// logical admission key. Unrelated accounts keep their own independent locks.
func (e *Execer) LockKey(tx *sql.Tx, key string) error {
	if e.dialect != DialectPG {
		return nil
	}
	_, err := e.ExecTxArgs(tx, `SELECT pg_advisory_xact_lock(hashtextextended(current_schema() || ':' || ?,0))`, key)
	return err
}

// SchemaDDL renders this service's static table definitions for a fresh database.
// Existing tables and business rows are never replaced by initialization.
func SchemaDDL(ddl string, d Dialect) string {
	if d != DialectPG {
		return ddl
	}
	return strings.NewReplacer("INTEGER PRIMARY KEY AUTOINCREMENT", "BIGSERIAL PRIMARY KEY",
		" REAL", " DOUBLE PRECISION").Replace(ddl)
}

// ---------------------------------------------------------------- 列清单

// ListColumns 返回一张表的列名（按定义顺序）。
//
//	SQLite:  PRAGMA table_info(t) → (cid, name, type, notnull, dflt, pk)
//	PG:      information_schema.columns → (column_name)
func (e *Execer) ListColumns(table string) ([]string, error) {
	switch e.dialect {
	case DialectPG:
		rows, err := e.Query(`
SELECT column_name FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = ?
ORDER BY ordinal_position`, table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err == nil {
				out = append(out, c)
			}
		}
		return out, rows.Err()
	default:
		rows, err := e.Query(`PRAGMA table_info(` + quoteIdent(table, DialectSQLite) + `)`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var cid, notnull, pk int
			var name, ctype string
			var dflt any
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err == nil {
				out = append(out, name)
			}
		}
		return out, rows.Err()
	}
}

// HasColumn 判断某表是否有某列。
func (e *Execer) HasColumn(table, column string) (bool, error) {
	cols, err := e.ListColumns(table)
	if err != nil {
		return false, err
	}
	for _, c := range cols {
		if c == column {
			return true, nil
		}
	}
	return false, nil
}

// TableExists 判断表是否已存在。
//
//	SQLite:  查 sqlite_master
//	PG:      查 information_schema.tables
func (e *Execer) TableExists(table string) (bool, error) {
	switch e.dialect {
	case DialectPG:
		var n int
		err := e.QueryRow(`
SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema = current_schema() AND table_name = ?`, table).Scan(&n)
		return n > 0, err
	default:
		var n int
		err := e.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		return n > 0, err
	}
}

// ---------------------------------------------------------------- 索引

// IndexSQL 返回索引的定义 SQL。
//
//	SQLite:  sqlite_master.sql（就是建索引的原语句）
//	PG:      pg_indexes.indexdef（也是可直接执行的语句）
//
// 用途：某些迁移逻辑要先判断「旧索引长什么样」来决定是否重建。
func (e *Execer) IndexSQL(name string) (string, error) {
	var def sql.NullString
	var err error
	switch e.dialect {
	case DialectPG:
		err = e.QueryRow(
			`SELECT indexdef FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ?`,
			name).Scan(&def)
	default:
		err = e.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&def)
	}
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !def.Valid {
		return "", nil
	}
	return def.String, nil
}

// BodyTextLen 返回 mail_messages.body_text 的声明类型。
//
// SQLite 里类型无关紧要（都是动态类型），所以原来用
// `SELECT sql FROM sqlite_master WHERE name='mail_messages'`
// 拿建表语句做字符串匹配。PG 里改用 information_schema 直接拿类型。
func (e *Execer) BodyTextLen() (string, error) {
	switch e.dialect {
	case DialectPG:
		var typ sql.NullString
		err := e.QueryRow(`
SELECT data_type FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name = 'mail_messages' AND column_name = 'body_text'`).Scan(&typ)
		if err == sql.ErrNoRows {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return typ.String, nil
	default:
		var def sql.NullString
		err := e.QueryRow(
			`SELECT sql FROM sqlite_master WHERE name='mail_messages'`).Scan(&def)
		if err == sql.ErrNoRows {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return def.String, nil
	}
}

// ---------------------------------------------------------------- 库路径

// DatabasePath 返回数据库标识，用于诊断输出。
//
//	SQLite:  PRAGMA database_list 的 file 字段
//	PG:      current_database() + current_schema()
func (e *Execer) DatabasePath() (string, error) {
	switch e.dialect {
	case DialectPG:
		var db, schema string
		err := e.QueryRow(
			`SELECT current_database(), current_schema()`).Scan(&db, &schema)
		if err != nil {
			return "", err
		}
		return db + "/" + schema, nil
	default:
		rows, err := e.Query(`PRAGMA database_list`)
		if err != nil {
			return "", err
		}
		defer rows.Close()
		for rows.Next() {
			var seq int
			var name, path sql.NullString
			if err := rows.Scan(&seq, &name, &path); err != nil {
				continue
			}
			if name.String == "main" {
				return path.String, nil
			}
		}
		return "", nil
	}
}

// ---------------------------------------------------------------- 标识符

// quoteIdent 按方言给标识符加引号。
func quoteIdent(name string, d Dialect) string {
	// 只留安全字符，防注入
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return -1
	}, name)
	switch d {
	case DialectPG:
		return `"` + clean + `"`
	default:
		return clean
	}
}

// EnsureColumn 是幂等的加列。
//
// 两种后端都支持 ALTER TABLE ADD COLUMN，但重复加会报错，
// 所以先查再加。
func (e *Execer) EnsureColumn(table, column, definition string) error {
	ok, err := e.HasColumn(table, column)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	_, err = e.db.Exec(
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`,
			quoteIdent(table, e.dialect), quoteIdent(column, e.dialect), definition))
	if err != nil && isDuplicateColumn(err) {
		return nil
	}
	return err
}

// isDuplicateColumn 判断「列已存在」错误（两种后端的措辞不同）。
func isDuplicateColumn(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") ||
		strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "42701")
}
