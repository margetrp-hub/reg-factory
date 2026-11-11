package nodeserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// Config 是节点身份与行为配置。
type Config struct {
	Enabled bool
	NodeID  string
	Key     []byte
	// ReceiptKey 签发回执用的密钥；为空时回退 Key（与 Team 侧 read-only 域一致）
	ReceiptKey []byte
	// BusinessKey 账号库投影签名密钥（与 Work 请求密钥分离）
	BusinessKey []byte
	// ArtifactKey 产物（凭据文件）加密导出用的 Fernet key（base64url 32 字节）
	ArtifactKey []byte
	Platform    string // account.register 默认平台：claude/chatgpt/grok/...
}

// receiptKey 返回签发回执使用的密钥（未单独配置时复用请求密钥）。
func (h *Handler) receiptKey() []byte {
	if len(h.cfg.ReceiptKey) >= 32 {
		return h.cfg.ReceiptKey
	}
	return h.cfg.Key
}

// Handler 提供 Work 协议的入站端点。
type Handler struct {
	cfg              Config
	mgr              *runner.Manager
	st               *store.Store
	productionCheck  func(map[string]any) map[string]any
	productionMu     sync.Mutex
	productionActive map[string]chan struct{}
}

// New 构造节点处理器。
func New(cfg Config, mgr *runner.Manager, st *store.Store) *Handler {
	return &Handler{cfg: cfg, mgr: mgr, st: st}
}

// Register 挂载节点端点（不走面板登录，靠 Work 签名鉴权）。
func (h *Handler) Register(r *gin.Engine) {
	if h.st != nil {
		if err := h.st.EnsureProduction(); err != nil {
			panic("production storage unavailable")
		}
		if err := h.st.EnsureProductionRuns(); err != nil {
			panic("production run storage unavailable")
		}
	}
	enabled := h.cfg.Enabled && h.cfg.NodeID != "" && len(h.cfg.Key) >= minKeyLength
	if !enabled {
		log.Printf("[node] Work 子节点未启用（缺 NODE_ID/NODE_KEY>=32 字节）")
	} else {
		log.Printf("[node] Work 子节点启用：node_id=%s platform=%s", h.cfg.NodeID, h.cfg.Platform)
	}

	// Work probes this unauthenticated endpoint before it projects the node's
	// inventory or submits a task.  Keep it separate from the panel fallback:
	// returning HTML here makes a healthy node look unavailable forever.
	r.GET("/ready", func(c *gin.Context) {
		actions := []string{"account.register", "account.authorize", "account.refresh", "account.liveness", "asset.liveness", "account.quota"}
		productionError := h.productionReadiness()
		if enabled && productionError == "" {
			actions = append(actions, productionAction, "production.team_json.existing_result", "production.team_json.produce")
		}
		c.JSON(http.StatusOK, gin.H{
			"ready":            enabled,
			"execution_ready":  enabled,
			"action_readiness": gin.H{productionAction: gin.H{"ready": enabled && productionError == "", "error_code": productionError, "processing_owner": "node_admin"}},
			"runtime": gin.H{
				"supported_actions": actions,
				"protocol_capabilities": []string{
					"account-node.work.v1",
					"account-node.receipt.v1",
					"work.inventory-relay.v1",
					"account-node.accounts.v1",
					productionResumeProtocol,
					productionAdmitProtocol,
					productionMaterialSchema,
				},
			},
		})
	})

	r.GET("/node/capabilities", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"node_id": h.cfg.NodeID,
			"config":  h.cfg.Platform,
			"detail":  Describe(),
			// 节点侧自报能力：Team 按此派单，不需要为节点改协议
			"schema": "account-node.work.v1",
			// 密钥指纹（sha256 前 8 位）：只为对齐 Team 侧配置，不泄露密钥
			"business_key_fp": fingerprint(h.cfg.BusinessKey),
			"receipt_key_fp":  fingerprint(h.receiptKey()),
			"work_key_fp":     fingerprint(h.cfg.Key),
			"actions": gin.H{
				"supported": []string{
					"account.register", "account.authorize", "account.refresh",
					"account.liveness", "asset.liveness", "account.quota",
					productionAction, "production.team_json.existing_result", "production.team_json.produce",
				},
				"unsupported": []string{
					"account.join", "account.verify_asset", "team.seat_price",
				},
				"notes": gin.H{
					"account.authorize": "走本地凭据修复通道（asset_ops refresh --allow-relogin）",
					"account.quota":     "同步返回本节点可用账号数，不落任务",
					"account.join":      "本节点没有 workspace 概念，不实现",
					"team.seat_price":   "席位价格属于 Team 业务事实，节点不掌握",
				},
			},
		})
	})
	if !enabled {
		return
	}
	group := r.Group("/internal/work/v1")
	group.POST("/tasks", h.acceptTask)
	group.POST("/production/resume", h.resumeProduction)
	group.POST("/production/admit", h.resumeProduction)
	group.POST("/production/continue", h.continueProduction)
	group.POST("/production/readback", h.renewProductionReadback)
	group.POST("/team/materials", h.productionMaterial)
	group.POST("/stop", h.stopProduction)
	group.GET("/tasks/:task_id", h.taskStatus)
	group.POST("/tasks/:task_id/result", h.taskReceipt)
	group.POST("/tasks/:task_id/events", h.executionEvents)
	group.POST("/tasks/:task_id/artifact", h.workArtifact)
	group.POST("/resolve", h.resolveTask)
	group.POST("/inventory/library", h.accountLibrary)
	group.POST("/inventory/reference", h.accountReference)
	group.GET("/accounts", h.accountsInventory)
	group.GET("/inventory/accounts", h.accountsInventory)
	group.POST("/inventory/:action", h.inventoryRelay)
	if err := h.st.EnsureInventoryRelay(); err != nil {
		log.Printf("[node] inventory relay schema unavailable: %v", err)
	}
	r.GET("/node/library/selftest", h.librarySelftest)
}

