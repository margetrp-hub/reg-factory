package nodeserver

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/gin-gonic/gin"
)

const workEventsDomain = "account-node-work-events-v1\n"

var logSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:authorization|cookie|set-cookie)\s*[:=].*`),
	regexp.MustCompile(`(?i)["']?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|token|secret|password|passwd|client[_-]?secret|api[_-]?key|session(?:_key|_id)?|totp(?:_secret)?|otp|verification_code|code|密码|密钥|验证码|口令)["']?\s*[:=]\s*(?:"[^"]*"|'[^']*'|[^\s,;}]+)`),
	regexp.MustCompile(`(?i)bearer\s+[^\s,;]+`),
	regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`),
	regexp.MustCompile(`[A-Za-z0-9_+/=.-]{32,}`),
	regexp.MustCompile(`\b[0-9]{6}\b`),
}
var logControls = regexp.MustCompile(`[\x00-\x08\x0b-\x1f\x7f]`)

func safeExecutionText(line string) string {
	if diagnostic, ok := safeAuthorizationDiagnostic(line); ok {
		return diagnostic
	}
	for _, pattern := range logSecrets {
		line = pattern.ReplaceAllString(line, "[REDACTED]")
	}
	line = logControls.ReplaceAllString(line, "")
	runes := []rune(strings.TrimSpace(line))
	if len(runes) > 2048 {
		return string(runes[:2048]) + "…"
	}
	return string(runes)
}

// Reauthorization emits an allowlisted diagnostic, not a raw provider reply.
// Preserve that evidence without letting the generic "code" redactor consume
// the suffix of "error_code", or permitting arbitrary values to escape it.
func safeAuthorizationDiagnostic(line string) (string, bool) {
	const prefix = "[reauthorize] "
	if !strings.HasPrefix(line, prefix) || len(line) > 4096 {
		return "", false
	}
	var value struct {
		Stage       string `json:"stage"`
		Status      string `json:"status"`
		Code        string `json:"error_code"`
		Page        string `json:"page_type"`
		HTTP        int    `json:"http_status"`
		Exchange    *bool  `json:"token_exchange_started"`
		Attempt     int    `json:"attempt"`
		MaxAttempts int    `json:"max_attempts"`
		RetryIn     int    `json:"retry_in_seconds"`
	}
	if json.Unmarshal([]byte(strings.TrimPrefix(line, prefix)), &value) != nil || value.Stage != "authorize" || value.Exchange == nil || *value.Exchange {
		return "", false
	}
	codes := map[string]string{
		"login_continue_failed":                 "登录继续步骤未完成",
		"oauth_tls_handshake_failed":            "登录校验连接 TLS 握手失败",
		"oauth_certificate_verification_failed": "登录校验证书验证失败",
		"oauth_network_timeout":                 "登录校验连接超时",
		"oauth_network_unavailable":             "登录校验连接不可用",
		"oauth_login_check_http_error":          "登录校验服务响应异常",
		"oauth_login_check_invalid_response":    "登录校验响应不完整",
		"invalid_username_or_password":          "邮箱或密码未通过校验",
		"rate_limit_exceeded":                   "登录请求被限流",
		"mfa_totp_secret_required":              "缺少两步验证资料",
		"mfa_totp_factor_not_available":         "两步验证方式不可用",
		"mfa_totp_code_unavailable":             "两步验证码未通过",
		"mfa_totp_verification_failed":          "两步验证未完成",
		"mfa_primary_factor_required":           "需要主验证方式确认",
		"email_otp_rejected_429":                "邮箱验证码被限流",
		"email_otp_not_found":                   "未取得邮箱验证码",
		"email_otp_provider_required":           "需要邮箱验证资料",
		"email_otp_verification_failed":         "邮箱验证未完成",
		"phone_required":                        "需要手机号验证",
		"add_phone_failed":                      "手机号验证未完成",
		"add_phone_requires_sms_client":         "需要手机号验证",
		"oauth_cloudflare_challenge":            "登录页需要额外验证",
		"oauth_callback_not_found":              "未取得授权回调",
		"oauth_callback_timeout":                "等待授权回调超时",
		"oauth_authorization_failed":            "授权未完成",
		"choose_account_failed":                 "选择账号未完成",
		"about_you_failed":                      "账号资料确认未完成",
	}
	label, ok := codes[value.Code]
	if !ok {
		return "", false
	}
	pages := map[string]bool{"start": true, "log-in": true, "login-check": true, "login_password": true,
		"mfa-challenge": true, "email-verification": true, "workspace": true, "consent": true,
		"edge-challenge": true, "http-page": true, "choose-an-account": true, "add-phone": true}
	if !pages[value.Page] {
		value.Page = "unknown"
	}
	result := prefix + label + " · " + value.Code + " · " + value.Page
	if value.HTTP >= 100 && value.HTTP <= 599 {
		result += fmt.Sprintf(" · HTTP %d", value.HTTP)
	}
	if value.Status == "retrying" && value.MaxAttempts == 3 && value.Attempt >= 1 && value.Attempt < 3 && value.RetryIn >= 0 && value.RetryIn <= 30 {
		result += fmt.Sprintf(" · 第%d/%d次未完成，%d秒后自动重试", value.Attempt, value.MaxAttempts, value.RetryIn)
	}
	return result + " · 尚未交换凭据", true
}

