package nodeassets

import (
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var ErrJournal = errors.New("ASSET_JOURNAL_INVALID")
var ErrJournalConflict = errors.New("ASSET_JOURNAL_CONFLICT")
var ErrResultMissing = errors.New("ASSET_RESULT_NOT_AVAILABLE")

// ResultJournal stores immutable encrypted results. The host still owns task
// admission and its pre-execution intent: this API never runs a business action.
type ResultJournal struct {
	directory string
	cipher    cipher.AEAD
	mu        sync.Mutex
}

type SavedResult struct {
	Expected ResultExpectation `json:"expected"`
	Result   map[string]any    `json:"result"`
}

func NewResultJournal(directory, key string) (*ResultJournal, error) {
	c, err := aead(key)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	return &ResultJournal{directory: directory, cipher: c}, nil
}

func (j *ResultJournal) path(task string) (string, error) {
	if !identifier.MatchString(task) {
		return "", ErrScope
	}
	sum := sha256.Sum256([]byte(task))
	return filepath.Join(j.directory, hex.EncodeToString(sum[:])+".result"), nil
}

func (j *ResultJournal) sync() error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(j.directory)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// VerifyWritable must pass before a host starts an external operation.
func (j *ResultJournal) VerifyWritable() error {
	f, err := os.CreateTemp(j.directory, ".probe-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write([]byte("asset-result-journal")); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return j.sync()
}

func validSaved(value SavedResult, task string) bool {
	e := value.Expected
	if e.TaskID != task || !identifier.MatchString(e.TaskID) || !identifier.MatchString(e.AccountID) ||
		!identifier.MatchString(e.ResultID) || e.ExpectedRevision < 1 ||
		(e.Outcome != "success" && e.Outcome != "failed" && e.Outcome != "unknown") || len(value.Result) != 6 ||
		value.Result["result_id"] != e.ResultID || value.Result["outcome"] != e.Outcome {
		return false
	}
	for _, name := range []string{"result_id", "outcome", "document", "source_status", "source_liveness", "checked_at"} {
		if _, ok := value.Result[name]; !ok {
			return false
		}
	}
	if e.Refresh && e.Outcome == "success" {
		if _, ok := value.Result["document"].(map[string]any); !ok {
			return false
		}
	} else if value.Result["document"] != nil {
		return false
	}
	_, err := ObserveResult(value.Result)
	return err == nil
}

func (j *ResultJournal) load(task string) (*SavedResult, error) {
	path, err := j.path(task)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024+64 {
		return nil, ErrJournal
	}
	sealed, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(sealed) < j.cipher.NonceSize()+j.cipher.Overhead() {
		return nil, ErrJournal
	}
	raw, err := j.cipher.Open(nil, sealed[:j.cipher.NonceSize()], sealed[j.cipher.NonceSize():], []byte("aishop.asset-journal.v1\n"+task))
	if err != nil || len(raw) > 1024*1024 || uniqueJSON(raw) != nil {
		return nil, ErrJournal
	}
	var value SavedResult
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	if d.Decode(&value) != nil || !validSaved(value, task) {
		return nil, ErrJournal
	}
	return &value, nil
}

func (j *ResultJournal) Load(task string) (*SavedResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.load(task)
}

// Save commits the original result before transmission and never replaces it.
func (j *ResultJournal) Save(value SavedResult) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !validSaved(value, value.Expected.TaskID) {
		return ErrJournal
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 1024*1024 {
		return ErrJournal
	}
	path, err := j.path(value.Expected.TaskID)
	if err != nil {
		return err
	}
	nonce := make([]byte, j.cipher.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	sealed := j.cipher.Seal(nonce, nonce, raw, []byte("aishop.asset-journal.v1\n"+value.Expected.TaskID))
	f, err := os.CreateTemp(j.directory, ".result-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(sealed); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Link(f.Name(), path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		old, loadErr := j.load(value.Expected.TaskID)
		if loadErr != nil {
			return loadErr
		}
		previous, _ := json.Marshal(old)
		if !bytes.Equal(raw, previous) {
			return ErrJournalConflict
		}
	}
	return j.sync()
}

// Deliver sends only a saved result. Lost replies query the original receipt;
// another call, including after restart, reuses the same result ID and content.
func (j *ResultJournal) Deliver(ctx context.Context, task string, send func(context.Context, map[string]any) (map[string]any, error)) (string, map[string]any, error) {
	value, err := j.Load(task)
	if err != nil {
		return "pending", nil, err
	}
	if value == nil {
		return "pending", nil, ErrResultMissing
	}
	receipt, err := send(ctx, map[string]any{"action": "result", "task_id": task, "result": value.Result})
	state := ""
	if err == nil {
		state, err = ValidateResultReceipt(receipt, value.Expected)
	}
	if err != nil {
		receipt, err = send(ctx, map[string]any{"action": "receipt", "task_id": task})
		if err == nil {
			state, err = ValidateResultReceipt(receipt, value.Expected)
		}
	}
	if err != nil {
		return "pending", nil, err
	}
	return state, receipt, nil
}

// Acknowledge is called only after the host commits its terminal task record.
func (j *ResultJournal) Acknowledge(task string, receipt map[string]any) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	value, err := j.load(task)
	if err != nil || value == nil {
		return err
	}
	state, err := ValidateResultReceipt(receipt, value.Expected)
	if err != nil {
		return err
	}
	if state != "completed" && state != "failed" && state != "closed" {
		return ErrAcknowledgement
	}
	path, _ := j.path(task)
	if err = os.Remove(path); err != nil {
		return err
	}
	return j.sync()
}
