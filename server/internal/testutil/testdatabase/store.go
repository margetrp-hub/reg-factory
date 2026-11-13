// Package testdatabase opens a per-test schema only in the disposable local DB.
package testdatabase

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

func Open(t *testing.T, root string) *store.Store {
	t.Helper()
	st, err := store.Open(DSN(t, root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// DSN lets the mail store exercise its own schema on the same isolated fixture.
func DSN(t *testing.T, root string) string {
	t.Helper()
	dsn := os.Getenv("REG_FACTORY_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = filepath.Join(root, "tasks.db")
	} else {
		u, err := url.Parse(dsn)
		if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.Path != "/sg_release_test" {
			t.Fatal("disposable local sg_release_test required")
		}
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		schema := fmt.Sprintf("journey_%x", time.Now().UnixNano())
		if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	return dsn
}