func signWorkEvents(payload map[string]any, key []byte) (string, error) {
	if len(key) < 32 {
		return "", perr("WORK_KEY_UNCONFIGURED", 503)
	}
	// Execution output may contain Unicode. Match Work's ASCII-escaped JSON;
	// the older task protocol's ASCII-only Canonical must remain unchanged.
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return "", err
	}
	var escaped strings.Builder
	for _, r := range string(bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))) {
		if r > 126 {
			if r > 0xffff {
				a, b := utf16.EncodeRune(r)
				fmt.Fprintf(&escaped, `\u%04x\u%04x`, a, b)
			} else {
				fmt.Fprintf(&escaped, `\u%04x`, r)
			}
		} else {
			escaped.WriteRune(r)
		}
	}
	raw := []byte(escaped.String())
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(workEventsDomain))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (h *Handler) executionEvents(c *gin.Context) {
	if c.GetHeader("Cookie") != "" || c.GetHeader("Origin") != "" || c.GetHeader("Sec-Fetch-Site") != "" {
		writeErr(c, perr("WORK_EVENTS_MACHINE_ONLY", 403))
		return
	}
	var doc Document
	if json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 65536)).Decode(&doc) != nil {
		writeErr(c, perr("WORK_EVENTS_INVALID", 422))
		return
	}
	p := doc.Payload
	fields := []string{"schema", "issuer", "node_id", "operation_id", "task_id", "attempt_id", "fence", "command_sha256", "node_task_id", "request_id", "issued_at", "expires_at", "page", "page_size"}
	if len(p) != len(fields) {
		writeErr(c, perr("WORK_EVENTS_INVALID", 422))
		return
	}
	for _, field := range fields {
		if _, ok := p[field]; !ok {
			writeErr(c, perr("WORK_EVENTS_INVALID", 422))
			return
		}
	}
	expected, err := signWorkEvents(p, h.cfg.Key)
	if err != nil || !hmac.Equal([]byte(expected), []byte(doc.Signature)) {
		writeErr(c, perr("WORK_EVENTS_SIGNATURE_INVALID", 403))
		return
	}
	now := time.Now().Unix()
	page, pageOK := toInt64(p["page"])
	size, sizeOK := toInt64(p["page_size"])
	issued, issuedOK := toInt64(p["issued_at"])
	expires, expiresOK := toInt64(p["expires_at"])
	if p["schema"] != "account-node.events-request.v1" || p["node_id"] != h.cfg.NodeID || p["node_task_id"] != c.Param("task_id") || p["operation_id"] != p["node_task_id"] || !pageOK || !sizeOK || page < 1 || page > 100000 || size < 1 || size > 50 || !issuedOK || !expiresOK || issued < now-120 || issued > now+30 || expires <= now || expires > issued+120 {
		writeErr(c, perr("WORK_EVENTS_SCOPE_INVALID", 403))
		return
	}
	task, err := h.taskByTicket(ls(p["node_task_id"]))
	if err != nil {
		writeErr(c, perr("WORK_TASK_UNAVAILABLE", 404))
		return
	}
	if task.ID != "work-"+ls(p["task_id"]) {
		writeErr(c, perr("WORK_EVENTS_SCOPE_MISMATCH", 403))
		return
	}
	rows, total, err := h.st.WorkEventsPage(task.ID, int(page), int(size))
	if err != nil {
		writeErr(c, perr("WORK_EVENTS_UNAVAILABLE", 503))
		return
	}
	items := []map[string]any{}
	for _, event := range rows {
		text := safeExecutionText(event.Line)
		if event.Stream == "stage" {
			var stage store.StageEvent
			if json.Unmarshal([]byte(event.Line), &stage) == nil {
				if stage.Action == "" {
					// Earlier Python runs retained the same guidance under action.
					// Read their original evidence without rewriting history.
					var prior struct {
						Action string `json:"action"`
					}
					if json.Unmarshal([]byte(event.Line), &prior) == nil {
						stage.Action = prior.Action
					}
				}
				text = safeExecutionText(stage.Stage + " · " + stage.Status + " · " + stage.Action)
				if regexp.MustCompile(`^PRODUCTION_[A-Z0-9_]{1,80}$`).MatchString(stage.Code) {
					text += " · " + stage.Code
				}
				if hex64Pattern.MatchString(stage.ResultSHA) {
					text += " · result=" + stage.ResultSHA[:12]
				}
				if stage.DurationMS > 0 {
					text += fmt.Sprintf(" · %dms", stage.DurationMS)
				}
			}
		}
		stream := event.Stream
		if stream != "stdout" && stream != "stderr" && stream != "system" {
			stream = "system"
		}
		items = append(items, map[string]any{"id": event.ID, "kind": "log", "text": text, "stream": stream, "created_at": event.TS})
	}
	result := map[string]any{}
	for _, field := range fields {
		result[field] = p[field]
	}
	result["schema"] = "account-node.events-response.v1"
	result["issued_at"], result["expires_at"] = now, now+120
	result["items"], result["total"], result["has_more"], result["raw_output_available"] = items, total, page*size < total, false
	signature, err := signWorkEvents(result, h.receiptKey())
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"payload": result, "signature": signature})
}
