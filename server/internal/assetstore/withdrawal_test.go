package assetstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHandoffWithdrawalHidesExportAndPreventsRediscovery(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	a := Asset{Ref: "one", Platform: "example", Kind: "password", Email: "one@example.test", Payload: map[string]any{"password": "synthetic"}}
	b := a
	b.Ref = "alias"
	for _, v := range []Asset{a, b} {
		if e := s.Put(v); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Withdraw(a.Ref, a.Platform, a.Email); e != nil {
		t.Fatal(e)
	}
	restarted := New(root)
	if rows, e := restarted.List(); e != nil || len(rows) != 0 {
		t.Fatal(rows, e)
	}
	if _, e := restarted.Get(a.Ref); e == nil {
		t.Fatal("private export still available")
	}
	if e := restarted.Put(b); e == nil {
		t.Fatal("scanner republished transferred identity")
	}
	files, e := filepath.Glob(filepath.Join(s.Root(), "example", "password", "*.json"))
	if e != nil || len(files) != 2 {
		t.Fatal("audit files lost", e)
	}
	if _, e = os.Stat(files[0]); e != nil {
		t.Fatal(e)
	}
}
