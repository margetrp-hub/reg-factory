package nodeserver

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

const productionMaterialSchema = "team.work-production-material.v1"

func (h *Handler) continueProduction(c *gin.Context) {
	if c.GetHeader("Cookie") != "" || c.GetHeader("Origin") != "" {
		writeErr(c, perr("WORK_MACHINE_ONLY", 403))
		return
	}
	var doc Document
	if c.ShouldBindJSON(&doc) != nil {
		writeErr(c, perr("WORK_REQUEST_INVALID", 422))
		return
	}
	signed, err := signInventory(doc.Payload, h.config().Key, "work-production-continue-v1\n")
	if err != nil || !hmac.Equal([]byte(ls(signed["signature"])), []byte(doc.Signature)) {
		writeErr(c, perr("WORK_SIGNATURE_INVALID", 401))
		return
	}
	doc.Signature, _ = Sign(doc.Payload, h.config().Key, false)
	v, err := Verify(doc, h.config().Key, h.config().NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	if v.Action != productionAction || v.Params["mode"] != "produce" {
		writeErr(c, perr("PRODUCTION_CONTROL_INVALID", 422))
		return
	}
	id := nodeTicket(v)
	prior, err := h.st.ProductionAdmission(id)
	if err != nil || prior.RequestSHA != productionScope(v) {
		writeErr(c, perr("PRODUCTION_REQUEST_CHANGED", 409))
		return
	}
	if prior.State == "queued" || prior.State == "running" {
		h.writeAccepted(c, v)
		return
	}
	if err = h.st.ControlProduction(id, "continue"); err != nil {
		writeErr(c, perr("PRODUCTION_CONTINUE_REJECTED", 409))
		return
	}
	h.writeAccepted(c, v)
}

func (h *Handler) openProductionMaterial(envelope map[string]any, expectedSchema ...string) (map[string]any, error) {
	schema := productionMaterialSchema
	if len(expectedSchema) > 0 {
		schema = expectedSchema[0]
	}
	if len(envelope) != 6 || envelope["schema"] != schema || envelope["node_id"] != h.config().NodeID ||
		!uuidLike.MatchString(ls(envelope["grant_id"])) || !hex64Pattern.MatchString(ls(envelope["context_sha256"])) {
		return nil, perr("PRODUCTION_MATERIAL_SCOPE_INVALID", 422)
	}
	key, err := decodeFernetKey(h.config().ArtifactKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	box, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.StdEncoding.Strict().DecodeString(ls(envelope["nonce"]))
	if err != nil || len(nonce) != 12 {
		return nil, perr("PRODUCTION_MATERIAL_INVALID", 422)
	}
	sealed, err := base64.StdEncoding.Strict().DecodeString(ls(envelope["ciphertext"]))
	if err != nil || len(sealed) > 65536 {
		return nil, perr("PRODUCTION_MATERIAL_INVALID", 422)
	}
	scope := map[string]any{}
	for _, k := range []string{"schema", "node_id", "grant_id", "context_sha256"} {
		scope[k] = envelope[k]
	}
	aad, _ := Canonical(scope)
	raw, err := box.Open(nil, nonce, sealed, aad)
	if err != nil {
		for _, old := range h.artifactKeys() {
			key, keyErr := decodeFernetKey(old)
			if keyErr != nil {
				continue
			}
			block, _ := aes.NewCipher(key)
			previous, _ := cipher.NewGCM(block)
			raw, err = previous.Open(nil, nonce, sealed, aad)
			if err == nil {
				break
			}
		}
	}
	if err != nil {
		return nil, perr("PRODUCTION_MATERIAL_INVALID", 422)
	}
	var material map[string]any
	if json.Unmarshal(raw, &material) != nil {
		return nil, perr("PRODUCTION_MATERIAL_INVALID", 422)
	}
	for k, v := range scope {
		if material[k] != v {
			return nil, perr("PRODUCTION_MATERIAL_SCOPE_INVALID", 422)
		}
	}
	return material, nil
}

func (h *Handler) productionMaterial(c *gin.Context) {
	if c.GetHeader("Cookie") != "" || c.GetHeader("Origin") != "" {
		writeErr(c, perr("WORK_MACHINE_ONLY", 403))
		return
	}
	var doc Document
	if json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 100000)).Decode(&doc) != nil {
		writeErr(c, perr("PRODUCTION_MATERIAL_INVALID", 422))
		return
	}
	signed, err := signInventory(doc.Payload, h.config().Key, "work-node-material-relay-v1\n")
	if err != nil || !hmac.Equal([]byte(ls(signed["signature"])), []byte(doc.Signature)) {
		writeErr(c, perr("WORK_SIGNATURE_INVALID", 401))
		return
	}
	material, err := h.openProductionMaterial(doc.Payload)
	if err != nil {
		writeErr(c, err)
		return
	}
	ctx, _ := material["context"].(map[string]any)
	params, _ := material["parameters"].(map[string]any)
	if material["action"] != productionAction || !validProduction(params, ls(ctx["subject_id"])) || params["mode"] != "produce" ||
		productionDigest(ctx) != material["context_sha256"] || typeInt(material["expires_at"]) <= int(time.Now().Unix()) {
		writeErr(c, perr("PRODUCTION_MATERIAL_EXPIRED", 409))
		return
	}
	raw, _ := Canonical(doc.Payload)
	if err = h.st.SaveProductionMaterial(ls(material["context_sha256"]), string(raw), int64(typeInt(material["expires_at"]))); err != nil {
		writeErr(c, perr("PRODUCTION_MATERIAL_CONFLICT", 409))
		return
	}
	c.JSON(200, gin.H{"accepted": true, "grant_id": material["grant_id"], "context_sha256": material["context_sha256"]})
}

