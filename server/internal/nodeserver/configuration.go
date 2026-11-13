package nodeserver

import (
	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"strings"
)

// Config is immutable startup state; runtime values come from a file snapshot.
// Tests and embedded users without an env file retain the explicit Config.
func (h *Handler) config() Config {
	if h.resolved != nil {
		return *h.resolved
	}
	c := h.cfg
	if c.EnvFile == "" {
		return c
	}
	values, err := settings.WorkerEnvironment(c.EnvFile)
	if err != nil {
		c.Enabled = false
		return c
	}
	for _, entry := range values {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "REG_FACTORY_NODE_PLATFORM":
			c.Platform = value
		case "REG_FACTORY_NODE_KEY":
			c.Key = []byte(value)
		case "REG_FACTORY_NODE_RECEIPT_KEY":
			c.ReceiptKey = []byte(value)
		case "REG_FACTORY_NODE_BUSINESS_KEY":
			c.BusinessKey = []byte(value)
		case "REG_FACTORY_NODE_ARTIFACT_KEY":
			c.ArtifactKey = []byte(value)
		}
	}
	c.Enabled = c.NodeID != "" && len(c.Key) >= minKeyLength
	return c
}

func (h *Handler) snapshot() *Handler {
	c := h.config()
	return &Handler{cfg: h.cfg, resolved: &c, mgr: h.mgr, st: h.st, productionCheck: h.productionCheck,
		productionMu: h.productionMu, productionActive: h.productionActive}
}

func (h *Handler) withConfig(handle func(*Handler, *gin.Context)) gin.HandlerFunc {
	return func(c *gin.Context) { handle(h.snapshot(), c) }
}

func (h *Handler) artifactKeys() [][]byte {
	c := h.config()
	keys := [][]byte{c.ArtifactKey}
	if c.EnvFile == "" {
		return keys
	}
	old, err := settings.KeyHistory(c.EnvFile, "REG_FACTORY_NODE_ARTIFACT_KEY")
	if err != nil {
		return keys
	}
	for _, key := range old {
		keys = append(keys, []byte(key))
	}
	return keys
}

func (h *Handler) replayInventoryResponse(raw, domain string) (map[string]any, error) {
	response, err := decodeStoredResponse(raw)
	if err != nil {
		return nil, err
	}
	payload, ok := response["payload"].(map[string]any)
	if !ok {
		return nil, perr("WORK_INVENTORY_RESULT_INVALID", 503)
	}
	if sealed, ok := payload["artifact_encrypted"].(string); ok && sealed != "" {
		var document []byte
		needsRotation := false
		for index, key := range h.artifactKeys() {
			document, err = fernetDecrypt(key, []byte(sealed), 0)
			if err == nil {
				needsRotation = index > 0
				break
			}
		}
		if err != nil {
			return nil, perr("WORK_INVENTORY_RESULT_UNREADABLE", 503)
		}
		if needsRotation {
			payload["artifact_encrypted"], err = fernetEncrypt(h.config().ArtifactKey, document)
			if err != nil {
				return nil, err
			}
		}
	}
	return signInventory(payload, h.config().BusinessKey, domain)
}
