package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/testdatabase"
)

func TestImportedAccountsRemainVisibleAfterSyncWithStableTimes(t *testing.T) {
	root := t.TempDir()
	st := testdatabase.Open(t, root)
	s := &Server{st: st, assets: assetstore.New(root)}
	route := gin.New()
	route.POST("/import", s.importAccounts)
	route.GET("/accounts", s.listAccounts)
	var inputs []string
	for i := 0; i < 10; i++ {
		inputs = append(inputs, fmt.Sprintf("new-%02d@example.test----private-password", i))
	}
	raw, _ := json.Marshal(map[string]any{"platform": "chatgpt", "accounts": strings.Join(inputs, "\n")})
	request := httptest.NewRequest("POST", "/import", strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	result := httptest.NewRecorder()
	route.ServeHTTP(result, request)
	if result.Code != 200 {
		t.Fatal(result.Code, result.Body.String())
	}
	var imported struct {
		Batch    string `json:"batch"`
		Imported int    `json:"imported"`
	}
	json.Unmarshal(result.Body.Bytes(), &imported)
	if imported.Imported != 10 || imported.Batch == "" {
		t.Fatal(imported)
	}
	read := func(query string) []store.Account {
		t.Helper()
		w := httptest.NewRecorder()
		route.ServeHTTP(w, httptest.NewRequest("GET", "/accounts?"+query, nil))
		var page struct {
			Accounts []store.Account `json:"accounts"`
			Total    int             `json:"total"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "private-password") {
			t.Fatal("credentials exposed")
		}
		return page.Accounts
	}
	before := read("batch=" + imported.Batch)
	if len(before) != 10 || before[0].CreatedAt == "" || before[0].ImportedAt == "" || before[0].HasCredentials {
		t.Fatal("import not discoverable", before)
	}
	seeds := []store.AccountSeed{{Ref: "old", Identity: "older@example.test", Platform: "chatgpt", Kind: "oauth"}}
	for _, a := range before {
		seeds = append(seeds, store.AccountSeed{Ref: a.Ref, Identity: a.Identity, Platform: a.Platform, Kind: a.Kind, SecretRef: "assets/imported"})
	}
	if _, err := st.UpsertAccounts(seeds[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execer().Exec("UPDATE accounts SET created_at='2020-01-01T00:00:00Z' WHERE ref='old'"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncAccounts(seeds); err != nil {
		t.Fatal(err)
	}
	after := read("batch=" + imported.Batch)
	if len(after) != 10 || after[0].CreatedAt != before[0].CreatedAt || after[0].ImportedAt != before[0].ImportedAt {
		t.Fatal("sync changed import time or batch", after)
	}
	if rows := read("platform=chatgpt&limit=10"); len(rows) != 10 || rows[0].Ref == "old" {
		t.Fatal("new imports buried", rows)
	}
	if rows := read("q=NEW-03&library_status=pending"); len(rows) != 1 || rows[0].Identity != "new-03@example.test" {
		t.Fatal("search mismatch", rows)
	}
	// Existing pre-release imports still carry their actual first-recorded time.
	if _, err := st.Execer().Exec("UPDATE accounts SET imported_at='',import_batch='' WHERE ref=?", before[0].Ref); err != nil {
		t.Fatal(err)
	}
	a, err := st.GetAccount(before[0].Ref)
	if err != nil || a.CreatedAt != before[0].CreatedAt {
		t.Fatal(a, err)
	}
}

func TestImportStorageFailureDoesNotReportSuccess(t *testing.T) {
	root := t.TempDir()
	st := testdatabase.Open(t, root)
	if err := os.WriteFile(filepath.Join(root, "assets"), []byte("blocked fixture path"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{st: st, assets: assetstore.New(root)}
	r := gin.New()
	r.POST("/import", s.importAccounts)
	req := httptest.NewRequest("POST", "/import", strings.NewReader(`{"platform":"chatgpt","accounts":"failure@example.test----private-password"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 500 || !strings.Contains(w.Body.String(), "import_save_failed") || strings.Contains(w.Body.String(), "private-password") {
		t.Fatal(w.Code, w.Body.String())
	}
}
