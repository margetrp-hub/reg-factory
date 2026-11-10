package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestRefreshProcessReceiptAndSavedCredentialMustAgree(t *testing.T) {
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal("Python required for subprocess acceptance", err)
	}
	for _, tc := range []struct{ name, body, status string }{
		{"saved", `p=json.loads(path.read_text());asset_ops._refresh_document_copies("one","",path,p,{"access_token":"new-at","refresh_token":"new-rt"});print(json.dumps({"ref":"one","result":"refreshed"}))`, "succeeded"},
		{"saved-then-crashed", `p=json.loads(path.read_text());asset_ops._refresh_document_copies("one","",path,p,{"access_token":"new-at","refresh_token":"new-rt"});sys.exit(75)`, "succeeded"},
		{"foreign-success", `p=json.loads(path.read_text());p["payload"]["access_token"]="foreign-at";path.write_text(json.dumps(p));print(json.dumps({"ref":"one","result":"refreshed"}))`, "failed"},
		{"false-success", `print(json.dumps({"ref":"one","result":"refreshed"}))`, "failed"},
		{"missing-id", `p=json.loads(path.read_text());p["payload"]["access_token"]="new-at";p["payload"].pop("id_token");path.write_text(json.dumps(p));print(json.dumps({"ref":"one","result":"refreshed"}))`, "failed"},
		{"unknown", `print(json.dumps({"ref":"one","result":"unknown"}));sys.exit(75)`, "failed"},
		{"crash", `sys.exit(1)`, "failed"},
		{"rejected", `print(json.dumps({"ref":"one","result":"relogin_required"}));sys.exit(1)`, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			st, err := store.Open(filepath.Join(root, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			st.UpsertAccounts([]store.AccountSeed{{Ref: "one", Platform: "chatgpt", Kind: "oauth"}})
			assets := assetstore.New(root)
			if err = assets.Put(assetstore.Asset{Ref: "one", Platform: "chatgpt", Kind: "oauth", Payload: map[string]any{"access_token": "old-at", "refresh_token": "old-rt", "id_token": "old-id"}}); err != nil {
				t.Fatal(err)
			}
			os.MkdirAll(filepath.Join(root, "tools"), 0700)
			repo, _ := filepath.Abs("../../..")
			script := fmt.Sprintf("import json,os,sys\nfrom pathlib import Path\nsys.path.insert(0,%q)\nfrom tools import asset_ops\nif sys.argv[1]=='recover-publication': raise SystemExit(asset_ops.main())\npath=Path(os.environ['REG_FACTORY_DATA_DIR'])/'assets/chatgpt/oauth/one.json'\n", repo) + tc.body + "\n"
			if err = os.WriteFile(filepath.Join(root, "tools", "asset_ops.py"), []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := &settings.Settings{Python: python, AppRoot: root, DataDir: root, EnvFile: filepath.Join(root, "missing.env"), MaxParallel: 1, GuardPct: 100}
			m := New(cfg, st)
			defer m.Shutdown(context.Background())
			task := store.Task{ID: "refresh", Script: "asset_ops", ArgsJSON: `["refresh","--ref","one","--platform","chatgpt"]`}
			if _, _, err = st.QueueAccountRefresh(task, "one"); err != nil {
				t.Fatal(err)
			}
			m.Wake()
			deadline := time.Now().Add(12 * time.Second)
			for time.Now().Before(deadline) {
				current, _ := st.GetTask(task.ID)
				if current.Status != "queued" && current.Status != "running" {
					if current.Status != tc.status {
						t.Fatal(current.Status, current.Error)
					}
					account, _ := st.GetAccount("one")
					if account.RefreshState != tc.status {
						t.Fatal("stale projection", account.RefreshState)
					}
					evidence, _ := st.RefreshEvidence(task.ID)
					if evidence.SourceSHA == "" || (tc.status == "succeeded" && evidence.ResultSHA == "") {
						t.Fatal("missing version evidence")
					}
					return
				}
				time.Sleep(25 * time.Millisecond)
			}
			t.Fatal("refresh did not finish")
		})
	}
}