func (h *Handler) productionDriver(ctx context.Context, input map[string]any) map[string]any {
	path, err := h.st.ProductionDatabaseDSN()
	if err != nil {
		return map[string]any{"status": "unknown", "error_code": "PRODUCTION_STORAGE_UNAVAILABLE"}
	}
	input["root"], input["db_path"] = dataDir(), path
	// The Python boundary uses one Fernet encoding for both active and retained
	// keys. Passing raw Go key bytes here made production admission fail.
	encodedKeys := []string{}
	for _, key := range h.artifactKeys() {
		decoded, err := decodeFernetKey(key)
		if err != nil {
			return map[string]any{"status": "unknown", "error_code": "PRODUCTION_ARTIFACT_KEY_UNAVAILABLE"}
		}
		encodedKeys = append(encodedKeys, base64.URLEncoding.EncodeToString(decoded))
	}
	input["artifact_key"], input["artifact_keys"] = encodedKeys[0], encodedKeys[1:]
	// This is local driver input, not a signed assignment. Renewal reasons may
	// contain Unicode; the assignment-only ASCII encoder would silently drop it.
	raw, err := json.Marshal(input)
	if err != nil {
		return map[string]any{"status": "unknown", "error_code": "PRODUCTION_DRIVER_INPUT_INVALID"}
	}
	cfg := settings.Load()
	command := exec.CommandContext(ctx, cfg.Python, filepath.Join(cfg.AppRoot, "tools", "team_production.py"))
	command.Dir = cfg.AppRoot
	command.Env, err = settings.WorkerEnvironment(cfg.EnvFile)
	if err != nil {
		return map[string]any{"status": "unknown", "error_code": "PRODUCTION_CONFIG_UNAVAILABLE"}
	}
	command.Env, err = settings.BindProductionProxy(command.Env, cfg.AppRoot)
	if err != nil {
		return map[string]any{"status": "unknown", "error_code": "PRODUCTION_PROXY_NOT_CONFIGURED"}
	}
	command.Stdin = bytes.NewReader(raw)
	var buffer bytes.Buffer
	command.Stdout = &buffer
	if err = command.Start(); err != nil {
		return map[string]any{"status": "unknown", "error_code": "PRODUCTION_DRIVER_UNAVAILABLE"}
	}
	// The durable stop flag is checked between external steps. Never kill a
	// request which may already have invited a member or issued credentials.
	err = command.Wait()
	out := buffer.Bytes()
	result := map[string]any{}
	if err != nil || len(out) > 65536 || json.Unmarshal(out, &result) != nil {
		return map[string]any{"status": "unknown", "error_code": "PRODUCTION_DRIVER_UNAVAILABLE"}
	}
	return result
}

