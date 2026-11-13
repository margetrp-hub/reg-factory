package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestLocalReportDoesNotRequireUnusedProductionConnection(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "tools"), 0700)
	if err := os.WriteFile(filepath.Join(root, "tools", "asset_ops.py"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	env := settings.NewEnv(filepath.Join(root, ".env"))
	if _, err := env.Update(map[string]string{"REG_FACTORY_PRODUCTION_PROXY_SOURCE": "residential", "REG_FACTORY_PROXY": "", "REG_FACTORY_PROXY_POOL": ""}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{cfg: &settings.Settings{AppRoot: root, DataDir: root, EnvFile: env.Path()}}
	read := store.Task{Script: "asset_ops", ArgsJSON: `["liveness","--ref","fixture","--platform","claude"]`}
	if _, _, _, err := m.buildArgv(read); err != nil {
		t.Fatal("local report blocked by an unused connection", err)
	}
	refresh := store.Task{Script: "asset_ops", ArgsJSON: `["refresh","--ref","fixture","--platform","chatgpt"]`}
	if _, _, _, err := m.buildArgv(refresh); err == nil {
		t.Fatal("credential repair silently used direct")
	}
	if _, err := env.Update(map[string]string{"REG_FACTORY_PROXY": "http://shared.test:8080"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := m.buildArgv(refresh); err != nil {
		t.Fatal("repair did not recover after configuration", err)
	}
}
