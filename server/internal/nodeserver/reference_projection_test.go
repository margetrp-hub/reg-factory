package nodeserver

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestAccountDisplayDeduplicationPreservesEveryRepairReference(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "references.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seeds := []store.AccountSeed{
		{Ref: "creds:original", Platform: "chatgpt", Kind: "oauth", Identity: "same@example.test"},
		{Ref: "import:new", Platform: "chatgpt", Kind: "oauth", Identity: " SAME@example.test "},
	}
	if _, err := st.UpsertAccounts(seeds); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkClaimed("creds:original"); err != nil {
		t.Fatal(err)
	}
	rows, total, err := st.ListAccountsPage("", "chatgpt", "", 50, 0)
	if err != nil || len(rows) != 1 || total != 1 || rows[0].SourceCount != 2 {
		t.Fatalf("identity display: rows=%+v total=%d error=%v", rows, total, err)
	}
	h := New(Config{NodeID: "node-sg-1"}, nil, st)
	for _, seed := range seeds {
		got, ok := h.subjectRef(SubjectID(seed.Ref))
		if !ok || got != seed.Ref {
			t.Fatalf("original repair reference lost: %s -> %q (%v)", seed.Ref, got, ok)
		}
	}
	if err := st.AdvanceAccountVersion("creds:original", 1); err != nil {
		t.Fatal(err)
	}
	original, _ := st.GetAccount("creds:original")
	newer, _ := st.GetAccount("import:new")
	if original.Version != 2 || !original.Claimed || newer.Version != 1 || newer.Claimed {
		t.Fatal("display projection changed original ownership/version", original, newer)
	}
}

func TestExistingAccountOperationsUseAccountPlatform(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.UpsertAccounts([]store.AccountSeed{{Ref: "import:chatgpt", Platform: "chatgpt", Kind: "oauth", Identity: "fixture@example.test"}}); err != nil {
		t.Fatal(err)
	}
	h := New(Config{NodeID: "fixture", Platform: "claude"}, nil, st)
	for _, action := range []string{"account.authorize", "account.refresh", "account.liveness"} {
		v := &Verified{Action: action, Subject: SubjectID("import:chatgpt"), Params: map[string]any{}}
		raw, err := h.assetOpArgv(v)
		if err != nil {
			t.Fatal(err)
		}
		var argv []string
		if err = json.Unmarshal([]byte(raw), &argv); err != nil {
			t.Fatal(err)
		}
		platform := ""
		for i := 0; i+1 < len(argv); i++ {
			if argv[i] == "--platform" {
				platform = argv[i+1]
			}
		}
		if platform != "chatgpt" {
			t.Fatalf("%s ran %s for a chatgpt asset: %v", action, platform, argv)
		}
	}
}
