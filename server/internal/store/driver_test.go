package store

import "testing"

// TestRebind 验证 ? → $N 转换。这是整个 PG 迁移的核心，
// 转错一处就是语法错误或参数错位。
func TestRebind(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "无占位符",
			in:   "SELECT 1",
			want: "SELECT 1",
		},
		{
			name: "单个",
			in:   "SELECT * FROM t WHERE id=?",
			want: "SELECT * FROM t WHERE id=$1",
		},
		{
			name: "多个",
			in:   "INSERT INTO t(a,b,c) VALUES(?,?,?)",
			want: "INSERT INTO t(a,b,c) VALUES($1,$2,$3)",
		},
		{
			name: "单引号字符串里的问号不能转",
			in:   "SELECT * FROM t WHERE a=? AND b='x?y'",
			want: "SELECT * FROM t WHERE a=$1 AND b='x?y'",
		},
		{
			name: "LIKE 模式",
			in:   "SELECT * FROM t WHERE name LIKE ?",
			want: "SELECT * FROM t WHERE name LIKE $1",
		},
		{
			name: "双引号标识符里的问号",
			in:   `SELECT * FROM "t?" WHERE a=?`,
			want: `SELECT * FROM "t?" WHERE a=$1`,
		},
		{
			name: "行注释",
			in:   "SELECT 1 -- 这里有个?\nWHERE a=?",
			want: "SELECT 1 -- 这里有个?\nWHERE a=$1",
		},
		{
			name: "块注释",
			in:   "SELECT 1 /* ? */ WHERE a=?",
			want: "SELECT 1 /* ? */ WHERE a=$1",
		},
		{
			name: "转义单引号",
			in:   "SELECT * FROM t WHERE a=? AND b='it''s ?'",
			want: "SELECT * FROM t WHERE a=$1 AND b='it''s ?'",
		},
		{
			name: "复杂 upsert",
			in: `INSERT INTO mail_pool(email,state) VALUES(?,?)
			     ON CONFLICT(email) DO UPDATE SET state=excluded.state
			     WHERE mail_pool.fail_count < ?`,
			want: `INSERT INTO mail_pool(email,state) VALUES($1,$2)
			     ON CONFLICT(email) DO UPDATE SET state=excluded.state
			     WHERE mail_pool.fail_count < $3`,
		},
		{
			name: "RETURNING",
			in:   "UPDATE t SET a=? WHERE b=? RETURNING id, name",
			want: "UPDATE t SET a=$1 WHERE b=$2 RETURNING id, name",
		},
		{
			name: "CASE WHEN",
			in:   "UPDATE t SET a=CASE WHEN b=? THEN ? ELSE a END WHERE c=?",
			want: "UPDATE t SET a=CASE WHEN b=$1 THEN $2 ELSE a END WHERE c=$3",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Rebind(c.in)
			if got != c.want {
				t.Errorf("\n输入: %s\n期望: %s\n实际: %s", c.in, c.want, got)
			}
		})
	}
}

// TestRebindIdempotent 已转换过的 SQL 再转一次不应改变（防止误用）。
func TestRebindIdempotent(t *testing.T) {
	src := "SELECT * FROM t WHERE a=$1"
	if got := Rebind(src); got != src {
		t.Errorf("已含 $N 的 SQL 不该变: %s → %s", src, got)
	}
}

// TestDialectOf 验证 DSN 识别。
func TestDialectOf(t *testing.T) {
	cases := map[string]Dialect{
		"/data/regd.db": DialectSQLite,
		"file:/data/regd.db?_pragma=busy_timeout(5000)": DialectSQLite,
		":memory:":                 DialectSQLite,
		"postgres://u:p@h:5433/db": DialectPG,
		"postgresql://u:p@h/db":    DialectPG,
		"host=127.0.0.1 port=5433 dbname=x user=y password=z": DialectPG,
	}
	for dsn, want := range cases {
		if got := DialectOf(dsn); got != want {
			t.Errorf("%s → %s，期望 %s", dsn, got, want)
		}
	}
}

// TestUpsertIgnore SQLite 与 PG 的「插入忽略」语法差异。
func TestUpsertIgnore(t *testing.T) {
	base := "INSERT INTO t(a) VALUES(?)"
	if got := UpsertIgnore(DialectSQLite, base); got != "INSERT OR IGNORE INTO t(a) VALUES(?)" {
		t.Errorf("SQLite: %s", got)
	}
	if got := UpsertIgnore(DialectPG, base); got != base+" ON CONFLICT DO NOTHING" {
		t.Errorf("PG: %s", got)
	}
}
