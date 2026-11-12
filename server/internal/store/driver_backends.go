package store

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// PG 连接构造。
//
// 用 pgx/v5 的 stdlib 适配层，保持 database/sql 接口不变，
// 这样业务代码完全不用改。

// openPG 打开 PG 读写连接。
func openPG(dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", normalizePGDSN(dsn))
	if err != nil {
		return nil, fmt.Errorf("打开 PG: %w", err)
	}
	applyPGPool(db)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("PG 连接失败: %w", err)
	}
	return db, nil
}

// openPGReadOnly 打开 PG 只读连接。
//
// 用 default_transaction_read_only=on 让服务端拒绝写操作，
// 比应用层「不发写语句」更可靠。
func openPGReadOnly(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(normalizePGDSN(dsn))
	if err != nil {
		return nil, fmt.Errorf("打开 PG 只读: %w", err)
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.ConnectTimeout = 10 * time.Second
	db := stdlib.OpenDB(*cfg)
	applyPGPool(db)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("PG 只读连接失败: %w", err)
	}
	return db, nil
}

// applyPGPool 设置连接池参数。
//
// 原来 SQLite 是 MaxOpenConns(8) / MaxIdleConns(4) —— 单文件怕写锁。
// PG 可以放开一些，但仍要防止 regd + mail + 定时任务同时打满。
func applyPGPool(db *sql.DB) {
	db.SetMaxOpenConns(24)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)
}

// normalizePGDSN 把几种写法统一成 URL 形式。
//
//	host=... port=... dbname=... user=... password=...   →  key=value，pgx 直接支持
//	postgres://...                                        →  原样
//
// 需要特殊处理的是密码里含特殊字符的 key=value 形式：
// 直接拼进 URL 会出问题，所以 key=value 一律原样交给 pgx，
// 只有显式 URL 才做校验。
func normalizePGDSN(dsn string) string {
	s := strings.TrimSpace(dsn)
	if s == "" {
		return s
	}
	low := strings.ToLower(s)

	// 已经是 URL
	if strings.HasPrefix(low, "postgres://") || strings.HasPrefix(low, "postgresql://") {
		// 确保带 sslmode，本地容器连接不需要 TLS
		if !strings.Contains(low, "sslmode=") {
			sep := "?"
			if strings.Contains(s, "?") {
				sep = "&"
			}
			s += sep + "sslmode=disable"
		}
		return s
	}

	// key=value 形式：补上缺省项
	if strings.Contains(s, "=") {
		if !strings.Contains(low, "sslmode") {
			s += " sslmode=disable"
		}
		return s
	}

	// 裸 URL（无 scheme），按 postgres 处理
	if u, err := url.Parse("postgres://" + s); err == nil && u.Host != "" {
		u.Scheme = "postgres"
		if !strings.Contains(u.RawQuery, "sslmode") {
			q := u.Query()
			q.Set("sslmode", "disable")
			u.RawQuery = q.Encode()
		}
		return u.String()
	}

	return s
}

// ---------------------------------------------------------------- SQLite

// openSQLite 打开 SQLite 读写连接。
//
// 保持原来的 PRAGMA 设置（busy_timeout / WAL / synchronous），
// 这是回滚路径，行为必须和迁移前完全一致。
func openSQLite(dsn string) (*sql.DB, error) {
	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite: %w", err)
	}
	handle.SetMaxOpenConns(8)
	handle.SetMaxIdleConns(4)
	if err := handle.Ping(); err != nil {
		handle.Close()
		return nil, fmt.Errorf("SQLite 连接失败: %w", err)
	}
	return handle, nil
}

// openSQLiteReadOnly 打开 SQLite 只读连接。
func openSQLiteReadOnly(dsn string) (*sql.DB, error) {
	ro := dsn
	if strings.HasPrefix(ro, "file:") {
		if !strings.Contains(ro, "mode=ro") {
			if strings.Contains(ro, "?") {
				ro += "&mode=ro"
			} else {
				ro += "?mode=ro"
			}
		}
		if !strings.Contains(ro, "query_only") {
			ro += "&_pragma=query_only(1)"
		}
	} else {
		ro = "file:" + ro + "?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"
	}
	return openSQLite(ro)
}
