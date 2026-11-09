package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestTaskSubmissionConcurrentReplayAndConflict(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "deploy"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "scripts.schema.json"), []byte(`[{"id":"example","title":"Example","args":[{"flag":"--mode","type":"choice","choices":["a","b"]}]}]`), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &settings.Settings{AppRoot: root, DataDir: root, MaxParallel: 0}
	m := runner.New(cfg, st)
	defer m.Shutdown(context.Background())
	s := &Server{cfg: cfg, st: st, run: m}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("actor", "operator"); c.Next() })
	r.POST("/api/tasks", s.createTask)
	invoke := func(mode string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"request_id":"d5f879d2-88c9-4fdc-9726-0e597246bc11","script":"example","args":{"--mode":"`+mode+`"}}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	var wg sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); responses <- invoke("a") }()
	}
	wg.Wait()
	close(responses)
	for w := range responses {
		if w.Code != 200 && w.Code != 202 {
			t.Fatalf("duplicate admission: %d %s", w.Code, w.Body.String())
		}
	}
	page, err := st.QueryTasks(store.TaskQuery{})
	if err != nil || page.Total != 1 {
		t.Fatalf("duplicate tasks: %+v %v", page, err)
	}
	if err := st.Finish(page.Tasks[0].ID, "failed", 1, "fixture_failure"); err != nil {
		t.Fatal(err)
	}
	w := invoke("a")
	var replay map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &replay); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || replay["status"] != "failed" || replay["replayed"] != true {
		t.Fatalf("terminal replay=%s", w.Body)
	}
	if w = invoke("b"); w.Code != 409 {
		t.Fatalf("changed request=%d", w.Code)
	}
}

func TestTaskListReturnsFullCountsAndOldHistory(t *testing.T) {
	st, ts := logServer(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := st.CreateTask(store.Task{ID: id, Script: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	var result store.TaskPage
	if err := json.Unmarshal([]byte(readLog(t, ts, "/api/tasks?limit=1&offset=2&script=fixture", "")), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 3 || result.Count != 1 || result.Stats["queued"] != 3 {
		t.Fatalf("page=%+v", result)
	}
}
