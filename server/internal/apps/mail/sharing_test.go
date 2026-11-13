package mail

import (
	"context"
	"testing"
)

func TestHandoffMailboxIsNotRediscoveredOrReenabled(t *testing.T) {
	a := poolApp(t)
	src := poolSource(t, a, "one@example.test")
	ctx := context.Background()
	if rows, n, e := a.ShareCandidates(ctx, "", 25, 0); e != nil || n != 1 {
		t.Fatal(rows, n, e)
	}
	if e := a.WithdrawShared("mail:one@example.test"); e != nil {
		t.Fatal(e)
	}
	if rows, n, e := a.ShareCandidates(ctx, "", 25, 0); e != nil || n != 0 {
		t.Fatal(rows, n, e)
	}
	if _, _, _, _, _, _, e := a.ShareSnapshot("mail:one@example.test"); e == nil {
		t.Fatal("former source reexported")
	}
	if e := a.DB.SetSourceEnabled(ctx, src.ID, true); e == nil {
		t.Fatal("former source reenabled")
	}
	if _, e := a.DB.UpsertSource(src); e == nil {
		t.Fatal("former source recreated")
	}
}
