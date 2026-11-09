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
	if err := s.FinishAccountRefresh(original, "one", "unknown", 75, "uncertain exchange", ""); err != nil {
		t.Fatal(err)
	}
	task.ID = "retry-unknown"
	id, created, err := s.QueueAccountRefresh(task, "one")
	if err != nil || created || id != original {
		t.Fatal(id, created, err)
	}
	if err = s.ResolveAccountRefresh(original, "operator", "上游证据确认请求未受理", "failed", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveAccountRefresh(original, "operator", "同一次结案请求重放", "failed", ""); err != nil {
		t.Fatal(err)
	}
	id, created, err = s.QueueAccountRefresh(task, "one")
	if err != nil || created || id != original {
		t.Fatal("lost response replay created exchange", id, created, err)
	}
	task.ID = "new-after-resolution"
	id, created, err = s.QueueAccountRefresh(task, "one")
	if err != nil || !created || id != task.ID {
		t.Fatal(id, created, err)
	}
	var count int
	s.db.QueryRow(`SELECT count(*) FROM audit WHERE action='account.refresh.resolve'`).Scan(&count)
	if count != 1 {
		t.Fatal("duplicate settlement audit", count)
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
	if account.RefreshState != "interrupted" {
		t.Fatal(account.RefreshState)
	}
	original := task.ID
	task.ID = "after-restart"
	if id, created, err := s.QueueAccountRefresh(task, "one"); err != nil || created || id != original {
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
