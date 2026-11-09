package nodeserver

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ReferenceDomain 是节点对 Team 签发账号引用证明的域分隔符。
// Team 侧 REFERENCE_DOMAIN = b"account-node-reference-v1\n"，两边必须逐字节一致。
const ReferenceDomain = "account-node-reference-v1\n"

const referenceSchema = "account-node.reference.v1"

// SubjectID 把本地账号 ref 映射成稳定 UUID。
// Team 的 verify_reference 要求 subject_id 是合法 UUID，而本地 ref 形如
// "creds:61pivotal-heart"，直接回传会被判 WORK_ACCOUNT_REFERENCE_INVALID。
// 用 SHA-1 前 16 字节构造确定性 UUID 并置版本位 4：同一个 ref 永远得到同一个
// UUID，Team 的幂等键不会随时间漂移。
func SubjectID(ref string) string {
	sum := sha1.Sum([]byte("account-node:" + ref))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// subjectRef 反查：给定 UUID 找本地 ref。
func (h *Handler) subjectRef(subject string) (string, bool) {
	alias, _ := h.st.AccountSubjectRef(subject)
	if strings.Contains(subject, ":") {
		return subject, true
	}
	rows, err := h.st.ListAccounts("", "", "", 10000)
	if err != nil {
		return "", false
	}
	for _, a := range rows {
		// Core may use the canonical account UUID directly for local intake.
		// Keep accepting the older deterministic SubjectID(ref) projection too.
		if a.Ref == subject {
			if alias != "" && alias != a.Ref {
				return "", false
			}
			return a.Ref, true
		}
		if SubjectID(a.Ref) == subject {
			if alias != "" && alias != a.Ref {
				return "", false
			}
			return a.Ref, true
		}
	}
	return alias, alias != ""
}

// parseStamp 把节点的 RFC3339 时间转成 Unix 秒；解析失败返回 0，
// 让 Team 的 liveness 判定按“未检查”处理而不是误判为新鲜。
func parseStamp(value string) int64 {
	if value == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// accountReference 处理 /internal/work/v1/inventory/reference。
// 与 library 同属 work.inventory-relay.v1 协议：Work 用 InventoryDomain 签名，
// 不能用 /tasks 那套工作执行协议（字段集完全不同，会在 Verify 处 400）。
// Team 用这张签名证明锁定源账号：证明节点确实拥有该号、当前存活、未被占用。
// 整条路径只读，不产生任何业务写操作。
func (h *Handler) accountReference(c *gin.Context) {
	key := h.businessKey()
	if len(key) < 32 {
		writeErr(c, perr("WORK_INVENTORY_NODE_UNCONFIGURED", http.StatusServiceUnavailable))
		return
	}
	var doc map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxPayload)).Decode(&doc); err != nil {
		writeErr(c, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity))
		return
	}
	payload, err := verifyLibraryRequest(doc, h.cfg.Key, h.cfg.NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	data, _ := payload["data"].(map[string]any)
	kind := ls(data["kind"])
	if kind != "account" {
		writeErr(c, perr("WORK_ACCOUNT_REFERENCE_INVALID", http.StatusUnprocessableEntity))
		return
	}
	subject := ls(data["subject_id"])
	ref, ok := h.subjectRef(subject)
	if !ok {
		writeErr(c, perr("WORK_ACCOUNT_REFERENCE_INVALID", http.StatusUnprocessableEntity))
		return
	}
	account, err := h.st.GetAccount(ref)
	if err != nil || account.Ref == "" {
		writeErr(c, perr("WORK_ACCOUNT_REFERENCE_INVALID", http.StatusUnprocessableEntity))
		return
	}

	status := account.LibraryStatus
	// joined/switched 表示该号已进入某个 workspace，不能再被当作空闲源派单。
	joined := status == "used"
	version := account.Version
	if version < 1 {
		version = 1
	}
	stamp := time.Now().Unix()
	requestID := ls(payload["request_id"])
	body := map[string]any{
		"schema": referenceSchema, "node_id": h.cfg.NodeID,
		"request_id": requestID, "kind": "account",
		"subject_id":       SubjectID(account.Ref),
		"resource_version": version,
		"email":            account.Identity,
		// 源账号尚未进入任何 workspace，它自己的 workspace 是空的。
		// Team 比对 owner.workspace 用的是母号那一侧，不是这一侧。
		"workspace_account_id":  "",
		"enabled":               status == "available" || status == "pending",
		"busy":                  joined || status == "leased",
		"ever_used":             joined,
		"business_locked":       joined || status == "leased",
		"ever_attempted":        account.OAuthStatus != "" && account.OAuthStatus != "pending",
		"issued_at":             stamp,
		"expires_at":            stamp + 120,
		"liveness_status":       account.LivenessStatus,
		"liveness_checked_at":   parseStamp(account.LivenessCheckedAt),
		"unused_stock_eligible": status == "available",
	}
	raw, err := Canonical(body)
	if err != nil {
		writeErr(c, perr("WORK_INVENTORY_SIGN_FAILED", http.StatusInternalServerError))
		return
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ReferenceDomain))
	mac.Write(raw)
	c.JSON(http.StatusOK, gin.H{"payload": body, "signature": hex.EncodeToString(mac.Sum(nil))})
}
