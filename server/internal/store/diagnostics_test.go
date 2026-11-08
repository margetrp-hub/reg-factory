package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistedAndHistoricalLogsRedactSecrets(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "node.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	line := `HTTP401 access_token="secret-at" refresh_token=secret-rt password=secret-password https://user:secret-proxy@proxy.test`
	if err := s.AppendEvent("test", "stderr", line); err != nil {
		t.Fatal(err)
	}
	// Simulate an old pre-redaction row, which must also be safe on read.
	if _, err := s.db.Exec(`INSERT INTO events(task_id,ts,stream,line) VALUES('test',?,'stderr',?)`, now(), line); err != nil {
		t.Fatal(err)
	}
	for _, read := range []func() ([]Event, error){func() ([]Event, error) { return s.TailEvents("test", 10) }, func() ([]Event, error) { return s.EventsAfter("test", 0, 10) }} {
		events, err := read()
		if err != nil || len(events) != 2 {
			t.Fatal(err)
		}
		for _, event := range events {
			if strings.Contains(event.Line, "secret-") || !strings.Contains(event.Line, "HTTP401") {
				t.Fatal("unsafe or useless diagnostic")
			}
		}
	}
}
