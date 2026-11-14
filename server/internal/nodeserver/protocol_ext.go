package nodeserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// VerifyRead 校验 Work 的只读请求（取结果 / 取工件）。
//
// Work 用 NodeReadDomain 签名只读请求，而 Verify 固定按 NodeDomain 比对，
// 只读请求走那条必被判签名无效。
func VerifyRead(doc Document, key []byte, nodeID string, now time.Time) (*Verified, error) {
	if len(key) < minKeyLength {
		return nil, perr("WORK_KEY_UNCONFIGURED", 503)
	}
	if doc.Payload == nil || doc.Signature == "" {
		return nil, perr("WORK_REQUEST_INVALID", 400)
	}
	expected, err := Sign(doc.Payload, key, true)
	if err != nil {
		return nil, err
	}
	if len(expected) != 64 || expected != doc.Signature {
		return nil, perr("WORK_SIGNATURE_INVALID", 401)
	}
	if schema, _ := doc.Payload["schema"].(string); schema != WorkSchema {
		return nil, perr("WORK_TARGET_INVALID", 409)
	}
	if id, _ := doc.Payload["node_id"].(string); id != nodeID {
		return nil, perr("WORK_TARGET_INVALID", 409)
	}
	action, _ := doc.Payload["action"].(string)
	subject, _ := doc.Payload["subject_id"].(string)
	taskID, _ := doc.Payload["task_id"].(string)
	if action == "" || subject == "" || taskID == "" {
		return nil, perr("WORK_REFERENCE_INVALID", 400)
	}
	params, _ := doc.Payload["parameters"].(map[string]any)
	return &Verified{Payload: doc.Payload, Action: action, TaskID: taskID, Subject: subject, Params: params}, nil
}

// fullResult 是这次执行对 Work 的完整交代。
//
// 回执只放脱敏视图，凭据本体只出现在工件里；两处的 result_sha256 必须由
// 同一份完整结果算出，否则 Core 判 WORK_ACCOUNT_ARTIFACT_SCOPE_CHANGED。
func (h *Handler) fullResult(verified *Verified, status string, exitCode int) map[string]any {
	if verified.Action == "account.quota" {
		full := map[string]any{"status": status, "exit_code": exitCode}
		if task, err := h.taskByTicket(nodeTicket(verified)); err == nil {
			if task.Error != "" {
				full["error_code"] = task.Error
			}
			if status == "completed" {
				var snapshot map[string]any
				if json.Unmarshal([]byte(task.ArgsJSON), &snapshot) == nil {
					full["observation"] = snapshot
				}
			}
		}
		return full
	}
	full := map[string]any{"status": status, "exit_code": exitCode}
	if status == "completed" {
		if verified.Action == "account.refresh" || verified.Action == "account.authorize" {
			if doc := h.taskCredential(verified); doc != nil {
				full["document"] = doc
			}
		} else if doc, _ := h.credDocument(verified); doc != nil {
			full["document"] = doc
		}
	}
	return full
}

// receiptSafe 是回执允许出现的部分。Work 的 SecretFree 把 document 列为
// 禁止字段，回执带上必被拒，所以这里只保留状态与观察。
func receiptSafe(full map[string]any) map[string]any {
	errorCode := full["error_code"]
	if errorCode == nil {
		errorCode = ""
	}
	safe := map[string]any{"status": full["status"], "error_code": errorCode}
	if v, ok := full["observation"]; ok {
		safe["observation"] = v
	}
	return safe
}

