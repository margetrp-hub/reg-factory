package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestAccountSummaryIncludesRowsBeyondListLimit(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seeds := make([]AccountSeed, 1105)
	for i := range seeds {
		seeds[i] = AccountSeed{Ref: fmt.Sprintf("sample-%d", i), Platform: "chatgpt", Kind: "oauth"}
	}
	if _, err := s.SyncAccounts(seeds); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHealth("sample-1104", "expired", "sample"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkHealth("sample-1103", "ok", "sample"); err != nil {
		t.Fatal(err)
	}
	// This is an additional source, not a full scan replacing the first batch.
	if _, err := s.UpsertAccounts([]AccountSeed{{Ref: "another", Platform: "outlook", Kind: "mail"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.AccountSummaries("chatgpt")
	if err != nil || len(rows) != 1 || rows[0].Total != 1105 || rows[0].Available != 1 || rows[0].Abnormal != 1 || rows[0].Unknown != 1103 {
		t.Fatalf("summary=%+v err=%v", rows, err)
	}
	all, err := s.AccountSummaries("")
	if err != nil || len(all) != 2 {
		t.Fatalf("all=%+v err=%v", all, err)
	}
	first, total, err := s.ListAccountsPage("", "chatgpt", "", 25, 0)
	if err != nil || total != 1105 || len(first) != 25 {
		t.Fatalf("first=%d total=%d err=%v", len(first), total, err)
	}
	second, total, err := s.ListAccountsPage("", "chatgpt", "", 25, 25)
	if err != nil || total != 1105 || len(second) != 25 || first[0].Ref == second[0].Ref {
		t.Fatalf("second=%d total=%d err=%v", len(second), total, err)
	}
	filtered, total, err := s.ListAccountsPage("", "chatgpt", "expired", 25, 0)
	if err != nil || total != 1 || len(filtered) != 1 || filtered[0].Ref != "sample-1104" {
		t.Fatalf("filter=%+v total=%d err=%v", filtered, total, err)
	}
}
