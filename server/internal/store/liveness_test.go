package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestScheduledLivenessSnapshotDeduplicatesAllIdentitiesAndBusyChecks(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seeds := []AccountSeed{}
	for i := 0; i < 125; i++ {
		seeds = append(seeds, AccountSeed{Ref: fmt.Sprintf("import:%03d", i), Platform: "chatgpt", Kind: "oauth", Identity: fmt.Sprintf("%d@example.test", i)})
	}
	seeds = append(seeds, AccountSeed{Ref: "old:0", Platform: "chatgpt", Kind: "oauth", Identity: "0@EXAMPLE.TEST"})
	if _, err = s.UpsertAccounts(seeds); err != nil {
		t.Fatal(err)
	}
	if err = s.CreateCredentialCheck(Task{ID: "manual-old-source", Actor: "operator"}, []Account{{Ref: "old:0", Platform: "chatgpt", Identity: "0@EXAMPLE.TEST"}}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	ids := make(chan string, 2)
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			id, err := s.ScheduleLivenessCheck(at, time.Hour)
			if err != nil {
				t.Error(err)
			}
			ids <- id
		}()
	}
	workers.Wait()
	close(ids)
	accepted := ""
	for id := range ids {
		if id != "" {
			if accepted != "" {
				t.Fatal("duplicate automatic batch", accepted, id)
			}
			accepted = id
		}
	}
	if accepted == "" {
		t.Fatal("no automatic batch")
	}
	items, err := s.CredentialCheckItems(accepted)
	if err != nil || len(items) != 124 {
		t.Fatal("current library truncated, duplicated or busy identity included", len(items), err)
	}
	for _, item := range items {
		if item.Ref == "old:0" || item.Ref == "import:000" || item.Mode != "at" {
			t.Fatal(item)
		}
	}
}
