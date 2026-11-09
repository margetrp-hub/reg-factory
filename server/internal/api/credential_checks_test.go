package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestCredentialChecksPersistUnknownRetryAndOriginalReceipt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{st: st, assets: assetstore.New(root), env: settings.NewEnv(filepath.Join(root, "fixture.env"))}
	defer func() { s.st.Close() }()
	_, err = st.UpsertAccounts([]store.AccountSeed{{Ref: "one", Identity: "one@example.test", Platform: "chatgpt", Kind: "oauth"}, {Ref: "missing", Platform: "chatgpt", Kind: "oauth"}, {Ref: "three", Platform: "chatgpt", Kind: "oauth"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"one", "three"} {
		if err = s.assets.Put(assetstore.Asset{Ref: ref, Platform: "chatgpt", Kind: "oauth", Payload: map[string]any{"refresh_token": "fixture-rt"}}); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.assets.Get("one")
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("actor", "operator"); c.Next() })
	r.POST("/checks", s.createCredentialCheck)
	r.GET("/checks", s.listCredentialChecks)
	r.GET("/checks/:id", s.getCredentialCheck)
	invoke := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	const id = "request-5bfcaa6e-5068-4c82-8128-40dd67238d65"
	body := `{"request_id":"5bfcaa6e-5068-4c82-8128-40dd67238d65","platform":"chatgpt","refs":["one","missing","three"]}`
	if w := invoke("POST", "/checks", body); w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	if next, err := st.NextScriptTask(); err != nil || next != nil {
		t.Fatal("Python runner claimed API batch", next, err)
	}
	if w := invoke("POST", "/checks", body); w.Code != 200 {
		t.Fatal("lost-response replay failed", w.Code, w.Body)
	}
	task, err := st.ClaimCredentialCheck()
	if err != nil {
		t.Fatal(err)
	}
	s.executeCredentialCheck(context.Background(), task, s.checkCredential)
	items, err := st.CredentialCheckItems(id)
	if err != nil || len(items) != 3 || items[0].Status != "needs_refresh" || items[1].Status != "unknown" || items[2].Status != "pending" {
		t.Fatal(items, err)
	}
	finished, _ := st.GetTask(id)
	if finished.Status != "unknown" {
		t.Fatal(finished)
	}
	after, _ := s.assets.Get("one")
	if before.ResultSHA != after.ResultSHA {
		t.Fatal("AT check mutated credentials")
	}
	account, _ := st.GetAccount("one")
	if account.Status != "needs_refresh" {
		t.Fatal(account)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	s.st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st = s.st
	if w := invoke("GET", "/checks/"+id, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "needs_refresh") {
		t.Fatal("history lost on restart", w.Code, w.Body)
	}
	w := invoke("GET", "/checks?platform=chatgpt", "")
	var history struct {
		Page   store.TaskPage                         `json:"page"`
		Counts map[string]store.CredentialCheckCounts `json:"counts"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &history); err != nil || history.Page.Total != 1 || history.Counts[id].Checked != 2 {
		t.Fatal(history, err)
	}
	if w = invoke("POST", "/checks", strings.Replace(body, `["one","missing","three"]`, `["one"]`, 1)); w.Code != 409 {
		t.Fatal("same request changed", w.Code, w.Body)
	}
	// Recover only the untested account in a new linked batch; the original
	// unknown receipt stays immutable and no refresh grant is ever exercised.
	retry := `{"request_id":"fd31fb9a-c4bd-43cf-af47-c8f085d505b4","platform":"chatgpt","parent_id":"` + id + `","refs":["three"]}`
	if w = invoke("POST", "/checks", retry); w.Code != 202 {
		t.Fatal(w.Code, w.Body)
	}
	task, err = st.ClaimCredentialCheck()
	if err != nil {
		t.Fatal(err)
	}
	s.executeCredentialCheck(context.Background(), task, s.checkCredential)
	done, _ := st.GetTask(task.ID)
	if done.Status != "succeeded" {
		t.Fatal(done)
	}
	original, _ := st.GetTask(id)
	if original.Status != "unknown" {
		t.Fatal("retry overwrote evidence")
	}
	if w = invoke("GET", "/checks?platform=kiro", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatal("platform history leaked", w.Code, w.Body)
	}
}

func TestCredentialCheckStopAndRestartPreserveReturnedItems(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	accounts := []store.Account{{Ref: "one", Platform: "chatgpt"}, {Ref: "two", Platform: "chatgpt"}}
	task := store.Task{ID: "stop-batch", Script: store.CredentialCheckScript, ArgsJSON: `{"platform":"chatgpt"}`}
	if err = st.CreateCredentialCheck(task, accounts); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimCredentialCheck()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{st: st}
	calls := 0
	s.executeCredentialCheck(context.Background(), claimed, func(_ context.Context, _ string, item store.CredentialCheckItem) store.CredentialCheckItem {
		calls++
		if err = st.StopCredentialCheck(claimed.ID); err != nil {
			t.Fatal(err)
		}
		item.Status, item.Message = "ok", "fixture success"
		return item
	})
	done, _ := st.GetTask(task.ID)
	items, _ := st.CredentialCheckItems(task.ID)
	if calls != 1 || done.Status != "stopped" || items[0].Status != "ok" || items[1].Status != "pending" {
		t.Fatal(calls, done, items)
	}
	task.ID = "restart-batch"
	if err = st.CreateCredentialCheck(task, accounts); err != nil {
		t.Fatal(err)
	}
	claimed, _ = st.ClaimCredentialCheck()
	if _, err = st.StartCredentialCheckItem(claimed.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = st.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	done, _ = st.GetTask(task.ID)
	items, _ = st.CredentialCheckItems(task.ID)
	if done.Status != "interrupted" || items[0].Status != "unknown" || items[1].Status != "pending" {
		t.Fatal(done, items)
	}
}
