package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestConfigurationSaveReadbackWorkerAndRejectedUpdate(t *testing.T) {
	t.Setenv("PROXY_MODE", "clash_auto")
	t.Setenv("REG_FACTORY_PRODUCTION_PROXY", "http://deployment.example.test")
	t.Setenv("REG_FACTORY_LIVENESS_ENABLED", "1")
	t.Setenv("REG_FACTORY_MAX_CONCURRENCY", "3")
	root := t.TempDir()
	repo, _ := filepath.Abs("../../..")
	st, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &settings.Settings{AppRoot: repo, DataDir: root, EnvFile: filepath.Join(root, ".env"), MaxParallel: 3, GuardPct: 85}
	mgr := runner.New(cfg, st)
	defer mgr.Shutdown(context.Background())
	s := &Server{cfg: cfg, env: settings.NewEnv(cfg.EnvFile), st: st, run: mgr}
	r := gin.New()
	r.PUT("/api/config", s.configPut)
	r.GET("/api/config", s.configGet)
	r.PUT("/api/config/raw", s.configRawPut)
	put := func(path string, value any, want int) {
		t.Helper()
		raw, _ := json.Marshal(value)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("PUT", path, strings.NewReader(string(raw))))
		if w.Code != want {
			t.Fatalf("save %d: %s", w.Code, w.Body)
		}
	}
	change := map[string]string{"PROXY_MODE": "residential", "REG_FACTORY_PRODUCTION_PROXY": "", "REG_FACTORY_LIVENESS_ENABLED": "0", "REG_FACTORY_MAX_CONCURRENCY": "7"}
	for i := 0; i < 2; i++ {
		put("/api/config", map[string]any{"updates": change}, 200)
	}
	env, err := settings.WorkerEnvironment(cfg.EnvFile)
	joined := "\n" + strings.Join(env, "\n") + "\n"
	if err != nil || !strings.Contains(joined, "\nPROXY_MODE=residential\n") || !strings.Contains(joined, "\nREG_FACTORY_PRODUCTION_PROXY=\n") || mgr.Capacity() != 7 {
		t.Fatal("execution did not consume saved values", err)
	}
	liveness, err := s.livenessConfig()
	if err != nil || liveness.enabled {
		t.Fatal("saved disabled flag ignored", err)
	}
	put("/api/config", map[string]any{"updates": map[string]string{"REG_FACTORY_MAX_CONCURRENCY": "0", "PROXY_MODE": "clash_fixed"}}, 400)
	if mgr.Capacity() != 7 || s.env.Effective("PROXY_MODE") != "residential" {
		t.Fatal("rejected save partially applied")
	}
	put("/api/config", map[string]any{"updates": map[string]string{"REGD_DB": "another-database"}}, 400)
	put("/api/config/raw", map[string]any{"content": "REG_FACTORY_MAX_CONCURRENCY=99"}, 400)
	if mgr.Capacity() != 7 {
		t.Fatal("raw editor bypassed validation")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/config", nil))
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var body struct {
		Groups map[string][]settings.ConfigEntry `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, group := range body.Groups {
		for _, entry := range group {
			if entry.Key == "PROXY_MODE" && (entry.EffectiveValue != "residential" || entry.EffectiveSource != "file" || entry.Overridden) {
				t.Fatal("readback differs from worker", entry)
			}
		}
	}
}
