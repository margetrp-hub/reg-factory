package settings

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSharedProxySnapshotRetainsRunningConnection(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "pool-index")
	if err := os.WriteFile(state, []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"REG_FACTORY_PRODUCTION_PROXY_SOURCE=residential", "REG_FACTORY_PRODUCTION_PROXY=http://unused.test:1",
		"REG_FACTORY_PROXY_POOL=http://first.test:80;http://second.test:80", "REG_FACTORY_PROXY_STATE_FILE=" + state}
	frozen, err := BindProductionProxy(env, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, []byte("0"), 0600); err != nil {
		t.Fatal(err)
	}
	current, err := ProductionProxy(env, root)
	if err != nil || current != "http://first.test:80" {
		t.Fatal("next task did not use current pool", current, err)
	}
	running, err := ProductionProxy(frozen, root)
	if err != nil || running != "http://second.test:80" {
		t.Fatal("running task changed connection", running, err)
	}
	direct, err := ProductionProxy(append(env, "REG_FACTORY_PRODUCTION_PROXY_SOURCE=direct"), root)
	if err != nil || direct != "" {
		t.Fatal("explicit direct used another connection", direct, err)
	}
	if _, err := ProductionProxy([]string{"REG_FACTORY_PRODUCTION_PROXY_SOURCE=residential"}, root); err == nil {
		t.Fatal("missing shared connection silently used direct")
	}
}
