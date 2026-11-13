package centralasset

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/tiantianGPU/reg-factory/server/internal/testutil/assetwork"
)

func TestPendingResultSurvivesKeyRotationAndCenterSwitch(t *testing.T) {
	for _, switched := range []bool{false, true} {
		t.Run(map[bool]string{false: "rotated-key", true: "changed-center"}[switched], func(t *testing.T) {
			f := assetwork.New(t)
			f.Mode("offline")
			cfg, err := FromEnvironment(t.TempDir(), os.Environ())
			if err != nil {
				t.Fatal(err)
			}
			scope := f.Scope("account.refresh")
			op, err := New(cfg, scope)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			execute := func(context.Context, map[string]any) (map[string]any, error) {
				calls++
				return map[string]any{"status": "completed", "error_code": "", "document": map[string]any{"access_token": "new-at", "refresh_token": "new-rt"}}, nil
			}
			result, err := op.Run(context.Background(), execute)
			if err != nil || result["state"] != "pending" {
				t.Fatal(result, err)
			}
			if err := PreserveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			cfg.JournalKey = base64.URLEncoding.EncodeToString([]byte(strings.Repeat("z", 32)))
			if switched {
				cfg.CenterID, cfg.Origin = "other-center", "http://127.0.0.1:1"
			}
			f.Mode("normal")
			restarted, err := New(cfg, scope)
			if err != nil {
				t.Fatal(err)
			}
			settled, err := restarted.Run(context.Background(), execute)
			if err != nil || settled["state"] != "completed" || settled["result_id"] != result["result_id"] || calls != 1 {
				t.Fatal(settled, err, calls)
			}
			if err := restarted.Acknowledge(settled["receipt"].(map[string]any)); err != nil {
				t.Fatal(err)
			}
			if value, err := restarted.saved(); value != nil || err != nil {
				t.Fatal("ack did not settle original result", err)
			}
			if switched {
				if _, err := New(cfg, f.Scope("account.refresh")); err == nil {
					t.Fatal("history authorized new work for old center")
				}
			}
		})
	}
}
