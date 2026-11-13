package nodeserver

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/testdatabase"
)

// Actual signed admission -> released Python driver -> PG -> retained output ->
// Go result projection and signed receipt, with only platform HTTP substituted.
func TestProductionPostgresSignedAdmissionAndRecovery(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("REG_FACTORY_TEST_POSTGRES_DSN") == "" {
		t.Skip("Linux and disposable PostgreSQL required")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"normal", "admission-recovery", "invite-response-lost", "issued-output-recovery"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			st := testdatabase.Open(t, root)
			t.Setenv("REG_FACTORY_DATA_DIR", root)
			t.Setenv("REG_FACTORY_ENV_FILE", filepath.Join(root, ".env"))
			t.Setenv("REG_FACTORY_PRODUCTION_PROXY", "")
			t.Setenv("REGD_PYTHON", "python")
			key := bytes.Repeat([]byte("p"), 32)
			h := New(Config{Enabled: true, NodeID: "sg", Key: key, ReceiptKey: key, BusinessKey: key, ArtifactKey: key}, nil, st)
			router := gin.New()
			h.Register(router)
			id, slot, workspace := SubjectID(mode), SubjectID(mode+"-slot"), SubjectID("fixture-workspace")
			email := "unused@example.test"
			jwt := func(plan, space string) string {
				raw, _ := json.Marshal(map[string]any{"email": email, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "https://api.openai.com/profile": map[string]any{"email": email}, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": space, "chatgpt_plan_type": plan}})
				return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".fixture"
			}
			_, err := st.UpsertAccounts([]store.AccountSeed{{Ref: "free", Identity: email, Platform: "chatgpt", Kind: "oauth"}})
			if err != nil {
				t.Fatal(err)
			}
			if err = st.MarkHealth("free", "ok", "fixture"); err != nil {
				t.Fatal(err)
			}
			if _, err = st.Execer().Exec(`INSERT INTO account_credentials(ref,password,totp,updated_at) VALUES('free','synthetic-password','synthetic-totp',?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			assets := assetstore.New(root)
			if err = assets.Put(assetstore.Asset{Ref: "free", Platform: "chatgpt", Kind: "oauth", Email: email, Payload: map[string]any{"email": email, "access_token": jwt("free", "personal"), "refresh_token": "fixture-free-rt"}}); err != nil {
				t.Fatal(err)
			}
			var invites, logins atomic.Int32
			var present, joined atomic.Bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				send := func(v any) { json.NewEncoder(w).Encode(v) }
				switch {
				case strings.HasPrefix(r.URL.Path, "/accounts/check/"):
					send(map[string]any{"accounts": map[string]any{"personal": map[string]any{"account": map[string]any{"plan_type": "free"}}}})
				case r.URL.Path == "/subscriptions":
					send(map[string]any{"seat_capacity": []any{map[string]any{"type": "default", "paid": 10, "available": 10, "held": 0}}})
				case strings.HasSuffix(r.URL.Path, "/invites") && r.Method == "POST":
					invites.Add(1)
					present.Store(true)
					if mode == "invite-response-lost" && invites.Load() == 1 {
						w.WriteHeader(503)
					}
					send(map[string]any{})
				case strings.HasSuffix(r.URL.Path, "/users") || strings.HasSuffix(r.URL.Path, "/invites"):
					items := []any{}
					if (strings.HasSuffix(r.URL.Path, "/users") && joined.Load()) || (strings.HasSuffix(r.URL.Path, "/invites") && present.Load() && !joined.Load()) {
						items = append(items, map[string]any{"email": email, "seat_type": "default", "status": "pending"})
					}
					send(map[string]any{"items": items, "total": len(items)})
				case r.URL.Path == "/authorize" && r.Method == "POST":
					logins.Add(1)
					joined.Store(true)
					token := jwt("team", workspace)
					send(map[string]any{"access_token": token, "id_token": token, "refresh_token": "fixture-team-rt"})
				default:
					t.Errorf("unexpected platform path %s", r.URL.Path)
					w.WriteHeader(400)
					send(map[string]any{})
				}
			}))
			defer upstream.Close()
			app := filepath.Join(root, "app")
			os.MkdirAll(filepath.Join(app, "tools"), 0700)
			shim := fmt.Sprintf(`import sys,json,requests
sys.path.insert(0,%q)
from tools import team_production as p
p.BASE=%q
def authorize(self,candidate,workspace,persist,progress):
 s=requests.Session();s.trust_env=False
 result=s.post(p.BASE+'/authorize',timeout=5).json()
 persist(result)
 if %q=='issued-output-recovery': raise RuntimeError('fixture lost executor response after save')
 return result
p.Backend.authorize=authorize
print(p.canonical(p.main(json.load(sys.stdin))))
`, repo, upstream.URL, mode)
			if err = os.WriteFile(filepath.Join(app, "tools", "team_production.py"), []byte(shim), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("REGD_APP_ROOT", app)
			now := time.Now().Unix()
			ctx := map[string]any{"step_id": id, "attempt_id": SubjectID(mode + "-attempt"), "subject_id": slot, "fence": 1}
			params := map[string]any{"production_ref": slot, "order_ref": SubjectID("order"), "product_id": 34, "seat_type": "standard", "workspace_ids": []any{workspace}, "mode": "produce", "source_ref": "", "source_sha256": "", "budget_units": 0}
			assignment := map[string]any{"schema": WorkSchema, "issuer": "work", "node_id": "sg", "operation_id": id, "task_id": id, "attempt_id": ctx["attempt_id"], "fence": 1, "action": productionAction, "subject_kind": "production", "subject_id": slot, "resource_version": 1, "issued_at": now, "expires_at": now + 600, "parameters": params, "command_sha256": productionDigest(ctx)}
			scope := map[string]any{"schema": productionMaterialSchema, "node_id": "sg", "grant_id": SubjectID(mode + "-grant"), "context_sha256": productionDigest(ctx)}
			material := map[string]any{}
			for k, v := range scope {
				material[k] = v
			}
			material["context"], material["parameters"], material["action"] = ctx, params, productionAction
			material["expires_at"], material["readback_until"] = now+600, now+3600
			material["workspace"] = map[string]any{"account_id": workspace, "access_token": "fixture-owner-at"}
			block, _ := aes.NewCipher(key)
			box, _ := cipher.NewGCM(block)
			nonce := bytes.Repeat([]byte{1}, 12)
			aad, _ := Canonical(scope)
			raw, _ := json.Marshal(material)
			envelope := map[string]any{}
			for k, v := range scope {
				envelope[k] = v
			}
			envelope["nonce"], envelope["ciphertext"] = base64.StdEncoding.EncodeToString(nonce), base64.StdEncoding.EncodeToString(box.Seal(nil, nonce, raw, aad))
			post := func(path string, document any) *httptest.ResponseRecorder {
				t.Helper()
				body, _ := json.Marshal(document)
				response := httptest.NewRecorder()
				router.ServeHTTP(response, httptest.NewRequest("POST", path, bytes.NewReader(body)))
				return response
			}
			signedMaterial, _ := signInventory(envelope, key, "work-node-material-relay-v1\n")
			if r := post("/internal/work/v1/team/materials", signedMaterial); r.Code != 200 {
				t.Fatal("material", r.Code, r.Body.String())
			}
			signature, _ := Sign(assignment, key, false)
			if mode == "admission-recovery" {
				t.Setenv("REGD_PYTHON", filepath.Join(root, "missing-python"))
				if r := post("/internal/work/v1/tasks", Document{assignment, signature}); r.Code != 503 {
					t.Fatal("pre-admission failure", r.Code, r.Body.String())
				}
				if st.HasProductionRun(id) || invites.Load() != 0 || logins.Load() != 0 {
					t.Fatal("unavailable driver created external work")
				}
				t.Setenv("REGD_PYTHON", "python")
			}
			for i := 0; i < 2; i++ {
				if r := post("/internal/work/v1/tasks", Document{assignment, signature}); r.Code != 200 && r.Code != 202 {
					t.Fatal("PG admission", r.Code, r.Body.String())
				}
			}
			runCtx, cancel := context.WithCancel(context.Background())
			defer func() {
				cancel()
				h.productionMu.Lock()
				done := h.productionActive[id]
				h.productionMu.Unlock()
				if done != nil {
					select {
					case <-done:
					case <-time.After(10 * time.Second):
						t.Error("production executor did not stop before cleanup")
					}
				}
			}()
			h.StartProduction(runCtx)
			sawUnknown := false
			for deadline := time.Now().Add(70 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				var status string
				if err = st.Execer().QueryRow(`SELECT status FROM production_runs WHERE operation_id=?`, id).Scan(&status); err != nil {
					t.Fatal(err)
				}
				if status == "delivered" {
					break
				}
				if status == "unknown" {
					sawUnknown = true
					st.Execer().Exec(`UPDATE production_runs SET next_at=0 WHERE operation_id=?`, id)
				}
				if status == "waiting_user" || status == "failed" {
					logs, _ := st.TailEvents("work-"+id, 20)
					t.Fatalf("production did not recover: %s %v", status, logs)
				}
			}
			produced, err := st.Production(id)
			if err != nil || produced.Artifact == "" {
				logs, _ := st.TailEvents("work-"+id, 20)
				t.Fatalf("original result missing: %v %v", err, logs)
			}
			if invites.Load() != 1 || logins.Load() != 1 || ((mode == "invite-response-lost" || mode == "issued-output-recovery") && !sawUnknown) {
				t.Fatal("duplicate external action or missing recovery", invites.Load(), logins.Load(), sawUnknown)
			}
			h.productionCheck = func(doc map[string]any) map[string]any {
				return map[string]any{"status": "completed", "error_code": ""}
			}
			verified, err := Verify(Document{assignment, signature}, key, "sg", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				receipt, err := h.productionReceipt(verified, key, time.Now())
				if err != nil || receipt["payload"].(map[string]any)["status"] != "completed" {
					t.Fatal("terminal receipt", receipt, err)
				}
			}
			final, _ := st.Production(id)
			if final.Artifact != produced.Artifact || final.State != "completed" {
				t.Fatal("receipt replaced original result")
			}
			if _, err := assets.Get("production:" + id); err != nil {
				t.Fatal("current export unavailable", err)
			}
			var outputs int
			st.Execer().QueryRow(`SELECT count(*) FROM production_outputs WHERE operation_id=?`, id).Scan(&outputs)
			if outputs != 1 {
				t.Fatal("issued result duplicated", outputs)
			}
		})
	}
}
