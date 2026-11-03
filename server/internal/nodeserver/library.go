package nodeserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// LibraryDomain 是节点对 Team 签发账号库投影的域分隔符。
// Work 请求用的是 work-inventory-relay-v1；Team 独立验证节点响应（不信任转发），
// 所以响应必须用 account-node-library-v1 签，两者互不替代。
const LibraryDomain = "account-node-library-v1\n"

const (
	librarySchema = "account-node.library.v1"
	relayDomain   = "work-inventory-relay-v1\n"
)

func ls(v any) string {
	s, _ := v.(string)
	return s
}

func li(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// 条目用 map 而非 struct：Team 侧 canonical 做 sort_keys 重排，
// Go 的 struct 按字段声明顺序序列化，两者不一致会导致签名永远对不上。

// libraryStatus 把节点内部状态收敛到 Team 认可的五个取值。
// Team 只接受 available/pending/leased/used/invalid；其余一律 invalid，
// 让不可用的号在选号阶段就被排除，而不是派单后才失败。
func libraryStatus(oauth, liveness, join string) string {
	switch {
	case join == "joined" || join == "switched":
		return "used"
	case oauth == "completed" && liveness == "alive" && join == "none":
		return "available"
	case oauth == "pending" || oauth == "" || liveness == "unchecked":
		return "pending"
	case liveness == "dead" || oauth == "failed":
		return "invalid"
	default:
		return "invalid"
	}
}

// businessKey 返回 Team 侧配置给本节点的业务密钥（账号库投影签名用）。
func (h *Handler) businessKey() []byte { return h.cfg.BusinessKey }

// signLibrary 用 business_key 对 payload 做 HMAC-SHA256 签名。
func signLibrary(payload map[string]any, key []byte) (string, error) {
	body, err := Canonical(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(LibraryDomain))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// verifyLibraryRequest 校验 work 下发的 inventory relay 请求。
func verifyLibraryRequest(doc map[string]any, key []byte, nodeID string, now time.Time) (map[string]any, error) {
	sig, _ := doc["signature"].(string)
	payload, ok := doc["payload"].(map[string]any)
	if !ok || sig == "" {
		return nil, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity)
	}
	body, err := Canonical(payload)
	if err != nil {
		return nil, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(relayDomain))
	mac.Write(body)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(sig)) {
		return nil, perr("WORK_INVENTORY_SIGNATURE_INVALID", http.StatusForbidden)
	}
	if ls(payload["schema"]) != "work.inventory-relay.v1" || ls(payload["node_id"]) != nodeID {
		return nil, perr("WORK_INVENTORY_SCOPE_INVALID", http.StatusForbidden)
	}
	issued, ok1 := toInt64(payload["issued_at"])
	expires, ok2 := toInt64(payload["expires_at"])
	if !ok1 || !ok2 {
		return nil, perr("WORK_INVENTORY_REQUEST_INVALID", http.StatusUnprocessableEntity)
	}
	ts := now.Unix()
	if issued < ts-120 || issued > ts+30 || expires <= ts {
		return nil, perr("WORK_INVENTORY_EXPIRED", http.StatusForbidden)
	}
	return payload, nil
}

// accountLibrary 处理 /internal/work/v1/inventory/library。
// 只读账号元数据；不导入、不标记可复用、不授予执行权。
func (h *Handler) accountLibrary(c *gin.Context) {
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
	page := int(li(data["page"]))
	if page < 1 {
		page = 1
	}
	pageSize := int(li(data["page_size"]))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 50
	}
	wantStatus := ls(data["status"])

	// 账号库跨平台：节点代理 chatgpt/claude/grok 等多种账号，
	// 不能用 NodePlatform（单平台注册默认值）过滤，否则其余平台全部不可见。
	rows, total, err := h.st.LibraryAccounts(ls(data["q"]), wantStatus, "", page, pageSize)
	if err != nil {
		writeErr(c, perr("WORK_INVENTORY_LIBRARY_FAILED", http.StatusInternalServerError))
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		st := libraryStatus(a.OAuthStatus, a.LivenessStatus, a.JoinStatus)
		if wantStatus != "" && wantStatus != st {
			continue
		}
		ver := a.Version
		if ver < 1 {
			ver = 1
		}
		// source_id 必须是 UUID：Team 的 account reference 协议拿它当幂等键，
		// 非 UUID 会在取证明时被拒，生产单永远起不来。
		subject := SubjectID(a.Ref)
		items = append(items, map[string]any{
			"id": "account_center:" + subject, "source": "account_center", "source_id": subject,
			"email": a.Identity, "status": st, "resource_version": ver, "updated_at": a.UpdatedAt,
			"sources":         map[string]string{"account_center": subject},
			"oauth_status":    a.OAuthStatus, "join_status": a.JoinStatus,
			"liveness_status": a.LivenessStatus,
		})
	}

	stamp := time.Now().Unix()
	body := map[string]any{
		"schema": librarySchema, "node_id": h.cfg.NodeID, "request_id": ls(payload["request_id"]),
		"issued_at": stamp, "expires_at": stamp + 120,
		"query": map[string]any{
			"source": ls(data["source"]), "q": ls(data["q"]), "status": wantStatus,
			"page": page, "page_size": pageSize,
		},
		"items": items, "total": total, "page": page,
		"source_counts": map[string]int{"account_center": total},
	}
	sig, err := signLibrary(body, key)
	if err != nil {
		writeErr(c, perr("WORK_INVENTORY_SIGN_FAILED", http.StatusInternalServerError))
		return
	}
	c.JSON(http.StatusOK, gin.H{"payload": body, "signature": sig})
}
