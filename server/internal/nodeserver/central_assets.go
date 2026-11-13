package nodeserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/centralasset"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func centralBinding(v *Verified) bool { _, ok := v.Params["asset_ref"]; return ok }

func (h *Handler) centralReadiness() map[string]any {
	path := h.cfg.EnvFile
	if path == "" {
		path = settings.Load().EnvFile
	}
	env, err := settings.WorkerEnvironment(path)
	if err != nil {
		return map[string]any{"configured": false, "error_code": "ASSET_CONFIGURATION_UNREADABLE"}
	}
	config, err := centralasset.FromEnvironment(dataDir(), env)
	if err != nil {
		return map[string]any{"configured": false, "error_code": err.Error()}
	}
	if h.mgr == nil || h.mgr.CentralRuntimeCheck() != nil {
		return map[string]any{"configured": false, "error_code": "ASSET_RUNTIME_UNAVAILABLE"}
	}
	raw, _ := json.Marshal(config)
	sum := sha256.Sum256(raw)
	return map[string]any{"configured": true, "center_id": config.CenterID, "contract": 1, "configuration_revision": hex.EncodeToString(sum[:8])}
}

func (h *Handler) acceptCentral(c *gin.Context, v *Verified, localID string) {
	if h.centralReadiness()["configured"] != true {
		writeErr(c, perr("ASSET_CONFIGURATION_REQUIRED", 503))
		return
	}
	raw, _ := json.Marshal(v.Payload)
	args, _ := json.Marshal([]string{"--assignment", string(raw)})
	if old, err := h.st.GetTask(localID); err == nil {
		if old.Script != "central_asset" || old.ArgsJSON != string(args) {
			writeErr(c, perr("ASSET_TASK_SCOPE_CHANGED", 409))
			return
		}
		h.writeAccepted(c, v)
		return
	}
	task := store.Task{ID: localID, Script: "central_asset", Title: "中央资产 · " + v.Action, ArgsJSON: string(args), Actor: "work:" + h.config().NodeID}
	if err := h.mgr.Submit(task); err != nil {
		writeErr(c, perr("ASSET_TASK_UNAVAILABLE", 503))
		return
	}
	h.writeAccepted(c, v)
}

func (h *Handler) centralReceipt(v *Verified, key []byte) (map[string]any, error) {
	task, err := h.taskByTicket(nodeTicket(v))
	if err != nil {
		return nil, err
	}
	scope, err := runner.CentralContext(task)
	if err != nil {
		return nil, err
	}
	// Later read requests may extend their read authorization; original command
	// and attempt identity remain bound to the durable admission.
	for _, name := range []string{"node_id", "task_id", "attempt_id", "operation_id", "subject_id", "resource_version", "command_sha256"} {
		if scope[name] != v.Payload[name] {
			return nil, errors.New("ASSET_TASK_SCOPE_CHANGED")
		}
	}
	ref := scope["parameters"].(map[string]any)["asset_ref"].(map[string]any)
	state, delivery := "running", map[string]any{"state": "pending", "center_id": ref["center_id"]}
	var resultVersion any
	code := ""
	if value, e := h.st.CentralReceipt(task.ID); e == nil {
		code, _ = value["error_code"].(string)
		delivery["result_id"] = value["result_id"]
		if id, ok := value["result_id"].(string); ok && id != "" {
			delivery["execution_finished"] = true
		}
		switch value["state"] {
		case "completed", "failed":
			state = value["state"].(string)
			delivery["state"] = "acknowledged"
			resultVersion = value["credential_revision"]
		case "closed":
			state = "failed"
			delivery["state"] = "closed"
		case "unknown":
			state = "unknown"
			delivery["state"] = "unknown"
		}
	}
	result := map[string]any{"status": state, "error_code": code, "asset_delivery": delivery}
	raw, _ := Canonical(result)
	digest := sha256.Sum256(raw)
	p := map[string]any{}
	for _, name := range []string{"issuer", "node_id", "operation_id", "task_id", "attempt_id", "fence", "subject_id", "resource_version", "command_sha256"} {
		p[name] = scope[name]
	}
	p["schema"] = ReceiptSchema
	p["node_task_id"] = nodeTicket(v)
	p["status"] = state
	p["result"] = result
	p["result_sha256"] = hex.EncodeToString(digest[:])
	p["artifact_available"] = false
	p["result_account_id"] = nil
	p["result_resource_version"] = nil
	p["finished_at"] = nil
	if state == "completed" {
		p["result_account_id"] = scope["subject_id"]
		p["result_resource_version"] = resultVersion
	}
	if state != "running" {
		p["finished_at"] = task.FinishedAt
	}
	sig, err := Sign(p, key, false)
	return map[string]any{"payload": p, "signature": sig}, err
}

func (h *Handler) replayCentral(data map[string]any) (map[string]any, error) {
	if len(data) != 3 {
		return nil, perr("ASSET_REPLAY_SCOPE_INVALID", 403)
	}
	task, err := h.taskByTicket(ls(data["node_task_id"]))
	if err != nil {
		return nil, perr("ASSET_REPLAY_UNAVAILABLE", 404)
	}
	p, err := runner.CentralContext(task)
	if err != nil || p["attempt_id"] != data["attempt_id"] || p["operation_id"] != data["node_task_id"] {
		return nil, perr("ASSET_REPLAY_SCOPE_INVALID", 403)
	}
	if p["parameters"].(map[string]any)["asset_ref"].(map[string]any)["center_id"] != data["center_id"] {
		return nil, perr("ASSET_REPLAY_SCOPE_INVALID", 403)
	}
	if task.Status == "running" || task.Status == "queued" {
		return nil, perr("ASSET_RESULT_NOT_AVAILABLE", 409)
	}
	_ = h.mgr.ReplayCentral(task)
	v := &Verified{Payload: p, Action: ls(p["action"]), Subject: ls(p["subject_id"]), Params: p["parameters"].(map[string]any)}
	return h.ReceiptFor(v, ls(data["node_task_id"]), task.Status, task.ExitCode, h.receiptKey(), time.Now())
}
