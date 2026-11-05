package nodeserver

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestUnconfiguredNodeExposesJSONDiagnostics(t *testing.T) {
	r := gin.New()
	New(Config{}, nil, nil).Register(r)
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
	}
}
