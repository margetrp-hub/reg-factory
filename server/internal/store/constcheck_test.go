package store

import "testing"

func TestConstSplit(t *testing.T) {
	sql := sqlEnsureProduction
	t.Logf("长度 %d", len(sql))
	t.Logf("hasMulti = %v", hasMultipleStatements(sql))
	reb := Rebind(sql)
	t.Logf("rebind 后 hasMulti = %v", hasMultipleStatements(reb))
	stmts := splitStatements(reb)
	t.Logf("拆出 %d 条", len(stmts))
	for i, s := range stmts {
		show := s
		if len(show) > 90 {
			show = show[:90] + "..."
		}
		t.Logf("  [%d] len=%d %q", i, len(s), show)
	}
}
