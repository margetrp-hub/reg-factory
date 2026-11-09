package nodeserver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A task receipt is immutable even after the account is reused. Current-account
// reads belong to the credential store, never to an old task artifact.
func (h *Handler) taskCredential(v *Verified) map[string]any {
	task, err := h.taskByTicket(nodeTicket(v))
	if err != nil {
		return nil
	}
	name := fmt.Sprintf("%x.json", sha256.Sum256([]byte(task.ID)))
	raw, err := os.ReadFile(filepath.Join(dataDir(), "credential-task-results", name))
	if err != nil {
		return nil
	}
	var result struct {
		TaskID   string         `json:"task_id"`
		Ref      string         `json:"ref"`
		Email    string         `json:"email"`
		Document map[string]any `json:"document"`
	}
	if json.Unmarshal(raw, &result) != nil || result.TaskID != task.ID {
		return nil
	}
	ref, found := h.subjectRef(v.Subject)
	if !found || result.Ref != ref || !strings.EqualFold(result.Email, h.resolveEmail(v)) {
		return nil
	}
	return normalizeDocument(result.Document, result.Email)
}
