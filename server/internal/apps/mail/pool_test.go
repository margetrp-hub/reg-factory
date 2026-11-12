package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func poolApp(t *testing.T) *App {
	t.Helper()
	a, err := New(Config{DBPath: filepath.Join(t.TempDir(), "mail.db"), BootstrapLimit: 100, FetchChunk: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func poolSource(t *testing.T, a *App, name string) Source {
	t.Helper()
	s, err := a.DB.UpsertSource(Source{Name: name, Host: "imap.example.test", Username: name, Password: "fixture-secret", Enabled: true, Idle: true})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPoolAddressJourneyAndExactLookup(t *testing.T) {
	a := poolApp(t)
	s := poolSource(t, a, "fixture")
	ctx := context.Background()
	n, err := a.DB.ImportMailboxes(ctx, s.ID, []string{"Person@example.test", "person@example.test", "no-mail@example.test"})
	if err != nil || n != 2 {
		t.Fatalf("import=%d %v", n, err)
	}
	items, total, err := a.DB.PoolPage(ctx, "", "", 0, 25, 0)
	if err != nil || total != 2 || items[0].Total != 0 {
		t.Fatalf("unreceived pool=%+v total=%d %v", items, total, err)
	}
	if _, err = a.DB.ImportMailboxes(ctx, s.ID, []string{"valid@example.test", "invalid"}); err == nil {
		t.Fatal("partial invalid import accepted")
	}
	_, total, _ = a.DB.PoolPage(ctx, "", "", 0, 25, 0)
	if total != 2 {
		t.Fatal("invalid import wrote partial addresses")
	}
	for i, email := range []string{"person@example.test", "other-person@example.test"} {
		rec := MessageRecord{SourceID: s.ID, Mailbox: "INBOX", UID: fmt.Sprint(i + 1), ToHeader: email, BodyText: fmt.Sprintf("verification code: %d", 123456+i), PrimaryTag: "otp"}
		if ok, err := a.DB.StoreMessage(rec); !ok || err != nil {
			t.Fatal(ok, err)
		}
		if ok, err := a.DB.StoreMessage(rec); ok || err != nil {
			t.Fatal("replay", ok, err)
		}
	}
	result, err := a.LookupCode("person@example.test", 20)
	if err != nil || result.Code != "123456" || result.Scanned != 1 {
		t.Fatalf("exact lookup=%+v %v", result, err)
	}
	for _, state := range []string{"paused", "blocked"} {
		if err = a.DB.SetMailbox(ctx, "person@example.test", s.ID, state); err != nil {
			t.Fatal(err)
		}
		if _, err = a.LookupCode("person@example.test", 20); !errors.Is(err, ErrMailboxUnavailable) {
			t.Fatal("inactive lookup", err)
		}
	}
	if err = a.DB.SetMailbox(ctx, "person@example.test", s.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.LookupCode("person@example.test", 20); err != nil {
		t.Fatal(err)
	}
	if err = a.DB.SetAliasState(ctx, "manual@example.test", "paused"); err != nil {
		t.Fatal(err)
	}
	aliases, err := a.DB.ListAliases(ctx)
	found := false
	for _, item := range aliases {
		if item.HME == "manual@example.test" && item.Manual && item.State == "paused" {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatalf("manual state missing from pool: %+v %v", aliases, err)
	}
	old := float64(time.Now().AddDate(0, 0, -100).Unix())
	if _, err = a.DB.StoreMessage(MessageRecord{SourceID: s.ID, Mailbox: "INBOX", UID: "9", ToHeader: "old@example.test", SentAt: &old}); err != nil {
		t.Fatal(err)
	}
	if err = a.DB.PruneOlderThan(90); err != nil {
		t.Fatal(err)
	}
	items, total, err = a.DB.PoolPage(ctx, "old@example.test", "", 0, 25, 0)
	if err != nil || total != 1 || items[0].Total != 0 {
		t.Fatalf("prune lost pool=%+v %v", items, err)
	}
}

func TestSourceHistoryAndLegacyIndexUpgrade(t *testing.T) {
	a := poolApp(t)
	s1, s2 := poolSource(t, a, "one"), poolSource(t, a, "two")
	for _, s := range []Source{s1, s2} {
		if _, err := a.DB.StoreMessage(MessageRecord{SourceID: s.ID, Mailbox: "INBOX", UID: "42", ToHeader: s.Name + "@example.test", BodyText: "verification code: 654321", PrimaryTag: "otp"}); err != nil {
			t.Fatal("cross-source UID", err)
		}
	}
	s1.Name, s1.Password = "renamed", ""
	updated, err := a.DB.UpsertSource(s1)
	if err != nil || updated.ID != s1.ID || updated.Password != "fixture-secret" {
		t.Fatal("rename/password", updated.ID, err)
	}
	s1.Host = "different.example.test"
	if _, err = a.DB.UpsertSource(s1); err == nil {
		t.Fatal("rewrote historical source identity")
	}
	// Rebuild the actual legacy schema with its one-source UID constraint and
	// no recipient tables, then reopen through the normal startup entry point.
	if _, err = a.DB.db.Exec("DELETE FROM mail_messages WHERE source_id=?", s2.ID); err != nil {
		t.Fatal(err)
	}
	if err = a.DB.SetAliasState(context.Background(), "one@example.test", "blocked"); err != nil {
		t.Fatal(err)
	}
	// Older versions accepted mixed-case addresses. The latest manual state
	// must survive normalization even when a stale lowercase row also exists.
	if _, err = a.DB.db.Exec(`UPDATE mail_alias_state SET alias_hme=' One@Example.Test ',updated_at=200;
INSERT INTO mail_alias_state(alias_hme,state,manual,updated_at) VALUES('one@example.test','active',1,100)`); err != nil {
		t.Fatal(err)
	}
	var definition string
	if err = a.DB.db.QueryRow("SELECT sql FROM sqlite_master WHERE name='mail_messages'").Scan(&definition); err != nil {
		t.Fatal(err)
	}
	definition = strings.Replace(definition, "mail_messages", "legacy_messages", 1)
	definition = strings.Replace(definition, "UNIQUE(source_id, mailbox, uid)", "UNIQUE(mailbox, uid)", 1)
	if _, err = a.DB.db.Exec(definition + `; INSERT INTO legacy_messages SELECT * FROM mail_messages; DROP TABLE mail_messages; ALTER TABLE legacy_messages RENAME TO mail_messages; DROP TABLE mail_recipients; DROP TABLE mail_pool; DROP TABLE mail_pool_meta;`); err != nil {
		t.Fatal(err)
	}
	path := a.DB.Path()
	a.DB.Close()
	a.DB, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	items, total, err := a.DB.PoolPage(context.Background(), "one@example.test", "", 0, 25, 0)
	if err != nil || total != 1 || items[0].Total != 1 || items[0].State != "blocked" || !items[0].Manual || items[0].LastTag != "otp" {
		t.Fatalf("legacy recovery=%+v %v", items, err)
	}
	if _, err = a.LookupCode("ONE@example.test", 20); !errors.Is(err, ErrMailboxUnavailable) {
		t.Fatal("legacy blocked state bypassed", err)
	}
	if err = a.DB.SetMailbox(context.Background(), "one@example.test", s1.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.LookupCode("one@example.test", 20); err != nil {
		t.Fatal("legacy mailbox cannot resume", err)
	}
	if _, err = a.DB.StoreMessage(MessageRecord{SourceID: s2.ID, Mailbox: "INBOX", UID: "42", ToHeader: "two@example.test"}); err != nil {
		t.Fatal("legacy constraint remained", err)
	}
}

func TestSyncReplayConflictDisableAndRestart(t *testing.T) {
	a := poolApp(t)
	s := poolSource(t, a, "one")
	ctx := context.Background()
	first, err := a.DB.QueueSync(ctx, s.ID, "", "same-request-123")
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	if _, err = a.DB.QueueSync(ctx, 0, "", "same-request-123"); err == nil {
		t.Fatal("request scope conflict accepted")
	}
	if err = a.DB.SetSourceEnabled(ctx, s.ID, false); err != nil {
		t.Fatal(err)
	}
	if id, err := a.DB.claimSync(s.ID); err != nil || id != 0 {
		t.Fatal("disabled source claimed", id, err)
	}
	if err = a.DB.cancelUnavailableSync(); err != nil {
		t.Fatal(err)
	}
	replay, err := a.DB.QueueSync(ctx, s.ID, "", "same-request-123")
	if err != nil || replay[0].ID != first[0].ID || replay[0].State != "cancelled" {
		t.Fatal(replay, err)
	}
	if err = a.DB.SetSourceEnabled(ctx, s.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.QueueSync(ctx, s.ID, "", "restart-request"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.claimSync(s.ID); err != nil {
		t.Fatal(err)
	}
	if err = a.DB.recoverSync(); err != nil {
		t.Fatal(err)
	}
	runs, _, err := a.DB.SyncPage(ctx, "restart-request", 25, 0)
	if err != nil || runs[0].State != "interrupted" || runs[0].FinishedAt == 0 {
		t.Fatal(runs, err)
	}
}

type poolClient struct {
	mu      sync.Mutex
	closed  bool
	fail    bool
	block   bool
	onFetch func()
}

func (c *poolClient) Close() error       { c.mu.Lock(); c.closed = true; c.mu.Unlock(); return nil }
func (c *poolClient) SupportsIdle() bool { return true }
func (c *poolClient) UIDNext(ctx context.Context) (uint32, error) {
	if c.block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if c.fail {
		return 0, errors.New("fixture-secret: disconnected")
	}
	return 2, nil
}
func (c *poolClient) RecentUIDs(ctx context.Context, last uint32, limit int) ([]uint32, error) {
	return []uint32{1}, nil
}
func (c *poolClient) Fetch(ctx context.Context, uids []uint32) ([]Fetched, error) {
	if c.onFetch != nil {
		c.onFetch()
	}
	return []Fetched{{UID: 1, Body: []byte("To: pool@example.test\r\nSubject: verification code\r\n\r\ncode: 654321")}}, nil
}

func TestSourceChangeDuringFetchKeepsMailAndCursorSeparated(t *testing.T) {
	a := poolApp(t)
	src := poolSource(t, a, "changing")
	client := &poolClient{onFetch: func() {
		changed := src
		changed.Username = "replacement"
		if _, err := a.DB.UpsertSource(changed); err != nil {
			t.Fatal(err)
		}
	}}
	old := a.newRunner(src)
	if err := old.catchUp(context.Background(), client); err == nil {
		t.Fatal("old connection attributed mail to the replacement source")
	}
	current, err := a.DB.getSource(src.ID)
	total, _, countErr := a.DB.Totals()
	if err != nil || countErr != nil || total != 0 || current.LastUID != 0 {
		t.Fatal("stale mail or cursor persisted", total, current.LastUID, err, countErr)
	}
	client.onFetch = nil
	if err := a.newRunner(current).catchUp(context.Background(), client); err != nil {
		t.Fatal("replacement cannot collect", err)
	}
	code, err := a.LookupCode("pool@example.test", 20)
	if err != nil || code.Code != "654321" {
		t.Fatal("replacement lookup", code, err)
	}
}
func (c *poolClient) WaitForMail(ctx context.Context, idle, recheck time.Duration, wake <-chan struct{}) (bool, error) {
	select {
	case <-ctx.Done():
	case <-wake:
	}
	return false, nil
}

func awaitPool(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("mailbox transition did not finish")
}

func TestSourceRuntimeConsumesSyncAndRecovers(t *testing.T) {
	a := poolApp(t)
	s := poolSource(t, a, "runtime")
	ctx := context.Background()
	var mu sync.Mutex
	var clients []*poolClient
	fail, block := true, false
	a.dial = func(_ context.Context, _ Source) (mailClient, error) {
		mu.Lock()
		defer mu.Unlock()
		c := &poolClient{fail: fail, block: block}
		clients = append(clients, c)
		return c, nil
	}
	if _, err := a.DB.QueueSync(ctx, s.ID, "", "failed-sync-1"); err != nil {
		t.Fatal(err)
	}
	a.Start(ctx)
	awaitPool(t, func() bool {
		r, _, _ := a.DB.SyncPage(ctx, "failed-sync-1", 25, 0)
		return len(r) > 0 && r[0].State == "failed"
	})
	r, _, _ := a.DB.SyncPage(ctx, "failed-sync-1", 25, 0)
	if strings.Contains(r[0].Note, "fixture-secret") || r[0].Indexed != 0 {
		t.Fatal("unsafe result", r)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if _, err := a.DB.QueueSync(ctx, s.ID, "", "recovered-sync-2"); err != nil {
		t.Fatal(err)
	}
	a.notifySources()
	awaitPool(t, func() bool {
		r, _, _ := a.DB.SyncPage(ctx, "recovered-sync-2", 25, 0)
		return len(r) > 0 && r[0].State == "completed"
	})
	code, err := a.LookupCode("pool@example.test", 20)
	if err != nil || code.Code != "654321" {
		t.Fatal(code, err)
	}
	s.Password = "changed-fixture-secret"
	if _, err = a.DB.UpsertSource(s); err != nil {
		t.Fatal(err)
	}
	a.notifySources()
	awaitPool(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(clients) >= 3 })
	if err = a.DB.SetSourceEnabled(ctx, s.ID, false); err != nil {
		t.Fatal(err)
	}
	a.notifySources()
	awaitPool(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.runners) == 0 })
	mu.Lock()
	block = true
	mu.Unlock()
	if err = a.DB.SetSourceEnabled(ctx, s.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.QueueSync(ctx, s.ID, "", "interrupted-sync-3"); err != nil {
		t.Fatal(err)
	}
	a.notifySources()
	awaitPool(t, func() bool {
		r, _, _ := a.DB.SyncPage(ctx, "interrupted-sync-3", 25, 0)
		return len(r) > 0 && r[0].State == "running"
	})
	a.Stop()
	r, _, _ = a.DB.SyncPage(ctx, "interrupted-sync-3", 25, 0)
	if r[0].State != "interrupted" {
		t.Fatal(r)
	}
}

func TestInactiveMailboxHTTPResult(t *testing.T) {
	a := poolApp(t)
	a.Cfg.AdminKey = "fixture-admin"
	a.DB.SetAliasState(context.Background(), "pool@example.test", "paused")
	r := gin.New()
	a.Register(r.Group("/api/apps/mail"))
	req := httptest.NewRequest("GET", "/api/apps/mail/code?email=pool@example.test", nil)
	req.Header.Set("X-Admin-Key", "fixture-admin")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var result struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 409 || result.Error.Code != "MAILBOX_UNAVAILABLE" {
		t.Fatal(w.Code, w.Body.String())
	}
}
