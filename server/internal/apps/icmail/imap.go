// Package imapclient wraps go-imap/v2 with exactly the operations the indexer
// needs: UIDNEXT heartbeat, incremental UID search, batched header+body fetch
// and a push-based IDLE wait.
//
// IDLE uses go-imap's unilateral data handler instead of polling the socket
// (which is what the Python version does). The handler fires from the client's
// own reader goroutine, so a new message wakes this package immediately and the
// caller never has to poll.
package icmail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// HeaderFields is the projection fetched per message. Content-Type and
// MIME-Version are mandatory: without them the parser treats a multipart body as
// flat text and the snippet ends up full of boundary markers.
var HeaderFields = []string{
	"From", "To", "Cc", "Delivered-To", "X-Original-To", "Envelope-To", "Resent-To",
	"Apparently-To", "Subject", "Date", "Message-ID",
	"Content-Type", "MIME-Version", "Content-Transfer-Encoding",
}

const bodyPartialBytes = 8000

// Fetched is one message as returned by Fetch.
type Fetched struct {
	UID      uint32
	Envelope *imap.Envelope
	Headers  []byte
	Body     []byte
}

// Client is one authenticated, selected connection.
type Client struct {
	c       *imapclient.Client
	mailbox string
	updates chan uint32
	caps    imap.CapSet
	stop    func() bool
}

// Dial connects, logs in and selects the source's folder.
func Dial(ctx context.Context, src Source) (*Client, error) {
	updates := make(chan uint32, 8)
	opts := &imapclient.Options{
		TLSConfig: &tls.Config{ServerName: src.Host, MinVersion: tls.VersionTLS12},
		UnilateralDataHandler: &imapclient.UnilateralDataHandler{
			Mailbox: func(data *imapclient.UnilateralDataMailbox) {
				if data == nil || data.NumMessages == nil {
					return
				}
				select {
				case updates <- *data.NumMessages:
				default: // coalesce: a slow consumer must not block the reader
				}
			},
		},
	}

	addr := src.Addr()
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	// go-imap manages its own socket deadlines. Bound the whole handshake/login
	// independently so a silent provider cannot occupy a source indefinitely.
	loginCtx, cancelLogin := context.WithTimeout(ctx, 30*time.Second)
	stopLogin := context.AfterFunc(loginCtx, func() { _ = conn.Close() })
	defer cancelLogin()
	defer stopLogin()
	ok := false
	defer func() {
		if !ok {
			stop()
			_ = conn.Close()
		}
	}()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	var (
		raw *imapclient.Client
	)
	if src.Port == 143 {
		raw, err = imapclient.NewStartTLS(conn, opts)
	} else {
		tlsConn := tls.Client(conn, opts.TLSConfig)
		err = tlsConn.HandshakeContext(ctx)
		if err == nil {
			raw = imapclient.New(tlsConn, opts)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	// New starts a reader but does not wait for the greeting. QQ ignores LOGIN
	// sent before its greeting/capability exchange has finished.
	if err := raw.WaitGreeting(); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	if raw.Caps() == nil {
		_ = raw.Close()
		return nil, errors.New("mailbox capability handshake failed")
	}
	if err := raw.Login(src.Username, src.Password).Wait(); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("login %s: %w", src.Username, err)
	}
	folder := src.Folder
	if folder == "" {
		folder = "INBOX"
	}
	selectData, err := raw.Select(folder, nil).Wait()
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("select %s: %w", folder, err)
	}
	_ = selectData

	caps := raw.Caps()
	if loginCtx.Err() != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("mailbox login timeout: %w", loginCtx.Err())
	}
	_ = conn.SetDeadline(time.Time{})
	ok = true
	return &Client{c: raw, mailbox: folder, updates: updates, caps: caps, stop: stop}, nil
}

// Close logs out and drops the socket.
func (c *Client) Close() error {
	if c.c == nil {
		return nil
	}
	if c.stop != nil {
		c.stop()
	}
	return c.c.Close()
}

// SupportsIdle reports whether the server advertises IDLE.
func (c *Client) SupportsIdle() bool {
	return c.caps.Has(imap.CapIdle)
}

