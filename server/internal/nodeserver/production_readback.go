package nodeserver

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func (h *Handler) renewProductionReadback(c *gin.Context) {
	if c.GetHeader("Cookie") != "" || c.GetHeader("Origin") != "" {
		writeErr(c, perr("WORK_MACHINE_ONLY", 403))
		return
	}
	var doc Document
	if json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 120000)).Decode(&doc) != nil {
		writeErr(c, perr("PRODUCTION_READBACK_INVALID", 422))
		return
	}
	signed, err := signInventory(doc.Payload, h.cfg.Key, "work-production-readback-v1\n")
	if err != nil || !hmac.Equal([]byte(ls(signed["signature"])), []byte(doc.Signature)) {
		writeErr(c, perr("WORK_SIGNATURE_INVALID", 401))
		return
	}
	assignment, ok := doc.Payload["assignment"].(map[string]any)
	envelope, sealOK := doc.Payload["envelope"].(map[string]any)
	if len(doc.Payload) != 2 || !ok || !sealOK {
		writeErr(c, perr("PRODUCTION_READBACK_INVALID", 422))
		return
	}
	mac, _ := Sign(assignment, h.cfg.Key, false)
	v, err := Verify(Document{Payload: assignment, Signature: mac}, h.cfg.Key, h.cfg.NodeID, time.Now())
	if err != nil {
		writeErr(c, err)
		return
	}
	if v.Action != productionAction || v.Params["mode"] != "produce" {
		writeErr(c, perr("PRODUCTION_READBACK_INVALID", 403))
		return
	}
	material, err := h.openProductionMaterial(envelope, "team.work-production-readback.v1")
	if err != nil {
		writeErr(c, err)
		return
	}
	ctx, _ := material["context"].(map[string]any)
	renewal, _ := material["renewal"].(map[string]any)
	stamp := int(time.Now().Unix())
	if len(material) != 11 || material["action"] != productionAction || productionDigest(ctx) != material["context_sha256"] ||
		material["context_sha256"] != assignment["command_sha256"] || productionDigest(material["parameters"]) != productionDigest(v.Params) ||
		ctx["step_id"] != assignment["operation_id"] || ctx["attempt_id"] != assignment["attempt_id"] ||
		ctx["subject_id"] != v.Subject || typeInt(ctx["fence"]) != int(v.Fence) ||
		len(renewal) != 4 || !uuidLike.MatchString(ls(renewal["id"])) || typeInt(renewal["actor_id"]) < 1 ||
		len([]rune(ls(renewal["reason"]))) < 3 || len([]rune(ls(renewal["reason"]))) > 300 ||
		typeInt(renewal["issued_at"]) < stamp-900 || typeInt(renewal["issued_at"]) > stamp+30 ||
		typeInt(material["expires_at"]) <= stamp || typeInt(material["expires_at"]) > stamp+900 ||
		typeInt(material["readback_until"]) < typeInt(material["expires_at"]) ||
		typeInt(material["readback_until"]) > typeInt(renewal["issued_at"])+3600 {
		writeErr(c, perr("PRODUCTION_READBACK_SCOPE_CHANGED", 403))
		return
	}
	id := nodeTicket(v)
	prior, err := h.st.ProductionAdmission(id)
	if err != nil || prior.RequestSHA != productionScope(v) {
		writeErr(c, perr("PRODUCTION_REQUEST_CHANGED", 409))
		return
	}
	if err = h.st.StopProduction(id, productionScope(v), time.Now().Unix()); err != nil {
		writeErr(c, perr("PRODUCTION_STOP_CONFLICT", 409))
		return
	}
	if prior.State != "completed" && prior.State != "cancelled" && prior.State != "failed" {
		if !h.st.HasProductionRun(id) {
			writeErr(c, perr("PRODUCTION_STOP_PENDING", 503))
			return
		}
		runCtx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
		result := h.productionDriver(runCtx, map[string]any{"action": "renew_readback", "operation_id": id, "material": material})
		cancel()
		if result["renewed"] != true {
			writeErr(c, perr("PRODUCTION_READBACK_PENDING", 503))
			return
		}
	}
	c.JSON(200, gin.H{"id": id, "origin": "work", "renewed": true})
}