func (h *Handler) acceptNewProduction(c *gin.Context, v *Verified) {
	id := nodeTicket(v)
	if h.st.HasProductionRun(id) {
		h.writeAccepted(c, v)
		return
	}
	raw, err := h.st.ProductionMaterial(ls(v.Payload["command_sha256"]))
	if err != nil {
		writeErr(c, perr("PRODUCTION_MATERIAL_REQUIRED", 409))
		return
	}
	var envelope map[string]any
	_ = json.Unmarshal([]byte(raw), &envelope)
	material, err := h.openProductionMaterial(envelope)
	if err != nil {
		writeErr(c, err)
		return
	}
	ctx, _ := material["context"].(map[string]any)
	if productionDigest(material["parameters"]) != productionDigest(v.Params) || ctx["step_id"] != v.Payload["operation_id"] ||
		ctx["attempt_id"] != v.Payload["attempt_id"] || ctx["subject_id"] != v.Subject || typeInt(ctx["fence"]) != int(v.Fence) ||
		typeInt(material["expires_at"]) <= int(time.Now().Unix()) {
		writeErr(c, perr("PRODUCTION_MATERIAL_SCOPE_CHANGED", 409))
		return
	}
	ctxRun, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	result := h.productionDriver(ctxRun, map[string]any{"action": "admit", "operation_id": id, "material": material})
	if result["status"] != "queued" {
		_ = h.st.RejectProductionAdmission(id, "unknown", ls(result["error_code"]))
		writeErr(c, perr("PRODUCTION_ADMISSION_DRIVER_UNAVAILABLE", 503))
		return
	}
	h.writeAccepted(c, v)
}

// Durable admissions survive a restart. The Python process uses a per-operation
// OS lock and writes every external-start marker before the network request.
func (h *Handler) StartProduction(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ids, err := h.st.PendingProductions(time.Now().Unix())
				if err != nil {
					continue
				}
				for _, id := range ids {
					h.productionMu.Lock()
					if h.productionActive == nil {
						h.productionActive = map[string]chan struct{}{}
					}
					if h.productionActive[id] != nil || len(h.productionActive) >= 2 {
						h.productionMu.Unlock()
						continue
					}
					done := make(chan struct{})
					h.productionActive[id] = done
					h.productionMu.Unlock()
					go func(id string, done chan struct{}) {
						h := h.snapshot()
						defer func() { h.productionMu.Lock(); delete(h.productionActive, id); close(done); h.productionMu.Unlock() }()
						runCtx, cancel := context.WithTimeout(ctx, 9*time.Minute)
						result := h.productionDriver(runCtx, map[string]any{"action": "advance", "operation_id": id})
						cancel()
						if result["status"] != "ready" {
							if result["error_code"] == "PRODUCTION_DRIVER_UNAVAILABLE" {
								_ = h.st.DeferProductionRun(id, "PRODUCTION_DRIVER_UNAVAILABLE")
							}
							return
						}
						doc, err := h.productionDocument(ls(result["artifact"]))
						if err != nil {
							_ = h.st.DeferProductionRun(id, "PRODUCTION_ARTIFACT_UNAVAILABLE")
							return
						}
						meta, err := productionMetadata(doc)
						if err != nil {
							_ = h.st.DeferProductionRun(id, "PRODUCTION_OUTPUT_METADATA_INVALID")
							return
						}
						admission, err := h.st.ProductionAdmission(id)
						if err != nil {
							return
						}
						p := store.Production{OperationID: id, RequestSHA: admission.RequestSHA, Parameters: admission.Parameters,
							SourceKey: ls(meta["workspace_id"]) + ":" + ls(meta["email"]), SourceRef: ls(result["source_ref"]), SourceSHA: ls(result["source_sha256"]),
							AccountRef: "production:" + id, Email: ls(meta["email"]), Artifact: ls(result["artifact"])}
						// Retain the exact issued version in the ordinary SG result picker.
						if err = assetstore.New(dataDir()).Put(assetstore.Asset{Ref: p.AccountRef, Platform: "chatgpt", Kind: "team", Email: p.Email,
							Source: "production.team_json", CreatedAt: time.Unix(int64(typeInt(result["created_at"])), 0).UTC().Format(time.RFC3339), Payload: doc}); err != nil {
							_ = h.st.DeferProductionRun(id, "PRODUCTION_RESULT_EXPORT_FAILED")
							return
						}
						if err = h.st.AdmitProduction(p); err == nil {
							_ = h.st.CompleteProductionRun(id)
						} else {
							_ = h.st.DeferProductionRun(id, "PRODUCTION_RESULT_STORE_FAILED")
						}
					}(id, done)
				}
			}
		}
	}()
}

