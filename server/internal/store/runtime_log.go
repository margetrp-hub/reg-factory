package store

import (
	"strings"
	"sync"
)

// RuntimeLog mirrors the service logger into the same terminal as worker output.
// Only service diagnostics rotate; task evidence retains its existing lifetime.
type RuntimeLog struct {
	Store  *Store
	mu     sync.Mutex
	writes int
}

func (w *RuntimeLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if err := w.Store.AppendEvent("", "system", line); err != nil {
			return 0, err
		}
	}
	w.writes++
	if w.writes%100 == 0 {
		_, err := w.Store.db.Exec(`DELETE FROM events WHERE task_id='' AND id <
			(SELECT id FROM events WHERE task_id='' ORDER BY id DESC LIMIT 1 OFFSET 9999)`)
		if err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}
