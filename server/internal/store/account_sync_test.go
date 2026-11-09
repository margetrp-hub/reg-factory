package store

import (
	"path/filepath"
	"testing"
)

func TestFileScanKeepsProductionAndCustodyAccounts(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.EnsureProduction(); err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureInventoryRelay(); err != nil {
		t.Fatal(err)
	}
	p := Production{OperationID: "produced", RequestSHA: "scope", Parameters: "{}", SourceKey: "workspace:child@example.test",
		SourceRef: "original.json", SourceSHA: "digest", AccountRef: "production:produced", Email: "child@example.test", Artifact: "sealed-original"}
	if err = s.AdmitProduction(p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimProductionCheck(p.OperationID, 100); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishProduction(p.OperationID, "completed", `{"status":"completed"}`, 100); err != nil {
		t.Fatal(err)
	}
	seeds := []AccountSeed{
		{Ref: "bound", Kind: "oauth", Identity: "bound@example.test"},
		{Ref: "claimed", Kind: "oauth", Identity: "claimed@example.test"},
		{Ref: "aliased", Kind: "oauth", Identity: "aliased@example.test"},
		{Ref: "removed-file", Kind: "oauth", Identity: "removed@example.test"},
	}
	if _, err = s.UpsertAccounts(seeds); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.CommitInventoryEvent("prepare", "digest", "receipt", "node", "asset", "bound", "prepared", 1, 1); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkClaimed("claimed"); err != nil {
		t.Fatal(err)
	}
	if err = s.BindAccountSubject("subject", "aliased", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE accounts SET updated_at='2000-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = s.SyncAccounts(nil); err != nil {
			t.Fatal(err)
		}
		for _, ref := range []string{p.AccountRef, "bound", "claimed", "aliased"} {
			if _, err = s.GetAccount(ref); err != nil {
				t.Fatalf("file scan deleted durable account %s: %v", ref, err)
			}
		}
	}
	if _, err = s.GetAccount("removed-file"); err == nil {
		t.Fatal("unreferenced deleted file was not pruned")
	}
	if _, _, err = s.CommitInventoryEvent("stock", "next", "stock-receipt", "node", "asset", "bound", "stocked", 2, 2); err != nil {
		t.Fatalf("custody could not continue after scan: %v", err)
	}
	if _, replay, err := s.CommitInventoryEvent("stock", "next", "stock-receipt", "node", "asset", "bound", "stocked", 2, 2); err != nil || !replay {
		t.Fatalf("lost receipt did not replay safely: %v", err)
	}
	account, err := s.GetAccount("bound")
	if err != nil || account.Version != 3 || !account.Claimed || account.JoinStatus != "joined" {
		t.Fatal("custody state changed", account, err)
	}
}
