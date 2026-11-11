package nodeserver

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestProductionReadbackHTTPRejectsChangedAuthorityAndPreservesStop(t *testing.T) {
	testProductionReadbackHTTP(t, false)
}

// Exercises the real Go HTTP -> Python subprocess -> persisted renewal path.
// The production driver uses flock and therefore runs in Linux acceptance.
func TestProductionReadbackPythonDriver(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production driver requires Linux flock")
	}
	t.Chdir("../../..")
	testProductionReadbackHTTP(t, true)
}

func testProductionReadbackHTTP(t *testing.T, driver bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	key := bytes.Repeat([]byte("r"), 32)
	root := t.TempDir()
	t.Setenv("REG_FACTORY_DATA_DIR", root)
	t.Setenv("REG_FACTORY_ENV_FILE", filepath.Join(root, ".env"))
	db, err := store.Open(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := New(Config{Enabled: true, NodeID: "sg", Key: key, ReceiptKey: key, ArtifactKey: key}, nil, db)
	router := gin.New()
	h.Register(router)
	now := time.Now().Unix()
	id, slot := SubjectID("renew-original"), SubjectID("renew-slot")
	ctx := map[string]any{"step_id": id, "attempt_id": SubjectID("renew-attempt"), "subject_id": slot, "fence": 1}
	params := map[string]any{"production_ref": slot, "order_ref": SubjectID("order"), "product_id": 34, "seat_type": "standard", "workspace_ids": []any{SubjectID("workspace")}, "mode": "produce", "source_ref": "", "source_sha256": "", "budget_units": 0}
	assignment := map[string]any{"schema": WorkSchema, "issuer": "work", "node_id": "sg", "operation_id": id, "task_id": id,
		"attempt_id": ctx["attempt_id"], "fence": 1, "action": productionAction, "subject_kind": "production", "subject_id": slot,
		"resource_version": 1, "issued_at": now, "expires_at": now + 120, "parameters": params, "command_sha256": productionDigest(ctx)}
	sig, _ := Sign(assignment, key, false)
	v, err := Verify(Document{Payload: assignment, Signature: sig}, key, "sg", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReserveProductionAdmission(id, productionScope(v), "{}"); err != nil {
		t.Fatal(err)
	}
	scope := map[string]any{"schema": "team.work-production-readback.v1", "node_id": "sg", "grant_id": SubjectID("grant"), "context_sha256": productionDigest(ctx)}
	material := map[string]any{}
	for k, value := range scope {
		material[k] = value
	}
	material["context"], material["parameters"], material["action"] = ctx, params, productionAction
	material["expires_at"], material["readback_until"] = now+600, now+3600
	material["workspace"] = map[string]any{"account_id": SubjectID("workspace"), "access_token": "synthetic"}
	material["renewal"] = map[string]any{"id": SubjectID("renewal"), "actor_id": 1, "reason": "核验原成品", "issued_at": now}
	seal := func() map[string]any {
		block, _ := aes.NewCipher(key)
		box, _ := cipher.NewGCM(block)
		nonce := bytes.Repeat([]byte{1}, 12)
		aad, _ := Canonical(scope)
		raw, err := json.Marshal(material)
		if err != nil {
			t.Fatal(err)
		}
		envelope := map[string]any{}
		for k, value := range scope {
			envelope[k] = value
		}
		envelope["nonce"] = base64.StdEncoding.EncodeToString(nonce)
		envelope["ciphertext"] = base64.StdEncoding.EncodeToString(box.Seal(nil, nonce, raw, aad))
		return envelope
	}
	post := func(domain string) *httptest.ResponseRecorder {
		doc, _ := signInventory(map[string]any{"assignment": assignment, "envelope": seal()}, key, domain)
		body, _ := json.Marshal(doc)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, httptest.NewRequest("POST", "/internal/work/v1/production/readback", bytes.NewReader(body)))
		return resp
	}
	const domain = "work-production-readback-v1\n"
	if r := post(domainRead); r.Code != 401 {
		t.Fatal(r.Code, r.Body.String())
	}
	material["expires_at"] = now - 1
	if r := post(domain); r.Code != 403 {
		t.Fatal(r.Code, r.Body.String())
	}
	material["expires_at"] = now + 600
	assignment["command_sha256"] = productionDigest("changed")
	if r := post(domain); r.Code != 403 {
		t.Fatal(r.Code, r.Body.String())
	}
	assignment["command_sha256"] = productionDigest(ctx)
	if db.ProductionStopped(id) {
		t.Fatal("rejected authority changed original task")
	}
	if driver {
		original := map[string]any{}
		for k, value := range material {
			original[k] = value
		}
		original["schema"] = productionMaterialSchema
		original["readback_until"] = now - 1
		delete(original, "renewal")
		result := h.productionDriver(context.Background(), map[string]any{"action": "admit", "operation_id": id, "material": original})
		if result["status"] != "queued" {
			t.Fatal("real driver admission", result)
		}
		for i := 0; i < 2; i++ {
			if r := post(domain); r.Code != 200 {
				t.Fatal("real driver renewal", r.Code, r.Body.String())
			}
		}
		// The Python driver checks the entire saved renewal on replay, including
		// the Unicode reason. A changed reason cannot silently replace it.
		material["renewal"].(map[string]any)["reason"] = "不同的核验原因"
		if r := post(domain); r.Code != 503 {
			t.Fatal("changed renewal reached the saved original", r.Code, r.Body.String())
		}
		material["renewal"].(map[string]any)["reason"] = "核验原成品"
		if r := post(domain); r.Code != 200 {
			t.Fatal("original renewal did not recover", r.Code, r.Body.String())
		}
		if !db.ProductionStopped(id) || !db.HasProductionRun(id) {
			t.Fatal("driver lost original stop or run")
		}
		if err := db.ControlProduction(id, "continue"); err == nil {
			t.Fatal("readback renewal restarted execution")
		}
		return
	}
	// An incomplete run cannot acknowledge renewal or create a replacement.
	r := post(domain)
	if r.Code != 503 || !bytes.Contains(r.Body.Bytes(), []byte("PRODUCTION_STOP_PENDING")) {
		t.Fatal(r.Code, r.Body.String())
	}
	// Stop confirms that admission never started. Replays acknowledge that same
	// terminal record without trying to select an account or execute Python.
	for i := 0; i < 2; i++ {
		if r := post(domain); r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	if !db.ProductionStopped(id) || db.HasProductionRun(id) {
		t.Fatal("original stop or source identity lost")
	}
	if err := db.ControlProduction(id, "continue"); err == nil {
		t.Fatal("stopped work resumed")
	}
}
