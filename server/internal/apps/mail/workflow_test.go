package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestWorkflowPythonClientWithRealMailboxAPI(t *testing.T) {
	python := os.Getenv("REG_FACTORY_TEST_PYTHON")
	if python == "" {
		t.Skip("set REG_FACTORY_TEST_PYTHON for cross-language mailbox test")
	}
	a := poolApp(t)
	source := poolSource(t, a, "primary")
	email := "python@example.test"
	router := gin.New()
	router.Use(func(c *gin.Context) {
		if c.GetHeader("X-API-Key") != "fixture-key" {
			c.AbortWithStatus(401)
			return
		}
		c.Next()
	})
	a.Register(router.Group("/api/apps/mail"))
	sequence := 0
	router.POST("/fixture/send", func(c *gin.Context) {
		sequence++
		_, err := a.DB.StoreMessage(MessageRecord{SourceID: source.ID, Mailbox: "INBOX", UID: fmt.Sprint(sequence),
			ToHeader: email, Subject: "OpenAI verification code", BodyText: fmt.Sprintf("Your code: %d", 123450+sequence)})
		if err != nil {
			c.AbortWithStatus(500)
			return
		}
		c.Status(204)
	})
	router.POST("/fixture/state/:state", func(c *gin.Context) {
		if err := a.DB.SetMailbox(context.Background(), email, source.ID, c.Param("state")); err != nil {
			c.AbortWithStatus(500)
			return
		}
		c.Status(204)
	})
	host := httptest.NewServer(router)
	defer host.Close()
	root, _ := filepath.Abs("../../../..")
	data := t.TempDir()
	cmd := exec.Command(python, "-c", `
import asyncio, os, requests
from common import mail_pool as mail, emails
from tools.import_plus_codex import MailCodeProvider
emails.EMAILS_FILE = os.path.join(os.environ['REG_FACTORY_DATA_DIR'], 'emails.txt')
url='http://' + os.environ['REG_FACTORY_MAIL_ADDR']
def action(path):
    with requests.Session() as s:
        s.trust_env=False
        s.post(url+'/fixture/'+path,headers={'X-API-Key':'fixture-key'},timeout=5).raise_for_status()
action('send')
try: mail.allocate()
except mail.MailboxError: pass
else: raise AssertionError('discovered recipient allocated without import')
discovered=mail._get('/pool')['items'][0]
with requests.Session() as s:
    s.trust_env=False
    body={'emails':[discovered['email']], 'source_id':discovered['sourceId']}
    response=s.post(url+'/api/apps/mail/pool/import',json=body,headers={'X-API-Key':'fixture-key'},timeout=5)
    assert response.json()['data']['added']==1, 'discovered address did not become an imported registration address'
    replay=s.post(url+'/api/apps/mail/pool/import',json=body,headers={'X-API-Key':'fixture-key'},timeout=5)
    assert replay.json()['data']['added']==0
m=mail.allocate()
action('send')
assert asyncio.run(mail.poll_code(m,max_wait=.2))=='123452'
p=MailCodeProvider({'email':m['email'],'provider':'icmail'},None,None)
asyncio.run(p.prepare())
action('state/paused')
try: asyncio.run(p.prepare())
except mail.MailboxError: pass
else: raise AssertionError('paused mailbox accepted')
action('state/active')
asyncio.run(p.prepare())
assert asyncio.run(mail.poll_code(p.icmail,max_wait=.05,poll_interval=.01)) is None
action('send')
assert asyncio.run(p(m['email'],0))=='123453'
print('MAILBOX_JOURNEY_OK')
`)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONPATH="+root, "REG_FACTORY_DATA_DIR="+data,
		"REG_FACTORY_MAIL_ADDR="+strings.TrimPrefix(host.URL, "http://"), "REG_FACTORY_MAIL_KEY=fixture-key", "PYTHONIOENCODING=utf-8")
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "MAILBOX_JOURNEY_OK") {
		t.Fatalf("mailbox journey: %v\n%s", err, output)
	}
}

