package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestLivenessWithOnlyRefreshTokenDoesNotExchangeOrRewriteCredentials(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_, err = st.UpsertAccounts([]store.AccountSeed{{Ref: "fixture", Platform: "chatgpt", Kind: "oauth"}})
	if err != nil {
		t.Fatal(err)
	}
	assets := assetstore.New(root)
	if err := assets.Put(assetstore.Asset{Ref: "fixture", Platform: "chatgpt", Kind: "oauth", Payload: map[string]any{"refresh_token": "fixture-rt", "id_token": "fixture-id"}}); err != nil {
		t.Fatal(err)
	}
	before, err := assets.Get("fixture")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: &settings.Settings{AppRoot: root}, st: st, assets: assets, env: settings.NewEnv(filepath.Join(root, "config.env"))}
	router := gin.New()
	router.POST("/test", server.accountLiveness)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest("POST", "/test", strings.NewReader(`{"ref":"fixture"}`)))
	if response.Code != 200 || !strings.Contains(response.Body.String(), "needs_refresh") {
		t.Fatal(response.Code, response.Body)
	}
	after, err := assets.Get("fixture")
	if err != nil || before.ResultSHA != after.ResultSHA {
		t.Fatal("test changed credential document", err)
	}
	account, err := st.GetAccount("fixture")
	if err != nil || account.Status != "needs_refresh" {
		t.Fatal(account, err)
	}
}

func TestRefreshReplaysOriginalAndAcceptsDeliveredAccount(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.UpsertAccounts([]store.AccountSeed{{Ref: "fixture", Platform: "chatgpt", Kind: "oauth"}, {Ref: "delivered", Platform: "chatgpt", Kind: "oauth"}}); err != nil {
		t.Fatal(err)
	}
	if err = st.MarkClaimed("delivered"); err != nil {
		t.Fatal(err)
	}
	cfg := &settings.Settings{AppRoot: root, DataDir: root, MaxParallel: 0}
	manager := runner.New(cfg, st)
	defer manager.Shutdown(context.Background())
	server := &Server{cfg: cfg, st: st, run: manager}
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("actor", "operator"); c.Next() })
	router.POST("/refresh", server.accountRefresh)
	invoke := func(ref, id string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/refresh", strings.NewReader(`{"ref":"`+ref+`","platform":"untrusted","request_id":"`+id+`"}`)))
		return response
	}
	const id = "d5f879d2-88c9-4fdc-9726-0e597246bc11"
	first := invoke("fixture", id)
	if first.Code != 202 {
		t.Fatal(first.Code, first.Body)
	}
	task, err := st.GetTask("request-" + id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(task.ArgsJSON, "untrusted") || !strings.Contains(task.ArgsJSON, "chatgpt") {
		t.Fatal("caller overrode account platform")
	}
	if err = st.Finish(task.ID, "unknown", 75, "fixture_unknown"); err != nil {
		t.Fatal(err)
	}
	replay := invoke("fixture", id)
	var body map[string]any
	_ = json.Unmarshal(replay.Body.Bytes(), &body)
	if replay.Code != 200 || body["status"] != "unknown" || body["task_id"] != task.ID {
		t.Fatal(replay.Code, replay.Body)
	}
	if result := invoke("delivered", "e5f879d2-88c9-4fdc-9726-0e597246bc11"); result.Code != 202 {
		t.Fatal("delivered refresh blocked", result.Code, result.Body)
	}
	page, err := st.QueryTasks(store.TaskQuery{})
	if err != nil || page.Total != 2 {
		t.Fatal("duplicate or missing task", page, err)
	}
}

func TestImportConversionUsesDurableRepairForDeliveredAccount(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.UpsertAccounts([]store.AccountSeed{{Ref: "import:one", Platform: "chatgpt", Kind: "imported"}, {Ref: "delivered", Platform: "chatgpt", Kind: "oauth"}})
	st.SaveCredential("import:one", "fixture-private-password", "fixture-private-totp")
	st.SaveCredential("delivered", "fixture-private-password", "fixture-private-totp")
	st.MarkClaimed("delivered")
	cfg := &settings.Settings{AppRoot: root, DataDir: root, MaxParallel: 0}
	manager := runner.New(cfg, st)
	defer manager.Shutdown(context.Background())
	s := &Server{cfg: cfg, st: st, run: manager}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("actor", "operator"); c.Next() })
	r.POST("/convert", s.convertAccounts)
	invoke := func(id string) string {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", "/convert", strings.NewReader(`{"refs":["import:one","delivered"],"request_id":"`+id+`"}`)))
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body)
		}
		var result struct {
			Queued []struct {
				TaskID string `json:"task_id"`
			}
			Skipped []struct{ Reason string }
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Queued) != 2 || len(result.Skipped) != 0 {
			t.Fatal(w.Body)
		}
		return result.Queued[0].TaskID
	}
	first := invoke("d5f879d2-88c9-4fdc-9726-0e597246bc11")
	task, _ := st.GetTask(first)
	if task.Script != "asset_ops" || !strings.Contains(task.ArgsJSON, "--allow-relogin") || strings.Contains(task.ArgsJSON, "fixture-private") {
		t.Fatal(task)
	}
	st.Finish(first, "failed", 75, "fixture: result not confirmed")
	if id := invoke("d5f879d2-88c9-4fdc-9726-0e597246bc11"); id != first {
		t.Fatal("lost response created duplicate", id)
	}
	if id := invoke("f5f879d2-88c9-4fdc-9726-0e597246bc11"); id == first {
		t.Fatal("terminal failure cannot recover")
	}
}
