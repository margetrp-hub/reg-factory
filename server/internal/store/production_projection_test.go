package store

import (
	"path/filepath"
	"testing"
)

func TestProductionEmailClaimUpgradeRequiresConfirmedStop(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.EnsureProduction(); err != nil {
		t.Fatal(err)
	}
	// Simulate the previous release's index, including a stopped issued result.
	_, err = s.db.Exec(`DROP INDEX production_run_email;
 CREATE UNIQUE INDEX production_run_email ON production_runs(email)
 WHERE email<>'' AND NOT (status='cancelled' AND stop_effect='none' AND external_started=0);
 INSERT INTO production_runs(operation_id,material,email,stage,status,external_started,stop_requested,stop_effect,updated_at)
 VALUES('original','sealed','child@example.test','result_ready','cancelled',1,1,'none',100);`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = s.EnsureProductionRuns(); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.db.Exec(`INSERT INTO production_runs(operation_id,material,email,stage,status,updated_at)
 VALUES('restart','new-sealed','child@example.test','admitted','queued',101)`)
	if err != nil {
		t.Fatal("confirmed absent membership still owns the email claim", err)
	}
	_, err = s.db.Exec(`INSERT INTO production_runs(operation_id,material,email,stage,status,updated_at)
 VALUES('duplicate','sealed','child@example.test','admitted','queued',102)`)
	if err == nil {
		t.Fatal("concurrent production reused an active email")
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM production_runs WHERE operation_id='original' AND material='sealed'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("upgrade lost the original production", count, err)
	}
}

func TestProductionNewOutputKeepsFailedVersionAndDeduplicatesItsOwnReceipt(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.EnsureProduction(); err != nil {
		t.Fatal(err)
	}
	old := Production{OperationID: "old", RequestSHA: "old-request", Parameters: "{}", SourceKey: "workspace:child@example.test",
		SourceRef: "input.json", SourceSHA: "old-input-digest", AccountRef: "production:old", Email: "child@example.test", Artifact: "old-output"}
	if err = s.AdmitProduction(old); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimProductionCheck(old.OperationID, 100); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishProduction(old.OperationID, "failed", `{"status":"failed"}`, 100); err != nil {
		t.Fatal(err)
	}
	next := old
	next.OperationID, next.RequestSHA, next.AccountRef = "new", "new-request", "production:new"
	next.SourceSHA, next.Artifact = "new-output-digest", "new-output"
	if err = s.AdmitProduction(next); err != nil {
		t.Fatal(err)
	}
	if err = s.AdmitProduction(next); err != nil {
		t.Fatal(err)
	}
	duplicate := next
	duplicate.OperationID, duplicate.AccountRef = "duplicate", "production:duplicate"
	if err = s.AdmitProduction(duplicate); err == nil {
		t.Fatal("same output admitted twice")
	}
	prior, err := s.Production(old.OperationID)
	if err != nil || prior.State != "failed" || prior.Artifact != old.Artifact {
		t.Fatal("failed output changed", err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM production_results").Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
}

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
