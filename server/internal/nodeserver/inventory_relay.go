package nodeserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

const (
	teamBusinessDomain = "account-node-team-business-v1\n"
	coreIntakeDomain   = "account-node-core-asset-intake-v1\n"
	teamBusinessSchema = "account-node.team-business.v1"
	teamReceiptSchema  = "account-node.team-business-receipt.v1"
	teamArtifactSchema = "account-node.team-handoff-artifact.v1"
	coreIntakeSchema   = "account-node.core-asset-intake.v1"
	coreReceiptSchema  = "account-node.core-asset-intake-receipt.v1"
)

var uuidLike = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

// inventoryRelay carries task-produced assets and custody events, not Plus
// stock discovery. REG account listings use the signed library endpoint.
// Work authenticates
// the outer envelope with the node request key; the returned business documents
// are signed with BusinessKey so Team can verify them independently.
func (h *Handler) inventoryRelay(c *gin.Context) {
	if len(h.config().BusinessKey) < minKeyLength {
		writeErr(c, perr("WORK_INVENTORY_NODE_UNCONFIGURED", http.StatusServiceUnavailable))
		return
	}
	var document map[string]any
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 256*1024))
	if err := decoder.Decode(&document); err != nil {
		writeErr(c, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity))
		return
	}
	payload, err := verifyLibraryRequest(document, h.config().Key, h.config().NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	action := ls(payload["action"])
	data, ok := payload["data"].(map[string]any)
	if action == "" || !ok {
		writeErr(c, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity))
		return
	}
	var response map[string]any
	switch action {
	case "asset_replay":
		response, err = h.replayCentral(data)
	case "production_sources":
		response, err = h.productionSources(payload, data)
	case "event":
		response, err = h.inventoryEvent(data)
	default:
		err = perr("WORK_INVENTORY_ACTION_FORBIDDEN", http.StatusForbidden)
	}
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, response)
}

func signInventory(payload map[string]any, key []byte, domain string) (map[string]any, error) {
	if len(key) < minKeyLength {
		return nil, perr("WORK_INVENTORY_NODE_UNCONFIGURED", http.StatusServiceUnavailable)
	}
	raw, err := Canonical(payload)
	if err != nil {
		return nil, perr("WORK_INVENTORY_SIGN_FAILED", http.StatusInternalServerError)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	mac.Write(raw)
	return map[string]any{"payload": payload, "signature": hex.EncodeToString(mac.Sum(nil))}, nil
}

func verifyInventorySigned(document map[string]any, key []byte, domain string, fields map[string]bool) (map[string]any, error) {
	payload, ok := document["payload"].(map[string]any)
	signature, _ := document["signature"].(string)
	if !ok || len(payload) != len(fields) || signature == "" {
		return nil, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity)
	}
	for name := range fields {
		if _, exists := payload[name]; !exists {
			return nil, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity)
		}
	}
	raw, err := Canonical(payload)
	if err != nil {
		return nil, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	mac.Write(raw)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hex64Pattern.MatchString(signature) || !hmac.Equal([]byte(want), []byte(signature)) {
		return nil, perr("WORK_INVENTORY_SIGNATURE_INVALID", http.StatusUnauthorized)
	}
	return payload, nil
}

func inventoryDigest(payload map[string]any) (string, error) {
	stable := make(map[string]any, len(payload))
	for key, value := range payload {
		if key != "issued_at" && key != "expires_at" {
			stable[key] = value
		}
	}
	raw, err := Canonical(stable)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

var teamBusinessFields = map[string]bool{
	"schema": true, "issuer": true, "node_id": true, "event_id": true, "event_type": true,
	"account_id": true, "source_asset_id": true, "team_asset_uuid": true, "state": true,
	"revision": true, "expected_resource_version": true, "issued_at": true, "expires_at": true,
	"customer_expires_at": true,
}

var coreIntakeFields = map[string]bool{
	"schema": true, "issuer": true, "node_id": true, "event_id": true, "team_asset_uuid": true,
	"asset_revision": true, "credential_revision": true, "json_kind": true, "material_sha256": true,
	"material_encrypted": true, "issued_at": true, "expires_at": true,
}

func (h *Handler) inventoryEvent(data map[string]any) (map[string]any, error) {
	if len(data) != 1 {
		return nil, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity)
	}
	event, ok := data["event"].(map[string]any)
	if !ok {
		return nil, perr("WORK_INVENTORY_EVENT_INVALID", http.StatusUnprocessableEntity)
	}
	pending, ok := event["payload"].(map[string]any)
	if !ok || ls(pending["node_id"]) != h.config().NodeID {
		return nil, perr("WORK_INVENTORY_SCOPE_INVALID", http.StatusForbidden)
	}
	switch ls(pending["schema"]) {
	case coreIntakeSchema:
		return h.inventoryCoreIntake(event)
	case teamBusinessSchema:
		return h.inventoryBusiness(event)
	default:
		return nil, perr("WORK_INVENTORY_EVENT_INVALID", http.StatusUnprocessableEntity)
	}
}

