package browser

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Run the compiled test inside the release image with --network none. Only a
// loopback fixture is visited; no account, challenge or upstream participates.
func TestPackagedBrowser(t *testing.T) {
	if os.Getenv("REG_FACTORY_TEST_PACKAGED_BROWSER") != "1" {
		t.Skip("explicit packaged-browser check required")
	}
	t.Setenv("REG_FACTORY_BROWSER_PATH", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		http.SetCookie(w, &http.Cookie{Name: "fixture", Value: "local-only", Path: "/"})
		fmt.Fprint(w, `<title>release fixture</title><input id="value"><button id="go" onclick="document.title=document.getElementById('value').value">check</button>`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	b, err := Launch(ctx, Config{Headless: true, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err = b.Navigate(ctx, server.URL); err != nil {
		t.Fatal(err)
	}
	if err = b.Fill(ctx, "#value", "passed"); err != nil {
		t.Fatal(err)
	}
	if err = b.Click(ctx, "#go"); err != nil {
		t.Fatal(err)
	}
	if title, err := b.Title(ctx); err != nil || title != "passed" {
		t.Fatalf("click/readback: title=%q error=%v", title, err)
	}
	if value, found, err := b.Cookie(ctx, "fixture"); err != nil || !found || value != "local-only" {
		t.Fatalf("cookie readback: found=%v error=%v", found, err)
	}
	image := filepath.Join(t.TempDir(), "browser.png")
	if err = b.Screenshot(ctx, image); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(image); err != nil || info.Size() < 100 {
		t.Fatal("empty screenshot")
	}
}
