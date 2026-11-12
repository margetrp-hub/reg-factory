package store

import "testing"

func TestSplitStatements(t *testing.T) {
	// 关键场景：字符串里的逗号和分号、注释里的分号
	sql := `CREATE TABLE a (x TEXT, y TEXT);
CREATE UNIQUE INDEX i ON a(x) WHERE y NOT IN ('failed','cancelled');
CREATE TABLE b (z TEXT)`
	got := splitStatements(sql)
	if len(got) != 3 {
		t.Fatalf("期望 3 条，实际 %d: %#v", len(got), got)
	}
	for i, s := range got {
		t.Logf("[%d] %s", i, s)
	}
	if got[1] != `CREATE UNIQUE INDEX i ON a(x) WHERE y NOT IN ('failed','cancelled')` {
		t.Errorf("第 2 条被拆断: %s", got[1])
	}
}
