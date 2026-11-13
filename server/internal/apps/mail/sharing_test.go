package mail

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestMailboxSharingPreservesProtocolAndKeepsAliasSecretsSeparate(t *testing.T) {
	a := poolApp(t)
	for _, transport := range []string{TransportIMAP, TransportPOP3, TransportGraph} {
		email := transport + "@example.test"
		src, err := a.DB.UpsertSource(Source{Name: transport, Username: email, Host: "mail.example.test", Password: "fixture-password", Transport: transport, ClientID: "fixture-client", RefreshToken: "fixture-rt", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		_, _, kind, role, _, doc, err := a.ShareSnapshot("mail:" + email)
		if err != nil || kind != transport || role != "parent" || doc["username"] != email {
			t.Fatal("source export", transport, kind, role, err)
		}
		if transport == TransportGraph {
			if doc["client_id"] != src.ClientID || doc["refresh_token"] != src.RefreshToken {
				t.Fatal("Graph material incomplete")
			}
		} else if doc["host"] != src.Host || doc["port"] != src.Port || doc["password"] != src.Password || doc["ssl"] != true {
			t.Fatal("password source material incomplete", transport)
		}
		alias := "alias-" + email
		if _, err = a.DB.ImportMailboxes(context.Background(), src.ID, []string{alias}); err != nil {
			t.Fatal(err)
		}
		_, _, kind, role, _, doc, err = a.ShareSnapshot("mail:" + alias)
		raw, _ := json.Marshal(doc)
		if err != nil || kind != "alias" || role != "alias" || doc["parent_source_ref"] != "mail:"+email || strings.Contains(string(raw), "fixture-") {
			t.Fatal("alias must refer to its separately selected source", transport, err)
		}
	}
}

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
