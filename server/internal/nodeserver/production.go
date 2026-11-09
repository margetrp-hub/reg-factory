package nodeserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

const productionAction = "production.team_json"
const productionResumeDomain = "work-production-existing-resume-v1\n"
const productionResumeProtocol = "work.production-existing-resume.v1"
const productionAdmitDomain = "work-production-admit-v1\n"
const productionAdmitProtocol = "work.production-admit.v1"

func (h *Handler) productionReadiness() string {
	if _, err := decodeFernetKey(h.cfg.ArtifactKey); err != nil {
		return "PRODUCTION_ADMISSION_ARTIFACT_KEY_UNCONFIGURED"
	}
	if len(h.cfg.BusinessKey) < minKeyLength || len(h.cfg.ReceiptKey) < minKeyLength {
		return "PRODUCTION_ADMISSION_KEYS_UNCONFIGURED"
	}
	return ""
}

// Recovery is an explicit mutation, never a read-domain side effect. Its fresh
// signature authorizes only the same frozen existing output with zero spending.
func (h *Handler) resumeProduction(c *gin.Context) {
	if c.GetHeader("Cookie") != "" || c.GetHeader("Origin") != "" || c.GetHeader("Sec-Fetch-Site") != "" {
		writeErr(c, perr("WORK_MACHINE_ONLY", 403))
		return
	}
	var doc Document
	if c.ShouldBindJSON(&doc) != nil {
		writeErr(c, perr("WORK_REQUEST_INVALID", 400))
		return
	}
	domain, mode := productionResumeDomain, "existing_result"
	if c.Request.URL.Path == "/internal/work/v1/production/admit" {
		domain, mode = productionAdmitDomain, "produce"
	}
	expected, err := signInventory(doc.Payload, h.cfg.Key, domain)
	if err != nil || !hmac.Equal([]byte(ls(expected["signature"])), []byte(doc.Signature)) {
		writeErr(c, perr("WORK_SIGNATURE_INVALID", 401))
		return
	}
	// Reuse the full mutation validator after checking the dedicated domain.
	doc.Signature, _ = Sign(doc.Payload, h.cfg.Key, false)
	v, err := Verify(doc, h.cfg.Key, h.cfg.NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	if v.Action != productionAction || !validProduction(v.Params, v.Subject) || v.Params["mode"] != mode {
		writeErr(c, perr("PRODUCTION_RESUME_FORBIDDEN", 422))
		return
	}
	h.acceptProduction(c, v)
	if c.Writer.Status() == 200 && mode == "existing_result" {
		_ = h.st.Stage("work-"+nodeTicket(v), store.StageEvent{Stage: "recovery", Status: "queued", ResultSHA: ls(v.Params["source_sha256"]), Action: "沿原任务接收同一结果；未重新生产或登录"})
	}
}

func productionDigest(v any) string {
	raw, _ := Canonical(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func productionScope(v *Verified) string {
	stable := map[string]any{}
	for _, key := range workFields {
		if key != "issued_at" && key != "expires_at" {
			stable[key] = v.Payload[key]
		}
	}
	return productionDigest(stable)
}

func productionMetadata(document map[string]any) (map[string]any, error) {
	token := ls(document["access_token"])
	parts := strings.Split(token, ".")
	if len(parts) != 3 || ls(document["refresh_token"]) == "" {
		return nil, perr("PRODUCTION_DOCUMENT_INVALID", 422)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, perr("PRODUCTION_DOCUMENT_INVALID", 422)
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil, perr("PRODUCTION_DOCUMENT_INVALID", 422)
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
	email := strings.ToLower(strings.TrimSpace(ls(document["email"])))
	workspace := ls(auth["chatgpt_account_id"])
	seat := map[string]string{"team": "standard", "business": "standard", "self_serve_business": "standard",
		"self_serve_business_prolite": "premium", "team_prolite": "premium", "business_prolite": "premium"}[ls(auth["chatgpt_plan_type"])]
	if !uuidLike.MatchString(workspace) || seat == "" || !strings.Contains(email, "@") ||
		ls(profile["email"]) != "" && !strings.EqualFold(ls(profile["email"]), email) {
		return nil, perr("PRODUCTION_DOCUMENT_IDENTITY_INVALID", 422)
	}
	for _, key := range []string{"account_id", "workspace_account_id", "chatgpt_account_id"} {
		if ls(document[key]) != "" && document[key] != workspace {
			return nil, perr("PRODUCTION_DOCUMENT_IDENTITY_INVALID", 422)
		}
	}
	return map[string]any{"email": email, "workspace_id": workspace, "seat_type": seat}, nil
}

func readProductionSource(name string) (map[string]any, map[string]any, error) {
	if !refPattern.MatchString(name) || filepath.Base(name) != name || !strings.HasSuffix(name, ".json") {
		return nil, nil, perr("PRODUCTION_SOURCE_INVALID", 422)
	}
	path := filepath.Join(dataDir(), "plus-import", "team-json", name)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
		return nil, nil, perr("PRODUCTION_SOURCE_UNAVAILABLE", 409)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, perr("PRODUCTION_SOURCE_UNAVAILABLE", 409)
	}
	var document map[string]any
	if json.Unmarshal(raw, &document) != nil {
		return nil, nil, perr("PRODUCTION_DOCUMENT_INVALID", 422)
	}
	metadata, err := productionMetadata(document)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(raw)
	metadata["source_ref"], metadata["source_sha256"] = name, hex.EncodeToString(sum[:])
	return document, metadata, nil
}

func validProduction(p map[string]any, subject string) bool {
	fields := workActions[productionAction]
	if len(p) != len(fields) || p["production_ref"] != subject || !uuidLike.MatchString(subject) ||
		!uuidLike.MatchString(ls(p["order_ref"])) || !productionInteger(p["product_id"]) || typeInt(p["product_id"]) < 1 ||
		(p["seat_type"] != "standard" && p["seat_type"] != "premium") || !productionInteger(p["budget_units"]) || typeInt(p["budget_units"]) < 0 {
		return false
	}
	workspaces, ok := p["workspace_ids"].([]any)
	if !ok || len(workspaces) > 100 {
		return false
	}
	seen := map[string]bool{}
	for _, id := range workspaces {
		if !uuidLike.MatchString(ls(id)) || seen[ls(id)] {
			return false
		}
		seen[ls(id)] = true
	}
	if p["mode"] == "produce" {
		return p["source_ref"] == "" && p["source_sha256"] == ""
	}
	return p["mode"] == "existing_result" && refPattern.MatchString(ls(p["source_ref"])) && hex64Pattern.MatchString(ls(p["source_sha256"])) && typeInt(p["budget_units"]) == 0
}

func productionInteger(value any) bool {
	switch n := value.(type) {
	case int, int64:
		return true
	case float64:
		return !math.IsInf(n, 0) && !math.IsNaN(n) && n == math.Trunc(n) && math.Abs(n) <= 9007199254740991
	default:
		return false
	}
}

func (h *Handler) productionSources(request map[string]any, data map[string]any) (map[string]any, error) {
	if len(data) != 0 {
		return nil, perr("PRODUCTION_REQUEST_INVALID", 422)
	}
	entries, err := os.ReadDir(filepath.Join(dataDir(), "plus-import", "team-json"))
	if os.IsNotExist(err) {
		entries = nil
	} else if err != nil {
		return nil, perr("PRODUCTION_SOURCE_UNAVAILABLE", 503)
	}
	items := []map[string]any{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		_, metadata, err := readProductionSource(entry.Name())
		if err != nil {
			continue
		}
		claimed, err := h.st.ProductionClaimed(ls(metadata["workspace_id"])+":"+ls(metadata["email"]), ls(metadata["source_sha256"]))
		if err != nil {
			return nil, perr("PRODUCTION_STORAGE_UNAVAILABLE", 503)
		}
		if claimed {
			continue
		}
		items = append(items, metadata)
		if len(items) >= 500 {
			break
		}
	}
	stamp := time.Now().Unix()
	return signInventory(map[string]any{"schema": "work.production-sources.v1", "node_id": h.cfg.NodeID,
		"request_id": request["request_id"], "issued_at": stamp, "expires_at": stamp + 120, "items": items}, h.receiptKey(), domainWork)
}

func (h *Handler) acceptProduction(c *gin.Context, v *Verified) {
	if !validProduction(v.Params, v.Subject) {
		writeErr(c, perr("PRODUCTION_PARAMETERS_INVALID", 422))
		return
	}
	id := nodeTicket(v)
	if h.st.ProductionStopped(id) {
		writeErr(c, perr("PRODUCTION_ADMIN_STOPPED", 409))
		return
	}
	if old, err := h.st.Production(id); err == nil {
		if old.RequestSHA != productionScope(v) {
			writeErr(c, perr("PRODUCTION_REQUEST_CHANGED", 409))
			return
		}
		h.writeAccepted(c, v)
		return
	} else if err != sql.ErrNoRows {
		writeErr(c, perr("PRODUCTION_STORAGE_UNAVAILABLE", 503))
		return
	}
	params, _ := Canonical(v.Params)
	if err := h.st.ReserveProductionAdmission(id, productionScope(v), string(params)); err != nil {
		writeErr(c, perr("PRODUCTION_ADMISSION_SCOPE_CONFLICT", 409))
		return
	}
	if admission, err := h.st.ProductionAdmission(id); err != nil || admission.State == "failed" {
		writeErr(c, perr("PRODUCTION_ADMISSION_TERMINAL", 409))
		return
	}
	reject := func(err error) {
		code, state := "PRODUCTION_ADMISSION_STORAGE_UNAVAILABLE", "unknown"
		if failure, ok := err.(*ProtocolError); ok {
			code = failure.Code
			if failure.Status < 500 {
				state = "failed"
			}
		}
		if saveErr := h.st.RejectProductionAdmission(id, state, code); saveErr != nil {
			writeErr(c, perr("PRODUCTION_STORAGE_UNAVAILABLE", 503))
			return
		}
		writeErr(c, err)
	}
	if code := h.productionReadiness(); code != "" {
		reject(perr(code, 503))
		return
	}
	if v.Params["mode"] == "produce" {
		h.acceptNewProduction(c, v)
		return
	}
	document, metadata, err := readProductionSource(ls(v.Params["source_ref"]))
	if err != nil {
		reject(err)
		return
	}
	if metadata["source_sha256"] != v.Params["source_sha256"] || metadata["seat_type"] != v.Params["seat_type"] {
		reject(perr("PRODUCTION_SOURCE_CHANGED", 409))
		return
	}
	allowed := v.Params["workspace_ids"].([]any)
	matches := len(allowed) == 0
	for _, id := range allowed {
		if id == metadata["workspace_id"] {
			matches = true
		}
	}
	if !matches {
		reject(perr("PRODUCTION_WORKSPACE_FORBIDDEN", 409))
		return
	}
	raw, err := Canonical(document)
	if err != nil {
		reject(perr("PRODUCTION_DOCUMENT_INVALID", 422))
		return
	}
	sealed, err := fernetEncrypt(h.cfg.ArtifactKey, raw)
	if err != nil {
		reject(perr("PRODUCTION_ADMISSION_ENCRYPTION_UNAVAILABLE", 503))
		return
	}
	sourceKey := ls(metadata["workspace_id"]) + ":" + ls(metadata["email"])
	p := store.Production{OperationID: id, RequestSHA: productionScope(v), Parameters: string(params), SourceKey: sourceKey,
		SourceRef: ls(metadata["source_ref"]), SourceSHA: ls(metadata["source_sha256"]),
		AccountRef: "production:" + id, Email: ls(metadata["email"]), Artifact: sealed}
	if err = h.st.AdmitProduction(p); err != nil {
		if old, readErr := h.st.Production(id); readErr == nil && old.RequestSHA == p.RequestSHA {
			h.writeAccepted(c, v)
			return
		}
		if claimed, readErr := h.st.ProductionClaimed(sourceKey, p.SourceSHA); readErr == nil && claimed {
			reject(perr("PRODUCTION_SOURCE_ALREADY_CLAIMED", 409))
		} else {
			reject(perr("PRODUCTION_ADMISSION_STORAGE_UNAVAILABLE", 503))
		}
		return
	}
	h.writeAccepted(c, v)
}

func checkProductionDocument(document map[string]any) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python", "tools/team_production_check.py")
	raw, _ := Canonical(document)
	command.Stdin = bytes.NewReader(raw)
	output, err := command.Output()
	result := map[string]any{}
	if err != nil || len(output) > 4096 || json.Unmarshal(output, &result) != nil {
		return map[string]any{"status": "unknown", "error_code": "PRODUCTION_CHECK_UNAVAILABLE"}
	}
	return result
}

func (h *Handler) productionReceipt(v *Verified, key []byte, now time.Time) (map[string]any, error) {
	p, err := h.st.Production(nodeTicket(v))
	// A retained failed/unknown output remains immutable, while a separate stop
	// receipt reconciles its external membership and releases Core's seat hold.
	if err == sql.ErrNoRows || err == nil && p.State != "completed" && h.st.ProductionStopped(p.OperationID) {
		p, err = h.st.ProductionAdmission(nodeTicket(v))
		if err == nil && p.RequestSHA == productionScope(v) {
			if p.State == "cancelled" && h.st.ProductionStopped(p.OperationID) {
				effect, email := h.st.ProductionStopEffect(p.OperationID)
				if effect == "" {
					effect = "unknown"
				}
				var value map[string]any
				_ = json.Unmarshal([]byte(p.Result), &value)
				value["observation"] = map[string]any{"stop_effect": effect, "email": email}
				if effect == "unknown" {
					p.State = "unknown"
					value["status"] = "unknown"
				}
				raw, _ := Canonical(value)
				p.Result = string(raw)
				return productionSignedReceipt(v, p, key)
			}
			if (p.State == "failed" || p.State == "cancelled") && h.st.ProductionRunSideEffectFree(p.OperationID) {
				var value map[string]any
				_ = json.Unmarshal([]byte(p.Result), &value)
				value["observation"] = map[string]any{"side_effect_free": true}
				raw, _ := Canonical(value)
				p.Result = string(raw)
			}
			return productionSignedReceipt(v, p, key)
		}
	}
	if err != nil {
		return nil, perr("WORK_TASK_NOT_FOUND", 404)
	}
	if p.RequestSHA != productionScope(v) {
		return nil, perr("PRODUCTION_REQUEST_CHANGED", 409)
	}
	if claimed, err := h.st.ClaimProductionCheck(p.OperationID, now.Unix()); err != nil {
		return nil, perr("PRODUCTION_STORAGE_UNAVAILABLE", 503)
	} else if claimed {
		started := time.Now()
		if err := h.st.Stage("work-"+p.OperationID, store.StageEvent{Stage: "verification", Status: "running", ResultSHA: p.SourceSHA, Action: "只读检查已保存结果，不刷新、不重新登录"}); err != nil {
			return nil, perr("PRODUCTION_STORAGE_UNAVAILABLE", 503)
		}
		document, err := h.productionDocument(p.Artifact)
		check := h.productionCheck
		if check == nil {
			check = checkProductionDocument
		}
		result := map[string]any{"status": "unknown", "error_code": "PRODUCTION_ARTIFACT_UNAVAILABLE"}
		if err == nil {
			result = check(document)
		}
		status := ls(result["status"])
		if status != "completed" && status != "failed" && status != "unknown" {
			status = "unknown"
			result = map[string]any{"status": status, "error_code": "PRODUCTION_CHECK_INVALID"}
		}
		safe := map[string]any{"status": status, "error_code": ls(result["error_code"])}
		if status == "completed" {
			metadata, err := productionMetadata(document)
			if err != nil {
				return nil, err
			}
			// Existing-result intake needs the same exact-ref working document
			// as newly produced output. Later customer repair must not guess a
			// different account/workspace by email from legacy files.
			if err := h.projectProductionDocument(p, document); err != nil {
				_ = h.st.Stage("work-"+p.OperationID, store.StageEvent{Stage: "projection", Status: "unknown", Code: "PRODUCTION_RESULT_EXPORT_FAILED",
					Action: "原成品已保留；修复本地资产存储后继续原任务，不重新生产或登录"})
				return nil, perr("PRODUCTION_RESULT_EXPORT_FAILED", 503)
			}
			var params map[string]any
			_ = json.Unmarshal([]byte(p.Parameters), &params)
			for _, field := range []string{"production_ref", "mode", "source_ref", "source_sha256"} {
				metadata[field] = params[field]
			}
			metadata["artifact_sha256"] = productionDigest(document)
			metadata["checked_at"] = now.Unix()
			metadata["valid_until"] = result["valid_until"]
			metadata["spent_units"] = 0
			safe["observation"], safe["liveness"] = metadata, "alive"
		}
		encoded, _ := Canonical(safe)
		if err = h.st.FinishProduction(p.OperationID, status, string(encoded), now.Unix()); err != nil {
			_ = h.st.Stage("work-"+p.OperationID, store.StageEvent{Stage: "verification", Status: "unknown", Code: "PRODUCTION_RESULT_SETTLEMENT_FAILED",
				DurationMS: time.Since(started).Milliseconds(), ResultSHA: p.SourceSHA, Action: "原成品已保存；修复本地入账后继续只读核验，不重新生产"})
			return nil, perr("PRODUCTION_RESULT_SETTLEMENT_FAILED", 503)
		}
		_ = h.st.Stage("work-"+p.OperationID, store.StageEvent{Stage: "verification_timing", Status: status, DurationMS: time.Since(started).Milliseconds(), ResultSHA: p.SourceSHA})
		p, err = h.st.Production(p.OperationID)
		if err != nil {
			return nil, err
		}
	}
	return productionSignedReceipt(v, p, key)
}

func (h *Handler) projectProductionDocument(p store.Production, document map[string]any) error {
	assets := assetstore.New(dataDir())
	current, err := assets.Get(p.AccountRef)
	if err == nil {
		if current.Platform != "chatgpt" || !strings.EqualFold(current.Email, p.Email) ||
			productionDigest(current.Payload) != productionDigest(document) {
			return perr("PRODUCTION_ACCOUNT_IDENTITY_CONFLICT", 409)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return assets.Put(assetstore.Asset{Ref: p.AccountRef, Platform: "chatgpt", Kind: "team", Email: p.Email,
		Source: "production.team_json", Payload: document})
}

func productionSignedReceipt(v *Verified, p store.Production, key []byte) (map[string]any, error) {
	result := map[string]any{}
	_ = json.Unmarshal([]byte(p.Result), &result)
	if len(result) == 0 {
		result = map[string]any{"status": p.State, "error_code": ""}
	}
	payload := map[string]any{}
	for _, field := range []string{"issuer", "node_id", "operation_id", "task_id", "attempt_id", "fence", "subject_id", "resource_version", "command_sha256"} {
		payload[field] = v.Payload[field]
	}
	payload["schema"], payload["node_task_id"], payload["status"] = ReceiptSchema, p.OperationID, p.State
	payload["result"], payload["result_sha256"] = result, productionDigest(result)
	payload["artifact_available"], payload["result_account_id"], payload["result_resource_version"], payload["finished_at"] = false, nil, nil, nil
	if p.FinishedAt != "" {
		payload["finished_at"] = p.FinishedAt
	}
	if p.State == "completed" {
		payload["artifact_available"], payload["result_account_id"], payload["result_resource_version"] = true, SubjectID(p.AccountRef), 1
	}
	return signInventory(payload, key, domainWork)
}

func (h *Handler) productionDocument(sealed string) (map[string]any, error) {
	raw, err := fernetDecrypt(h.cfg.ArtifactKey, []byte(sealed), 0)
	var document map[string]any
	if err != nil || json.Unmarshal(raw, &document) != nil {
		return nil, perr("PRODUCTION_ARTIFACT_UNAVAILABLE", 409)
	}
	return document, nil
}
