package nodeserver

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestCentralLibraryOnlyReportsTechnicalAvailabilityAndWithdrawsHandoffs(t *testing.T) {
	st := centralJourneyStore(t, t.TempDir())
	if _, err := st.UpsertAccounts([]store.AccountSeed{{Ref: "available-source", Platform: "chatgpt", Kind: "oauth", Identity: "fixture@example.test"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkHealth("available-source", "ok", "synthetic check"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkClaimed("available-source"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Execer().Exec("UPDATE accounts SET join_status='joined' WHERE ref=?", "available-source"); err != nil {
		t.Fatal(err)
	}
	key := []byte("synthetic-library-key-01234567890123456789")
	h := New(Config{Enabled: true, NodeID: "sg-test", Key: key, BusinessKey: key}, nil, st)
	router := gin.New()
	h.Register(router)
	stamp := time.Now().Unix()
	payload := map[string]any{"schema": "work.inventory-relay.v1", "node_id": "sg-test", "request_id": "query-original",
		"issued_at": stamp, "expires_at": stamp + 120, "data": map[string]any{"source": "account_center", "q": "", "status": "available", "page": 1, "page_size": 25}}
	raw, _ := Canonical(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(relayDomain))
	mac.Write(raw)
	raw, _ = json.Marshal(map[string]any{"payload": payload, "signature": hex.EncodeToString(mac.Sum(nil))})
	read := func() map[string]any {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", "/internal/work/v1/inventory/library", bytes.NewReader(raw)))
		if w.Code != 200 {
			t.Fatalf("library status %d: %s", w.Code, w.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result["payload"].(map[string]any)
	}
	value := read()
	if value["total"] != float64(1) {
		t.Fatalf("node history decided eligibility: %v", value)
	}
	row := value["items"].([]any)[0].(map[string]any)
	for _, field := range []string{"join_status", "claimed", "used", "leased", "history"} {
		if _, ok := row[field]; ok {
			t.Fatalf("node exported business fact %s", field)
		}
	}
	if row["status"] != "available" {
		t.Fatal(row)
	}
	if err := st.WithdrawAccount("available-source"); err != nil {
		t.Fatal(err)
	}
	if value = read(); value["total"] != float64(0) {
		t.Fatalf("handoff remained available: %v", value)
	}
}
