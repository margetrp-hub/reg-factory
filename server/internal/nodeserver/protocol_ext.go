package nodeserver

import (
	"crypto/sha256"
	"encoding/hex"
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
	doc, _ := h.credDocument(verified)
	full := map[string]any{"status": status, "exit_code": exitCode}
	if doc != nil {
		full["document"] = doc
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

// ReceiptFor 生成 account-node.receipt.v1。
func (h *Handler) ReceiptFor(verified *Verified, nodeTaskID, nodeStatus string, exitCode int, key []byte, now time.Time) (map[string]any, error) {
	workStatus := map[string]string{
		"succeeded": "completed", "completed": "completed",
		"failed": "failed", "error": "failed", "interrupted": "failed",
		"stopped": "cancelled", "cancelled": "cancelled",
		"queued": "queued", "running": "running", "waiting_user": "waiting_user",
	}[nodeStatus]
	if workStatus == "" {
		workStatus = "unknown"
	}
	full := h.fullResult(verified, workStatus, exitCode)
	var resultAccountID any
	var resultResourceVersion any
	versionError := ""
	if workStatus == "completed" {
		resultAccountID = verified.Payload["subject_id"]
		if v, ok := toInt64(verified.Payload["resource_version"]); ok {
			resultVersion := v + 1
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