// UIDNext reads the mailbox's next UID — one cheap command, no result set.
func (c *Client) UIDNext(ctx context.Context) (uint32, error) {
	data, err := c.c.Status(c.mailbox, &imap.StatusOptions{UIDNext: true, NumMessages: true}).Wait()
	if err != nil {
		return 0, fmt.Errorf("status: %w", err)
	}
	return uint32(data.UIDNext), nil
}

// RecentUIDs returns UIDs past lastUID, or the newest bootstrap window when the
// cursor is unset.
func (c *Client) RecentUIDs(ctx context.Context, lastUID uint32, bootstrap int) ([]uint32, error) {

	crit := &imap.SearchCriteria{}
	if lastUID == 0 {
		crit = &imap.SearchCriteria{}
	} else {
		crit.UID = []imap.UIDSet{{imap.UIDRange{Start: imap.UID(lastUID + 1), Stop: 0}}}
	}
	data, err := c.c.UIDSearch(crit, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("uid search: %w", err)
	}
	all := data.AllUIDs()
	uids := make([]uint32, 0, len(all))
	for _, uid := range all {
		if lastUID != 0 && uint32(uid) <= lastUID {
			continue
		}
		uids = append(uids, uint32(uid))
	}
	if lastUID == 0 && bootstrap > 0 && len(uids) > bootstrap {
		uids = uids[len(uids)-bootstrap:]
	}
	return uids, nil
}

// Fetch pulls headers plus a partial body for the given UIDs.
func (c *Client) Fetch(ctx context.Context, uids []uint32) ([]Fetched, error) {
	if len(uids) == 0 {
		return nil, nil
	}

	set := make(imap.UIDSet, 0, len(uids))
	for _, uid := range uids {
		set = append(set, imap.UIDRange{Start: imap.UID(uid), Stop: imap.UID(uid)})
	}
	// Fetch the whole message plus the server-decoded envelope.
	//
	// A HEADER.FIELDS projection returns an empty block on QQ's IMAP (measured),
	// and partial ranges are ignored anyway, so the reliable shape is: envelope
	// for the decoded headers, BODY[] for the text.
	opts := &imap.FetchOptions{
		UID:          true,
		Envelope:     true,
		InternalDate: true,
		BodySection: []*imap.FetchItemBodySection{
			{Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: bodyPartialBytes}},
		},
	}
	buffers, err := c.c.Fetch(set, opts).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	out := make([]Fetched, 0, len(buffers))
	for _, buf := range buffers {
		item := Fetched{UID: uint32(buf.UID), Envelope: buf.Envelope}
		for _, section := range buf.BodySection {
			item.Body = append(item.Body, section.Bytes...)
		}
		out = append(out, item)
	}
	return out, nil
}

// WaitForMail idles until the server reports a change, the deadline expires or
// ctx is cancelled. A false result with nil error means "nothing new".
//
// The server's unilateral EXISTS notification is the fast path (~1s), but it is
// not guaranteed to reach the handler on every provider, so the loop also exits
// on a short tick and lets the caller re-check with STATUS(UIDNEXT). That keeps
// worst-case detection bounded at the tick interval while the common case stays
// push driven.
func (c *Client) WaitForMail(ctx context.Context, idleFor, recheckEvery time.Duration, wake <-chan struct{}) (bool, error) {
	if !c.SupportsIdle() {
		return false, errors.New("server does not advertise IDLE")
	}
	if recheckEvery <= 0 {
		recheckEvery = 15 * time.Second
	}
	idle, err := c.c.Idle()
	if err != nil {
		return false, fmt.Errorf("idle: %w", err)
	}
	timer := time.NewTimer(idleFor)
	ticker := time.NewTicker(recheckEvery)
	defer timer.Stop()
	defer ticker.Stop()

	changed := false
loop:
	for {
		select {
		case <-c.updates:
			changed = true
			break loop
		case <-ticker.C:
			break loop // let the caller verify with a cheap STATUS
		case <-wake:
			break loop
		case <-timer.C:
			break loop
		case <-ctx.Done():
			break loop
		}
	}

	// A server that never acknowledges DONE must not stall manual sync or
	// source reconfiguration indefinitely.
	exitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	stop := context.AfterFunc(exitCtx, func() { _ = c.Close() })
	defer cancel()
	defer stop()
	if err := idle.Close(); err != nil {
		return false, err
	}
	if err := idle.Wait(); err != nil {
		return false, err
	}
	return changed, exitCtx.Err()
}
