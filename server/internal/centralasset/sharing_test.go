package centralasset

import (
	"context"
	"encoding/base64"
	"errors"
	"github.com/google/uuid"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
	"github.com/tiantianGPU/reg-factory/server/internal/testutil/testdatabase"
	"strings"
	"testing"
	"time"
)

func handoffFixture(t *testing.T) (*Sharing, *Config, *map[string]any) {
	t.Helper()
	st := testdatabase.Open(t, t.TempDir())
	cfg := Config{Directory: t.TempDir(), NodeID: "sg", CenterID: "center", Origin: "http://127.0.0.1:1", NodeKey: strings.Repeat("n", 32), ReceiptKey: strings.Repeat("r", 32), AssetKey: base64.URLEncoding.EncodeToString([]byte(strings.Repeat("a", 32))), JournalKey: base64.URLEncoding.EncodeToString([]byte(strings.Repeat("j", 32)))}
	doc := map[string]any{}
	s := &Sharing{Store: st, Config: func() (Config, error) { return cfg, nil }, Current: func(ref string, _ map[string]any) (map[string]any, error) {
		copy := map[string]any{}
		for k, v := range doc {
			copy[k] = v
		}
		return map[string]any{"schema": "aishop.asset-snapshot.v1", "source_ref": ref, "source_revision": 1, "type": "account", "document": copy}, nil
	}}
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	return s, &cfg, &doc
}
func TestHandoffOptionalImmutablePacketAndLostAckRestart(t *testing.T) {
	s, cfg, doc := handoffFixture(t)
	ctx := context.Background()
	if refs, e := s.Refs(); e != nil || len(refs) != 0 {
		t.Fatal(refs, e)
	}
	reads := 0
	current := s.Current
	s.Current = func(ref string, opts map[string]any) (map[string]any, error) { reads++; return current(ref, opts) }
	(*doc)["email_password"] = "ORIGINAL-SYNTHETIC"
	id := uuid.NewString()
	if _, e := s.Consent("one", id, map[string]any{}); e != nil {
		t.Fatal(e)
	}
	(*doc)["email_password"] = "LATER-SYNTHETIC"
	calls, detached := 0, 0
	s.Detach = func(string) error { detached++; return nil }
	s.Send = func(_ context.Context, c Config, b map[string]any) (map[string]any, error) {
		calls++
		if c.CenterID != "center" || b["task_id"] != id || b["action"] != "snapshot" {
			t.Fatal("handoff scope changed")
		}
		if b["entry"].(map[string]any)["document"].(map[string]any)["email_password"] != "ORIGINAL-SYNTHETIC" {
			t.Fatal("rebuilt immutable packet")
		}
		if calls == 1 {
			return nil, errors.New("lost acknowledgement")
		}
		return map[string]any{"task_id": id, "state": "accepted", "sharing": map[string]any{"node_id": "sg", "source_ref": "one", "account_id": "central-id", "source_version": 1, "state": "transferred", "saved_at": "2026-11-13T01:00:00Z"}}, nil
	}
	if r, e := s.Sync(ctx, "one"); e == nil || r.Pending == nil || detached != 0 {
		t.Fatal(r, e, detached)
	}
	var sealed string
	_ = s.Store.Execer().QueryRow("SELECT record_ciphertext FROM asset_sharing WHERE source_ref='one'").Scan(&sealed)
	if strings.Contains(sealed, "ORIGINAL-SYNTHETIC") {
		t.Fatal("secret stored as cleartext")
	}
	cfg.CenterID = "changed-center"
	restarted := &Sharing{Store: s.Store, Config: s.Config, Current: s.Current, Send: s.Send, Detach: s.Detach}
	r, e := restarted.Sync(ctx, "one")
	if e != nil || r.Status != "transferred" || r.Pending != nil || detached != 1 {
		t.Fatal(r, e, detached)
	}
	for i := 0; i < 3; i++ {
		if _, e = restarted.Sync(ctx, "one"); e != nil {
			t.Fatal(e)
		}
	}
	if calls != 2 || reads != 1 || detached != 1 {
		t.Fatal("completed handoff kept a source relationship", calls, reads, detached)
	}
	if _, e = restarted.Consent("one", uuid.NewString(), map[string]any{}); e == nil {
		t.Fatal("former source reacquired ownership")
	}
}
func TestHandoffAcknowledgedWithdrawalResumesWithoutResending(t *testing.T) {
	s, _, _ := handoffFixture(t)
	id := uuid.NewString()
	_, _ = s.Consent("one", id, map[string]any{})
	sends := 0
	s.Send = func(_ context.Context, _ Config, b map[string]any) (map[string]any, error) {
		sends++
		return map[string]any{"task_id": id, "state": "accepted", "sharing": map[string]any{"node_id": "sg", "source_ref": "one", "account_id": "asset", "source_version": 1, "state": "transferred"}}, nil
	}
	s.Detach = func(string) error { return errors.New("local storage temporarily unavailable") }
	if r, e := s.Sync(context.Background(), "one"); e == nil || r.Receipt == nil {
		t.Fatal(r, e)
	}
	s.Detach = func(string) error { return nil }
	if r, e := s.Sync(context.Background(), "one"); e != nil || r.Status != "transferred" {
		t.Fatal(r, e)
	}
	if sends != 1 {
		t.Fatal("resent completed ownership transfer")
	}
}
func TestReceivedPartialMaterialStaysSeparateFromPrivatePool(t *testing.T) {
	s, cfg, _ := handoffFixture(t)
	scope := map[string]any{"attempt_id": uuid.NewString(), "node_id": cfg.NodeID, "subject_id": "central-asset", "resource_version": float64(1), "parameters": map[string]any{"asset_ref": map[string]any{"center_id": cfg.CenterID}}}
	material := map[string]any{"account_id": "central-asset", "snapshot": map[string]any{"schema": "aishop.asset-snapshot.v1", "source_ref": "central:central-asset", "source_revision": 1, "type": "account", "document": map[string]any{"sms_enabled": false}}}
	for i := 0; i < 2; i++ {
		if r, e := s.Restore(context.Background(), scope, material); e != nil || r["status"] != "completed" {
			t.Fatal(r, e)
		}
	}
	received, e := s.Received()
	if e != nil || len(received) != 1 {
		t.Fatal(received, e)
	}
	refs, e := s.Refs()
	if e != nil || len(refs) != 0 {
		t.Fatal("received data became a sharing source", refs, e)
	}
	material["snapshot"].(map[string]any)["document"] = map[string]any{"sms_enabled": true}
	if r, e := s.Restore(context.Background(), scope, material); e != nil || r["error_code"] != "ASSET_RECEIVE_REQUEST_CONFLICT" {
		t.Fatal("conflicting receive accepted", r, e)
	}
	rows, total, e := s.Store.LibraryAccounts("", "", "", 1, 10)
	if e != nil || total != 0 {
		t.Fatal("received data became private available stock", rows, total, e)
	}
}

