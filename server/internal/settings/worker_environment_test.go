package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeDatabaseMustBeExplicitPostgres(t *testing.T) {
	t.Setenv("REG_FACTORY_PANEL_PASSWORD", "fixture-password")
	for _, dsn := range []string{"", "/data/regd.db", "postgresql://fixture@localhost/regd"} {
		t.Setenv("REGD_DB", dsn)
		cfg := Load()
		err := cfg.Validate()
		if dsn == "postgresql://fixture@localhost/regd" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "REGD_DB") {
			t.Fatal("missing PG configuration started a file database", dsn, err)
		}
	}
}

func TestWorkerSettingsRefreshFileAndOverrideDeploymentSeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.env")
	const fileKey = "SG_TEST_WORKER_FILE"
	const externalKey = "SG_TEST_WORKER_EXTERNAL"
	t.Setenv("REGD_ADDR", "127.0.0.1:8800")
	t.Setenv(externalKey, "deployment")
	os.Unsetenv(fileKey)
	defer func() {
		os.Unsetenv(fileKey)
		fileEnvironment.Lock()
		delete(fileEnvironment.keys, fileKey)
		fileEnvironment.Unlock()
	}()
	if err := os.WriteFile(path, []byte(fileKey+"=first\n"+externalKey+"=ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	LoadEnvFile(path)
	first, err := WorkerEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEnv(path).Update(map[string]string{fileKey: "second", externalKey: "saved", "REGD_ADDR": "cannot-change-live"}); err != nil {
		t.Fatal(err)
	}
	second, err := WorkerEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(first, "\n"), fileKey+"=first") || !strings.Contains(strings.Join(second, "\n"), fileKey+"=second") || !strings.Contains(strings.Join(second, "\n"), externalKey+"=saved") || !strings.Contains(strings.Join(second, "\n"), "REGD_ADDR=127.0.0.1:8800") {
		t.Fatal("file refresh or environment precedence broken")
	}
	if os.Getenv(fileKey) != "first" {
		t.Fatal("process snapshot changed")
	}
	if _, err := NewEnv(path).Update(map[string]string{externalKey: ""}); err != nil {
		t.Fatal(err)
	}
	if NewEnv(path).Effective(externalKey) != "" {
		t.Fatal("clearing resurrected deployment seed")
	}
	third, err := WorkerEnvironment(path)
	if err != nil || !strings.Contains(strings.Join(third, "\n"), externalKey+"=\n") {
		t.Fatal("worker did not receive explicit empty value", err)
	}
}
