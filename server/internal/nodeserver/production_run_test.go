package nodeserver

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestProductionMaterialIsBoundAndReplayable(t *testing.T) {
	key := bytes.Repeat([]byte("s"), 32)
	db, err := store.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := New(Config{Enabled: true, NodeID: "sg", Key: key, ReceiptKey: key, BusinessKey: key, ArtifactKey: key}, nil, db)
	router := gin.New()
	h.Register(router)
	ctx := map[string]any{"step_id": SubjectID("step"), "subject_id": SubjectID("slot"), "attempt_id": SubjectID("attempt"), "fence": 1}
	params := map[string]any{"production_ref": ctx["subject_id"], "order_ref": SubjectID("order"), "product_id": 34, "seat_type": "standard", "workspace_ids": []any{SubjectID("workspace")}, "mode": "produce", "source_ref": "", "source_sha256": "", "budget_units": 0}
	scope := map[string]any{"schema": productionMaterialSchema, "node_id": "sg", "grant_id": SubjectID("grant"), "context_sha256": productionDigest(ctx)}
	material := map[string]any{}
	for k, v := range scope {
		material[k] = v
	}
	material["context"], material["parameters"], material["action"], material["expires_at"] = ctx, params, productionAction, time.Now().Unix()+120
	block, _ := aes.NewCipher(key)
	box, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{1}, 12)
	aad, _ := Canonical(scope)
	raw, _ := Canonical(material)
	envelope := map[string]any{}
	for k, v := range scope {
		envelope[k] = v
	}
	envelope["nonce"] = base64.StdEncoding.EncodeToString(nonce)
	envelope["ciphertext"] = base64.StdEncoding.EncodeToString(box.Seal(nil, nonce, raw, aad))
	post := func(doc any) int {
		body, _ := json.Marshal(doc)
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/internal/work/v1/team/materials", bytes.NewReader(body))
		router.ServeHTTP(w, r)
		return w.Code
	}
	doc, _ := signInventory(envelope, key, "work-node-material-relay-v1\n")
	for i := 0; i < 2; i++ {
		if code := post(doc); code != 200 {
			t.Fatal("original material", code)
		}
	}
	foreign := map[string]any{}
	for k, v := range envelope {
		foreign[k] = v
	}
	foreign["context_sha256"] = productionDigest("foreign")
	bad, _ := signInventory(foreign, key, "work-node-material-relay-v1\n")
	if post(bad) < 400 {
		t.Fatal("foreign material accepted")
	}
	read, _ := signInventory(envelope, key, domainRead)
	if post(read) != 401 {
		t.Fatal("read signature accepted")
	}
}

func TestProductionStopBeforeAdmissionSurvivesReplayAndRejectsLateSubmission(t *testing.T) {
	key := bytes.Repeat([]byte("s"), 32)
	db, err := store.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := New(Config{Enabled: true, NodeID: "sg", Key: key, ReceiptKey: key, BusinessKey: key, ArtifactKey: key}, nil, db)
	router := gin.New()
	h.Register(router)
	id, slot := SubjectID("stopped-operation"), SubjectID("stopped-slot")
	params := map[string]any{"production_ref": slot, "order_ref": SubjectID("order"), "product_id": 34, "seat_type": "standard", "workspace_ids": []any{}, "mode": "produce", "source_ref": "", "source_sha256": "", "budget_units": 0}
	p := map[string]any{"schema": WorkSchema, "issuer": "work", "node_id": "sg", "operation_id": id, "task_id": id, "attempt_id": SubjectID("attempt"), "fence": 1, "action": productionAction, "subject_kind": "production", "subject_id": slot, "resource_version": 1, "issued_at": time.Now().Unix(), "expires_at": time.Now().Unix() + 120, "parameters": params, "command_sha256": productionDigest(params)}
	post := func(path string, doc any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(doc)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", path, bytes.NewReader(raw)))
		return w
	}
	stop, _ := signInventory(p, key, "work-node-stop-v1\n")
	for i := 0; i < 2; i++ {
		if r := post("/internal/work/v1/stop", stop); r.Code != 200 {
			t.Fatal(r.Code, r.Body.String())
		}
	}
	sig, _ := Sign(p, key, false)
	if r := post("/internal/work/v1/tasks", Document{Payload: p, Signature: sig}); r.Code != 409 {
		t.Fatal("late production admitted", r.Code)
	}
	sig, _ = Sign(p, key, true)
	read := post("/internal/work/v1/resolve", Document{Payload: p, Signature: sig})
	if read.Code != 200 || !bytes.Contains(read.Body.Bytes(), []byte(`"stop_effect":"none"`)) {
		t.Fatal("no original cancellation proof", read.Body.String())
	}
	if db.HasProductionRun(id) {
		t.Fatal("stopped task selected an account")
	}
}

func TestProductionStopKeepsFailedArtifactAndReturnsSettlementEvidence(t *testing.T) {
	key := bytes.Repeat([]byte("s"), 32)
	db, err := store.Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.EnsureProduction(); err != nil {
		t.Fatal(err)
	}
	h := New(Config{Enabled: true, NodeID: "sg", Key: key, ReceiptKey: key, ArtifactKey: key}, nil, db)
	id, slot := SubjectID("failed-output"), SubjectID("slot")
	params := map[string]any{"production_ref": slot, "mode": "produce"}
	payload := map[string]any{"operation_id": id, "task_id": id, "subject_id": slot, "parameters": params}
	v := &Verified{Payload: payload, Params: params, TaskID: id, Subject: slot, Action: productionAction}
	p := store.Production{OperationID: id, RequestSHA: productionScope(v), Parameters: `{}`, SourceKey: "source", SourceSHA: "digest", AccountRef: "production:" + id, Email: "child@example.test", Artifact: "original-sealed-output"}
	if err = db.ReserveProductionAdmission(id, p.RequestSHA, p.Parameters); err != nil {
		t.Fatal(err)
	}
	if err = db.AdmitProduction(p); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ClaimProductionCheck(id, 100); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = db.FinishProduction(id, "failed", `{"status":"failed","error_code":"PRODUCTION_CREDENTIAL_TOKEN_REVOKED"}`, 100); err != nil {
		t.Fatal(err)
	}
	if err = db.StopProduction(id, p.RequestSHA, 101); err != nil {
		t.Fatal(err)
	}
	receipt, err := h.productionReceipt(v, key, time.Unix(102, 0))
	if err != nil {
		t.Fatal(err)
	}
	result := receipt["payload"].(map[string]any)
	if result["status"] != "cancelled" || result["artifact_available"] != false {
		t.Fatal("failed production cannot settle its stop", result)
	}
	original, _ := db.Production(id)
	if original.State != "failed" || original.Artifact != p.Artifact {
		t.Fatal("stop replaced original failed result")
	}
}
