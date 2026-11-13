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
		inputs = append(inputs, fmt.Sprintf("new-%02d@example.test----private-password----JBSWY3DPEHPK3PXP", i))
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
	for _, account := range after {
		password, totp, err := st.GetCredential(account.Ref)
		if err != nil || password != "private-password" || totp != "JBSWY3DPEHPK3PXP" {
			t.Fatal("imported login material missing after sync", err)
		}
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

func TestImportRetryPreservesLoginMaterial(t *testing.T) {
	root := t.TempDir()
	st := testdatabase.Open(t, root)
	s := &Server{st: st, assets: assetstore.New(root)}
	r := gin.New()
	r.POST("/import", s.importAccounts)
	for _, line := range []string{"retry@example.test----private-password----JBSWY3DPEHPK3PXP", "retry@example.test"} {
		raw, _ := json.Marshal(map[string]string{"platform": "chatgpt", "accounts": line})
		req := httptest.NewRequest("POST", "/import", strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		password, totp, err := st.GetCredential("import:chatgpt:retry@example.test")
		if err != nil || password != "private-password" || totp != "JBSWY3DPEHPK3PXP" {
			t.Fatal("retry lost stored login material", err)
		}
		asset, err := s.assets.Get("import:chatgpt:retry@example.test")
		if err != nil || asset.Payload["password"] != password || asset.Payload["totp"] != totp {
			t.Fatal("export disagrees with production login material", err)
		}
	}
	if err := st.WithdrawAccount("import:chatgpt:retry@example.test"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/import", strings.NewReader(`{"platform":"chatgpt","accounts":"retry@example.test----replacement"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	password, _, err := st.GetCredential("import:chatgpt:retry@example.test")
	if w.Code != 409 || err != nil || password != "private-password" {
		t.Fatal("transferred source was mutated", w.Code, err)
	}
}

func TestImportDatabaseFailureCanRetryWithoutDuplicate(t *testing.T) {
	if os.Getenv("REG_FACTORY_TEST_POSTGRES_DSN") == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	root := t.TempDir()
	st := testdatabase.Open(t, root)
	s := &Server{st: st, assets: assetstore.New(root)}
	r := gin.New()
	r.POST("/import", s.importAccounts)
	if _, err := st.Execer().Exec("ALTER TABLE account_credentials ADD CONSTRAINT synthetic_import_failure CHECK (ref <> 'import:chatgpt:db-retry@example.test')"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{500, 200, 200} {
		req := httptest.NewRequest("POST", "/import", strings.NewReader(`{"platform":"chatgpt","accounts":"db-retry@example.test----private-password----JBSWY3DPEHPK3PXP"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want || strings.Contains(w.Body.String(), "private-password") {
			t.Fatal(w.Code, w.Body.String())
		}
		if want == 500 {
			if !strings.Contains(w.Body.String(), "import_save_failed") {
				t.Fatal("missing visible save failure")
			}
			if _, err := st.Execer().Exec("ALTER TABLE account_credentials DROP CONSTRAINT synthetic_import_failure"); err != nil {
				t.Fatal(err)
			}
		}
	}
	accounts, err := st.ListAccounts("", "", "", 100)
	if err != nil || len(accounts) != 1 {
		t.Fatal("retry duplicated account", err)
	}
	password, totp, err := st.GetCredential("import:chatgpt:db-retry@example.test")
	if err != nil || password != "private-password" || totp != "JBSWY3DPEHPK3PXP" {
		t.Fatal("retry did not restore production login material", err)
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

func TestImportSameMailboxAcrossPlatformsKeepsIndependentCredentials(t *testing.T) {
	root := t.TempDir()
	st := testdatabase.Open(t, root)
	s := &Server{st: st, assets: assetstore.New(root)}
	r := gin.New()
	r.POST("/import", s.importAccounts)
	platforms := []string{"claude", "chatgpt", "grok", "outlook", "kiro", "github"}
	for _, platform := range platforms {
		for _, material := range []string{"SAME@example.test----" + platform + "-password", "same@example.test"} {
			body, _ := json.Marshal(map[string]string{"platform": platform, "accounts": material})
			req := httptest.NewRequest("POST", "/import", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatal(platform, w.Code, w.Body.String())
			}
		}
	}
	rows, err := st.ListAccounts("", "", "", 100)
	if err != nil || len(rows) != len(platforms) {
		t.Fatalf("platform identities overwritten: count=%d err=%v", len(rows), err)
	}
	for _, account := range rows {
		password, _, err := st.GetCredential(account.Ref)
		if err != nil || password != account.Platform+"-password" {
			t.Fatal("cross-platform login material", account.Platform, err)
		}
		asset, err := s.assets.Get(account.Ref)
		if err != nil || asset.Platform != account.Platform || asset.Payload["password"] != password {
			t.Fatal("export identity disagrees", account.Platform, err)
		}
	}
}

func TestReimportKeepsExistingBusinessReference(t *testing.T) {
	root := t.TempDir()
	st := testdatabase.Open(t, root)
	s := &Server{st: st, assets: assetstore.New(root)}
	ref := "import:existing@example.test"
	if _, err := st.UpsertAccounts([]store.AccountSeed{{Ref: ref, Platform: "chatgpt", Identity: "existing@example.test", Kind: "imported"}}); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.POST("/import", s.importAccounts)
	for _, platform := range []string{"chatgpt", "github"} {
		body, _ := json.Marshal(map[string]string{"platform": platform, "accounts": "EXISTING@example.test----" + platform + "-password"})
		req := httptest.NewRequest("POST", "/import", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
	}
	rows, err := st.ListAccounts("", "", "", 100)
	if err != nil || len(rows) != 2 {
		t.Fatal("reimport duplicated business identity", len(rows), err)
	}
	for _, key := range []string{ref, "import:github:existing@example.test"} {
		account, err := st.GetAccount(key)
		if err != nil {
			t.Fatal("business reference lost", err)
		}
		password, _, err := st.GetCredential(key)
		if err != nil || password != account.Platform+"-password" {
			t.Fatal("cross-platform credential changed", err)
		}
	}
}
