package nodeserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tiantianGPU/reg-factory/server/internal/assetstore"
	"github.com/tiantianGPU/reg-factory/server/internal/runner"
	"github.com/tiantianGPU/reg-factory/server/internal/settings"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

// TestCoreRepairFixture is invoked by Core's cross-language integration test.
// Only the upstream credential rotation is synthetic; queueing, signed reads,
// account versions and encrypted artifacts use the real SG handlers.
func TestCoreRepairFixture(t *testing.T) {
	if os.Getenv("REG_FACTORY_CORE_FIXTURE") != "1" {
		t.Skip("Core integration bridge")
	}
	var input struct {
		Request     Document       `json:"request"`
		RequestKey  string         `json:"request_key"`
		ReceiptKey  string         `json:"receipt_key"`
		ArtifactKey string         `json:"artifact_key"`
		Original    map[string]any `json:"original"`
		Rotated     map[string]any `json:"rotated"`
		Status      string         `json:"status"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&input); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("REG_FACTORY_DATA_DIR", root)
	st, err := store.Open(filepath.Join(root, "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	subject := ls(input.Request.Payload["subject_id"])
	email := emailFromDocument(input.Original)
	if _, err := st.UpsertAccounts([]store.AccountSeed{{Ref: subject, Platform: "chatgpt", Kind: "oauth", Identity: email}}); err != nil {
		t.Fatal(err)
	}
	for version := 1; version < int(li(input.Request.Payload["resource_version"])); version++ {
		if err := st.AdvanceAccountVersion(subject, version); err != nil {
			t.Fatal(err)
		}
	}
	assets := assetstore.New(root)
	put := func(doc map[string]any) {
		t.Helper()
		if err := assets.Put(assetstore.Asset{Ref: subject, Platform: "chatgpt", Kind: "oauth", Email: email, Payload: doc}); err != nil {
			t.Fatal(err)
		}
	}
	put(input.Original)
	// A historical producer file must not win over the rotated canonical asset.
	oldDir := filepath.Join(root, "plus-import", "team-json")
	if err := os.MkdirAll(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(input.Original)
	if err := os.WriteFile(filepath.Join(oldDir, "team-"+slug(email)+".json"), old, 0600); err != nil {
		t.Fatal(err)
	}
	mgr := runner.New(&settings.Settings{MaxParallel: 0}, st)
	h := New(Config{NodeID: ls(input.Request.Payload["node_id"]), Key: []byte(input.RequestKey), ReceiptKey: []byte(input.ReceiptKey), ArtifactKey: []byte(input.ArtifactKey), Platform: "chatgpt"}, mgr, st)
	gin.SetMode(gin.TestMode)
	gin.DefaultWriter = io.Discard
	r := gin.New()
	r.POST("/tasks", h.acceptTask)
	r.POST("/tasks/:task_id/result", h.taskReceipt)
	r.POST("/tasks/:task_id/artifact", h.workArtifact)
	r.POST("/resolve", h.resolveTask)
	call := func(path string, doc Document) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(doc)
		request := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		r.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("%s: HTTP %d %s", path, response.Code, response.Body.String())
		}
		var value map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	ack := call("/tasks", input.Request)
	duplicate := call("/tasks", input.Request)
	read := input.Request
	read.Signature, err = Sign(read.Payload, []byte(input.RequestKey), true)
	if err != nil {
		t.Fatal(err)
	}
	ticket := ls(input.Request.Payload["operation_id"])
	pending := call("/tasks/"+ticket+"/result", read)
	local, err := h.taskByTicket(ticket)
	if err != nil {
		t.Fatal(err)
	}
	status := input.Status
	if status == "" {
		status = "succeeded"
	}
	exitCode := 1
	if status == "succeeded" {
		put(input.Rotated)
		exitCode = 0
	}
	if err := st.Finish(local.ID, status, exitCode, ""); err != nil {
		t.Fatal(err)
	}
	receipt := call("/tasks/"+ticket+"/result", read)
	var artifact map[string]any
	if status == "succeeded" {
		artifact = call("/tasks/"+ticket+"/artifact", read)
	}
	recovered := call("/resolve", read)
	account, err := st.GetAccount(subject)
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(map[string]any{"ack": ack, "duplicate": duplicate, "pending": pending, "receipt": receipt, "artifact": artifact, "recovered": recovered, "account_version": account.Version, "argv": local.ArgsJSON})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("CORE_FIXTURE:" + string(output))
}
