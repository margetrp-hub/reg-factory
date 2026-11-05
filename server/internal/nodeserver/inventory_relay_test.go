package nodeserver

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func hmacSHA256(key []byte, domain string, payload []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestInventoryRelayRouteUsesRelayEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := store.Open(t.TempDir() + "/node.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Setenv("REG_FACTORY_DATA_DIR", t.TempDir())
	key := bytes.Repeat([]byte("k"), 32)
	h := New(Config{Enabled: true, NodeID: "sg", Key: key, BusinessKey: key, ArtifactKey: key}, nil, db)
	router := gin.New()
	h.Register(router)
	now := time.Now().Unix()
	payload := map[string]any{
		"schema": "work.inventory-relay.v1", "node_id": "sg", "request_id": "request-1",
		"issued_at": now, "expires_at": now + 120, "action": "offer",
		"data": map[string]any{"cursor": "", "limit": int64(1)},
	}
	raw, err := Canonical(payload)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmacSHA256(key, relayDomain, raw)
	body, _ := json.Marshal(map[string]any{"payload": payload, "signature": mac})
	req := httptest.NewRequest(http.MethodPost, "/internal/work/v1/inventory/offer", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("relay route status=%d body=%s", resp.Code, resp.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["signature"] == nil {
		t.Fatalf("relay response missing signature: %s", resp.Body.String())
	}
}

func TestAccountsSyncCompatibilityRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := store.Open(t.TempDir() + "/node.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Setenv("REG_FACTORY_DATA_DIR", t.TempDir())
	imported := filepath.Join(os.Getenv("REG_FACTORY_DATA_DIR"), "assets", "chatgpt", "imported")
	if err := os.MkdirAll(imported, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imported, "account.json"), []byte(`{"email":"a@example.com","access_token":"access","refresh_token":"refresh"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte("k"), 32)
	h := New(Config{Enabled: true, NodeID: "sg", Key: key, BusinessKey: key, ArtifactKey: key}, nil, db)
	router := gin.New()
	h.Register(router)
	for _, path := range []string{"/internal/work/v1/accounts?limit=500", "/internal/work/v1/inventory/accounts?limit=500"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("accounts route %s status=%d body=%s", path, resp.Code, resp.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
			t.Fatalf("accounts route %s returned invalid JSON: %v", path, err)
		}
		if result["schema"] != "account-node.accounts.v1" {
			t.Fatalf("accounts route %s schema=%v", path, result["schema"])
		}
		if result["total"] != float64(1) {
			t.Fatalf("accounts route %s total=%v", path, result["total"])
		}
	}
}

func TestInventoryFernetRoundTrip(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	plain := []byte(`{"schema":"account-node.team-handoff-artifact.v1","email":"a@example.com"}`)
	token, err := fernetEncrypt(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := fernetDecrypt(key, []byte(token), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded) != string(plain) {
		t.Fatalf("plaintext changed: %q", decoded)
	}
	if _, err := fernetDecrypt([]byte("bad"), []byte(token), time.Minute); err == nil {
		t.Fatal("invalid key was accepted")
	}
}

func TestInventoryDigestIgnoresRelayTimestamps(t *testing.T) {
	payload := map[string]any{
		"schema": "work.inventory-offer.v1", "node_id": "sg", "event_id": "event-1",
		"issued_at": int64(100), "expires_at": int64(200), "state": "prepared",
	}
	first, err := inventoryDigest(payload)
	if err != nil {
		t.Fatal(err)
	}
	payload["issued_at"], payload["expires_at"] = int64(101), int64(201)
	second, err := inventoryDigest(payload)
	if err != nil || first != second {
		t.Fatalf("relay timestamps changed digest: %q %q %v", first, second, err)
	}
}

func TestInventoryCommitEventIsIdempotent(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/node.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.UpsertAccounts([]store.AccountSeed{{Ref: "import:a@example.com", Platform: "chatgpt", Kind: "oauth", Identity: "a@example.com"}}); err != nil {
		t.Fatal(err)
	}
	response := `{"payload":{"status":"completed"},"signature":"` + strings.Repeat("a", 64) + `"}`
	if _, replay, err := db.CommitInventoryEvent("event-1", "digest-1", response, "sg", "00000000-0000-4000-8000-000000000001", "import:a@example.com", "prepared", 1, 1); err != nil || replay {
		t.Fatalf("first commit replay=%v err=%v", replay, err)
	}
	saved, replay, err := db.CommitInventoryEvent("event-1", "digest-1", `{"different":true}`, "sg", "00000000-0000-4000-8000-000000000001", "import:a@example.com", "stocked", 2, 2)
	if err != nil || !replay || saved != response {
		t.Fatalf("replay=%v saved=%q err=%v", replay, saved, err)
	}
	account, err := db.GetAccount("import:a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if account.Version != 2 || !account.Claimed || account.JoinStatus != "joined" {
		t.Fatalf("account advanced incorrectly: %+v", account)
	}
	var decoded map[string]any
	if json.Unmarshal([]byte(saved), &decoded) != nil {
		t.Fatal("saved response is not JSON")
	}
}
