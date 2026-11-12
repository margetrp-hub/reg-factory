package mail

import (
	"path/filepath"
	"testing"
)

func TestExtractCode(t *testing.T) {
	cases := map[string]string{
		"您的验证码是 482913，5 分钟内有效":             "482913",
		"Your verification code: 918273":    "918273",
		"918273 is your code":               "918273",
		"驗證碼：5566":                          "5566",
		"order 2048 shipped, invoice 99.00": "",
	}
	for text, want := range cases {
		got, _ := ExtractCode(text)
		if got != want {
			t.Fatalf("%q -> %q, want %q", text, got, want)
		}
	}
}

func TestStoreRoundTrip(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	sent := float64(1793450000)
	rec := MessageRecord{
		Mailbox: "INBOX", UID: "42", SourceID: 1, AliasHME: "someone@icloud.com",
		FromAddr: "no-reply@example.com", Subject: "验证码", PrimaryTag: "otp",
		Tags: []string{"otp"}, Snippet: "482913", BodyText: "您的验证码是 482913", SentAt: &sent,
	}
	isNew, err := db.StoreMessage(rec)
	if err != nil || !isNew {
		t.Fatalf("insert: new=%v err=%v", isNew, err)
	}
	if again, err := db.StoreMessage(rec); again || err != nil {
		t.Fatalf("duplicate must be a no-op: new=%v err=%v", again, err)
	}

	rows, err := db.Query("SOMEONE@icloud.com", "", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("query by alias: rows=%d err=%v", len(rows), err)
	}
	if code, ok := ExtractCode(rows[0].BodyText); !ok || code != "482913" {
		t.Fatalf("body lookup failed: %q %v", code, ok)
	}

	total, lastUID, err := db.Totals()
	if err != nil || total != 1 || lastUID != 42 {
		t.Fatalf("totals: %d %d %v", total, lastUID, err)
	}
	tags, err := db.CountsByTag()
	if err != nil || tags["otp"] != 1 {
		t.Fatalf("tag counts: %v %v", tags, err)
	}
}

func TestSourceUpsertAndCursor(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	src, err := db.UpsertSource(Source{
		Name: "default", Host: "imap.example.com", Port: 993, Username: "u", Password: "p",
		Folder: "INBOX", Enabled: true, Idle: true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	uid := uint32(5147)
	if err := db.Cursor(src, &uid, nil, ""); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	list, err := db.ListSources(true)
	if err != nil || len(list) != 1 || list[0].LastUID != 5147 {
		t.Fatalf("list: %+v err=%v", list, err)
	}
}