func (h *Handler) inventoryBusiness(event map[string]any) (map[string]any, error) {
	pending, err := verifyInventorySigned(event, h.config().BusinessKey, teamBusinessDomain, teamBusinessFields)
	if err != nil {
		return nil, err
	}
	if !uuidLike.MatchString(ls(pending["team_asset_uuid"])) || !uuidLike.MatchString(ls(pending["account_id"])) ||
		ls(pending["account_id"]) != ls(pending["source_asset_id"]) || ls(pending["issuer"]) == "" ||
		ls(pending["state"]) == "" || typeInt(pending["revision"]) < 1 || typeInt(pending["expected_resource_version"]) < 1 {
		return nil, perr("WORK_INVENTORY_EVENT_INVALID", http.StatusUnprocessableEntity)
	}
	state := ls(pending["state"])
	if state != "prepared" && state != "stocked" && state != "reserved" && state != "sold" && state != "used" && state != "delivered" && state != "quarantined" && state != "retired" && state != "released" {
		return nil, perr("WORK_INVENTORY_STATE_INVALID", http.StatusUnprocessableEntity)
	}
	digest, err := inventoryDigest(pending)
	if err != nil {
		return nil, perr("WORK_INVENTORY_EVENT_INVALID", http.StatusUnprocessableEntity)
	}
	if oldSHA, oldResponse, found, lookupErr := h.st.InventoryEvent(ls(pending["event_id"])); lookupErr != nil {
		return nil, perr("WORK_INVENTORY_STATE_UNAVAILABLE", http.StatusInternalServerError)
	} else if found {
		if oldSHA != digest {
			return nil, perr("WORK_INVENTORY_EVENT_CONFLICT", http.StatusConflict)
		}
		return h.replayInventoryResponse(oldResponse, teamBusinessDomain)
	}
	accountRef, ok := h.inventoryAccountRef(ls(pending["account_id"]))
	if !ok {
		return nil, perr("WORK_ACCOUNT_REFERENCE_INVALID", http.StatusUnprocessableEntity)
	}
	account, err := h.st.GetAccount(accountRef)
	if err != nil {
		return nil, perr("WORK_ACCOUNT_REFERENCE_INVALID", http.StatusUnprocessableEntity)
	}
	expectedVersion := intValue(pending["expected_resource_version"])
	if account.Version < 1 {
		account.Version = 1
	}
	if account.Version != expectedVersion {
		return nil, perr("WORK_INVENTORY_RESOURCE_VERSION_CONFLICT", http.StatusConflict)
	}
	responsePayload := map[string]any{
		"schema":   teamReceiptSchema,
		"event_id": pending["event_id"], "event_type": pending["event_type"], "node_id": pending["node_id"],
		"issuer": pending["issuer"], "account_id": pending["account_id"], "source_asset_id": pending["source_asset_id"],
		"team_asset_uuid": pending["team_asset_uuid"], "state": pending["state"], "revision": pending["revision"],
		"customer_expires_at": pending["customer_expires_at"], "resource_version": expectedVersion + 1,
		"ever_used": state == "sold" || state == "used" || state == "delivered", "business_locked": state != "released",
		"request_sha256": digest,
	}
	if state == "prepared" {
		// Only preparation exports credentials. Lifecycle delivery and release
		// must not depend on a still-readable credential file.
		document, err := h.accountDocument(accountRef)
		if err != nil {
			return nil, perr("WORK_INVENTORY_ARTIFACT_UNAVAILABLE", http.StatusConflict)
		}
		artifact := map[string]any{"schema": teamArtifactSchema}
		for _, key := range []string{"event_id", "event_type", "node_id", "issuer", "account_id", "source_asset_id", "team_asset_uuid", "state", "revision", "customer_expires_at", "resource_version", "ever_used", "business_locked", "request_sha256"} {
			artifact[key] = responsePayload[key]
		}
		artifact["document"] = document
		encoded, _ := Canonical(artifact)
		sealed, sealErr := fernetEncrypt(h.config().ArtifactKey, encoded)
		if sealErr != nil {
			return nil, perr("TEAM_ARTIFACT_KEY_UNCONFIGURED", http.StatusServiceUnavailable)
		}
		responsePayload["artifact_encrypted"] = sealed
	}
	response, err := signInventory(responsePayload, h.config().BusinessKey, teamBusinessDomain)
	if err != nil {
		return nil, err
	}
	encodedResponse, _ := json.Marshal(response)
	if saved, replay, commitErr := h.st.CommitInventoryEvent(ls(pending["event_id"]), digest, string(encodedResponse), h.config().NodeID,
		ls(pending["team_asset_uuid"]), accountRef, state, intValue(pending["revision"]), expectedVersion); commitErr != nil {
		return nil, perr("WORK_INVENTORY_COMMIT_FAILED", http.StatusConflict)
	} else if replay {
		return h.replayInventoryResponse(saved, teamBusinessDomain)
	}
	return response, nil
}

