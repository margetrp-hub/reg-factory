package centralasset

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"aishop-next/execution-go/nodeassets"
	"github.com/google/uuid"
	"github.com/tiantianGPU/reg-factory/server/internal/fernet"
	"github.com/tiantianGPU/reg-factory/server/internal/store"
)

type ShareRecord struct {
	Ref           string
	RequestID     string
	Config        Config
	OptionsDigest string
	Pending       map[string]any
	Receipt       map[string]any
	Status        string
	Error         string
	LastSync      string
}

func (r ShareRecord) Public() map[string]any {
	return map[string]any{"ref": r.Ref, "request_id": r.RequestID, "status": r.Status, "error": r.Error,
		"last_sync": r.LastSync, "pending": r.Status != "transferred", "center_id": r.Config.CenterID}
}

// Retry only explicitly submitted immutable handoffs. Never observe a node to
// import assets, poll ownership, or synchronize later local changes.
type Sharing struct {
	Store   *store.Store
	Config  func() (Config, error)
	Current func(string, map[string]any) (map[string]any, error)
	Detach  func(string) error
	Send    func(context.Context, Config, map[string]any) (map[string]any, error)
	locks   [64]sync.Mutex
}

func (s *Sharing) lock(ref string) *sync.Mutex {
	sum := sha256.Sum256([]byte(ref))
	return &s.locks[int(sum[0])%len(s.locks)]
}
func (s *Sharing) Init() error {
	_, err := s.Store.Execer().Exec(`CREATE TABLE IF NOT EXISTS asset_sharing(source_ref TEXT PRIMARY KEY,record_ciphertext TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS central_received_assets(task_id TEXT PRIMARY KEY,account_id TEXT NOT NULL,revision INTEGER NOT NULL,record_ciphertext TEXT NOT NULL,received_at TEXT NOT NULL);`)
	return err
}
func (s *Sharing) load(ref string) (ShareRecord, error) {
	var r ShareRecord
	var sealed string
	if err := s.Store.Execer().QueryRow(`SELECT record_ciphertext FROM asset_sharing WHERE source_ref=?`, ref).Scan(&sealed); err != nil {
		return r, err
	}
	c, _ := s.Config()
	keys := []Config{c}
	previous, err := history(c.Directory)
	if err != nil {
		return r, err
	}
	keys = append(keys, previous...)
	for _, key := range keys {
		raw, e := fernet.Decrypt([]byte(key.JournalKey), []byte(sealed), 0)
		if e == nil && json.Unmarshal(raw, &r) == nil && r.Ref == ref {
			return r, nil
		}
	}
	return r, errors.New("ASSET_HANDOFF_RECORD_UNREADABLE")
}
func (s *Sharing) save(r ShareRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	sealed, err := fernet.Encrypt([]byte(r.Config.JournalKey), raw)
	if err != nil {
		return err
	}
	_, err = s.Store.Execer().Exec(`INSERT INTO asset_sharing(source_ref,record_ciphertext) VALUES(?,?)
      ON CONFLICT(source_ref) DO UPDATE SET record_ciphertext=excluded.record_ciphertext`, r.Ref, sealed)
	return err
}
func (s *Sharing) Refs() ([]string, error) {
	rows, err := s.Store.Execer().Query(`SELECT source_ref FROM asset_sharing ORDER BY source_ref`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []string{}
	for rows.Next() {
		var ref string
		if err = rows.Scan(&ref); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
func (s *Sharing) List() ([]map[string]any, error) {
	refs, err := s.Refs()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, ref := range refs {
		r, e := s.load(ref)
		if e != nil {
			out = append(out, map[string]any{"ref": ref, "status": "unreadable", "error": "ASSET_HANDOFF_RECORD_UNREADABLE"})
			continue
		}
		out = append(out, r.Public())
	}
	return out, nil
}
func (s *Sharing) Preview(ref string, options map[string]any) (map[string]any, error) {
	return s.Current(ref, options)
}
func (s *Sharing) Consent(ref, id string, options map[string]any) (ShareRecord, error) {
	mu := s.lock(ref)
	mu.Lock()
	defer mu.Unlock()
	if _, err := uuid.Parse(id); err != nil {
		return ShareRecord{}, errors.New("ASSET_REQUEST_ID_INVALID")
	}
	r, err := s.load(ref)
	if err == nil {
		if r.RequestID == id && r.OptionsDigest == optionsDigest(options) {
			return r, nil
		}
		if r.Status != "rejected" || r.RequestID == id {
			return r, errors.New("ASSET_HANDOFF_ALREADY_SUBMITTED")
		}
		// Keep the rejected request's identity/reason without retaining secrets.
		if _, e := s.Store.Execer().Exec(`INSERT INTO audit(ts,actor,action,detail,ip) VALUES(?,?,?,?,?)`, time.Now().UTC().Format(time.RFC3339Nano), "admin", "asset.handoff.replaced", r.RequestID+":"+r.Error, ""); e != nil {
			return r, e
		}
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return r, err
	}
	cfg, err := s.Config()
	if err != nil {
		return r, err
	}
	if err = PreserveConfig(cfg); err != nil {
		return r, err
	}
	entry, err := s.Current(ref, options)
	if err != nil {
		return r, err
	}
	if len(nodeassets.MissingSnapshotFields(entry)) != 0 {
		return r, errors.New("ASSET_ENVELOPE_INVALID")
	}
	r = ShareRecord{Ref: ref, RequestID: id, Config: cfg, OptionsDigest: optionsDigest(options), Status: "pending", Pending: map[string]any{"action": "snapshot", "task_id": id, "entry": entry}}
	return r, s.save(r)
}
func optionsDigest(options map[string]any) string {
	raw, _ := json.Marshal(options)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
func (s *Sharing) send(ctx context.Context, c Config, body map[string]any) (map[string]any, error) {
	if s.Send != nil {
		return s.Send(ctx, c, body)
	}
	send, err := c.sender()
	if err != nil {
		return nil, err
	}
	return send(ctx, body)
}
func (s *Sharing) Sync(ctx context.Context, ref string) (ShareRecord, error) {
	mu := s.lock(ref)
	mu.Lock()
	defer mu.Unlock()
	r, err := s.load(ref)
	if err != nil {
		return r, err
	}
	if r.Status == "transferred" || r.Status == "rejected" {
		return r, nil
	}
	if r.Receipt == nil {
		answer, e := s.send(ctx, r.Config, r.Pending)
		if e == nil {
			e = nodeassets.VerifySnapshotReceipt(r.Pending, answer, r.Config.NodeID, "")
		}
		if e != nil {
			var rejected *nodeassets.SnapshotRejectedError
			if errors.As(e, &rejected) {
				r.Status = "rejected"
				r.Receipt = answer
				r.Pending = nil
			}
			r.Error = "ASSET_HANDOFF_UNCONFIRMED"
			if strings.HasPrefix(e.Error(), "ASSET_") {
				r.Error = strings.Split(e.Error(), ":")[0]
			}
			if saveErr := s.save(r); saveErr != nil {
				return r, saveErr
			}
			return r, e
		}
		r.Receipt = answer
		r.Status = "received"
		r.LastSync, _ = answer["sharing"].(map[string]any)["saved_at"].(string)
		r.Error = ""
		if err = s.save(r); err != nil {
			return r, err
		}
	}
	// The original source is withdrawn only after a matching durable ACK.
	if s.Detach != nil {
		if err = s.Detach(ref); err != nil {
			r.Error = "ASSET_SOURCE_WITHDRAW_PENDING"
			_ = s.save(r)
			return r, err
		}
	}
	r.Status, r.Error, r.Pending = "transferred", "", nil
	return r, s.save(r)
}
func (s *Sharing) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			refs, err := s.Refs()
			if err != nil {
				continue
			}
			for _, ref := range refs {
				bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
				_, _ = s.Sync(bounded, ref)
				cancel()
				if ctx.Err() != nil {
					return
				}
			}
		}
	}()
	return done
}
