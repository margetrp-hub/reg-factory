package icmail

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in read-only probe. It uses an already saved source and never prints a
// credential, fetches message bodies, updates the index or sends mail.
func TestConfiguredIMAPReadOnly(t *testing.T) {
	path, sourceID := os.Getenv("REG_FACTORY_TEST_IMAP_DB"), os.Getenv("REG_FACTORY_TEST_IMAP_SOURCE")
	if path == "" || sourceID == "" {
		t.Skip("set REG_FACTORY_TEST_IMAP_DB and REG_FACTORY_TEST_IMAP_SOURCE")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var source Source
	err = db.QueryRow("SELECT host,port,username,password,folder FROM mail_sources WHERE id=?", sourceID).Scan(
		&source.Host, &source.Port, &source.Username, &source.Password, &source.Folder)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	t.Log("connecting and logging in")
	client, err := Dial(ctx, source)
	if err != nil {
		t.Fatal(strings.ReplaceAll(err.Error(), source.Password, "[redacted]"))
	}
	defer client.Close()
	t.Log("login and folder selection succeeded; checking mailbox status")
	if _, err := client.UIDNext(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("IMAP_READ_ONLY_OK")
}
