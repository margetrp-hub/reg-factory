package nodeassets

import (
	_ "embed"
	"encoding/json"
	"errors"
	"sort"
)

//go:embed asset-profiles.v1.json
var profileJSON []byte

const SnapshotSchema = "aishop.asset-snapshot.v1"

type AssetProfile struct {
	Label    string   `json:"label"`
	Required []string `json:"required"`
	Optional []string `json:"optional"`
	Roles    []string `json:"roles"`
}
type SnapshotRejectedError struct{ Code string }

func (e *SnapshotRejectedError) Error() string { return e.Code }
func AssetProfiles() map[string]AssetProfile {
	var v struct {
		Profiles map[string]AssetProfile `json:"profiles"`
	}
	_ = json.Unmarshal(profileJSON, &v)
	return v.Profiles
}
func snapshotInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, n > 0 && n <= 2147483647
	case int64:
		return int(n), n > 0 && n <= 2147483647
	case float64:
		return int(n), n > 0 && n <= 2147483647 && n == float64(int(n))
	}
	return 0, false
}

// Only the transport envelope is required. Domain fields are all optional.
func MissingSnapshotFields(e map[string]any) []string {
	bad := []string{}
	if e["schema"] != SnapshotSchema {
		bad = append(bad, "schema")
	}
	ref, ok := e["source_ref"].(string)
	if !ok || len([]rune(ref)) < 1 || len([]rune(ref)) > 255 {
		bad = append(bad, "source_ref")
	}
	if _, ok := snapshotInt(e["source_revision"]); !ok {
		bad = append(bad, "source_revision")
	}
	if e["type"] != "account" && e["type"] != "mailbox" && e["type"] != "credential" {
		bad = append(bad, "type")
	}
	for k, limit := range map[string]int{"platform": 80, "kind": 80, "email": 190, "role": 80, "usage": 500} {
		if v, exists := e[k]; exists {
			s, ok := v.(string)
			if !ok || len([]rune(s)) > limit {
				bad = append(bad, k)
			}
		}
	}
	if _, ok := e["document"].(map[string]any); !ok {
		bad = append(bad, "document")
	}
	if state, exists := e["state"]; exists {
		if _, ok := state.(map[string]any); !ok {
			bad = append(bad, "state")
		}
	}
	sort.Strings(bad)
	return bad
}
func VerifySnapshotReceipt(request, response map[string]any, nodeID, accountID string) error {
	if entry, ok := request["entry"].(map[string]any); ok && request["action"] == "snapshot" &&
		response["state"] == "rejected" && response["task_id"] == request["task_id"] &&
		response["node_id"] == nodeID && response["source_ref"] == entry["source_ref"] {
		code, ok := response["code"].(string)
		if ok && len(code) <= 95 {
			valid := len(code) > 6 && code[:6] == "ASSET_"
			for _, r := range code {
				if r != '_' && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
					valid = false
				}
			}
			if valid {
				return &SnapshotRejectedError{Code: code}
			}
		}
	}
	share, ok := response["sharing"].(map[string]any)
	entry, _ := request["entry"].(map[string]any)
	id, valid := share["account_id"].(string)
	got, gok := snapshotInt(share["source_version"])
	want, wok := snapshotInt(entry["source_revision"])
	if !ok || !valid || id == "" || request["action"] != "snapshot" || response["state"] != "accepted" ||
		response["task_id"] != request["task_id"] || share["node_id"] != nodeID || share["source_ref"] != entry["source_ref"] ||
		share["state"] != "transferred" || !gok || !wok || got != want || (accountID != "" && id != accountID) {
		return errors.New("ASSET_HANDOFF_ACK_INVALID")
	}
	return nil
}
