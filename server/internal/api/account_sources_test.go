package api

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestAccountSourcesReadOriginalDetailsWithoutCredentials(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "sources.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.UpsertAccounts([]store.AccountSeed{
		{Ref: "import:new", Platform: "chatgpt", Identity: "same@example.test", Kind: "oauth"},
		{Ref: "creds:old", Platform: "chatgpt", Identity: " SAME@example.test ", Kind: "team"},
		{Ref: "other", Platform: "outlook", Identity: "same@example.test", Kind: "mail"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.SaveCredential("creds:old", "never-output-password", "never-output-totp"); err != nil {
		t.Fatal(err)
	}
	if err = st.MarkClaimed("creds:old"); err != nil {
		t.Fatal(err)
	}
	s := &Server{st: st}
	r := gin.New()
	r.GET("/sources", s.accountSources)
	r.GET("/detail", s.accountDetail)
	read := func(url string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		return w
	}
	if w := read("/sources"); w.Code != 400 {
		t.Fatal(w.Code, w.Body)
	}
	if w := read("/sources?ref=missing"); w.Code != 404 {
		t.Fatal(w.Code, w.Body)
	}
	w := read("/sources?ref=import:new")
	var result struct {
		Accounts []store.Account `json:"accounts"`
		Total    int             `json:"total"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Total != 2 || len(result.Accounts) != 2 {
		t.Fatal(w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "never-output") {
		t.Fatal("credentials leaked")
	}
	for _, a := range result.Accounts {
		if a.Ref == "other" || a.Claimed != (a.Ref == "creds:old") {
			t.Fatal("sources changed owner", a)
		}
		if detail := read("/detail?ref=" + a.Ref); detail.Code != 200 {
			t.Fatal("old detail unavailable", detail.Code, detail.Body)
		}
	}
	st.Close()
	if w := read("/sources?ref=import:new"); w.Code != 503 || !strings.Contains(w.Body.String(), "account_sources_unavailable") {
		t.Fatal("read failure must be retryable, not an empty result", w.Code, w.Body)
	}
}
