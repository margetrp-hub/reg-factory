package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestRefreshAutomaticallyVerifiesAfterFailureWithoutChangingCustody(t *testing.T) {
	s, task := refreshFixture(t)
	if err := s.MarkHealth("one", "ok", "original AT"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetAccount("one")
	if _, _, err := s.QueueAccountRefresh(task, "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginAccountRefresh(task.ID, "one", "source-sha"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkATHealth("one", "ok", "old probe", before.Version); !errors.Is(err, ErrCheckStale) {
		t.Fatal("stale check accepted", err)
	}
	if err := s.FinishAccountRefresh(task.ID, "one", "failed", 75, "exchange failed", ""); err != nil {
		t.Fatal(err)
	}
	locked, _ := s.GetAccount("one")
	if locked.LibraryStatus == "leased" {
		t.Fatal("failed task kept its account lock", locked.LibraryStatus)
	}
	if err := s.MarkHealth("one", "ok", "old AT still works"); err != nil {
		t.Fatal("ended task blocked testing", err)
	}
	task.ID = "new-repair"
	if _, _, err := s.QueueAccountRefresh(task, "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginAccountRefresh(task.ID, "one", "source-sha"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishAccountRefresh(task.ID, "one", "succeeded", 0, "", "new-sha"); err != nil {
		t.Fatal(err)
	}
	checkID := RefreshVerificationID(task.ID)
	check, err := s.GetTask(checkID)
	if err != nil || check.Status != "queued" || check.Script != CredentialCheckScript {
		t.Fatal(check, err)
	}
	if _, err = s.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	check, err = s.ClaimCredentialCheck()
	if err != nil || check.ID != checkID {
		t.Fatal("queued AT check lost on restart", check, err)
	}
	account, _ := s.GetAccount("one")
	items, _ := s.CredentialCheckItems(checkID)
	if len(items) != 1 || account.LibraryStatus != "pending" {
		t.Fatal(account, items)
	}
	if ok, err := s.StartCredentialCheckItem(checkID, 0); err != nil || !ok {
		t.Fatal(ok, err)
	}
	items[0].Status = "ok"
	items[0].CheckVersion = account.Version
	if err = s.CompleteCredentialCheckItem(checkID, items[0]); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishCredentialCheck(checkID, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	ready, _ := s.GetAccount("one")
	if ready.LibraryStatus != "available" || ready.RefreshState != "succeeded" || ready.Claimed || ready.LivenessCheckedAt == "" {
		t.Fatal(ready)
	}
	if err = s.MarkClaimed("one"); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkHealth("one", "ok", "claimed account test"); err != nil {
		t.Fatal(err)
	}
	claimed, _ := s.GetAccount("one")
	if claimed.LibraryStatus != "used" || claimed.Version != ready.Version {
		t.Fatal("health changed custody version", claimed)
	}
}

func TestCredentialCheckCannotPromoteAReplacedVersion(t *testing.T) {
	s, task := refreshFixture(t)
	a, _ := s.GetAccount("one")
	check := Task{ID: "check-race", Script: CredentialCheckScript, ArgsJSON: `{"platform":"chatgpt","refs":["one"]}`}
	if err := s.CreateCredentialCheck(check, []Account{a}); err != nil {
		t.Fatal(err)
	}
	s.ClaimCredentialCheck()
	s.StartCredentialCheckItem(check.ID, 0)
	if _, _, err := s.QueueAccountRefresh(task, "one"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteCredentialCheckItem(check.ID, CredentialCheckItem{Ref: "one", Mode: "at", Status: "ok", CheckVersion: a.Version}); err != nil {
		t.Fatal(err)
	}
	items, _ := s.CredentialCheckItems(check.ID)
	account, _ := s.GetAccount("one")
	if items[0].Status != "unknown" || account.LibraryStatus != "leased" || account.Status == "ok" {
		t.Fatal(items, account)
	}
}

func TestAccountLibraryCheckRecoveryAndPagination(t *testing.T) {
	s, _ := refreshFixture(t)
	for i := 0; i < 7; i++ {
		ref := fmt.Sprintf("ready-%d", i)
		if _, err := s.UpsertAccounts([]AccountSeed{{Ref: ref, Identity: ref + "@example.test", Platform: "chatgpt", Kind: "oauth"}}); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkHealth(ref, "ok", "AT verified"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkClaimed("ready-0"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHealth("ready-1", "needs_refresh", "AT rejected, RT untouched"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHealth("ready-2", "unknown", "TLS retries exhausted"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for page := 1; page <= 2; page++ {
		rows, total, err := s.LibraryAccounts("ready-", "available", "", page, 2)
		if err != nil || total != 4 || len(rows) != 2 {
			t.Fatalf("page %d: total=%d rows=%d error=%v", page, total, len(rows), err)
		}
		for _, row := range rows {
			if seen[row.Ref] || row.Claimed || row.OAuthStatus != "completed" || row.LivenessStatus != "alive" || row.JoinStatus != "none" {
				t.Fatalf("invalid eligible row: %+v", row)
			}
			seen[row.Ref] = true
		}
	}
	// A read-only retry recovers uncertainty without claiming or refreshing.
	if err := s.MarkHealth("ready-2", "ok", "AT retested"); err != nil {
		t.Fatal(err)
	}
	rows, total, err := s.LibraryAccounts("ready-", "available", "", 1, 100)
	if err != nil || total != 5 || len(rows) != 5 {
		t.Fatal(total, len(rows), err)
	}
	a, _ := s.GetAccount("ready-0")
	if !a.Claimed {
		t.Fatal("health projection released custody")
	}
}
