package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerSettingsRefreshFileButPreserveDeploymentOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.env")
	const fileKey = "SG_TEST_WORKER_FILE"
	const externalKey = "SG_TEST_WORKER_EXTERNAL"
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
	if _, err := NewEnv(path).Update(map[string]string{fileKey: "second", externalKey: "still-ignored"}); err != nil {
		t.Fatal(err)
	}
	second, err := WorkerEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(first, "\n"), fileKey+"=first") || !strings.Contains(strings.Join(second, "\n"), fileKey+"=second") || !strings.Contains(strings.Join(second, "\n"), externalKey+"=deployment") {
		t.Fatal("file refresh or environment precedence broken")
	}
	if os.Getenv(fileKey) != "first" {
		t.Fatal("process snapshot changed")
	}
}
