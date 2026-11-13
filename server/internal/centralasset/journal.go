package centralasset

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"github.com/google/uuid"
	"github.com/tiantianGPU/reg-factory/server/internal/fernet"
)

// Keep the released Python journal's filenames, Fernet wire format and payload.
// Upgrading or rolling back must never turn a saved result into another action.
type journal struct{ directory, key string }

func equal(a, b any) bool {
	x, e1 := json.Marshal(a)
	y, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && bytes.Equal(x, y)
}

func (j journal) path(id string) (string, error) {
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id {
		return "", errors.New("ASSET_JOURNAL_ID_INVALID")
	}
	return filepath.Join(j.directory, id+".result"), nil
}

func (j journal) sync() error {
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

func (j journal) get(id string) (map[string]any, error) {
	path, err := j.path(id)
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
	if !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
		return nil, errors.New("ASSET_JOURNAL_INVALID")
	}
	sealed, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw, err := fernet.Decrypt([]byte(j.key), sealed, 0)
	var value map[string]any
	if err != nil || len(raw) > 1024*1024 || json.Unmarshal(raw, &value) != nil || value["task_id"] != id {
		return nil, errors.New("ASSET_JOURNAL_UNREADABLE")
	}
	return value, nil
}

func (j journal) put(id string, value map[string]any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > 1024*1024 || value["task_id"] != id {
		return errors.New("ASSET_JOURNAL_INVALID")
	}
	path, err := j.path(id)
	if err != nil {
		return err
	}
	sealed, err := fernet.Encrypt([]byte(j.key), raw)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(j.directory, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(j.directory, ".result-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(sealed); err == nil {
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
		previous, err := j.get(id)
		if err != nil {
			return err
		}
		if !equal(previous, value) {
			return errors.New("ASSET_JOURNAL_CONFLICT")
		}
	}
	return j.sync()
}

func (j journal) verifyWritable() error {
	if err := os.MkdirAll(j.directory, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(j.directory, ".probe-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString("asset-result-journal"); err == nil {
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

func (j journal) acknowledge(id string, value map[string]any) error {
	current, err := j.get(id)
	if err != nil {
		return err
	}
	if !equal(current, value) {
		return errors.New("ASSET_JOURNAL_CONFLICT")
	}
	path, err := j.path(id)
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	return j.sync()
}
