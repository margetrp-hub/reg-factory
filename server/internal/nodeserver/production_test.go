package nodeserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestProductionRetainsOriginalAcrossUnknownLostAckAndChangedSource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	root := t.TempDir()
	t.Setenv("REG_FACTORY_DATA_DIR", root)
	db, err := store.Open(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := bytes.Repeat([]byte("k"), 32)
	h := New(Config{Enabled: true, NodeID: "sg", Key: key, ReceiptKey: key, BusinessKey: key}, nil, db)
	router := gin.New()
	h.Register(router)
	workspace := SubjectID("workspace")
	email := "child@example.test"
	claims, _ := json.Marshal(map[string]any{"exp": time.Now().Unix() + 86400, "https://api.openai.com/auth": map[string]any{
		"chatgpt_account_id": workspace, "chatgpt_plan_type": "self_serve_business_prolite"}, "https://api.openai.com/profile": map[string]any{"email": email}})
	token := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".synthetic"
	document := map[string]any{"email": email, "access_token": token, "id_token": token, "refresh_token": "synthetic-retained"}
	raw, _ := json.Marshal(document)
	dir := filepath.Join(root, "plus-import", "team-json")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "team-test.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, source, err := readProductionSource("team-test.json")
	if err != nil {
		t.Fatal(err)
	}
	id, slot := SubjectID("operation"), SubjectID("slot")
	stamp := time.Now()
	params := map[string]any{"production_ref": slot, "order_ref": SubjectID("order"), "product_id": 33,
		"seat_type": "premium", "workspace_ids": []any{workspace}, "mode": "existing_result",
		"source_ref": source["source_ref"], "source_sha256": source["source_sha256"], "budget_units": 0}
	payload := map[string]any{"schema": WorkSchema, "issuer": "work", "node_id": "sg", "operation_id": id, "task_id": id,
		"attempt_id": SubjectID("attempt"), "fence": 1, "action": productionAction, "subject_kind": "production", "subject_id": slot,
		"resource_version": 1, "issued_at": stamp.Unix(), "expires_at": stamp.Unix() + 120, "parameters": params, "command_sha256": productionDigest(params)}
	postDocument := func(path string, document any) *httptest.ResponseRecorder {
		body, _ := json.Marshal(document)
		req := httptest.NewRequest("POST", path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}
	post := func(payload map[string]any) int {
		sig, _ := Sign(payload, key, false)
		return postDocument("/internal/work/v1/tasks", Document{Payload: payload, Signature: sig}).Code
	}
	ready := httptest.NewRecorder()
	router.ServeHTTP(ready, httptest.NewRequest("GET", "/ready", nil))
	var readiness map[string]any
	_ = json.Unmarshal(ready.Body.Bytes(), &readiness)
	for _, action := range readiness["runtime"].(map[string]any)["supported_actions"].([]any) {
		if action == productionAction {
			t.Fatal("advertised unusable production")
		}
	}
	if code := post(payload); code != 503 {
		t.Fatalf("expected missing config, got %d", code)
	}
	readSig, _ := Sign(payload, key, true)
	readDoc := Document{Payload: payload, Signature: readSig}
	rejected := postDocument("/internal/work/v1/resolve", readDoc)
	if rejected.Code != 200 || !bytes.Contains(rejected.Body.Bytes(), []byte("PRODUCTION_ADMISSION_ARTIFACT_KEY_UNCONFIGURED")) {
		t.Fatal("first failure disappeared behind 404", rejected.Body.String())
	}
	if resp := postDocument("/internal/work/v1/production/resume", readDoc); resp.Code != 401 {
		t.Fatal("read signature authorized mutation", resp.Code)
	}
	h.cfg.ArtifactKey = key
	for _, invalid := range []string{"expired", "produce", "budget", "scope"} {
		encoded, _ := json.Marshal(payload)
		var changed map[string]any
		_ = json.Unmarshal(encoded, &changed)
		p := changed["parameters"].(map[string]any)
		switch invalid {
		case "expired":
			changed["expires_at"] = stamp.Unix() - 1
		case "produce":
			p["mode"] = "produce"
		case "budget":
			p["budget_units"] = 1
		case "scope":
			p["source_sha256"] = productionDigest("different")
		}
		bad, _ := signInventory(changed, key, productionResumeDomain)
		if resp := postDocument("/internal/work/v1/production/resume", bad); resp.Code < 400 {
			t.Fatal("unsafe recovery accepted", invalid)
		}
	}
	resume, _ := signInventory(payload, key, productionResumeDomain)
	if resp := postDocument("/internal/work/v1/production/resume", resume); resp.Code != 200 {
		t.Fatal("original admission did not recover", resp.Body.String())
	}
	if code := post(payload); code != 200 {
		t.Fatalf("admit %d", code)
	}
	v := &Verified{Payload: payload, Params: params, Subject: slot, Action: productionAction, TaskID: id}
	calls := 0
	h.productionCheck = func(doc map[string]any) map[string]any {
		calls++
		if doc["refresh_token"] != "synthetic-retained" {
			t.Fatal("read mutable source")
		}
		if calls == 1 {
			return map[string]any{"status": "unknown", "error_code": "PRODUCTION_CHECK_UNAVAILABLE"}
		}
		return map[string]any{"status": "completed", "error_code": "", "valid_until": stamp.Unix() + 86400}
	}
	first, err := h.productionReceipt(v, key, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if first["payload"].(map[string]any)["status"] != "unknown" {
		t.Fatal(first)
	}
	if _, err := db.RecoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if task, err := db.GetTask("work-" + id); err != nil || task.Status != "unknown" {
		t.Fatal("restart lost unknown result", err)
	}
	// Lost submission ACK and changed file must preserve the frozen identity.
	if err = os.WriteFile(path, []byte(`{"refresh_token":"replaced"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code := post(payload); code != 200 {
		t.Fatalf("lost ACK %d", code)
	}
	completed, err := h.productionReceipt(v, key, stamp.Add(31*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	done := completed["payload"].(map[string]any)
	if done["status"] != "completed" || done["result_sha256"] != productionDigest(done["result"]) {
		t.Fatal(done)
	}
	replay, err := h.productionReceipt(v, key, stamp.Add(61*time.Second))
	if err != nil || productionDigest(replay) != productionDigest(completed) || calls != 2 {
		t.Fatalf("immutable replay failed: %v", err)
	}
	events, err := db.TailEvents("work-"+id, 100)
	if err != nil || len(events) < 5 {
		t.Fatal("missing execution stages", err)
	}
	for _, event := range events {
		if bytes.Contains([]byte(event.Line), []byte("synthetic-retained")) {
			t.Fatal("credential leaked to logs")
		}
	}
	foundAdmission := false
	for _, event := range events {
		foundAdmission = foundAdmission || bytes.Contains([]byte(event.Line), []byte("PRODUCTION_ADMISSION_ARTIFACT_KEY_UNCONFIGURED"))
	}
	if !foundAdmission {
		t.Fatal("recovery erased original failure")
	}
	accountRef, found := h.inventoryAccountRef(ls(done["result_account_id"]))
	if !found {
		t.Fatal("output missing")
	}
	frozen, err := h.accountDocument(accountRef)
	if err != nil || productionDigest(frozen) != productionDigest(document) {
		t.Fatal("custody did not use retained output", err)
	}
	// Another task may not claim the same source under a different filename.
	if err = os.WriteFile(filepath.Join(dir, "duplicate.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	_, other, _ := readProductionSource("duplicate.json")
	params["source_ref"], params["source_sha256"] = "duplicate.json", other["source_sha256"]
	payload["operation_id"], payload["task_id"] = SubjectID("duplicate"), SubjectID("duplicate")
	if code := post(payload); code != 409 {
		t.Fatalf("duplicated source admitted: %d", code)
	}
}

func TestProductionSourceRejectsPathsAndInconsistentIdentity(t *testing.T) {
	t.Setenv("REG_FACTORY_DATA_DIR", t.TempDir())
	for _, ref := range []string{"../secret.json", "/tmp/secret.json", "x\\secret.json"} {
		if _, _, err := readProductionSource(ref); err == nil {
			t.Fatal("accepted unsafe source")
		}
	}
	if _, err := productionMetadata(map[string]any{"email": "x@example.test", "access_token": "invalid", "refresh_token": "synthetic"}); err == nil {
		t.Fatal("accepted unparseable identity")
	}
}
