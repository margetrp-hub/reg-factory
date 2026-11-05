package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/auth"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func logServer(t *testing.T) (*store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &settings.Settings{PanelUser: "admin", PanelPass: "fixture-password", LogTailMax: 500}
	s := &Server{st: st, cfg: cfg, auth: auth.New(cfg)}
	r := gin.New()
	s.Register(r)
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return st, ts
}

func readLog(t *testing.T, ts *httptest.Server, path, cursor string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+path, nil)
	req.SetBasicAuth("admin", "fixture-password")
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, data)
	}
	return string(data)
}

func TestTaskTerminalReplayAndReconnect(t *testing.T) {
	st, ts := logServer(t)
	if err := st.CreateTask(store.Task{ID: "task-1", Script: "test"}); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"first", "second", "finished"} {
		if err := st.AppendEvent("task-1", "stdout", line); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Finish("task-1", "failed", 7, "worker failed"); err != nil {
		t.Fatal(err)
	}
	full := readLog(t, ts, "/api/tasks/task-1/stream", "")
	if strings.Count(full, "event: log\n") != 3 || !strings.Contains(full, `"status":"failed"`) || !strings.Contains(full, "event: done") {
		t.Fatalf("terminal history: %s", full)
	}
	replay := readLog(t, ts, "/api/tasks/task-1/stream?after=0", "1")
	if strings.Count(replay, "event: log\n") != 2 || strings.Contains(replay, `"line":"first"`) {
		t.Fatalf("Last-Event-ID must override query and avoid duplicates: %s", replay)
	}
	exported := readLog(t, ts, "/api/tasks/task-1/logs?download=1", "")
	if !strings.Contains(exported, "first") || !strings.Contains(exported, "finished") {
		t.Fatal(exported)
	}
}

func TestLiveLogDrainsPersistedBacklogBeforeDone(t *testing.T) {
	st, ts := logServer(t)
	if err := st.CreateTask(store.Task{ID: "live", Script: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkRunning("live", 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/tasks/live/stream?after=0", nil)
	req.SetBasicAuth("admin", "fixture-password")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// Larger than both the old lossy subscription queue and one DB read page.
	for i := 0; i < 1100; i++ {
		if err := st.AppendEvent("live", "stdout", fmt.Sprintf("line-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Finish("live", "succeeded", 0, ""); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(resp.Body)
	count := 0
	done := false
	var last int64
	for scanner.Scan() {
		line := scanner.Text()
		if line == "event: done" {
			done = true
		}
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"line":`) {
			continue
		}
		var e store.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatal(err)
		}
		if e.ID <= last {
			t.Fatalf("duplicate/out-of-order event %d after %d", e.ID, last)
		}
		last = e.ID
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 1100 || !done {
		t.Fatalf("count=%d done=%v", count, done)
	}
}

func TestUnifiedLogsAndAuthorization(t *testing.T) {
	st, ts := logServer(t)
	writer := &store.RuntimeLog{Store: st}
	if _, err := writer.Write([]byte("[node] accepted task\n")); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendEvent("work-123", "stderr", "worker diagnostic"); err != nil {
		t.Fatal(err)
	}
	logs := readLog(t, ts, "/api/logs", "")
	if !strings.Contains(logs, "accepted task") || !strings.Contains(logs, "worker diagnostic") {
		t.Fatal(logs)
	}
	for _, path := range []string{"/api/logs", "/api/logs/stream", "/api/logs?download=1"} {
		resp, err := ts.Client().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
	}
}
