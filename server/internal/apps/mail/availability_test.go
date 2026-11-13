package mail

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestMailboxAvailabilityAcrossPlatformsAndRecovery(t *testing.T) {
	a := poolApp(t)
	src := poolSource(t, a, "availability")
	ctx := context.Background()
	if _, err := a.DB.ImportMailboxes(ctx, src.ID, []string{"person@example.test", "other@example.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.ex.Exec(`UPDATE mail_pool SET parent_id='parent@example.test'`); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	a.Register(router.Group("/api/apps/mail"))
	request := func(method, path, body string) (int, map[string]any) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/api/apps/mail"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, r)
		var data map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return w.Code, data
	}
	for _, state := range []string{BanBanned, BanBlocked, BanRateLimited, BanRegistered} {
		body := `{"email":"Person@example.test","platform":"ChatGPT","state":"` + state + `"}`
		if code, data := request("POST", "/pool/platform-state", body); code != 200 {
			t.Fatal(code, data)
		}
		for _, platform := range []string{"claude", "chatgpt", "grok", "outlook", "kiro", "github"} {
			code, data := request("GET", "/workflow/mailbox?email=person@example.test&platform="+platform, "")
			want := 200
			if platform == "chatgpt" && state != BanRegistered {
				want = 409
			}
			if code != want {
				t.Fatalf("%s %s: %d %v", state, platform, code, data)
			}
		}
	}
	if err := a.DB.SetMailbox(ctx, "other@example.test", src.ID, "paused"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"banned", "active"} {
		if code, data := request("POST", "/pool/parent-state", `{"parent_id":"parent@example.test","state":"`+state+`"}`); code != 200 {
			t.Fatal(code, data)
		}
		_, data := request("GET", "/workflow/candidates?platform=claude", "")
		want := float64(0)
		if state == "active" {
			want = 1
		}
		if data["data"].(map[string]any)["total"] != want {
			t.Fatal("parent recovery changed child pause", data)
		}
	}
	items, total, err := a.DB.PoolPage(ctx, "other", "paused", 0, 25, 0)
	if err != nil || total != 1 || items[0].ManualState != "paused" {
		t.Fatal(total, items, err)
	}
	if code, data := request("GET", "/pool/availability?email=person@example.test", ""); code != 200 || len(data["data"].(map[string]any)["platforms"].([]any)) != 1 {
		t.Fatal(code, data)
	}
	if err := a.DB.SetSourceEnabled(ctx, src.ID, false); err != nil {
		t.Fatal(err)
	}
	if code, _ := request("GET", "/workflow/mailbox?email=person@example.test&platform=claude", ""); code != 409 {
		t.Fatal(code)
	}
	if err := a.DB.SetSourceEnabled(ctx, src.ID, true); err != nil {
		t.Fatal(err)
	}
	if code, _ := request("GET", "/workflow/mailbox?email=person@example.test&platform=claude", ""); code != 200 {
		t.Fatal(code)
	}
	if err := a.DB.Cursor(src, nil, nil, "fixture connection failure"); err != nil {
		t.Fatal(err)
	}
	if code, _ := request("GET", "/workflow/mailbox?email=person@example.test&platform=claude", ""); code != 409 {
		t.Fatal("source failure masked as empty inbox", code)
	}
	_, data := request("GET", "/workflow/candidates?platform=claude", "")
	if data["data"].(map[string]any)["total"] != float64(0) {
		t.Fatal("failing source allocated", data)
	}
	now := time.Now()
	if err := a.DB.Cursor(src, nil, &now, ""); err != nil {
		t.Fatal(err)
	}
	if code, _ := request("GET", "/workflow/mailbox?email=person@example.test&platform=claude", ""); code != 200 {
		t.Fatal("source did not recover", code)
	}
	if code, _ := request("GET", "/workflow/mailbox?email=other@example.test&platform=claude", ""); code != 409 {
		t.Fatal("recovery cleared manual pause", code)
	}
}