// acceptTask 接收 account.register 等动作并转成本地任务。
func (h *Handler) acceptTask(c *gin.Context) {
	if c.GetHeader("cookie") != "" || c.GetHeader("Origin") != "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "WORK_MACHINE_ONLY"})
		return
	}
	var doc Document
	if err := c.ShouldBindJSON(&doc); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WORK_REQUEST_INVALID", "detail": err.Error()})
		return
	}
	verified, err := Verify(doc, h.cfg.Key, h.cfg.NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}

	// 幂等：同一 operation_id 只生产一次
	if verified.Action == productionAction {
		h.acceptProduction(c, verified)
		return
	}
	localID := "work-" + verified.TaskID
	// Legacy non-production rows do not retain a complete assignment. Do not
	// manufacture its original scope from a later read or duplicate submission.
	_, existing := h.st.OperationTask(nodeTicket(verified))
	_, _, bound := h.st.WorkEventScope(nodeTicket(verified))
	if existing != nil && existing != sql.ErrNoRows || bound != nil && bound != sql.ErrNoRows {
		writeErr(c, perr("WORK_EVENTS_UNAVAILABLE", 503))
		return
	}
	if existing == sql.ErrNoRows || bound == nil {
		if err := h.bindExecutionEvents(verified, localID); err != nil {
			writeErr(c, err)
			return
		}
	}
	if verified.Action == "account.quota" {
		operationID, _ := verified.Payload["operation_id"].(string)
		version, _ := toInt64(verified.Payload["resource_version"])
		if err := h.st.RecordQuotaObservation(operationID, localID, h.cfg.Platform, int(version)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "QUOTA_SNAPSHOT_FAILED"})
			return
		}
		h.writeAccepted(c, verified)
		return
	}
	if operationID, _ := verified.Payload["operation_id"].(string); operationID != "" {
		inputVersion, _ := toInt64(verified.Payload["resource_version"])
		_, fresh, err := h.st.ReserveOperation(operationID, localID, verified.Action, int(inputVersion))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "IDEMPOTENCY_FAILED", "detail": err.Error()})
			return
		}
		if !fresh {
			// A retry after a lost response must be indistinguishable from the
			// original acceptance to Work.  The duplicate marker is deliberately
			// omitted: Submit validates the compact ACK contract exactly.
			h.writeAccepted(c, verified)
			return
		}
	}

	switch verified.Action {
	case "account.register":
		script, args := h.registerTask(verified.Subject)
		task := store.Task{
			ID:       "work-" + verified.TaskID,
			Script:   script,
			Title:    "节点生产 · " + verified.Subject,
			ArgsJSON: args,
			Actor:    "work:" + h.cfg.NodeID,
		}
		if err := h.mgr.Submit(task); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "submit_failed", "detail": err.Error()})
			return
		}
		log.Printf("[node] accept %s task=%s subject=%s -> %s", verified.Action, verified.TaskID, verified.Subject, task.ID)
		h.writeAccepted(c, verified)
	case "account.authorize":
		// 授权 = 重新登录/换凭据，走本地 refresh 通道（带 --allow-relogin）
		argvTail := h.authorizeArgv(verified)
		task := store.Task{
			ID:       "work-" + verified.TaskID,
			Script:   "asset_ops",
			Title:    "节点授权 · " + verified.Subject,
			ArgsJSON: argvTail,
			Actor:    "work:" + h.cfg.NodeID,
		}
		if err := h.mgr.Submit(task); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "submit_failed", "detail": err.Error()})
			return
		}
		log.Printf("[node] accept %s task=%s subject=%s", verified.Action, verified.TaskID, verified.Subject)
		h.writeAccepted(c, verified)

	case "account.liveness", "asset.liveness", "account.refresh":
		argvTail := h.assetOpArgv(verified)
		task := store.Task{
			ID:       "work-" + verified.TaskID,
			Script:   "asset_ops",
			Title:    "节点" + verified.Action + " · " + verified.Subject,
			ArgsJSON: argvTail,
			Actor:    "work:" + h.cfg.NodeID,
		}
		if err := h.mgr.Submit(task); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "submit_failed", "detail": err.Error()})
			return
		}
		log.Printf("[node] accept %s task=%s subject=%s", verified.Action, verified.TaskID, verified.Subject)
		h.writeAccepted(c, verified)

	default:
		// 其余动作只登记不执行
		log.Printf("[node] note %s task=%s（本节点不实现该动作，见 /node/capabilities）", verified.Action, verified.TaskID)
		c.JSON(http.StatusAccepted, gin.H{
			"accepted":     false,
			"reason":       "ACTION_NOT_SUPPORTED_BY_NODE",
			"capabilities": "/node/capabilities",
			"node_id":      h.cfg.NodeID,
			"task_id":      verified.TaskID,
			"action":       verified.Action,
		})
	}
}

