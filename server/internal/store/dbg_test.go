package store

import "testing"

func TestEnsureProductionSQL(t *testing.T) {
	sql := `CREATE TABLE IF NOT EXISTS production_results (
 operation_id TEXT PRIMARY KEY, request_sha TEXT NOT NULL, parameters TEXT NOT NULL,
 source_key TEXT NOT NULL, source_ref TEXT NOT NULL, source_sha TEXT NOT NULL,
 account_ref TEXT NOT NULL UNIQUE, email TEXT NOT NULL, artifact TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'queued', result TEXT NOT NULL DEFAULT '{}',
 checked_at INTEGER NOT NULL DEFAULT 0, finished_at TEXT NOT NULL DEFAULT '',
 UNIQUE(source_key,source_sha));
 CREATE UNIQUE INDEX IF NOT EXISTS production_source_active ON production_results(source_key)
 WHERE state NOT IN ('failed','cancelled');
 CREATE TABLE IF NOT EXISTS production_admissions (
 operation_id TEXT PRIMARY KEY, request_sha TEXT NOT NULL, parameters TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'unknown', error_code TEXT NOT NULL DEFAULT 'PRODUCTION_ADMISSION_INTERRUPTED',
 finished_at TEXT NOT NULL DEFAULT '')`

	t.Logf("hasMultipleStatements = %v", hasMultipleStatements(sql))
	rebound := Rebind(sql)
	t.Logf("rebind 后 hasMulti = %v", hasMultipleStatements(rebound))
	stmts := splitStatements(rebound)
	t.Logf("拆出 %d 条", len(stmts))
	for i, s := range stmts {
		// 只打印前 80 字符，避免刷屏
		show := s
		if len(show) > 80 {
			show = show[:80] + "..."
		}
		t.Logf("  [%d] %s", i, show)
	}
}

// 测真实的 EnsureProduction 调用的那段（带尾分号的 4 条）
func TestProductionRunsSQL(t *testing.T) {
	sql := `CREATE TABLE IF NOT EXISTS production_materials (
 context_sha TEXT PRIMARY KEY, envelope TEXT NOT NULL, expires_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS production_runs (
 operation_id TEXT PRIMARY KEY, material TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS production_stops(operation_id TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS production_outputs(operation_id TEXT NOT NULL);`

	t.Logf("hasMultipleStatements = %v", hasMultipleStatements(sql))
	stmts := splitStatements(sql)
	t.Logf("拆出 %d 条", len(stmts))
	for i, s := range stmts {
		show := s
		if len(show) > 70 {
			show = show[:70] + "..."
		}
		t.Logf("  [%d] %s", i, show)
	}
}
