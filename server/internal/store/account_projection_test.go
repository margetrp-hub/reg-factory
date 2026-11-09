package store

import (
	"path/filepath"
	"testing"
)

func TestIdentityCountsRetainSourcesKindsAndRunningOwnership(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "identities.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seeds := []AccountSeed{
		{Ref: "import:shared", Platform: "chatgpt", Kind: "oauth", Identity: "same@example.test"},
		{Ref: "result:shared", Platform: "chatgpt", Kind: "team", Identity: " SAME@example.test "},
		{Ref: "ready", Platform: "chatgpt", Kind: "oauth", Identity: "ready@example.test"},
		{Ref: "bad", Platform: "chatgpt", Kind: "oauth", Identity: "bad@example.test"},
		{Ref: "pending", Platform: "chatgpt", Kind: "oauth", Identity: "pending@example.test"},
		{Ref: "lease", Platform: "chatgpt", Kind: "oauth", Identity: "lease@example.test"},
		{Ref: "import:lease", Platform: "chatgpt", Kind: "oauth", Identity: "lease@example.test"},
		{Ref: "blank1", Platform: "chatgpt", Kind: "oauth"},
		{Ref: "blank2", Platform: "chatgpt", Kind: "oauth", Identity: " "},
		{Ref: "mail", Platform: "outlook", Kind: "mail", Identity: "same@example.test"},
	}
	if _, err = s.UpsertAccounts(seeds); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"import:shared", "ready", "import:lease", "mail"} {
		if err = s.MarkHealth(ref, "ok", "checked"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.MarkClaimed("result:shared"); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkHealth("bad", "needs_refresh", "AT rejected"); err != nil {
		t.Fatal(err)
	}
	task := Task{ID: "refresh-unknown", Script: "asset_ops", ArgsJSON: `["refresh","--ref","lease"]`}
	if _, _, err = s.QueueAccountRefresh(task, "lease"); err != nil {
		t.Fatal(err)
	}
	if err = s.BeginAccountRefresh(task.ID, "lease", "source-digest"); err != nil {
		t.Fatal(err)
	}
	beforeLease, err := s.GetAccount("lease")
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := s.AccountSummaries("chatgpt")
	if err != nil || len(summaries) != 1 {
		t.Fatal(summaries, err)
	}
	x := summaries[0]
	if x.Total != 7 || x.Sources != 9 || x.Available != 1 || x.Abnormal != 1 || x.Used != 1 || x.Leased != 1 || x.Unknown != 3 {
		t.Fatalf("identity counts do not reconcile: %+v", x)
	}
	if x.Total != x.Available+x.Abnormal+x.Unknown+x.Used+x.Leased {
		t.Fatal("categories overlap", x)
	}
	seen := map[string]bool{}
	for offset := 0; offset < 7; offset += 3 {
		rows, total, err := s.ListAccountsPage("", "chatgpt", "", 3, offset)
		if err != nil || total != x.Total {
			t.Fatal(total, err)
		}
		for _, a := range rows {
			if seen[a.Ref] || a.Version < 1 {
				t.Fatal("unstable page", a)
			}
			seen[a.Ref] = true
		}
	}
	if len(seen) != 7 {
		t.Fatal(seen)
	}
	rows, total, err := s.ListAccountsPage("team", "chatgpt", "", 50, 0)
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Ref != "result:shared" || rows[0].SourceCount != 2 || rows[0].LibraryStatus != "used" {
		t.Fatal("kind filter lost original source or ownership", rows, total, err)
	}
	sources, err := s.AccountSources("import:shared")
	if err != nil || len(sources) != 2 {
		t.Fatal(sources, err)
	}
	for _, a := range sources {
		if a.Claimed != (a.Ref == "result:shared") || a.Version != 1 {
			t.Fatal("projection rewrote source", a)
		}
	}
	sources, err = s.AccountSources("blank1")
	if err != nil || len(sources) != 1 || sources[0].Ref != "blank1" {
		t.Fatal("blank identities merged", sources, err)
	}
	other, err := s.AccountSummaries("outlook")
	if err != nil || len(other) != 1 || other[0].Total != 1 || other[0].Available != 1 {
		t.Fatal("non-OAuth platform health lost", other, err)
	}
	lease, err := s.GetAccount("lease")
	if err != nil || lease.LibraryStatus != "leased" || lease.Version != beforeLease.Version {
		t.Fatal("unknown exchange unlocked", lease, err)
	}
}