// registerTask 把 mailbox 主体映射成已有邮箱注册任务。
func (h *Handler) registerTask(mailbox string) (string, string) {
	platform := strings.TrimSpace(h.cfg.Platform)
	if platform == "" {
		platform = "claude"
	}
	// subject_id 若是邮箱地址就直接用；否则（mailbox UUID）交给脚本从邮箱池取号
	if strings.Contains(mailbox, "@") {
		args := `{"--email":"` + escapeJSON(mailbox) + `","--platforms":"` + escapeJSON(platform) + `"}`
		return "register_three_platforms", args
	}
	args := `{"--from-pool":true,"--platforms":"` + escapeJSON(platform) + `"}`
	return "register_three_platforms", args
}

// authorizeArgv 把 account.authorize 映射成 asset_ops refresh（允许重新登录）。
func (h *Handler) authorizeArgv(v *Verified) string {
	platform := strings.TrimSpace(h.cfg.Platform)
	ref := v.Subject
	if resolved, ok := h.subjectRef(ref); ok {
		ref = resolved
	}
	argv := []string{"refresh", "--ref", ref, "--allow-relogin"}
	if email := h.subjectEmail(v); email != "" {
		argv = append(argv, "--email", email)
	}
	if platform != "" {
		argv = append(argv, "--platform", platform)
	}
	raw, _ := json.Marshal(argv)
	return string(raw)
}

// assetOpArgv 把 Work 动作映射成 asset_ops.py 的参数数组。
func (h *Handler) assetOpArgv(v *Verified) string {
	platform := strings.TrimSpace(h.cfg.Platform)
	ref := v.Subject
	if resolved, ok := h.subjectRef(ref); ok {
		ref = resolved
	}
	argv := []string{}
	if v.Action == "account.refresh" {
		argv = append(argv, "refresh", "--ref", ref)
	} else {
		argv = append(argv, "liveness", "--ref", ref)
	}
	if email := h.subjectEmail(v); email != "" {
		argv = append(argv, "--email", email)
	}
	if platform != "" {
		argv = append(argv, "--platform", platform)
	}
	raw, _ := json.Marshal(argv)
	return string(raw)
}

// subjectEmail keeps credential lookup on the node side.  Work deliberately
// sends a stable subject UUID; the node already owns the local ref -> email
// mapping and must not require credentials in the signed command itself.
func (h *Handler) subjectEmail(v *Verified) string {
	if v == nil {
		return ""
	}
	if email, ok := v.Params["email"].(string); ok && strings.Contains(email, "@") {
		return strings.TrimSpace(email)
	}
	ref, ok := h.subjectRef(v.Subject)
	if !ok {
		return ""
	}
	account, err := h.st.GetAccount(ref)
	if err != nil || !strings.Contains(account.Identity, "@") {
		return ""
	}
	return strings.TrimSpace(account.Identity)
}

func escapeJSON(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", "", "\r", "")
	return replacer.Replace(value)
}

func (h *Handler) taskStatus(c *gin.Context) {
	taskID := c.Param("task_id")
	// 既支持 work task_id（带 work- 前缀）也支持本地 id
	task, err := h.st.GetTask(taskID)
	if err != nil {
		task, err = h.st.GetTask("work-" + taskID)
	}
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "NOT_FOUND", "task_id": taskID})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"task_id":     taskID,
		"local_id":    task.ID,
		"status":      task.Status,
		"exit_code":   task.ExitCode,
		"started_at":  task.StartedAt,
		"finished_at": task.FinishedAt,
	})
}

