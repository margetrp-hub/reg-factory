package nodeserver

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// Config 是节点身份与行为配置。
type Config struct {
	Enabled  bool
	NodeID   string
	Key      []byte
	Platform string // account.register 默认平台：claude/chatgpt/grok/...
}

// Handler 提供 Work 协议的入站端点。
type Handler struct {
	cfg Config
	mgr *runner.Manager
	st  *store.Store
}

// New 构造节点处理器。
func New(cfg Config, mgr *runner.Manager, st *store.Store) *Handler {
	return &Handler{cfg: cfg, mgr: mgr, st: st}
}

// Register 挂载节点端点（不走面板登录，靠 Work 签名鉴权）。
func (h *Handler) Register(r *gin.Engine) {
	if !h.cfg.Enabled || h.cfg.NodeID == "" || len(h.cfg.Key) < minKeyLength {
		log.Printf("[node] Work 子节点未启用（缺 NODE_ID/NODE_KEY>=32 字节）")
		return
	}
	log.Printf("[node] Work 子节点启用：node_id=%s platform=%s", h.cfg.NodeID, h.cfg.Platform)

	group := r.Group("/internal/work/v1")
	group.POST("/tasks", h.acceptTask)
	group.GET("/tasks/:task_id", h.taskStatus)
	group.POST("/tasks/:task_id/result", h.taskReceipt)
	group.POST("/inventory/:action", h.inventory)

	r.GET("/node/capabilities", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"ok":      true,
			"node_id": h.cfg.NodeID,
			"config":  h.cfg.Platform,
			"detail":  Describe(),
		})
	})
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
	localID := "work-" + verified.TaskID
	if operationID, _ := verified.Payload["operation_id"].(string); operationID != "" {
		existing, fresh, err := h.st.ReserveOperation(operationID, localID, verified.Action)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "IDEMPOTENCY_FAILED", "detail": err.Error()})
			return
		}
		if !fresh {
			c.JSON(http.StatusOK, gin.H{
				"accepted":  true,
				"duplicate": true,
				"node_id":   h.cfg.NodeID,
				"task_id":   verified.TaskID,
				"local_id":  existing,
				"action":    verified.Action,
			})
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
		c.JSON(http.StatusAccepted, gin.H{
			"accepted": true,
			"node_id":  h.cfg.NodeID,
			"task_id":  verified.TaskID,
			"local_id": task.ID,
			"action":   verified.Action,
		})
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
		c.JSON(http.StatusAccepted, gin.H{
			"accepted": true, "node_id": h.cfg.NodeID,
			"task_id": verified.TaskID, "local_id": task.ID, "action": verified.Action,
		})

	default:
		// 其余动作只登记不执行
		log.Printf("[node] note %s task=%s（v1 仅接受 account.register）", verified.Action, verified.TaskID)
		c.JSON(http.StatusAccepted, gin.H{
			"accepted": false,
			"reason":   "ACTION_NOT_IMPLEMENTED_IN_V1",
			"node_id":  h.cfg.NodeID,
			"task_id":  verified.TaskID,
			"action":   verified.Action,
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

// assetOpArgv 把 Work 动作映射成 asset_ops.py 的参数数组。
func (h *Handler) assetOpArgv(v *Verified) string {
	platform := strings.TrimSpace(h.cfg.Platform)
	argv := []string{}
	if v.Action == "account.refresh" {
		argv = append(argv, "refresh", "--ref", v.Subject)
	} else {
		argv = append(argv, "liveness", "--ref", v.Subject)
	}
	if platform != "" {
		argv = append(argv, "--platform", platform)
	}
	raw, _ := json.Marshal(argv)
	return string(raw)
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
	taskID := c.Param("task_id")
	task, err := h.st.GetTask(taskID)
	if err != nil {
		task, err = h.st.GetTask("work-" + taskID)
	}
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "NOT_FOUND", "task_id": taskID})
		return
	}
	status := task.Status
	if status == "running" || status == "queued" {
		c.JSON(http.StatusAccepted, gin.H{"pending": true, "status": status})
		return
	}
	doc, err := Receipt(taskID, h.cfg.NodeID, status, task.ExitCode,
		map[string]any{"local_id": task.ID}, h.cfg.Key, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, doc)
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
