package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func TestWorkRefreshRemainsOwnedBySignedProtocol(t *testing.T) {
	task := Task{ID: "work-original-operation", Script: "asset_ops", ArgsJSON: `["refresh","--ref","delivered","--platform","chatgpt","--allow-relogin"]`}
	if AccountRefreshRef(task) != "" {
		t.Fatal("panel recovery took over Work task")
	}
	task.ID = "request-panel"
	if AccountRefreshRef(task) != "delivered" {
		t.Fatal("panel recovery missing")
	}
}

func refreshFixture(t *testing.T) (*Store, Task) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err = s.UpsertAccounts([]AccountSeed{{Ref: "one", Platform: "chatgpt", Kind: "oauth"}}); err != nil {
		t.Fatal(err)
	}
	return s, Task{ID: "first", Script: "asset_ops", ArgsJSON: `["refresh","--ref","one","--platform","chatgpt"]`, Actor: "operator"}
}

func TestRefreshConcurrentRequestsRemainBoundAfterResolution(t *testing.T) {
	s, task := refreshFixture(t)
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			request := task
			request.ID = fmt.Sprint("request-", i)
			id, _, err := s.QueueAccountRefresh(request, "one")
			if err != nil {
				t.Error(err)
				return
			}
			ids <- id
		}(i)
	}
	wg.Wait()
	close(ids)
	var original string
	for id := range ids {
		if original == "" {
			original = id
		}
		if id != original {
			t.Fatal("concurrent RT exchanges accepted")
		}
	}
	if err := s.BeginAccountRefresh(original, "one", "original-digest"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAccountRefresh(original, "one", "failed", 75, "unconfirmed result", ""); err != nil {
		t.Fatal(err)
	}
	task.ID = "retry-unknown"
	id, created, err := s.QueueAccountRefresh(task, "one")
	if err != nil || !created || id != task.ID {
		t.Fatal(id, created, err)
	}
	id, created, err = s.QueueAccountRefresh(task, "one")
	if err != nil || created || id != task.ID {
		t.Fatal("lost response replay created exchange", id, created, err)
	}
	s.FinishAccountRefresh(task.ID, "one", "failed", 1, "rejected", "")
	task.ID = "new-after-resolution"
	id, created, err = s.QueueAccountRefresh(task, "one")
	if err != nil || !created || id != task.ID {
		t.Fatal(id, created, err)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='account.refresh.resolve'`).Scan(&count)
	if count != 0 {
		t.Fatal("failure should not require manual settlement", count)
	}
}

func TestRefreshAliasesSharePendingTaskAndFailedTaskCanRetry(t *testing.T) {
	s, task := refreshFixture(t)
	s.db.Exec(`UPDATE accounts SET identity='same@example.test' WHERE ref='one'`)
	s.UpsertAccounts([]AccountSeed{{Ref: "old", Platform: "chatgpt", Kind: "oauth", Identity: " SAME@example.test "}})
	if _, _, err := s.QueueAccountRefresh(task, "one"); err != nil {
		t.Fatal(err)
	}
	other := task
	other.ID, other.ArgsJSON = "alias-request", `["refresh","--ref","old","--platform","chatgpt"]`
	id, created, err := s.QueueAccountRefresh(other, "old")
	if err != nil || created || id != task.ID {
		t.Fatal("alias started duplicate", id, created, err)
	}
	s.BeginAccountRefresh(task.ID, "one", "old-sha")
	s.FinishAccountRefresh(task.ID, "one", "failed", 75, "network failed", "")
	other.ID = "unknown-alias"
	id, created, err = s.QueueAccountRefresh(other, "old")
	if err != nil || !created || id != other.ID {
		t.Fatal("failed alias could not retry", id, created, err)
	}
	s.FinishAccountRefresh(other.ID, "old", "failed", 1, "rejected", "")
	other.ID = "retry-alias"
	id, created, err = s.QueueAccountRefresh(other, "old")
	if err != nil || !created || id != other.ID {
		t.Fatal("failure blocked retry", id, created, err)
	}
}

func TestRefreshStopRestartAndCoreOwnership(t *testing.T) {
	s, task := refreshFixture(t)
	if _, _, err := s.QueueAccountRefresh(task, "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.StopQueuedScripts(); err != nil {
		t.Fatal(err)
	}
	account, _ := s.GetAccount("one")
	if account.RefreshState != "stopped" {
		t.Fatal(account.RefreshState)
	}
	task.ID = "after-stop"
	if _, created, err := s.QueueAccountRefresh(task, "one"); err != nil || !created {
		t.Fatal(created, err)
	}
	if err := s.BeginAccountRefresh(task.ID, "one", "digest"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	account, _ = s.GetAccount("one")
	if account.RefreshState != "failed" {
		t.Fatal(account.RefreshState)
	}
	original := task.ID
	task.ID = "after-restart"
	if id, created, err := s.QueueAccountRefresh(task, "one"); err != nil || !created || id != task.ID {
		t.Fatal(id, created, err)
	}
	if err := s.MarkClaimed("one"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveAccountRefresh(original, "operator", "确认失败证据", "failed", ""); err != ErrAccountOwned {
		t.Fatal("claimed resolution", err)
	}
	task.ID = "after-claimed"
	if _, _, err := s.QueueAccountRefresh(task, "one"); err != ErrAccountOwned {
		t.Fatal("claimed refresh", err)
	}
}