func (h *Handler) inventoryCoreIntake(event map[string]any) (map[string]any, error) {
	pending, err := verifyInventorySigned(event, h.config().BusinessKey, coreIntakeDomain, coreIntakeFields)
	if err != nil {
		return nil, err
	}
	if !uuidLike.MatchString(ls(pending["team_asset_uuid"])) || ls(pending["issuer"]) != "team-inventory" ||
		ls(pending["json_kind"]) != "plus" {
		return nil, perr("CORE_INTAKE_EVENT_INVALID", http.StatusUnprocessableEntity)
	}
	digest, err := inventoryDigest(pending)
	if err != nil {
		return nil, perr("CORE_INTAKE_EVENT_INVALID", http.StatusUnprocessableEntity)
	}
	if oldSHA, oldResponse, found, lookupErr := h.st.InventoryEvent(ls(pending["event_id"])); lookupErr != nil {
		return nil, perr("CORE_INTAKE_STATE_UNAVAILABLE", http.StatusInternalServerError)
	} else if found {
		if oldSHA != digest {
			return nil, perr("CORE_INTAKE_EVENT_CONFLICT", http.StatusConflict)
		}
		return h.replayInventoryResponse(oldResponse, coreIntakeDomain)
	}
	materialRaw, err := fernetDecrypt(h.config().ArtifactKey, []byte(ls(pending["material_encrypted"])), 30*time.Minute)
	if err != nil {
		return nil, perr("CORE_INTAKE_MATERIAL_INVALID", http.StatusUnprocessableEntity)
	}
	var material map[string]any
	if json.Unmarshal(materialRaw, &material) != nil || ls(material["schema"]) != "core.asset-intake-material.v1" ||
		ls(material["team_asset_uuid"]) != ls(pending["team_asset_uuid"]) {
		return nil, perr("CORE_INTAKE_MATERIAL_INVALID", http.StatusUnprocessableEntity)
	}
	materialSum := sha256.Sum256(materialRaw)
	if hex.EncodeToString(materialSum[:]) != ls(pending["material_sha256"]) {
		return nil, perr("CORE_INTAKE_MATERIAL_INVALID", http.StatusUnprocessableEntity)
	}
	document, err := materialDocument(material)
	if err != nil {
		return nil, perr("CORE_INTAKE_MATERIAL_INVALID", http.StatusUnprocessableEntity)
	}
	ref := ls(pending["team_asset_uuid"])
	if err := h.persistInventoryDocument(ref, document); err != nil {
		return nil, perr("CORE_INTAKE_STORAGE_FAILED", http.StatusInternalServerError)
	}
	if _, err := h.st.UpsertAccounts([]store.AccountSeed{{Ref: ref, Platform: "chatgpt", Kind: "oauth", Identity: ls(document["email"]), SecretRef: "team-intake"}}); err != nil {
		return nil, perr("CORE_INTAKE_STORAGE_FAILED", http.StatusInternalServerError)
	}
	account, err := h.st.GetAccount(ref)
	if err != nil {
		return nil, perr("CORE_INTAKE_STORAGE_FAILED", http.StatusInternalServerError)
	}
	expected := account.Version
	if expected < 1 {
		expected = 1
	}
	responsePayload := map[string]any{
		"schema": coreReceiptSchema, "issuer": pending["issuer"], "node_id": pending["node_id"], "event_id": pending["event_id"],
		"team_asset_uuid": pending["team_asset_uuid"], "asset_revision": pending["asset_revision"], "credential_revision": pending["credential_revision"],
		"request_sha256": digest, "status": "accepted", "error_code": "", "account_id": ref,
		"resource_version": expected + 1, "business_locked": true,
	}
	response, err := signInventory(responsePayload, h.config().BusinessKey, coreIntakeDomain)
	if err != nil {
		return nil, err
	}
	encodedResponse, _ := json.Marshal(response)
	if _, _, commitErr := h.st.CommitCoreInventoryEventForAccount(ls(pending["event_id"]), digest, string(encodedResponse), ref, expected); commitErr != nil {
		return nil, perr("CORE_INTAKE_COMMIT_FAILED", http.StatusConflict)
	}
	return response, nil
}

