package runner

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/testdatabase"
)

func TestCredentialPublicationRecoveryAndSettlement(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"restart", "database-retry", "work-restart", "saved-python-unavailable", "work-saved-python-unavailable", "before-receipt", "current-without-receipt", "changed-current", "imported-login"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			st := testdatabase.Open(t, root)
			if _, err = st.UpsertAccounts([]store.AccountSeed{{Ref: "one", Identity: "one@example.test", Platform: "chatgpt", Kind: "oauth"}}); err != nil {
				t.Fatal(err)
			}
			assets := assetstore.New(root)
			initial := assetstore.Asset{Ref: "one", Email: "one@example.test", Platform: "chatgpt", Kind: "oauth", Payload: map[string]any{"access_token": "old-at", "refresh_token": "old-rt"}}
			if mode == "imported-login" {
				initial.Kind, initial.Payload = "imported", map[string]any{"password": "fixture-password"}
			}
			if err = assets.Put(initial); err != nil {
				t.Fatal(err)
			}
			before, err := assets.Snapshot("one")
			if err != nil {
				t.Fatal(err)
			}
			task := store.Task{ID: "original", Script: "asset_ops", Status: "queued", ArgsJSON: `["refresh","--ref","one","--platform","chatgpt"]`}
			if strings.HasPrefix(mode, "work-") {
				task.ID = "work-original"
				if err = st.CreateTask(task); err != nil {
					t.Fatal(err)
				}
				if err = st.MarkRunning(task.ID, 0); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, _, err = st.QueueAccountRefresh(task, "one"); err != nil {
					t.Fatal(err)
				}
				if err = st.BeginAccountRefresh(task.ID, "one", before.ResultSHA); err != nil {
					t.Fatal(err)
				}
			}
			repo, _ := filepath.Abs("../../..")
			// Simulate power loss after the actual Python journal was persisted,
			// before the current credential file was replaced. No HTTP is involved.
			script := fmt.Sprintf(`import sys,os,json
from pathlib import Path
from unittest.mock import patch
sys.path.insert(0,%q)
from tools import asset_ops
path=Path(os.environ['REG_FACTORY_DATA_DIR']).resolve()/'assets/chatgpt'/os.environ['FIXTURE_KIND']/'one.json'
original=json.loads(path.read_text())
write=asset_ops._atomic_json_write
def interrupted(destination,document):
    stage=os.environ['FIXTURE_STAGE']
    if stage=='before-receipt' and destination.parent.name=='credential-task-results': raise OSError('fixture receipt failure')
    if stage not in ('saved-python-unavailable','work-saved-python-unavailable','current-without-receipt','before-receipt') and 'assets' in destination.parts: raise OSError('fixture publication failure')
    write(destination,document)
with patch.object(asset_ops,'_atomic_json_write',side_effect=interrupted):
    try: asset_ops._refresh_document_copies('one','one@example.test',path,original,{'access_token':'new-at','refresh_token':'new-rt'})
    except OSError: pass
`, repo)
			cmd := exec.Command(python, "-c", script)
			cmd.Env = append(os.Environ(), "REG_FACTORY_DATA_DIR="+root, "REG_FACTORY_RUN_ID="+task.ID, "PYTHONIOENCODING=utf-8", "FIXTURE_STAGE="+mode, "FIXTURE_KIND="+initial.Kind)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("prepare: %v %s", err, out)
			}
			if mode == "current-without-receipt" {
				path := filepath.Join(root, "credential-task-results", fmt.Sprintf("%x.json", sha256.Sum256([]byte(task.ID))))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "changed-current" {
				initial.Payload = map[string]any{"access_token": "later-at", "refresh_token": "later-rt"}
				if err := assets.Put(initial); err != nil {
					t.Fatal(err)
				}
			}
			// All local settlement must work without Python or Work configuration,
			// including a journal saved before current credentials were published.
			cfg := &settings.Settings{DataDir: root, AppRoot: repo, Python: filepath.Join(root, "missing-python")}
			if mode == "database-retry" {
				faults := st.DB()
				createFault := `CREATE TRIGGER reject_finish BEFORE UPDATE OF status ON tasks WHEN NEW.status='succeeded' BEGIN SELECT RAISE(FAIL,'fixture database unavailable'); END`
				dropFault := `DROP TRIGGER reject_finish`
				if st.DialectName() == "postgres" {
					createFault = `CREATE FUNCTION reject_finish() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fixture database unavailable'; END $$;
CREATE TRIGGER reject_finish BEFORE UPDATE OF status ON tasks FOR EACH ROW WHEN (NEW.status='succeeded') EXECUTE FUNCTION reject_finish()`
					dropFault = `DROP TRIGGER reject_finish ON tasks`
				}
				if _, err = faults.Exec(createFault); err != nil {
					t.Fatal(err)
				}
				m := &Manager{cfg: cfg, st: st, stop: make(chan struct{})}
				defer close(m.stop)
				done := make(chan error, 1)
				go func() { _, _, _, err := m.settleCredential(task, "unknown", "unknown", 75, ""); done <- err }()
				deadline := time.Now().Add(5 * time.Second)
				for {
					events, _ := st.TailEvents(task.ID, 20)
					found := false
					for _, e := range events {
						if e.Stream == "stage" && strings.Contains(e.Line, "CREDENTIAL_SETTLEMENT_RETRY") {
							found = true
						}
					}
					if found {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("settlement did not expose recoverable retry")
					}
					time.Sleep(20 * time.Millisecond)
				}
				current, _ := st.GetTask(task.ID)
				if current.Status != "running" {
					t.Fatal("reported success before commit", current.Status)
				}
				if _, err = faults.Exec(dropFault); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("settlement did not recover")
				}
			} else {
				if err = RecoverCredentials(cfg, st); err != nil {
					t.Fatal(err)
				}
				if _, err = st.RecoverInterrupted(); err != nil {
					t.Fatal(err)
				}
				if err = RecoverCredentials(cfg, st); err != nil {
					t.Fatal("repeat restart", err)
				}
			}
			current, _ := st.GetTask(task.ID)
			if mode == "changed-current" {
				saved, err := assets.Get("one")
				if current.Status != "failed" || err != nil || saved.Payload["refresh_token"] != "later-rt" {
					t.Fatal("old publication overwrote a newer result", current.Status, err)
				}
				events, _ := st.TailEvents(task.ID, 20)
				found := false
				for _, event := range events {
					found = found || strings.Contains(event.Line, "CREDENTIAL_CHANGED_BEFORE_SAVE")
				}
				if !found {
					t.Fatal("publication conflict missing from task log")
				}
				return
			}
			if current.Status != "succeeded" {
				t.Fatal("task did not settle", current.Status, current.Error)
			}
			saved, err := assets.Get("one")
			if err != nil || saved.Payload["refresh_token"] != "new-rt" {
				t.Fatal("current credentials not recovered", err)
			}
			if !strings.HasPrefix(mode, "work-") {
				account, _ := st.GetAccount("one")
				check, err := st.GetTask(store.RefreshVerificationID(task.ID))
				if account.RefreshState != "succeeded" || err != nil || check.Status != "queued" {
					t.Fatal("account/automatic AT projection missing", err)
				}
			}
			files, _ := filepath.Glob(filepath.Join(root, "oauth-results", "*.json"))
			if len(files) != 0 {
				t.Fatal("settled publication journal retained")
			}
		})
	}
}
