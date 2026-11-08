package runner

import (
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"path/filepath"
	"testing"
)

func TestScriptRunnerDoesNotOwnProductionTasks(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateTask(store.Task{ID: "production", Script: "production.team_json", ArgsJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	m := &Manager{st: s, procs: map[string]*proc{}}
	if next, err := m.nextQueued(); err != nil || next != nil {
		t.Fatal("runner claimed production", err)
	}
	if err := m.Stop("production"); err == nil {
		t.Fatal("runner stopped production")
	}
	m.StopAll()
	if _, err := s.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	task, err := s.GetTask("production")
	if err != nil || task.Status != "queued" {
		t.Fatal("restart corrupted durable task", task.Status, err)
	}
}
