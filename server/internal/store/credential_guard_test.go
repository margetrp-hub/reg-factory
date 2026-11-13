package store

import (
	"fmt"
	"testing"
)

func TestCredentialGuardDistinguishesUnstartedBusyUncertainAndRecovered(t *testing.T) {
	s, task := refreshFixture(t)
	task.ID = "next"
	sequence := 0
	prior := func(state string, code int, started bool) {
		sequence++
		taskCopy := task
		taskCopy.ID = fmt.Sprintf("prior-%d", sequence)
		if err := s.CreateTask(taskCopy); err != nil {
			t.Fatal(err)
		}
		start := ""
		if started {
			start = "2026-11-13T01:00:00Z"
		}
		if _, err := s.ex.Exec(`UPDATE tasks SET status=?,exit_code=?,started_at=?,finished_at=? WHERE id=?`, state, code, start, fmt.Sprintf("2026-11-13T02:00:%02dZ", sequence), taskCopy.ID); err != nil {
			t.Fatal(err)
		}
	}
	check := func(want string) {
		t.Helper()
		got, err := s.CredentialExecutionGuard(task)
		if err != nil || got != want {
			t.Fatal(got, err, want)
		}
	}
	prior("stopped", -1, false)
	check("")
	prior("failed", 1, true)
	check("")
	prior("failed", 75, true)
	check("relogin_required")
	task.ArgsJSON = `["refresh","--ref","one","--platform","chatgpt","--allow-relogin"]`
	check("")
	prior("running", -1, true)
	check("busy")
	if _, err := s.ex.Exec(`UPDATE tasks SET status='failed',exit_code=75 WHERE id='prior-4'`); err != nil {
		t.Fatal(err)
	}
	prior("succeeded", 0, true)
	task.ArgsJSON = `["refresh","--ref","one","--platform","chatgpt"]`
	check("")
	s.read.Close()
	if _, err := s.CredentialExecutionGuard(task); err == nil {
		t.Fatal("unreadable current history permitted execution")
	}
}

func TestCredentialGuardDoesNotUseRandomIDsForSameSecondResults(t *testing.T) {
	s, task := refreshFixture(t)
	task.ID = "next"
	for _, id := range []string{"z-success", "a-lost"} {
		prior := task
		prior.ID = id
		if err := s.CreateTask(prior); err != nil {
			t.Fatal(err)
		}
		state, code := "succeeded", 0
		if id == "a-lost" {
			state, code = "failed", 75
		}
		if _, err := s.ex.Exec(`UPDATE tasks SET status=?,exit_code=?,started_at='2026-11-13T01:00:00Z',finished_at='2026-11-13T01:00:00Z' WHERE id=?`, state, code, id); err != nil {
			t.Fatal(err)
		}
		if err := s.Stage(id, StageEvent{Stage: "execution", Status: "running"}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.CredentialExecutionGuard(task); err != nil || got != "relogin_required" {
		t.Fatal("later lost response bypassed guard", got, err)
	}
	// A successful explicit authorization in the same second clears the fence;
	// the persisted execution sequence, not lexical UUID order, proves it is newer.
	newer := task
	newer.ID = "0-authorized"
	if err := s.CreateTask(newer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ex.Exec(`UPDATE tasks SET status='succeeded',exit_code=0,started_at='2026-11-13T01:00:00Z',finished_at='2026-11-13T01:00:00Z' WHERE id=?`, newer.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Stage(newer.ID, StageEvent{Stage: "execution", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CredentialExecutionGuard(task); err != nil || got != "" {
		t.Fatal("newer authorization remained fenced", got, err)
	}
	if _, err := s.ex.Exec(`DELETE FROM events WHERE task_id='a-lost'`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CredentialExecutionGuard(task); err != nil || got != "relogin_required" {
		t.Fatal("missing same-second execution evidence permitted replay", got, err)
	}
}