func TestHandoffRejectionAllowsCorrectionButUnknownDoesNot(t *testing.T) {
	s, _, doc := handoffFixture(t)
	first := uuid.NewString()
	if _, e := s.Consent("one", first, map[string]any{}); e != nil {
		t.Fatal(e)
	}
	s.Send = func(context.Context, Config, map[string]any) (map[string]any, error) {
		return nil, errors.New("connection lost")
	}
	if _, e := s.Sync(context.Background(), "one"); e == nil {
		t.Fatal("expected unknown")
	}
	if _, e := s.Consent("one", uuid.NewString(), map[string]any{}); e == nil {
		t.Fatal("unknown original replaced")
	}
	s.Send = func(_ context.Context, c Config, b map[string]any) (map[string]any, error) {
		return map[string]any{"task_id": b["task_id"], "state": "rejected", "node_id": c.NodeID, "source_ref": "one", "code": "ASSET_IDENTITY_EXISTS"}, nil
	}
	if r, e := s.Sync(context.Background(), "one"); e == nil || r.Status != "rejected" {
		t.Fatal(r, e)
	}
	(*doc)["status"] = "operator corrected"
	next := uuid.NewString()
	if r, e := s.Consent("one", next, map[string]any{}); e != nil || r.RequestID != next || r.Status != "pending" {
		t.Fatal(r, e)
	}
}
func TestHandoffWithdrawalRemovesAvailabilityWithoutUsageFacts(t *testing.T) {
	s, _, _ := handoffFixture(t)
	_, e := s.Store.UpsertAccounts([]store.AccountSeed{{Ref: "one", Platform: "chatgpt", Kind: "oauth", Identity: "one@example.test"}, {Ref: "two", Platform: "chatgpt", Kind: "oauth", Identity: "two@example.test"}})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Store.WithdrawAccount("one"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Store.GetAccount("one"); e == nil {
		t.Fatal("transferred source is still available")
	}
	rows, n, e := s.Store.LibraryAccounts("", "", "", 1, 10)
	if e != nil || n != 1 || rows[0].Ref != "two" {
		t.Fatal(rows, n, e)
	}
	var claimed int
	_ = s.Store.Execer().QueryRow("SELECT claimed FROM accounts WHERE ref='one'").Scan(&claimed)
	if claimed != 0 {
		t.Fatal("handoff invented a production claim")
	}
}
func TestRestoreInterruptedLocalReceiptReplaysWithoutPlatformAction(t *testing.T) {
	s, cfg, _ := handoffFixture(t)
	id := uuid.NewString()
	scope := map[string]any{"attempt_id": id, "subject_id": "central-asset", "node_id": cfg.NodeID, "action": "asset.restore", "resource_version": float64(1), "expires_at": float64(time.Now().Unix() + 300), "parameters": map[string]any{"asset_ref": map[string]any{"center_id": cfg.CenterID}}}
	op, e := New(*cfg, scope)
	if e != nil {
		t.Fatal(e)
	}
	material := map[string]any{"task_id": id, "account_id": "central-asset", "credential_revision": 1, "snapshot": map[string]any{"schema": "aishop.asset-snapshot.v1", "source_ref": "central:central-asset", "source_revision": 1, "type": "account", "document": map[string]any{}}}
	op.claim = func(context.Context) (map[string]any, error) {
		return map[string]any{"task_id": id, "account_id": "central-asset", "node_id": cfg.NodeID, "expected_revision": 1, "state": "claimed"}, nil
	}
	var committed map[string]any
	sender := func(_ context.Context, b map[string]any) (map[string]any, error) {
		switch b["action"] {
		case "read":
			return material, nil
		case "result":
			r := b["result"].(map[string]any)
			committed = map[string]any{"task_id": id, "account_id": "central-asset", "result_id": r["result_id"], "state": "completed", "credential_revision": 1}
			return committed, nil
		default:
			if committed != nil {
				return committed, nil
			}
			return map[string]any{"task_id": id, "state": "claimed"}, nil
		}
	}
	op.send = sender
	op.Restore = func(ctx context.Context, body map[string]any) (map[string]any, error) {
		_, e := s.Restore(ctx, scope, body)
		if e != nil {
			return nil, e
		}
		return nil, errors.New("crash after local commit")
	}
	if _, e = op.Run(context.Background(), func(context.Context, map[string]any) (map[string]any, error) {
		t.Fatal("invoked platform")
		return nil, nil
	}); e == nil {
		t.Fatal("crash missing")
	}
	restarted, e := New(*cfg, scope)
	if e != nil {
		t.Fatal(e)
	}
	restarted.send = sender
	restarted.Restore = func(ctx context.Context, body map[string]any) (map[string]any, error) {
		return s.Restore(ctx, scope, body)
	}
	r, e := restarted.Replay(context.Background(), true)
	if e != nil || r["state"] != "completed" {
		t.Fatal(r, e)
	}
	received, e := s.Received()
	if e != nil || len(received) != 1 {
		t.Fatal(received, e)
	}
}
