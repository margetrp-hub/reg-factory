package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/nodeserver"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func TestAccountLibraryJourneyFromATCheckThroughUnknownRecovery(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	assets := assetstore.New(root)
	refs := []string{}
	for i := 0; i < 10; i++ {
		ref := fmt.Sprintf("import:chatgpt:account-%d@example.test", i)
		refs = append(refs, ref)
		if _, err = st.UpsertAccounts([]store.AccountSeed{{Ref: ref, Platform: "chatgpt", Kind: "oauth", Identity: ref[7:]}}); err != nil {
			t.Fatal(err)
		}
		if err = assets.Put(assetstore.Asset{Ref: ref, Platform: "chatgpt", Kind: "oauth", Payload: map[string]any{"access_token": fmt.Sprint("fixture-", i), "refresh_token": "retained-rt"}}); err != nil {
			t.Fatal(err)
		}
	}
	uncertain := true
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/backend-api/wham/usage" {
			t.Error("unexpected external side effect", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		if uncertain && r.Header.Get("Authorization") == "Bearer fixture-4" {
			w.Write([]byte("not-json"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"plan_type":"free"}`))
	}))
	defer upstream.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // isolated TLS fixture
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	previous := http.DefaultTransport
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections() }()
	s := &Server{st: st, assets: assets, env: settings.NewEnv(filepath.Join(root, "fixture.env"))}
	router := gin.New()
	router.POST("/checks", s.createCredentialCheck)
	key := bytes.Repeat([]byte("k"), 32)
	nodeserver.New(nodeserver.Config{Enabled: true, NodeID: "sg", Key: key, BusinessKey: key}, nil, st).Register(router)
	invoke := func(path string, body []byte) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("POST", path, bytes.NewReader(body)))
		return w
	}
	check := func(id, parent string, selected []string) {
		body, _ := json.Marshal(credentialCheckRequest{RequestID: id, Platform: "chatgpt", Refs: selected, ParentID: parent})
		if w := invoke("/checks", body); w.Code != 202 {
			t.Fatal(w.Code, w.Body)
		}
		if w := invoke("/checks", body); w.Code != 200 {
			t.Fatal("request replay", w.Code, w.Body)
		}
		task, err := st.ClaimCredentialCheck()
		if err != nil {
			t.Fatal(err)
		}
		s.executeCredentialCheck(context.Background(), task, s.checkCredential)
	}
	library := func(expected int) {
		t.Helper()
		payload := map[string]any{"schema": "work.inventory-relay.v1", "node_id": "sg", "request_id": "library-read", "issued_at": time.Now().Unix(), "expires_at": time.Now().Unix() + 120, "action": "library", "data": map[string]any{"source": "account_center", "status": "available", "q": "", "page": 1, "page_size": 100}}
		raw, _ := nodeserver.Canonical(payload)
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte("work-inventory-relay-v1\n"))
		mac.Write(raw)
		body, _ := json.Marshal(map[string]any{"payload": payload, "signature": hex.EncodeToString(mac.Sum(nil))})
		w := invoke("/internal/work/v1/inventory/library", body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body)
		}
		var result struct {
			Payload   map[string]any
			Signature string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		raw, _ = nodeserver.Canonical(result.Payload)
		mac = hmac.New(sha256.New, key)
		mac.Write([]byte(nodeserver.LibraryDomain))
		mac.Write(raw)
		if result.Signature != hex.EncodeToString(mac.Sum(nil)) {
			t.Fatal("signature mismatch")
		}
		if int(result.Payload["total"].(float64)) != expected || len(result.Payload["items"].([]any)) != expected {
			t.Fatal(result.Payload)
		}
	}
	library(0)
	const first = "79aab84c-4377-4b6b-8a16-677bb9835821"
	check(first, "", refs)
	library(4)
	task, _ := st.GetTask("request-" + first)
	if task.Status != "unknown" {
		t.Fatal(task.Status)
	}
	uncertain = false
	check("efde4322-2a44-4f6f-9cc9-75610c41c195", "request-"+first, refs[4:])
	library(10)
	for _, ref := range refs {
		a, _ := assets.Get(ref)
		if a.Payload["refresh_token"] != "retained-rt" {
			t.Fatal("AT test rotated RT")
		}
	}
	if err := st.MarkClaimed(refs[0]); err != nil {
		t.Fatal(err)
	}
	// The central discovery feed reports technical availability; the central
	// production facts determine reuse without erasing the node's local record.
	library(10)
	if rows, _, err := st.LibraryAccounts("account-0", "available", "", 1, 100); err != nil || len(rows) != 1 || !rows[0].Claimed {
		t.Fatal("technical availability or local record changed", rows, err)
	}
	if strings.Contains(task.Error, "fixture-") {
		t.Fatal("credential leaked")
	}
}
