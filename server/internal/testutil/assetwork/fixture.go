// Package assetwork supplies an isolated authenticated Work/asset-center fixture.
// It accepts only synthetic task scopes registered by the test.
package assetwork

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"aishop-next/execution-go/nodeassets"
	"github.com/google/uuid"
)

type Fixture struct {
	Server                                    *httptest.Server
	NodeKey, ReceiptKey, AssetKey, JournalKey string
	mu                                        sync.Mutex
	mode                                      string
	scopes, results                           map[string]map[string]any
	calls                                     map[string]int
	document                                  map[string]any
}

func New(t testing.TB) *Fixture {
	f := &Fixture{NodeKey: strings.Repeat("n", 32), ReceiptKey: strings.Repeat("r", 32),
		AssetKey: base64.URLEncoding.EncodeToString([]byte(strings.Repeat("a", 32))), JournalKey: base64.URLEncoding.EncodeToString([]byte(strings.Repeat("j", 32))),
		scopes: map[string]map[string]any{}, results: map[string]map[string]any{}, calls: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	for k, v := range map[string]string{"NODE_ID": "sg-test", "NODE_KEY": f.NodeKey, "NODE_RECEIPT_KEY": f.ReceiptKey, "WORK_ORIGIN": f.Server.URL,
		"ASSET_CENTER_ID": "test-center", "ASSET_KEY": f.AssetKey, "ASSET_JOURNAL_KEY": f.JournalKey} {
		t.Setenv("REG_FACTORY_"+k, v)
	}
	return f
}

func (f *Fixture) Scope(action string) map[string]any {
	now := time.Now().Unix()
	p := map[string]any{"schema": "account-node.work.v1", "issuer": "work", "node_id": "sg-test", "operation_id": uuid.NewString(),
		"task_id": uuid.NewString(), "attempt_id": uuid.NewString(), "fence": float64(1), "action": action, "subject_kind": "account",
		"subject_id": uuid.NewString(), "resource_version": float64(3), "issued_at": float64(now), "expires_at": float64(now + 600),
		"parameters": map[string]any{"asset_ref": map[string]any{"center_id": "test-center"}}, "command_sha256": strings.Repeat("a", 64)}
	f.mu.Lock()
	f.scopes[p["attempt_id"].(string)] = p
	f.mu.Unlock()
	return p
}

func (f *Fixture) Mode(mode string)            { f.mu.Lock(); defer f.mu.Unlock(); f.mode = mode }
func (f *Fixture) Document(doc map[string]any) { f.mu.Lock(); defer f.mu.Unlock(); f.document = doc }
func (f *Fixture) Calls(action string) int     { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[action] }
func (f *Fixture) Result(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(f.results[id])
	var v map[string]any
	json.Unmarshal(raw, &v)
	return v
}

func sign(p map[string]any, key, domain string) string {
	raw, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(domain))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

func (f *Fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	var d map[string]any
	if json.NewDecoder(r.Body).Decode(&d) != nil {
		w.WriteHeader(400)
		return
	}
	p, _ := d["payload"].(map[string]any)
	if r.URL.Path == "/internal/work/v1/asset-claim" {
		if d["signature"] != sign(p, f.ReceiptKey, "aishop-node-asset-claim-v1\n") {
			w.WriteHeader(401)
			return
		}
		id, _ := p["attempt_id"].(string)
		scope := f.scopes[id]
		if scope == nil {
			w.WriteHeader(404)
			return
		}
		f.calls["claim"]++
		p["schema"] = "aishop.node-asset-claim-receipt.v1"
		delete(p, "result")
		p["result"] = map[string]any{"task_id": id, "account_id": scope["subject_id"], "node_id": "sg-test", "state": "claimed", "expected_revision": scope["resource_version"]}
		json.NewEncoder(w).Encode(map[string]any{"payload": p, "signature": sign(p, f.NodeKey, "aishop-node-asset-claim-v1\n")})
		return
	}
	if r.URL.Path != nodeassets.RelayPath || d["signature"] != sign(p, f.ReceiptKey, nodeassets.RelayDomain) {
		w.WriteHeader(401)
		return
	}
	raw, _ := json.Marshal(p["envelope"])
	var envelope nodeassets.Envelope
	json.Unmarshal(raw, &envelope)
	m, err := nodeassets.Unpack(envelope, f.AssetKey, "sg-test", "", "request", time.Now())
	if err != nil {
		w.WriteHeader(400)
		return
	}
	id, _ := m.Body["task_id"].(string)
	scope := f.scopes[id]
	if scope == nil {
		w.WriteHeader(404)
		return
	}
	action, _ := m.Body["action"].(string)
	f.calls[action]++
	if f.mode == "offline" && action != "read" {
		w.WriteHeader(503)
		return
	}
	out := map[string]any{"task_id": id, "account_id": scope["subject_id"]}
	if action == "read" {
		out["credential_revision"] = scope["resource_version"]
		out["document"] = map[string]any{"email": "fixture@example.invalid", "access_token": "fixture-at", "refresh_token": "fixture-rt"}
		if f.document != nil {
			out["document"] = f.document
		}
		out["recovery"] = map[string]any{"login": map[string]any{"password": "fixture-password"}}
	} else {
		if action == "result" {
			result, _ := m.Body["result"].(map[string]any)
			if old := f.results[id]; old != nil && old["result_id"] != result["result_id"] {
				w.WriteHeader(409)
				return
			}
			f.results[id] = result
		}
		result := f.results[id]
		if result == nil || f.mode == "absent" {
			if action == "result" {
				w.WriteHeader(409)
				return
			}
			out = map[string]any{"task_id": id, "state": "absent"}
		} else {
			out["result_id"] = result["result_id"]
			out["state"] = map[string]string{"success": "completed", "failed": "failed", "unknown": "unknown"}[result["outcome"].(string)]
			version := scope["resource_version"].(float64)
			if result["outcome"] == "success" && scope["action"] != "asset.liveness" {
				version++
			}
			out["credential_revision"] = version
			if f.mode == "wrong" {
				out["account_id"] = "foreign-account"
			}
			if f.mode == "closed" {
				out["state"] = "closed"
				out["expected_revision"] = scope["resource_version"]
			}
		}
		if f.mode == "lost" && action == "result" {
			w.WriteHeader(503)
			return
		}
	}
	response, err := nodeassets.Pack("sg-test", f.AssetKey, m.RequestID, "response", out, time.Now())
	if err != nil {
		w.WriteHeader(500)
		return
	}
	json.NewEncoder(w).Encode(response)
}