func (h *Handler) inventoryAccountRef(subject string) (string, bool) {
	if subject == "" {
		return "", false
	}
	if account, err := h.st.GetAccount(subject); err == nil && account.Ref != "" {
		return account.Ref, true
	}
	return h.subjectRef(subject)
}

func (h *Handler) accountDocument(ref string) (map[string]any, error) {
	account, err := h.st.GetAccount(ref)
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(os.Getenv("REG_FACTORY_DATA_DIR"))
	if root == "" {
		root = "/data"
	}
	if asset, err := assetstore.New(root).Get(ref, account.Identity); err == nil && asset.Payload != nil {
		document := normalizeDocument(asset.Payload, asset.Email)
		if document != nil {
			return document, nil
		}
		return nil, errors.New("credential result incomplete or ambiguous")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// Keep the old /data/creds path as a read-only fallback for migrated SG
	// installations; new writes use assetstore above.
	if strings.HasPrefix(account.SecretRef, "assets/") {
		path := filepath.Join(root, filepath.FromSlash(account.SecretRef))
		if raw, readErr := os.ReadFile(path); readErr == nil {
			var envelope map[string]any
			if json.Unmarshal(raw, &envelope) == nil {
				if payload, ok := envelope["payload"].(map[string]any); ok {
					if document := normalizeDocument(payload, account.Identity); document != nil {
						return document, nil
					}
				}
			}
		}
	}
	verified := &Verified{Payload: map[string]any{"subject_id": SubjectID(account.Identity)}, Params: map[string]any{}}
	if document, _ := h.credDocument(verified); document != nil {
		return normalizeDocument(document, account.Identity), nil
	}
	return nil, errors.New("credential document not found")
}

func normalizeDocument(payload map[string]any, fallbackEmail string) map[string]any {
	if payload == nil {
		return nil
	}
	document, err := assetstore.OAuthDocument(payload, fallbackEmail)
	if err != nil {
		return nil
	}
	for _, section := range []string{"account", "profile", "payload", "credentials", "oauth", "tokens"} {
		if nested, ok := payload[section].(map[string]any); ok {
			for _, key := range []string{"plan_type", "chatgpt_plan_type", "account_id", "chatgpt_account_id", "chatgpt_user_id", "organization_id", "client_id"} {
				if document[key] == nil || ls(document[key]) == "" {
					document[key] = nested[key]
				}
			}
		}
	}
	if ls(document["access_token"]) == "" {
		document["access_token"] = document["accessToken"]
	}
	if ls(document["refresh_token"]) == "" {
		document["refresh_token"] = document["refreshToken"]
	}
	if ls(document["id_token"]) == "" {
		document["id_token"] = document["idToken"]
	}
	if ls(document["email"]) == "" {
		document["email"] = emailFromDocument(payload)
	}
	if ls(document["email"]) == "" {
		document["email"] = fallbackEmail
	}
	if !strings.Contains(ls(document["email"]), "@") || ls(document["refresh_token"]) == "" {
		return nil
	}
	return document
}

func materialDocument(material map[string]any) (map[string]any, error) {
	document := map[string]any{"email": ls(material["email"])}
	fields, _ := material["fields"].(map[string]any)
	secrets, _ := material["secrets"].(map[string]any)
	for key, value := range fields {
		if key != "" && value != nil {
			document[key] = value
		}
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		if value := ls(secrets[key]); value != "" {
			document[key] = value
		}
	}
	if normalize := normalizeDocument(document, ls(material["email"])); normalize != nil {
		return normalize, nil
	}
	return nil, errors.New("incomplete material")
}

func (h *Handler) persistInventoryDocument(ref string, document map[string]any) error {
	root := strings.TrimSpace(os.Getenv("REG_FACTORY_DATA_DIR"))
	if root == "" {
		root = "/data"
	}
	return assetstore.New(root).Put(assetstore.Asset{
		Ref: ref, Platform: "chatgpt", Kind: "oauth", Email: ls(document["email"]),
		Source: "team-inventory-relay", Payload: document,
	})
}

func decodeStoredResponse(raw string) (map[string]any, error) {
	var response map[string]any
	if json.Unmarshal([]byte(raw), &response) != nil || response == nil {
		return nil, perr("WORK_INVENTORY_RESPONSE_INVALID", http.StatusInternalServerError)
	}
	return response, nil
}

func intValue(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		v, _ := n.Int64()
		return int(v)
	default:
		return 0
	}
}

func typeInt(value any) int { return intValue(value) }
