package api

import (
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"net/http/httptest"
	"testing"
)

func TestExportSelectedHistoryAndPreserveWorkspace(t *testing.T) {
	root := t.TempDir()
	assets := assetstore.New(root)
	a := assetstore.Asset{Ref: "fixture", Platform: "chatgpt", Kind: "oauth", Email: "test@example.test", Payload: map[string]any{
		"access_token": "first", "refresh_token": "first-rt", "account_id": "workspace", "seat_type": "premium"}}
	if err := assets.Put(a); err != nil {
		t.Fatal(err)
	}
	first, _ := assets.Get(a.Ref)
	a.Payload["access_token"], a.Payload["refresh_token"] = "second", "second-rt"
	if err := assets.Put(a); err != nil {
		t.Fatal(err)
	}
	s := &Server{assets: assetstore.New(root)}
	r := gin.New()
	r.GET("/export", s.assetExport)
	r.GET("/history", s.assetResults)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest("GET", "/export?ref=fixture&result_sha256="+first.ResultSHA, nil))
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var result struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Accounts) != 1 || result.Accounts[0]["access_token"] != "first" || result.Accounts[0]["account_id"] != "workspace" {
		t.Fatal("export selected wrong version")
	}
	history := httptest.NewRecorder()
	r.ServeHTTP(history, httptest.NewRequest("GET", "/history?ref=fixture", nil))
	var records struct {
		Items []assetstore.Asset `json:"items"`
	}
	_ = json.Unmarshal(history.Body.Bytes(), &records)
	if len(records.Items) != 2 {
		t.Fatal("history missing")
	}
	for _, row := range records.Items {
		if row.Payload != nil {
			t.Fatal("history leaked payload")
		}
	}
}

func TestResultSearchFindsRetainedOutputWithoutCurrentProjection(t *testing.T) {
	assets := assetstore.New(t.TempDir())
	a := assetstore.Asset{Ref: "lost-projection", Platform: "chatgpt", Kind: "oauth", Email: "recover@example.test", Payload: map[string]any{"access_token": "synthetic-retained"}}
	raw, _ := json.Marshal(a)
	if _, err := assets.Retain(raw); err != nil {
		t.Fatal(err)
	}
	s := &Server{assets: assets}
	r := gin.New()
	r.GET("/history", s.assetResults)
	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest("GET", "/history?query=recover", nil))
	var result struct {
		Items []assetstore.Asset `json:"items"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Items) != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	if result.Items[0].Ref != a.Ref || result.Items[0].Current || result.Items[0].Payload != nil {
		t.Fatal("retained result missing or incorrectly exposed")
	}
}
