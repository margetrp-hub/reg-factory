package runner

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestRealExportSchemaPreservesCLIFlags(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &settings.Settings{AppRoot: root, Python: "python"}
	argv, err := BuildArgv(cfg, store.Task{Script: "export_accounts", ArgsJSON: `{"--platform":"all","--format":"accounts"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(argv[3:], []string{"--platform", "all", "--format", "accounts"}) {
		t.Fatal(argv)
	}
	if _, err := BuildArgv(cfg, store.Task{Script: "export_accounts", ArgsJSON: `{"--platform":"not-a-platform"}`}); err == nil {
		t.Fatal("invalid positional platform accepted")
	}
}

func testScripts() []Script {
	return []Script{{
		ID:   "export_accounts",
		File: "tools/export_accounts.py",
		Args: []Arg{
			{Flag: "--platform", Type: "choice", Choices: []string{"all", "claude", "chatgpt"}},
			{Flag: "--format", Type: "choice", Choices: []string{"accounts", "cookies"}},
			{Flag: "--index", Type: "int", Default: float64(-1)},
			{Flag: "--dry-run", Type: "bool"},
		},
	}}
}

func TestNormalizeArgsAcceptsBareAndFlagKeys(t *testing.T) {
	list := testScripts()
	cases := []map[string]any{
		{"platform": "claude"},
		{"--platform": "claude"},
	}
	for _, input := range cases {
		out, err := NormalizeArgs(list, "export_accounts", input)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if out["--platform"] != "claude" {
			t.Fatalf("输入 %v 未归一化为 --platform，得到 %v", input, out)
		}
	}
}

func TestNormalizeArgsDropsUnknownKeys(t *testing.T) {
	list := testScripts()
	out, err := NormalizeArgs(list, "export_accounts", map[string]any{
		"platform":       "claude",
		"evil; rm -rf /": "x",
		"--exec":         "y",
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("应只保留 1 个已知参数，得到 %v", out)
	}
}

func TestTaskWeight(t *testing.T) {
	m := &Manager{cfg: &settings.Settings{MaxParallel: 6}}
	cases := []struct {
		args string
		want int
	}{
		{`{}`, 1},
		{`{"--concurrency":3}`, 3},
		{`{"concurrency":"4"}`, 4},
		{`{"--concurrency":99}`, 6},
		{`{"--concurrency":0}`, 1},
		{`not-json`, 1},
	}
	for _, c := range cases {
		got := m.taskWeight(store.Task{ArgsJSON: c.args})
		if got != c.want {
			t.Fatalf("args=%s want=%d got=%d", c.args, c.want, got)
		}
	}
}
