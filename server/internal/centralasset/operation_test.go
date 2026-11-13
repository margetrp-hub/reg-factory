package centralasset

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tiantianGPU/reg-factory/server/internal/testutil/assetwork"
)

func setupOperation(t *testing.T, f *assetwork.Fixture, root string, scope map[string]any) *Operation {
	t.Helper()
	cfg, err := FromEnvironment(root, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	o, err := New(cfg, scope)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestOriginalResultSurvivesDeliveryFailuresAndRestart(t *testing.T) {
	for _, mode := range []string{"normal", "lost", "offline", "wrong"} {
		t.Run(mode, func(t *testing.T) {
			f := assetwork.New(t)
			f.Mode(mode)
			root := t.TempDir()
			scope := f.Scope("account.refresh")
			o := setupOperation(t, f, root, scope)
			calls := 0
			execute := func(context.Context, map[string]any) (map[string]any, error) {
				calls++
				return map[string]any{"status": "completed", "document": map[string]any{"access_token": "new-at", "refresh_token": "new-rt"}, "error_code": ""}, nil
			}
			value, err := o.Run(context.Background(), execute)
			if err != nil {
				t.Fatal(err)
			}
			want := "completed"
			if mode == "offline" || mode == "wrong" {
				want = "pending"
			}
			if value["state"] != want {
				t.Fatalf("state=%v want=%s", value["state"], want)
			}
			id := value["result_id"]
			if want == "pending" && o.Acknowledge(value["receipt"].(map[string]any)) == nil {
				t.Fatal("unconfirmed result deleted")
			}
			f.Mode("normal")
			// A new host instance reads the old intent and encrypted result.
			o = setupOperation(t, f, root, scope)
			value, err = o.Run(context.Background(), execute)
			if err != nil || value["state"] != "completed" || value["result_id"] != id || calls != 1 || f.Calls("claim") != 1 || f.Calls("read") != 1 {
				t.Fatalf("repeated action or changed result: %v %v calls=%d", value, err, calls)
			}
			stored := f.Result(scope["attempt_id"].(string))
			if stored["document"].(map[string]any)["refresh_token"] != "new-rt" {
				t.Fatal("wrong credential version")
			}
			if err = o.Acknowledge(value["receipt"].(map[string]any)); err != nil {
				t.Fatal(err)
			}
			if v, e := o.saved(); e != nil || v != nil {
				t.Fatal("journal not acknowledged", e)
			}
			if err = o.Acknowledge(value["receipt"].(map[string]any)); err != nil {
				t.Fatal("ACK not idempotent", err)
			}
		})
	}
}

func TestInterruptedExecutionRequiresOriginalResolution(t *testing.T) {
	f := assetwork.New(t)
	root := t.TempDir()
	scope := f.Scope("account.authorize")
	o := setupOperation(t, f, root, scope)
	if err := o.intents.put(o.id, map[string]any{"task_id": o.id, "context": scope}); err != nil {
		t.Fatal(err)
	}
	value, err := o.Run(context.Background(), func(context.Context, map[string]any) (map[string]any, error) {
		t.Fatal("restarted external action")
		return nil, nil
	})
	if err != nil || value["state"] != "unknown" || f.Calls("claim") != 0 {
		t.Fatal(value, err)
	}
	if o.Acknowledge(value["receipt"].(map[string]any)) == nil {
		t.Fatal("unknown was discarded")
	}
	f.Mode("closed")
	value, err = o.Replay(context.Background(), true)
	if err != nil || value["state"] != "closed" {
		t.Fatal(value, err)
	}
	if err = o.Acknowledge(value["receipt"].(map[string]any)); err != nil {
		t.Fatal(err)
	}
}

func TestUnstartedAndMissingEvidenceRemainDistinct(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "unstarted", true: "missing-evidence"}[started], func(t *testing.T) {
			f := assetwork.New(t)
			o := setupOperation(t, f, t.TempDir(), f.Scope("account.refresh"))
			value, err := o.Replay(context.Background(), started)
			want := "failed"
			if started {
				want = "unknown"
			}
			if err != nil || value["state"] != want || f.Calls("claim") != 0 {
				t.Fatal(value, err)
			}
		})
	}
}

func TestExecutorFailureRetainsUnappliedTokensAndScope(t *testing.T) {
	f := assetwork.New(t)
	root := t.TempDir()
	scope := f.Scope("account.refresh")
	o := setupOperation(t, f, root, scope)
	value, err := o.Run(context.Background(), func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"access_token": "unapplied"}, errors.New("execution interrupted")
	})
	if err != nil || value["state"] != "unknown" {
		t.Fatal(value, err)
	}
	saved, err := o.saved()
	if err != nil || saved["result"].(map[string]any)["unapplied_result"].(map[string]any)["access_token"] != "unapplied" {
		t.Fatal("lost returned token", err)
	}
	if f.Result(o.id)["document"] != nil {
		t.Fatal("unknown published credentials")
	}
	scope["subject_id"] = "foreign-account"
	o = setupOperation(t, f, root, scope)
	if _, err = o.Replay(context.Background(), true); err == nil {
		t.Fatal("changed scope accepted")
	}
}

func TestPythonJournalUpgradeAndRollbackCompatibility(t *testing.T) {
	python := os.Getenv("REG_FACTORY_TEST_PYTHON")
	if python == "" {
		python = "python"
	}
	python, err := exec.LookPath(python)
	if err != nil {
		t.Fatal(err)
	}
	f := assetwork.New(t)
	root := t.TempDir()
	scope := f.Scope("account.refresh")
	o := setupOperation(t, f, root, scope)
	repo, _ := filepath.Abs("../../..")
	raw, _ := json.Marshal(scope)
	script := `import sys,json
from pathlib import Path
sys.path.insert(0,str(Path(sys.argv[1])/'shared/node-sdk/python'))
from aishop_node.execution import AssetOperation
c=json.loads(sys.argv[3]);op=AssetOperation(Path(sys.argv[2])/'central-assets',sys.argv[4],c,claim=None,send=None)
if sys.argv[5]=='save':
 op.intents.put(c['attempt_id'],{'task_id':c['attempt_id'],'context':c})
 op.stage({'status':'completed','document':{'access_token':'python-at','refresh_token':'python-rt'},'error_code':''})
else:
 assert op.saved()['message']['result']['document']['access_token']=='go-at'
 op.acknowledge(json.loads(sys.argv[5]))
`
	run := func(arg string) {
		t.Helper()
		cmd := exec.Command(python, "-c", script, repo, root, string(raw), f.JournalKey, arg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("compatibility: %v %s", err, out)
		}
	}
	run("save")
	value, err := o.Replay(context.Background(), true)
	if err != nil || value["state"] != "completed" || f.Calls("claim") != 0 {
		t.Fatal(value, err)
	}
	if err = o.Acknowledge(value["receipt"].(map[string]any)); err != nil {
		t.Fatal(err)
	}
	// Rollback to the previous Python adapter can consume a new Go result.
	scope = f.Scope("account.refresh")
	raw, _ = json.Marshal(scope)
	o = setupOperation(t, f, root, scope)
	value, err = o.Run(context.Background(), func(context.Context, map[string]any) (map[string]any, error) {
		return map[string]any{"status": "completed", "document": map[string]any{"access_token": "go-at"}, "error_code": ""}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ack, _ := json.Marshal(value["receipt"])
	run(string(ack))
	if v, e := o.saved(); e != nil || v != nil {
		t.Fatal("Python ACK did not clean Go result", e)
	}
}