func TestWorkflowMailboxFreshMailAndRecovery(t *testing.T) {
	a := poolApp(t)
	source := poolSource(t, a, "primary")
	other := poolSource(t, a, "other")
	email := "login@example.test"
	a.DB.ImportMailboxes(context.Background(), source.ID, []string{email})
	router := gin.New()
	a.Register(router.Group("/mail"))
	request := func(query string) (int, map[string]any) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/mail/workflow/mailbox?email="+email+query, nil))
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return w.Code, body
	}
	now := float64(time.Now().Unix())
	store := func(id, address string, sourceID int64, at float64) {
		t.Helper()
		if _, err := a.DB.StoreMessage(MessageRecord{SourceID: sourceID, Mailbox: "INBOX", UID: id,
			ToHeader: address, Subject: "OpenAI code", BodyText: "code: 123456", SentAt: &at}); err != nil {
			t.Fatal(err)
		}
	}
	store("old", email, source.ID, now-3600)
	// A malformed Date must neither hide the ID watermark nor crowd new OTPs
	// out of a page before the cursor and time predicates are applied.
	for i := 0; i < 105; i++ {
		store(fmt.Sprintf("future-%d", i), email, source.ID, now+86400)
	}
	store("latest-id-old-date", email, source.ID, now-7200)
	status, body := request("")
	if status != 200 {
		t.Fatal(status, body)
	}
	cursor := int64(body["data"].(map[string]any)["cursor"].(float64))
	if cursor != 107 {
		t.Fatal("watermark must use ID, not the newest page by Date", cursor)
	}
	store("wrong-email", "other-"+email, source.ID, now)
	store("wrong-source", email, other.ID, now)
	store("old-delayed", email, source.ID, now-60)
	store("new", email, source.ID, now)
	query := fmt.Sprintf("&cursor=%d&after=%.0f", cursor, now)
	status, body = request(query)
	if status != 200 {
		t.Fatal(status, body)
	}
	items := body["data"].(map[string]any)["messages"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["uid"] != "new" {
		t.Fatal(items)
	}
	for _, state := range []string{"paused", "blocked"} {
		if err := a.DB.SetMailbox(context.Background(), email, source.ID, state); err != nil {
			t.Fatal(err)
		}
		if code, _ := request(query); code != 409 {
			t.Fatal("disabled address supplied mail", code)
		}
	}
	a.DB.SetMailbox(context.Background(), email, source.ID, "active")
	if code, _ := request(query); code != 200 {
		t.Fatal("reconfigured address did not recover", code)
	}
	source.Enabled = false
	if _, err := a.DB.UpsertSource(source); err != nil {
		t.Fatal(err)
	}
	if code, _ := request(query); code != 409 {
		t.Fatal("disabled source supplied mail", code)
	}
	for _, bad := range []string{"&after=NaN", "&after=Inf", "&cursor=-1"} {
		if code, _ := request(bad); code != 400 {
			t.Fatal("invalid bound", bad, code)
		}
	}
}

func TestImportDiscoveredMailboxPreservesSourceAndPausedState(t *testing.T) {
	a := poolApp(t)
	one, two := poolSource(t, a, "one"), poolSource(t, a, "two")
	email := "discovered@example.test"
	if _, err := a.DB.StoreMessage(MessageRecord{SourceID: one.ID, Mailbox: "INBOX", UID: "1", ToHeader: email}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.DB.SetMailbox(ctx, email, one.ID, "paused"); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		source int64
		added  int
	}{{two.ID, 0}, {one.ID, 1}, {one.ID, 0}} {
		added, err := a.DB.ImportMailboxes(ctx, check.source, []string{email})
		if err != nil || added != check.added {
			t.Fatal(added, err)
		}
		items, total, err := a.DB.PoolPage(ctx, email, "", 0, 25, 0)
		if err != nil || total != 1 || items[0].SourceID != one.ID || items[0].State != "paused" {
			t.Fatal("import changed source, paused state or identity count", items, total, err)
		}
	}
}
