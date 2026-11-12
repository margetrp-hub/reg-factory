package nodeassets

import (
	"errors"
	"regexp"
)

const SDKVersion = "1.0.0"

func runtimeError() error { return errors.New("SDK_CONFIGURATION_INVALID") }

// Runtime reports only actually applied material revisions supplied by the host.
// It does not install configuration or infer that an external action succeeded.
func Runtime(center, revision string, materials []map[string]any) (map[string]any, error) {
	if len(materials) > 8 {
		return nil, runtimeError()
	}
	seen := map[string]bool{}
	for _, item := range materials {
		kind, k := item["kind"].(string)
		digest, d := item["digest"].(string)
		v, n := item["revision"].(int)
		if len(item) != 3 || !k || !d || !n || v < 1 || v > 2147483647 || seen[kind] ||
			!regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`).MatchString(kind) ||
			!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(digest) {
			return nil, runtimeError()
		}
		seen[kind] = true
	}
	if materials == nil {
		materials = []map[string]any{}
	}
	value := map[string]any{"node_sdk": map[string]any{"version": SDKVersion, "contract": 1,
		"capabilities": []string{"asset_claim", "durable_result", "scoped_ack", "repair_observation", "runtime_versions"}}, "material_versions": materials}
	if center != "" || revision != "" {
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`).MatchString(center) ||
			!regexp.MustCompile(`^[a-f0-9]{16}$`).MatchString(revision) {
			return nil, runtimeError()
		}
		value["central_assets"] = map[string]any{"configured": true, "contract": 1, "center_id": center, "configuration_revision": revision}
	}
	return value, nil
}