// taskReceipt 返回签名回执（Team/Work 主动来取的模式）。
func (h *Handler) taskReceipt(c *gin.Context) {
	ticket := c.Param("task_id")
	var doc Document
	if err := c.ShouldBindJSON(&doc); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WORK_REQUEST_INVALID"})
		return
	}
	verified, err := VerifyRead(doc, h.cfg.Key, h.cfg.NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	if nodeTicket(verified) != ticket {
		writeErr(c, perr("WORK_RESULT_SCOPE_MISMATCH", http.StatusForbidden))
		return
	}
	task, err := h.taskByTicket(ticket)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "NOT_FOUND", "task_id": ticket})
		return
	}
	h.restoreProductionEventScope(verified, task)
	// Pending work is still a signed receipt.  Returning HTTP 202 with an
	// unsigned marker makes NodeClient classify every poll as a transport
	// failure and can strand a valid task indefinitely.
	result, err := h.ReceiptFor(verified, ticket, task.Status, task.ExitCode, h.receiptKey(), time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// resolveTask recovers a lost acceptance response without submitting the
// operation again.  The read assignment carries the same operation scope as
// the original request; taskByTicket only looks up the local record.
func (h *Handler) resolveTask(c *gin.Context) {
	var doc Document
	if err := c.ShouldBindJSON(&doc); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WORK_REQUEST_INVALID"})
		return
	}
	verified, err := VerifyRead(doc, h.cfg.Key, h.cfg.NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	ticket := nodeTicket(verified)
	task, err := h.taskByTicket(ticket)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "NOT_FOUND", "task_id": ticket})
		return
	}
	h.restoreProductionEventScope(verified, task)
	result, err := h.ReceiptFor(verified, ticket, task.Status, task.ExitCode, h.receiptKey(), time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// inventory v1 只做幂等登记（成品入库仍由 Team 侧审批流处理）。
func (h *Handler) inventory(c *gin.Context) {
	action := c.Param("action")
	var doc Document
	if err := c.ShouldBindJSON(&doc); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "WORK_REQUEST_INVALID"})
		return
	}
	if _, err := Verify(doc, h.cfg.Key, h.cfg.NodeID, time.Now()); err != nil {
		writeErr(c, err)
		return
	}
	log.Printf("[node] inventory action=%s accepted (v1 register-only)", action)
	c.JSON(http.StatusAccepted, gin.H{"accepted": true, "action": action, "node_id": h.cfg.NodeID})
}

func writeErr(c *gin.Context, err error) {
	if proto, ok := err.(*ProtocolError); ok {
		c.JSON(proto.Status, gin.H{"error": proto.Code})
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "WORK_REQUEST_INVALID", "detail": err.Error()})
}

// fingerprint 返回密钥的 sha256 前缀，用于和 Team 侧核对是否同一把钥匙。
func fingerprint(key []byte) string {
	if len(key) == 0 {
		return "unset"
	}
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:4])
}

// librarySelftest 用自身 business key 生成签名并自验，再返回账号库里可派单的数量。
func (h *Handler) librarySelftest(c *gin.Context) {
	now := time.Now()
	payload := map[string]any{
		"schema":     "work.inventory-relay.v1",
		"node_id":    h.cfg.NodeID,
		"request_id": "selftest-" + strconv.FormatInt(now.UnixNano(), 36),
		"issued_at":  now.Unix(),
		"expires_at": now.Unix() + 120,
		"data": map[string]any{
			"q": "", "status": "", "page": int64(1), "page_size": int64(50),
			"source": "account_center",
		},
	}
	raw, err := Canonical(payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "canonical_failed", "detail": err.Error()})
		return
	}
	key := h.businessKey()
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(relayDomain))
	mac.Write(raw)
	sig := hex.EncodeToString(mac.Sum(nil))

	if _, verifyErr := verifyLibraryRequest(
		map[string]any{"signature": sig, "payload": payload}, key, h.cfg.NodeID, now,
	); verifyErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "selftest_failed", "detail": verifyErr.Error()})
		return
	}

	rows, total, listErr := h.st.LibraryAccounts("", "", "", 1, 50)
	if listErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "library_failed", "detail": listErr.Error()})
		return
	}
	emails := make([]string, 0, len(rows))
	for _, account := range rows {
		if len(emails) < 12 {
			emails = append(emails, account.Identity)
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"ok": true, "schema": "work.inventory-relay.v1",
		"node_id": h.cfg.NodeID, "signature_verified": true,
		"total": total, "sample": emails,
		"business_key_fp": fingerprint(key),
	})
}
