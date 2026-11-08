package store

import (
	"path/filepath"
	"testing"
)

func TestProductionCompletesAlreadyScannedResultWithoutReplacingIdentity(t *testing.T) {
	for _, identity := range []string{"child@example.test", "other@example.test"} {
		t.Run(identity, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "node.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.EnsureProduction(); err != nil {
				t.Fatal(err)
			}
			p := Production{OperationID: "operation", RequestSHA: "scope", Parameters: "{}", SourceKey: "workspace:child@example.test",
				SourceRef: "source.json", SourceSHA: "digest", AccountRef: "production:operation", Email: "child@example.test", Artifact: "sealed-original"}
			if err = s.ReserveProductionAdmission(p.OperationID, p.RequestSHA, p.Parameters); err != nil {
				t.Fatal(err)
			}
			if err = s.AdmitProduction(p); err != nil {
				t.Fatal(err)
			}
			// The independent asset scanner sees the retained export before the
			// Work poll completes verification of the same production result.
			if _, err = s.UpsertAccounts([]AccountSeed{{Ref: p.AccountRef, Platform: "chatgpt", Kind: "team", Identity: identity,
				SecretRef: "assets/chatgpt/team/result.json"}}); err != nil {
				t.Fatal(err)
			}
			if claimed, err := s.ClaimProductionCheck(p.OperationID, 100); err != nil || !claimed {
				t.Fatal(claimed, err)
			}
			err = s.FinishProduction(p.OperationID, "completed", `{"status":"completed","error_code":""}`, 100)
			if identity != p.Email {
				if err == nil {
					t.Fatal("foreign account projection was overwritten")
				}
				unchanged, _ := s.Production(p.OperationID)
				account, _ := s.GetAccount(p.AccountRef)
				if unchanged.State != "queued" || account.Identity != identity || account.Status != "unknown" {
					t.Fatal("conflict was partially committed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.FinishProduction(p.OperationID, "completed", `{"status":"completed","error_code":""}`, 100); err != nil {
				t.Fatal(err)
			}
			finished, _ := s.Production(p.OperationID)
			account, _ := s.GetAccount(p.AccountRef)
			admission, _ := s.ProductionAdmission(p.OperationID)
			task, _ := s.GetTask("work-" + p.OperationID)
			if finished.State != "completed" || admission.State != "completed" || task.Status != "completed" || finished.Artifact != p.Artifact {
				t.Fatal("incomplete production settlement")
			}
			if account.Kind != "team" || account.SecretRef != "assets/chatgpt/team/result.json" || account.Status != "normal" {
				t.Fatal("saved export identity was replaced")
			}
			var count int
			if err = s.db.QueryRow("SELECT count(*) FROM accounts WHERE ref=?", p.AccountRef).Scan(&count); err != nil || count != 1 {
				t.Fatal(count, err)
			}
		})
	}
}