// Only new structured outcomes carry a public reason. Historical receipts keep
// their original generic code and digest when polled after an upgrade.
func (h *Handler) refreshFailureCode(ticket string) string {
	const fallback = "WORK_ACCOUNT_REFRESH_FAILED"
	if h.st == nil || ticket == "" {
		return fallback
	}
	// Receipts expose the operation ticket; runner events belong to the local
	// task. Use the same resolved owner as taskReceipt/resolveTask.
	task, err := h.taskByTicket(ticket)
	if err != nil {
		return fallback
	}
	events, err := h.st.TailEvents(task.ID, 100)
	if err != nil {
		return fallback
	}
	for i := len(events) - 1; i >= 0; i-- {
		var result struct {
			Result string `json:"result"`
			Code   string `json:"error_code"`
			Schema string `json:"error_schema"`
		}
		if events[i].Stream != "stdout" || json.Unmarshal([]byte(events[i].Line), &result) != nil || result.Result != "failed" {
			continue
		}
		if result.Schema == "account.failure.v2" {
			switch result.Code {
			case "account_deactivated", "account_banned", "workspace_deactivated", "deactivated_workspace":
				return strings.ToUpper(result.Code)
			case "platform_authorization_unsupported", "credential_task_busy", "credential_document_ambiguous",
				"credential_not_found", "refresh_token_required", "credential_save_failed", "credential_login_material_invalid",
				"invalid_grant", "invalid_token", "refresh_token_reused", "refresh_token_expired", "refresh_token_revoked",
				"oauth_service_unavailable", "oauth_response_invalid", "oauth_request_rejected", "oauth_callback_invalid",
				"phone_required", "add_phone_failed", "add_phone_requires_sms_client", "email_otp_rejected_429",
				"email_otp_not_found", "email_otp_provider_required", "email_otp_verification_failed",
				"oauth_cloudflare_challenge", "oauth_callback_not_found", "oauth_callback_timeout", "login_continue_failed",
				"choose_account_failed", "about_you_failed", "oauth_authorization_failed", "authorization_browser_error",
				"credential_result_incomplete", "credential_identity_mismatch", "credential_worker_exited",
				"mfa_totp_verification_failed", "mfa_challenge_rejected", "mfa_totp_secret_required",
				"mfa_totp_factor_not_available", "mfa_primary_factor_required", "mfa_totp_code_unavailable",
				"invalid_username_or_password", "rate_limit_exceeded", "oauth_tls_handshake_failed",
				"oauth_certificate_verification_failed", "oauth_network_timeout", "oauth_network_unavailable",
				"oauth_login_check_http_error", "oauth_login_check_invalid_response":
				return "WORK_ACCOUNT_" + strings.ToUpper(result.Code)
			default:
				return "WORK_ACCOUNT_FAILURE_REASON_INVALID"
			}
		}
		if result.Schema != "account.failure.v1" {
			return fallback
		}
		switch result.Code {
		case "account_deactivated", "account_banned", "workspace_deactivated", "deactivated_workspace":
			return strings.ToUpper(result.Code)
		case "mfa_totp_verification_failed", "mfa_challenge_rejected", "mfa_totp_secret_required",
			"mfa_totp_factor_not_available", "mfa_primary_factor_required", "mfa_totp_code_unavailable",
			"invalid_username_or_password", "rate_limit_exceeded", "oauth_tls_handshake_failed",
			"oauth_certificate_verification_failed", "oauth_network_timeout", "oauth_network_unavailable",
			"oauth_login_check_http_error", "oauth_login_check_invalid_response":
			return "WORK_ACCOUNT_" + strings.ToUpper(result.Code)
		default:
			return fallback
		}
	}
	return fallback
}

