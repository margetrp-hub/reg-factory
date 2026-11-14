package mail

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
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

const idleWindow = 25 * time.Minute
const recheckEvery = 15 * time.Second

// Runner owns the connection and cursor for one source.
type Runner struct {
	App  *App
	Src  Source
	Conn mailClient
	wake chan struct{}

	mu           sync.Mutex
	lastError    string
	lastOK       time.Time
	lastIdle     time.Time
	lastBatch    int
	graphToken   string
	graphExpiry  time.Time
	graphRefresh string
}

func (a *App) newRunner(src Source) *Runner {
	return &Runner{App: a, Src: src, wake: make(chan struct{}, 1)}
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
			case <-r.wake:
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

type mailClient interface {
	Close() error
	SupportsIdle() bool
	UIDNext(context.Context) (uint32, error)
	RecentUIDs(context.Context, uint32, int) ([]uint32, error)
	Fetch(context.Context, []uint32) ([]Fetched, error)
	WaitForMail(context.Context, time.Duration, time.Duration, <-chan struct{}) (bool, error)
}

func (r *Runner) tick(ctx context.Context) error {
	r.lastBatch = 0
	job, err := r.App.DB.claimSync(r.Src.ID)
	if err != nil {
		return err
	}
	var client mailClient
	switch r.Src.Transport {
	case "", TransportIMAP:
		client, err = r.ensureClient(ctx)
	case TransportGraph, TransportPOP3:
		op, cancel := context.WithTimeout(ctx, 60*time.Second)
		err = r.pollTransport(op)
		cancel()
	case TransportCloudmail:
		op, cancel := context.WithTimeout(ctx, 90*time.Second)
		err = r.pollCloudmail(op)
		cancel()
	default:
		err = fmt.Errorf("不支持此收件协议：%s", r.Src.Transport)
	}
	if err == nil && client != nil {
		op, cancel := context.WithTimeout(ctx, 60*time.Second)
		stop := context.AfterFunc(op, func() { _ = client.Close() })
		err = r.catchUp(op, client)
		stop()
		cancel()
	}
	if job > 0 {
		state, note := "completed", "同步完成"
		if err != nil {
			state, note = "failed", r.safeError(err)
		}
		if ctx.Err() != nil {
			state, note = "interrupted", "同步被中断，已保存收信仍保留；可重新同步"
		}
		if errors.Is(err, ErrSourceChanged) {
			state, note = "interrupted", err.Error()
		}
		if finishErr := r.App.DB.finishSync(job, state, note, r.lastBatch); finishErr != nil {
			return finishErr
		}
	}
	if err != nil {
		return err
	}
	var pending int
	if err = r.App.DB.ex.QueryRow("SELECT count(*) FROM mail_sync_requests WHERE source_id=? AND state='pending'", r.Src.ID).Scan(&pending); err != nil {
		return err
	}
	if pending > 0 {
		return nil
	}
	if client != nil && r.Src.Idle && client.SupportsIdle() {
		_, err = client.WaitForMail(ctx, idleWindow, recheckEvery, r.wake)
		return err
	}
	select {
	case <-time.After(time.Duration(r.heartbeat()) * time.Second):
	case <-r.wake:
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

func (r *Runner) ensureClient(ctx context.Context) (mailClient, error) {
	if r.Conn != nil {
		return r.Conn, nil
	}
	dial := r.App.dial
	if dial == nil {
		dial = func(ctx context.Context, src Source) (mailClient, error) { return Dial(ctx, src) }
	}
	client, err := dial(ctx, r.Src)
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
func (r *Runner) catchUp(ctx context.Context, client mailClient) error {
	r.lastBatch = 0
	uidNext, err := client.UIDNext(ctx)
	if err != nil {
		return err
	}
	if uidNext > 0 && uidNext-1 <= r.Src.LastUID {
		return r.markOK(0)
	}

	uids, err := client.RecentUIDs(ctx, r.Src.LastUID, r.App.Cfg.BootstrapLimit)
	if err != nil {
		return err
	}
	if len(uids) == 0 {
		return r.markOK(0)
	}

	chunk := r.App.Cfg.FetchChunk
	if chunk <= 0 {
		chunk = 40
	}
	stored := 0
	highest := r.Src.LastUID
	aliases, err := r.App.DB.SourceAliases(r.Src.ID)
	if err != nil {
		return err
	}
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
				Recipients: decoded.Recipients,
				Mailbox:    r.Src.Folder,
				UID:        itoa(msg.UID),
				SourceID:   r.Src.ID,
				AliasHME:   matchAlias(decoded.Recipients, aliases),
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
			isNew, err := r.App.DB.StoreSourceMessage(r.Src, rec)
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
				r.lastBatch = stored
			}
		}
	}

	if err := r.App.DB.Cursor(r.Src, &highest, timePtr(time.Now()), ""); err != nil {
		return err
	}
	r.Src.LastUID = highest
	return r.markOK(stored)
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

func (r *Runner) markOK(batch int) error {
	if err := r.App.DB.Cursor(r.Src, nil, timePtr(time.Now()), ""); err != nil {
		return err
	}
	r.mu.Lock()
	r.lastOK = time.Now()
	r.lastError = ""
	r.lastBatch = batch
	r.mu.Unlock()
	return nil
}

func (r *Runner) recordError(err error) {
	r.mu.Lock()
	r.lastError = r.safeError(err)
	r.mu.Unlock()
	_ = r.App.DB.Cursor(r.Src, nil, nil, r.safeError(err))
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

func (r *Runner) safeError(err error) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	for _, secret := range []string{r.Src.Password, r.Src.RefreshToken, r.Src.APIKey, r.Src.JWT, r.graphToken, r.graphRefresh} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if len(message) > 400 {
		message = message[:400]
	}
	return message
}

type managedRunner struct {
	runner *Runner
	source Source
	cancel context.CancelFunc
	done   chan struct{}
}

func sameSource(a, b Source) bool {
	// Graph reads its latest RT at each poll; normal token rotation must not
	// restart the source or discard the cached AT. Account/protocol changes do.
	return a.Host == b.Host && a.Port == b.Port && a.Username == b.Username && a.Password == b.Password && a.Folder == b.Folder && a.Idle == b.Idle && a.IntervalSeconds == b.IntervalSeconds && a.HeartbeatSeconds == b.HeartbeatSeconds && a.Transport == b.Transport && a.CredentialMode == b.CredentialMode && a.ClientID == b.ClientID && a.Endpoint == b.Endpoint
}

func (a *App) notifySources() {
	select {
	case a.changed <- struct{}{}:
	default:
	}
}

func (a *App) sourceRuntime(src Source) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !src.Enabled {
		return "paused"
	}
	if active := a.runners[src.ID]; active != nil && sameSource(src, active.source) {
		return "running"
	}
	return "starting"
}

// Source updates cancel and join the previous worker before opening a replacement.
func (a *App) RunSources(ctx context.Context) {
	_ = a.DB.recoverSync()
	active := map[int64]*managedRunner{}
	defer func() {
		for _, worker := range active {
			worker.cancel()
		}
		for _, worker := range active {
			<-worker.done
		}
		a.mu.Lock()
		a.runners = map[int64]*managedRunner{}
		a.mu.Unlock()
	}()
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	// One retention job for all sources/transports, rather than a full-store
	// cleanup after every individual IMAP batch.
	retention := time.NewTicker(time.Hour)
	defer retention.Stop()
	for {
		sources, err := a.DB.ListSources(true)
		if err == nil {
			desired := map[int64]Source{}
			for _, src := range sources {
				desired[src.ID] = src
			}
			for id, worker := range active {
				src, exists := desired[id]
				if !exists || !sameSource(src, worker.source) {
					worker.cancel()
					<-worker.done
					delete(active, id)
				}
			}
			for id, src := range desired {
				if active[id] == nil {
					runCtx, cancel := context.WithCancel(ctx)
					worker := &managedRunner{runner: a.newRunner(src), source: src, cancel: cancel, done: make(chan struct{})}
					active[id] = worker
					go func() { defer close(worker.done); worker.runner.Serve(runCtx) }()
				}
			}
			a.mu.Lock()
			a.runners = map[int64]*managedRunner{}
			for id, worker := range active {
				a.runners[id] = worker
			}
			a.mu.Unlock()
			_ = a.DB.cancelUnavailableSync()
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-retention.C:
			if a.Cfg.RetentionDays > 0 {
				if err := a.DB.PruneOlderThan(a.Cfg.RetentionDays); err != nil {
					log.Printf("[mail] scheduled retention failed: %v", err)
				}
			}
		case <-a.changed:
			for _, worker := range active {
				select {
				case worker.runner.wake <- struct{}{}:
				default:
				}
			}
		}
	}
}
