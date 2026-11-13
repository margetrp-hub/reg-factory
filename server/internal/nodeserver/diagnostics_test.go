package nodeserver

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"aishop-next/execution-go/nodeassets"
	"github.com/gin-gonic/gin"
)

func TestUnconfiguredNodeExposesJSONDiagnostics(t *testing.T) {
	r := gin.New()
	New(Config{Version: "test-module-version"}, nil, nil).Register(r)
	for _, path := range []string{"/ready", "/node/capabilities"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s returned %d: %s", path, w.Code, w.Body.String())
		}
		if path == "/ready" && (body["ready"] != false || body["execution_ready"] != false) {
			t.Fatalf("unconfigured node claimed readiness: %v", body)
		}
		if path == "/ready" {
			runtime := body["runtime"].(map[string]any)
			sdk := runtime["node_sdk"].(map[string]any)
			if runtime["module_version"] != "test-module-version" || sdk["version"] != nodeassets.SDKVersion {
				t.Fatalf("runtime versions do not describe this node: %v", runtime)
			}
		}
	}
}