// ReceiptFor 生成 account-node.receipt.v1。
func (h *Handler) ReceiptFor(verified *Verified, nodeTaskID, nodeStatus string, exitCode int, key []byte, now time.Time) (map[string]any, error) {
	if centralBinding(verified) {
		return h.centralReceipt(verified, key)
	}
	if verified.Action == productionAction {
		return h.productionReceipt(verified, key, now)
	}
	workStatus := map[string]string{
		"succeeded": "completed", "completed": "completed",
		"failed": "failed", "error": "failed", "interrupted": "failed",
		"stopped": "cancelled", "cancelled": "cancelled",
		"queued": "queued", "running": "running", "waiting_user": "waiting_user",
	}[nodeStatus]
	if workStatus == "" {
		workStatus = "unknown"
	}
	if (workStatus == "unknown" || nodeStatus == "interrupted") && (verified.Action == "account.authorize" || verified.Action == "account.refresh") {
		workStatus = "failed"
	}
	full := h.fullResult(verified, workStatus, exitCode)
	if workStatus == "unknown" && exitCode == 75 {
		full["error_code"] = "WORK_ACCOUNT_REFRESH_UNKNOWN"
	}
	if workStatus == "failed" && (verified.Action == "account.authorize" || verified.Action == "account.refresh") {
		full["error_code"] = h.refreshFailureCode(nodeTaskID)
	}
	var resultAccountID any
	var resultResourceVersion any
	versionError := ""
	if workStatus == "completed" && (verified.Action == "account.refresh" || verified.Action == "account.authorize") && full["document"] == nil {
		versionError = "WORK_ACCOUNT_RESULT_UNAVAILABLE"
	}
	if workStatus == "completed" && versionError == "" {
		resultAccountID = verified.Payload["subject_id"]
		if v, ok := toInt64(verified.Payload["resource_version"]); ok {
			resultVersion := v + 1
			if verified.Action == "account.quota" {
				// Work's completed observation carries the observed source version.
				// A read-only quota snapshot never advances that version.
				resultVersion = v
			}
			if verified.Action == "account.refresh" || verified.Action == "account.authorize" {
				ref, found := h.subjectRef(verified.Subject)
				if !found {
					versionError = "WORK_ACCOUNT_VERSION_UNAVAILABLE"
				} else {
					operationID, _ := verified.Payload["operation_id"].(string)
					committed, commitErr := h.st.CommitOperationResult(operationID, verified.Action, ref, int(v))
					if commitErr != nil {
						versionError = "WORK_ACCOUNT_VERSION_CONFLICT"
					} else {
						resultVersion = int64(committed)
					}
				}
			}
			resultResourceVersion = resultVersion
		}
	}
	if versionError != "" {
		// The local process finished, but the durable source version did not.
		// Return signed terminal evidence so Work can settle or review it instead
		// of retrying an HTTP conflict forever. No artifact is advertised.
		workStatus = "failed"
		full = h.fullResult(verified, workStatus, exitCode)
		full["error_code"] = versionError
		resultAccountID = nil
		resultResourceVersion = nil
	}
	var finishedAt any
	if workStatus != "queued" && workStatus != "running" && workStatus != "waiting_user" {
		finishedAt = now.UTC().Format(time.RFC3339)
		if h.st != nil {
			if task, err := h.taskByTicket(nodeTaskID); err == nil && task.FinishedAt != "" {
				finishedAt = task.FinishedAt
			}
		}
	}
	rawFull, err := Canonical(full)
	if err != nil {
		return nil, perr("WORK_REQUEST_INVALID", 400)
	}
	sum := sha256.Sum256(rawFull)
	payload := map[string]any{
		"issuer":                  verified.Payload["issuer"],
		"node_id":                 verified.Payload["node_id"],
		"operation_id":            verified.Payload["operation_id"],
		"task_id":                 verified.Payload["task_id"],
		"attempt_id":              verified.Payload["attempt_id"],
		"fence":                   verified.Payload["fence"],
		"subject_id":              verified.Payload["subject_id"],
		"resource_version":        verified.Payload["resource_version"],
		"command_sha256":          verified.Payload["command_sha256"],
		"schema":                  ReceiptSchema,
		"node_task_id":            nodeTaskID,
		"status":                  workStatus,
		"result":                  receiptSafe(full),
		"result_sha256":           hex.EncodeToString(sum[:]),
		"artifact_available":      workStatus == "completed" && full["document"] != nil,
		"result_account_id":       resultAccountID,
		"result_resource_version": resultResourceVersion,
		"finished_at":             finishedAt,
	}
	sig, err := Sign(payload, key, false)
	if err != nil {
		return nil, err
	}
	return map[string]any{"payload": payload, "signature": sig}, nil
}
