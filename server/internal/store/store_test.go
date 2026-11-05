package store

import (
	"path/filepath"
	"testing"
)

func TestCommitOperationResultIsIdempotent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.UpsertAccounts([]AccountSeed{{Ref: "creds:test", Identity: "test@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := st.ReserveOperation("op-1", "work-op-1", "account.refresh", 1); err != nil || !fresh {
		t.Fatalf("reserve: fresh=%v err=%v", fresh, err)
	}
	first, err := st.CommitOperationResult("op-1", "account.refresh", "creds:test", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.CommitOperationResult("op-1", "account.refresh", "creds:test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if first != 2 || second != 2 {
		t.Fatalf("operation version changed across replay: first=%d second=%d", first, second)
	}
	account, err := st.GetAccount("creds:test")
	if err != nil {
		t.Fatal(err)
	}
	if account.Version != 2 {
		t.Fatalf("account version advanced more than once: %d", account.Version)
	}
}
