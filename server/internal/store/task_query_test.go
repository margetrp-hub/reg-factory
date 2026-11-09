package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryFindsOldTasksAndCountsBeforePagination(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 145; i++ {
		id := fmt.Sprintf("task-%03d", i)
		if err := s.CreateTask(Task{ID: id, Script: "register", Title: "批次"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("UPDATE tasks SET created_at=?,status=? WHERE id=?", "2026-11-09T00:00:00Z", "failed", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateTask(Task{ID: "older", Script: "authorize", Title: "100%_原任务"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE tasks SET created_at=?,status='unknown' WHERE id='older'", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	first, err := s.QueryTasks(TaskQuery{Limit: 25, Status: "failed"})
	if err != nil || first.Total != 145 || first.Count != 25 || first.Stats["unknown"] != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := s.QueryTasks(TaskQuery{Limit: 25, Offset: 25, Status: "failed"})
	if err != nil || second.Tasks[0].ID == first.Tasks[0].ID || first.Tasks[0].ID != "task-144" {
		t.Fatalf("unstable pagination: %v", err)
	}
	found, err := s.QueryTasks(TaskQuery{Scripts: []string{"authorize"}, Search: "100%_"})
	if err != nil || found.Total != 1 || found.Tasks[0].ID != "older" {
		t.Fatalf("old/literal search: %+v %v", found, err)
	}
	empty, err := s.QueryTasks(TaskQuery{Search: "' OR 1=1 --"})
	if err != nil || empty.Total != 0 || empty.Tasks == nil {
		t.Fatalf("empty search: %+v %v", empty, err)
	}
	plan, err := s.db.Query("EXPLAIN QUERY PLAN SELECT id FROM tasks WHERE status=? ORDER BY created_at DESC,id DESC LIMIT 25", "failed")
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	detail := ""
	for plan.Next() {
		var a, b, c int
		var d string
		if err := plan.Scan(&a, &b, &c, &d); err != nil {
			t.Fatal(err)
		}
		detail += d
	}
	if !strings.Contains(detail, "idx_tasks_status_history") || strings.Contains(detail, "TEMP B-TREE") {
		t.Fatalf("history not indexed: %s", detail)
	}
}
