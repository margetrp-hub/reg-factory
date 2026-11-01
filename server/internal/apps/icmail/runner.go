package icmail

// Ingestion runner: one IMAP source, push first, poll as fallback.
//
// Ported from the standalone service; the behaviour that matters is unchanged:
//  - IDLE wakes us immediately (the server's unilateral EXISTS), with a 15s
//    recheck so a missed notification costs at most one heartbeat;
//  - STATUS(UIDNEXT) is the cheap "is there anything new" probe;
//  - the cursor advances over messages we already have (a restart must not
//    re-bootstrap the whole mailbox);
//  - auth failures are recorded on the source row so the panel can show them.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const idleWindow = 25 * time.Minute
const recheckEvery = 15 * time.Second

// Runner owns the connection and cursor for one source.
type Runner struct {
	App  *App
	Src  Source
	Conn *Client

	aliasCache []string
	aliasAt    time.Time
	mu         sync.Mutex
	lastError  string
	lastOK     time.Time
	lastIdle   time.Time
	lastBatch  int
}

func (a *App) newRunner(src Source) *Runner {
	return &Runner{App: a, Src: src}
}

// Serve runs until ctx is cancelled.
func (r *Runner) Serve(ctx context.Context) {
	backoff := time.Duration(0)
	for {
		if ctx.Err() != nil {
			r.close()
			return
		}
		if backoff > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				r.close()
				return
			}
		}
		if err := r.tick(ctx); err != nil {
			r.recordError(err)
			r.close()
			backoff = time.Duration(r.Src.IntervalSeconds) * time.Second
			if backoff < 5*time.Second {
				backoff = 5 * time.Second
			}
			continue
		}
		backoff = 0
	}
}

func (r *Runner) tick(ctx context.Context) error {
	client, err := r.ensureClient(ctx)
	if err != nil {
		return err
	}
	if r.Src.Idle && client.SupportsIdle() {
		if err := r.catchUp(ctx, client); err != nil {
			return err
		}
		_, err := client.WaitForMail(ctx, idleWindow, recheckEvery)
		if err != nil {
			// IDLE refused/dropped: fall back to a heartbeat, never fail the source.
			select {
			case <-time.After(time.Duration(r.heartbeat()) * time.Second):
			case <-ctx.Done():
			}
			return r.catchUp(ctx, client)
		}
		return r.catchUp(ctx, client)
	}

	if err := r.catchUp(ctx, client); err != nil {
		return err
	}
	select {
	case <-time.After(time.Duration(r.heartbeat()) * time.Second):
	case <-ctx.Done():
	}
	return nil
}

func (r *Runner) heartbeat() int {
	if r.Src.HeartbeatSeconds > 0 {
		return r.Src.HeartbeatSeconds
	}
	return 60
}

func (r *Runner) ensureClient(ctx context.Context) (*Client, error) {
	if r.Conn != nil {
		return r.Conn, nil
	}
	client, err := Dial(ctx, r.Src)
	if err != nil {
		return nil, err
	}
	r.Conn = client
	return client, nil
}

func (r *Runner) close() {
	if r.Conn != nil {
		_ = r.Conn.Close()
		r.Conn = nil
	}
}

// catchUp indexes everything past the cursor.
func (r *Runner) catchUp(ctx context.Context, client *Client) error {
	uidNext, err := client.UIDNext(ctx)
	if err != nil {
		return err
	}
	if uidNext > 0 && uidNext-1 <= r.Src.LastUID {
		r.markOK(0)
		return nil
	}

	uids, err := client.RecentUIDs(ctx, r.Src.LastUID, r.App.Cfg.BootstrapLimit)
	if err != nil {
		return err
	}
	if len(uids) == 0 {
		r.markOK(0)
		return nil
	}

	chunk := r.App.Cfg.FetchChunk
	if chunk <= 0 {
		chunk = 40
	}
	stored := 0
	highest := r.Src.LastUID
	for start := 0; start < len(uids); start += chunk {
		end := start + chunk
		if end > len(uids) {
			end = len(uids)
		}
		messages, err := client.Fetch(ctx, uids[start:end])
		if err != nil {
			return err
		}
		for _, msg := range messages {
			decoded := Parse(msg.Body)
			verdict := Classify(decoded.Subject, decoded.From, decoded.Text)
			rec := MessageRecord{
				Mailbox:    r.Src.Folder,
				UID:        itoa(msg.UID),
				SourceID:   r.Src.ID,
				AliasHME:   matchAlias(decoded.Recipients, r.aliasSet()),
				FromAddr:   decoded.From,
				ToHeader:   decoded.To,
				Subject:    decoded.Subject,
				MessageID:  decoded.MessageID,
				Tags:       verdict.Tags,
				PrimaryTag: verdict.PrimaryTag,
				Score:      verdict.Score,
				Snippet:    decoded.Snippet,
				BodyText:   decoded.Text,
			}
			if decoded.SentAt != nil {
				ts := float64(decoded.SentAt.Unix())
				rec.SentAt = &ts
			}
			isNew, err := r.App.DB.StoreMessage(rec)
			if err != nil {
				return err
			}
			// The cursor must move past messages we already had; otherwise a
			// restart re-bootstraps the same window forever.
			if msg.UID > highest {
				highest = msg.UID
			}
			if isNew {
				stored++
			}
		}
	}

	r.Src.LastUID = highest
	if err := r.App.DB.Cursor(r.Src.ID, &highest, timePtr(time.Now()), ""); err != nil {
		return err
	}
	if r.App.Cfg.RetentionDays > 0 {
		_ = r.App.DB.PruneOlderThan(r.App.Cfg.RetentionDays)
	}
	r.markOK(stored)
	return nil
}

func (r *Runner) aliasSet() []string {
	if r.aliasCache != nil && time.Since(r.aliasAt) < time.Minute {
		return r.aliasCache
	}
	// reg-factory has no mailbox table of its own; aliases are optional here, so
	// an empty set simply means "no alias attribution".
	r.aliasCache = []string{}
	r.aliasAt = time.Now()
	return r.aliasCache
}

func matchAlias(recipients, aliases []string) string {
	if len(aliases) == 0 || len(recipients) == 0 {
		return ""
	}
	set := make(map[string]bool, len(aliases))
	for _, a := range aliases {
		set[strings.ToLower(a)] = true
	}
	for _, r := range recipients {
		if set[strings.ToLower(r)] {
			return r
		}
	}
	return ""
}

func (r *Runner) markOK(batch int) {
	r.mu.Lock()
	r.lastOK = time.Now()
	r.lastError = ""
	r.lastBatch = batch
	r.mu.Unlock()
}

func (r *Runner) recordError(err error) {
	r.mu.Lock()
	r.lastError = err.Error()
	r.mu.Unlock()
	_ = r.App.DB.Cursor(r.Src.ID, nil, nil, err.Error())
}

func timePtr(t time.Time) *time.Time { return &t }

func itoa(uid uint32) string {
	if uid == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for uid > 0 {
		i--
		buf[i] = byte('0' + uid%10)
		uid /= 10
	}
	return string(buf[i:])
}

var errNoSource = errors.New("no enabled mail source")

// RunSources keeps one runner per enabled source until ctx is cancelled.
func (a *App) RunSources(ctx context.Context) {
	sources, err := a.DB.ListSources(true)
	if err != nil {
		return
	}
	if len(sources) == 0 {
		<-ctx.Done()
		return
	}
	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func(s Source) {
			defer wg.Done()
			a.newRunner(s).Serve(ctx)
		}(src)
	}
	wg.Wait()
}

var _ = imap.CapIdle
var _ = imapclient.DialTLS
