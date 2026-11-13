// Package nodeserver 让 reg-factory 以「子节点」身份接入 Team / Work：
// 校验带签名的 Work 请求、把 account.register 映射成本地注册任务、并用同样
// 的签名算法把结果回执推回编排侧。
//
// 协议对齐 team-platform 的 services/account-node/account_node/work_protocol.py：
//
//	schema      account-node.work.v1
//	receipt     account-node.receipt.v1
//	domain      "account-node-work-v1\n"（结果回执用 "account-node-work-read-v1\n"）
//	canonical   JSON sort_keys / 无空格 / 全 ASCII
//	HMAC-SHA256(key, domain + canonical(payload))，key 至少 32 字节
package nodeserver

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

const (
	// WorkSchema 是编排侧下发的请求 schema。
	WorkSchema = "account-node.work.v1"
	// ReceiptSchema 是节点回传的结果 schema。
	ReceiptSchema = "account-node.receipt.v1"

	domainWork   = "account-node-work-v1\n"
	domainRead   = "account-node-work-read-v1\n"
	maxPayload   = 16384
	minKeyLength = 32
)

var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,159}$`)
var hex64Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// workFields 是请求 payload 必须且仅有的字段集合。
var workFields = []string{
	"schema", "issuer", "node_id", "operation_id", "task_id", "attempt_id", "fence",
	"action", "subject_kind", "subject_id", "resource_version", "issued_at", "expires_at",
	"parameters", "command_sha256",
}

// 支持的动作与各自允许的业务参数。
var workActions = map[string]map[string]bool{
	"asset.restore": {"asset_ref":true},
	productionAction:    {"production_ref": true, "order_ref": true, "product_id": true, "seat_type": true, "workspace_ids": true, "mode": true, "source_ref": true, "source_sha256": true, "budget_units": true},
	"account.register":  {"goal": true, "sms_allowance": true},
	"account.authorize": {"sms_allowance": true, "custody_grant": true},
	// A refresh issued by Team carries the signed custody grant that binds the
	// node operation to the original account/asset authority.  The node does
	// not need a Core connection; Work authenticates the envelope and the
	// execution path consumes the grant as part of the task material.
	"account.refresh":  {"custody_grant": true},
	"account.liveness": {},
	"account.quota":    {},
	"asset.liveness":   {},
	"team.seat_price":  {},
	"account.join": {"target_account_id": true, "target_version": true, "team_ref": true,
		"workspace_type": true, "reservation_ref": true, "custody_grant": true},
	"account.verify_asset": {"target_account_id": true, "target_version": true, "team_ref": true,
		"workspace_type": true, "reservation_ref": true},
}

// ProtocolError 带 HTTP 状态码，便于直接回给调用方。
type ProtocolError struct {
	Code   string
	Status int
}

func (e *ProtocolError) Error() string { return e.Code }

func perr(code string, status int) *ProtocolError { return &ProtocolError{Code: code, Status: status} }

// Canonical 复刻 Python 的 json.dumps(sort_keys=True, separators=(",", ":"), ensure_ascii=True)。
// Go 对 map 的编码本身按键排序；这里额外要求全 ASCII，避免与 ensure_ascii 产生分歧。
func Canonical(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	raw := bytes.TrimRight(buf.Bytes(), "\n")
	for _, b := range raw {
		if b >= 0x80 {
			return nil, errors.New("payload 含非 ASCII 字符")
		}
	}
	return raw, nil
}

// Sign 按 domain + canonical 计算签名。
func Sign(payload any, key []byte, read bool) (string, error) {
	if len(key) < minKeyLength {
		return "", perr("WORK_KEY_UNCONFIGURED", 503)
	}
	raw, err := Canonical(payload)
	if err != nil {
		return "", perr("WORK_REQUEST_INVALID", 400)
	}
	domain := domainWork
	if read {
		domain = domainRead
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// Document 是 {"payload": {...}, "signature": "..."} 的外层结构。
type Document struct {
	Payload   map[string]any `json:"payload"`
	Signature string         `json:"signature"`
}

// Verified 是校验通过后的请求。
type Verified struct {
	Payload map[string]any
	Action  string
	TaskID  string
	Subject string
	Params  map[string]any
	Fence   int64
}

// Verify 校验签名、时间窗、字段集合、动作与参数白名单。
func Verify(doc Document, key []byte, nodeID string, now time.Time) (*Verified, error) {
	if len(key) < minKeyLength {
		return nil, perr("WORK_KEY_UNCONFIGURED", 503)
	}
	if doc.Payload == nil || doc.Signature == "" {
		return nil, perr("WORK_REQUEST_INVALID", 400)
	}
	if len(doc.Payload) != len(workFields) {
		return nil, perr("WORK_REQUEST_INVALID", 400)
	}
	for _, name := range workFields {
		if _, ok := doc.Payload[name]; !ok {
			return nil, perr("WORK_REQUEST_INVALID", 400)
		}
	}
	expected, err := Sign(doc.Payload, key, false)
	if err != nil {
		return nil, err
	}
	if len(expected) != 64 || !hex64Pattern.MatchString(doc.Signature) ||
		!hmac.Equal([]byte(expected), []byte(doc.Signature)) {
		return nil, perr("WORK_SIGNATURE_INVALID", 401)
	}
	if schema, _ := doc.Payload["schema"].(string); schema != WorkSchema {
		return nil, perr("WORK_TARGET_INVALID", 409)
	}
	if id, _ := doc.Payload["node_id"].(string); id != nodeID {
		return nil, perr("WORK_TARGET_INVALID", 409)
	}

	refs := []string{"issuer", "node_id", "operation_id", "task_id", "attempt_id", "subject_id"}
	for _, name := range refs {
		text, ok := doc.Payload[name].(string)
		if !ok || !refPattern.MatchString(text) {
			return nil, perr("WORK_REFERENCE_INVALID", 400)
		}
	}

	ints := map[string]int64{}
	for _, name := range []string{"fence", "resource_version", "issued_at", "expires_at"} {
		value, ok := toInt64(doc.Payload[name])
		if !ok || value < 1 {
			return nil, perr("WORK_AUTHORITY_INVALID", 400)
		}
		ints[name] = value
	}
	ts := now.Unix()
	issued, expires := ints["issued_at"], ints["expires_at"]
	if issued > ts+30 || issued < ts-300 || expires <= ts || expires-issued > 900 || expires-issued <= 0 {
		return nil, perr("WORK_AUTHORIZATION_EXPIRED", 403)
	}

	action, _ := doc.Payload["action"].(string)
	allowed, ok := workActions[action]
	if !ok {
		return nil, perr("WORK_ACTION_UNSUPPORTED", 422)
	}
	subjectKind, _ := doc.Payload["subject_kind"].(string)
	wantKind := "account"
	if action == "account.register" {
		wantKind = "mailbox"
	}
	if action == productionAction {
		wantKind = "production"
	}
	if subjectKind != wantKind {
		return nil, perr("WORK_SUBJECT_INVALID", 400)
	}
	command, _ := doc.Payload["command_sha256"].(string)
	if !hex64Pattern.MatchString(command) {
		return nil, perr("WORK_COMMAND_INVALID", 400)
	}

	params, _ := doc.Payload["parameters"].(map[string]any)
	if params == nil {
		return nil, perr("WORK_PARAMETERS_INVALID", 400)
	}
	for name := range params {
		if name == "asset_ref" {
			binding, ok := params[name].(map[string]any)
			if !ok || len(binding) != 1 || len(params) != 1 || !refPattern.MatchString(ls(binding["center_id"])) ||
				(action != "asset.liveness" && action != "account.refresh" && action != "account.authorize" && action != "asset.restore") {
				return nil, perr("WORK_ASSET_BINDING_INVALID", 400)
			}
			continue
		}
		if !allowed[name] {
			return nil, perr("WORK_PARAMETERS_INVALID", 400)
		}
	}
	if action == "account.register" {
		if goal, ok := params["goal"].(string); ok && goal != "register" {
			return nil, perr("NODE_REGISTER_THEN_AUTHORIZE_REQUIRED", 422)
		}
	}
	if action == "asset.restore" && len(params)!=1 { return nil,perr("WORK_ASSET_BINDING_INVALID",400) }

	subject, _ := doc.Payload["subject_id"].(string)
	taskID, _ := doc.Payload["task_id"].(string)
	return &Verified{
		Payload: doc.Payload,
		Action:  action,
		TaskID:  taskID,
		Subject: subject,
		Params:  params,
		Fence:   ints["fence"],
	}, nil
}

// Receipt 生成可回传的签名结果文档（用 read domain，与 account-node 的 signed_work_result 一致）。
func Receipt(taskID, nodeID, status string, exitCode int, detail map[string]any, key []byte, now time.Time) (map[string]any, error) {
	payload := map[string]any{
		"schema":      ReceiptSchema,
		"node_id":     nodeID,
		"task_id":     taskID,
		"status":      status,
		"exit_code":   exitCode,
		"finished_at": now.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if detail != nil {
		payload["detail"] = detail
	}
	sig, err := Sign(payload, key, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{"payload": payload, "signature": sig}, nil
}

func toInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		return n, err == nil
	}
	return 0, false
}

// Describe 给 /health 用的能力声明。
func Describe() map[string]any {
	actions := make([]string, 0, len(workActions))
	for name := range workActions {
		actions = append(actions, name)
	}
	return map[string]any{
		"schema":      WorkSchema,
		"receipt":     ReceiptSchema,
		"actions":     actions,
		"subject":     map[string]string{"account.register": "mailbox", "default": "account"},
		"receipt_key": fmt.Sprintf("hmac-sha256/%s", "account-node-work-read-v1"),
	}
}
