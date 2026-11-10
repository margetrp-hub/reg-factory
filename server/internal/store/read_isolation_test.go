package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestTaskAndLogReadsRemainAvailableDuringWriterTransaction(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.CreateTask(Task{ID: "one", Script: "asset_ops", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendEvent("one", "system", "saved event"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.UpsertAccounts([]AccountSeed{{Ref: "one", Platform: "chatgpt", Kind: "oauth"}}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE tasks SET error='uncommitted' WHERE id='one'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.GetTask("one")
		if err == nil {
			_, err = s.ListTasks("", 10)
		}
		if err == nil {
			_, err = s.TailEvents("one", 10)
		}
		if err == nil {
			_, err = s.EventsAfter("one", 0, 10)
		}
		if err == nil {
			_, err = s.CredentialStages("one")
		}
		if err == nil {
			_, err = s.QueryTasks(TaskQuery{Limit: 10})
		}
		if err == nil {
			_, err = s.GetAccount("one")
		}
		if err == nil {
			_, _, err = s.ListAccountsPage("", "chatgpt", "", 10, 0)
		}
		if err == nil {
			_, err = s.AccountSummaries("chatgpt")
		}
		if err == nil {
			_, err = s.AccountSources("one")
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("UI reads blocked behind task-state writer")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.read.ExecContext(ctx, `DELETE FROM tasks`); err == nil {
		t.Fatal("read pool could mutate task state")
	}
}

func TestCredentialMethodSurvivesLongSettlementLog(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Stage("one", StageEvent{Stage: "credential_authorize", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 300; i++ {
		if err = s.Stage("one", StageEvent{Stage: "credential_settlement", Status: "running", Attempt: i}); err != nil {
			t.Fatal(err)
		}
	}
	stages, err := s.CredentialStages("one")
	if err != nil || len(stages) != 2 || stages[0].Stage != "credential_authorize" || stages[1].Attempt != 300 {
		t.Fatalf("lost original method or latest progress: %v %v", stages, err)
	}
}