func (h *Handler) stopProduction(c *gin.Context) {
	if c.GetHeader("Cookie") != "" || c.GetHeader("Origin") != "" {
		writeErr(c, perr("WORK_MACHINE_ONLY", 403))
		return
	}
	var doc Document
	if c.ShouldBindJSON(&doc) != nil {
		writeErr(c, perr("WORK_REQUEST_INVALID", 422))
		return
	}
	signed, err := signInventory(doc.Payload, h.config().Key, "work-node-stop-v1\n")
	if err != nil || !hmac.Equal([]byte(ls(signed["signature"])), []byte(doc.Signature)) {
		writeErr(c, perr("WORK_SIGNATURE_INVALID", 401))
		return
	}
	doc.Signature, _ = Sign(doc.Payload, h.config().Key, false)
	v, err := Verify(doc, h.config().Key, h.config().NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	if v.Action != productionAction {
		writeErr(c, perr("PRODUCTION_STOP_SCOPE_INVALID", 422))
		return
	}
	id := nodeTicket(v)
	params, _ := Canonical(v.Params)
	if err = h.st.ReserveProductionAdmission(id, productionScope(v), string(params)); err != nil {
		writeErr(c, perr("PRODUCTION_STOP_CONFLICT", 409))
		return
	}
	if err = h.bindExecutionEvents(v, "work-"+id); err != nil {
		writeErr(c, err)
		return
	}
	if err = h.st.StopProduction(id, productionScope(v), time.Now().Unix()); err != nil {
		writeErr(c, perr("PRODUCTION_STOP_CONFLICT", 409))
		return
	}
	h.productionMu.Lock()
	done := h.productionActive[id]
	h.productionMu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			writeErr(c, perr("PRODUCTION_STOP_PENDING", 503))
			return
		}
	}
	if h.st.HasProductionRun(id) {
		_, resultErr := h.st.Production(id)
		effect, _ := h.st.ProductionStopEffect(id)
		if resultErr != nil && effect == "" {
			writeErr(c, perr("PRODUCTION_STOP_PENDING", 503))
			return
		}
	}
	payload := map[string]any{"schema": "work.stop.v1", "stopped": true, "external_outcome": "preserved", "node_task_id": id, "stopped_at": time.Now().Unix()}
	for _, k := range []string{"issuer", "node_id", "operation_id", "task_id", "attempt_id", "fence", "subject_id", "resource_version", "command_sha256"} {
		payload[k] = v.Payload[k]
	}
	out, err := signInventory(payload, h.receiptKey(), "work-node-stop-v1\n")
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(200, out)
}
