package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/apps/mail"
	"github.com/tiantianGPU/reg-factory/server/internal/centralasset"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/testdatabase"
)

func TestHandoffMaterialsAndJSONAreIndependentAndOptional(t *testing.T) {
	native := map[string]any{"email": "synthetic@example.test", "password": "MATERIAL_SECRET", "host": "imap.example.test", "api_key": "MATERIAL_KEY", "refresh_token": "CREDENTIAL_SECRET"}
	material := selectHandoffParts(native, map[string]any{"include_credentials": false, "fields": map[string]any{"credential_json": map[string]any{"secret": "unselected"}, "sms_enabled": false}})
	raw, _ := json.Marshal(material)
	if strings.Contains(string(raw), "CREDENTIAL_SECRET") || strings.Contains(string(raw), "unselected") || material["sms_enabled"] != false {
		t.Fatal("unselected JSON or false value lost")
	}
	credential := selectHandoffParts(native, map[string]any{"include_materials": false})
	raw, _ = json.Marshal(credential)
	if strings.Contains(string(raw), "MATERIAL_SECRET") || strings.Contains(string(raw), "MATERIAL_KEY") || strings.Contains(string(raw), "imap.example.test") {
		t.Fatal("unselected material leaked")
	}
	empty := selectHandoffParts(native, map[string]any{"include_materials": false, "include_credentials": false})
	if len(empty) != 0 {
		t.Fatal(empty)
	}
	partial := selectHandoffParts(map[string]any{}, map[string]any{"fields": map[string]any{"email_password": "ONLY-ONE-FIELD"}})
	if len(partial) != 1 {
		t.Fatal("partial material rejected")
	}
}

func TestHandoffTableCategoriesPaginationAndIndependentPages(t *testing.T) {
	st := testdatabase.Open(t, t.TempDir())
	dsn, err := st.ProductionDatabaseDSN()
	if err != nil {
		t.Fatal(err)
	}
	mailApp, err := mail.New(mail.Config{DBPath: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mailApp.Close() })
	source, err := mailApp.DB.UpsertSource(mail.Source{Name: "fixture", Host: "imap.example.test", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	addresses := []string{}
	seeds := []store.AccountSeed{}
	for i := 0; i < 27; i++ {
		seeds = append(seeds, store.AccountSeed{Ref: fmt.Sprintf("account-%02d", i), Platform: "chatgpt", Kind: "oauth", Identity: fmt.Sprintf("account-%02d@example.test", i)})
		if i < 26 {
			addresses = append(addresses, fmt.Sprintf("mail-%02d@example.test", i))
		}
	}
	if _, err = st.UpsertAccounts(seeds); err != nil {
		t.Fatal(err)
	}
	if _, err = mailApp.DB.ImportMailboxes(context.Background(), source.ID, addresses); err != nil {
		t.Fatal(err)
	}
	s := &Server{st: st, icApp: mailApp, sharing: &centralasset.Sharing{Store: st}}
	if err = s.sharing.Init(); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.GET("/api/sharing", s.shareList)
	read := func(query string) map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", "/api/sharing?"+query, nil))
		if w.Code != 200 {
			t.Fatalf("list %d: %s", w.Code, w.Body)
		}
		var value map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	seen := map[string]bool{}
	for _, offset := range []int{0, 25, 50} {
		value := read(fmt.Sprintf("type=all&offset=%d", offset))
		rows := value["candidates"].([]any)
		if value["total"] != float64(53) || len(rows) != min(25, 53-offset) {
			t.Fatalf("bad page: %v", value)
		}
		for _, raw := range rows {
			row := raw.(map[string]any)
			ref := row["ref"].(string)
			if seen[ref] {
				t.Fatalf("duplicate %s", ref)
			}
			seen[ref] = true
		}
	}
	if len(seen) != 53 {
		t.Fatal("all selection omitted a page")
	}
	if read("type=account")["total"] != float64(27) || read("type=mailbox")["total"] != float64(26) || read("type=all&q=mail-00")["total"] != float64(1) {
		t.Fatal("category or search changed the source scope")
	}
	if refs, err := s.sharing.Refs(); err != nil || len(refs) != 0 {
		t.Fatal("reading implicitly handed off material", err)
	}
	if err = st.WithdrawAccount("account-00"); err != nil {
		t.Fatal(err)
	}
	if read("type=all")["total"] != float64(52) {
		t.Fatal("transferred source remained selectable")
	}
	// A failed live mailbox query must not block receipts or received material.
	mailApp.Close()
	if len(read("view=history")["records"].([]any)) != 0 || len(read("view=received")["received"].([]any)) != 0 {
		t.Fatal("unexpected receipts")
	}
}
