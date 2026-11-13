package assetstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type credentialTask struct {
	TaskID        string         `json:"task_id"`
	Ref           string         `json:"ref"`
	Email         string         `json:"email"`
	UpdatedAt     string         `json:"credential_updated_at"`
	PublicationID string         `json:"publication_id,omitempty"`
	Document      map[string]any `json:"document"`
}

func (s *Store) credentialTaskPath(id string) string {
	return filepath.Join(filepath.Dir(s.root), "credential-task-results", fmt.Sprintf("%x.json", sha256.Sum256([]byte(id))))
}

func (s *Store) readCredentialTask(id string) (credentialTask, error) {
	var result credentialTask
	raw, err := os.ReadFile(s.credentialTaskPath(id))
	if err != nil {
		return result, err
	}
	if json.Unmarshal(raw, &result) != nil || result.TaskID != id || result.Ref == "" || result.UpdatedAt == "" {
		return result, errors.New("CREDENTIAL_TASK_RESULT_INVALID")
	}
	return result, nil
}

// ConfirmCredentialTask binds success to this worker's durable result, rather
// than mistaking another task's newer credentials for this task's success.
func (s *Store) ConfirmCredentialTask(id, ref string) (Asset, error) {
	result, err := s.readCredentialTask(id)
	if err != nil || result.Ref != ref {
		return Asset{}, errors.New("CREDENTIAL_TASK_RESULT_MISSING")
	}
	return s.confirmCredentialResult(result)
}

func (s *Store) confirmCredentialResult(result credentialTask) (Asset, error) {
	current, err := s.Get(result.Ref, result.Email)
	if err != nil {
		return Asset{}, err
	}
	expected := Asset{Platform: "chatgpt", Email: result.Email, Payload: result.Document}
	if current.Platform != "chatgpt" || !strings.EqualFold(current.Email, result.Email) ||
		current.CredentialUpdatedAt != result.UpdatedAt || !credentialComplete(expected) ||
		!credentialEqual(current, expected) {
		return Asset{}, errors.New("CREDENTIAL_TASK_RESULT_NOT_CURRENT")
	}
	return current, nil
}

type credentialPublication struct {
	TaskID string         `json:"task_id"`
	Ref    string         `json:"ref"`
	Email  string         `json:"email"`
	Source map[string]any `json:"source"`
	Files  []struct {
		Path   string         `json:"path"`
		Before map[string]any `json:"before"`
		After  map[string]any `json:"after"`
	} `json:"files"`
}

// RecoverCredentialTask publishes the executor's saved write set locally.
// It shares the account lock with Python and never repeats an upstream action.
func (s *Store) RecoverCredentialTask(id, ref string) (Asset, error) {
	result, err := s.readCredentialTask(id)
	receiptMissing := os.IsNotExist(err)
	var journal credentialPublication
	dataDir := filepath.Dir(s.root)
	if receiptMissing {
		paths, err := filepath.Glob(filepath.Join(dataDir, "oauth-results", "*.json"))
		if err != nil {
			return Asset{}, err
		}
		for _, path := range paths {
			raw, readErr := os.ReadFile(path)
			var saved credentialPublication
			if readErr != nil || json.Unmarshal(raw, &saved) != nil || saved.TaskID != id || saved.Ref != ref {
				continue
			}
			if result.PublicationID != "" {
				return Asset{}, errors.New("CREDENTIAL_PUBLICATION_AMBIGUOUS")
			}
			journal = saved
			result.PublicationID = strings.TrimSuffix(filepath.Base(path), ".json")
		}
		if result.PublicationID == "" {
			return Asset{}, os.ErrNotExist
		}
		if len(journal.Files) == 0 {
			return Asset{}, errors.New("CREDENTIAL_PUBLICATION_EMPTY")
		}
		after := journal.Files[len(journal.Files)-1].After
		document, err := OAuthDocument(after, journal.Email)
		if err != nil {
			return Asset{}, err
		}
		result.TaskID, result.Ref, result.Email = id, ref, journal.Email
		result.UpdatedAt = credentialString(after, "credential_updated_at")
		result.Document = document
	} else if err != nil {
		return Asset{}, err
	}
	if result.Ref != ref || result.UpdatedAt == "" || !credentialComplete(Asset{Payload: result.Document, Email: result.Email}) {
		return Asset{}, errors.New("CREDENTIAL_TASK_RESULT_INVALID")
	}
	unlock, err := s.lock(Asset{Platform: "chatgpt", Ref: ref, Email: result.Email})
	if err != nil {
		return Asset{}, err
	}
	defer unlock()
	if receiptMissing {
		if err := os.MkdirAll(filepath.Dir(s.credentialTaskPath(id)), 0700); err != nil {
			return Asset{}, err
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return Asset{}, err
		}
		if err = atomicWrite(s.credentialTaskPath(id), raw); err != nil {
			return Asset{}, err
		}
	}
	if current, err := s.confirmCredentialResult(result); err == nil {
		return current, nil
	}
	publicationID, err := hex.DecodeString(result.PublicationID)
	if err != nil || len(publicationID) != 32 || result.PublicationID != strings.ToLower(result.PublicationID) {
		return Asset{}, errors.New("CREDENTIAL_PUBLICATION_ID_INVALID")
	}
	if journal.TaskID == "" {
		raw, err := os.ReadFile(filepath.Join(dataDir, "oauth-results", result.PublicationID+".json"))
		if err != nil {
			return Asset{}, err
		}
		if err = json.Unmarshal(raw, &journal); err != nil {
			return Asset{}, errors.New("CREDENTIAL_PUBLICATION_INVALID")
		}
	}
	if journal.TaskID != id || journal.Ref != ref || journal.Email != result.Email {
		return Asset{}, errors.New("CREDENTIAL_TASK_MISMATCH")
	}
	current, err := s.Get(ref, result.Email)
	source := Asset{Platform: "chatgpt", Email: result.Email, Payload: journal.Source}
	if err != nil || !credentialEqual(current, source) || current.CredentialUpdatedAt != credentialString(journal.Source, "credential_updated_at") {
		return Asset{}, errors.New("CREDENTIAL_CHANGED_BEFORE_SAVE")
	}
	if len(journal.Files) == 0 {
		return Asset{}, errors.New("CREDENTIAL_PUBLICATION_EMPTY")
	}
	// Validate the persisted write set before replacing any current file.
	paths := make([]string, len(journal.Files))
	for i, item := range journal.Files {
		path := filepath.Join(dataDir, filepath.FromSlash(item.Path))
		parent, tail := filepath.Dir(path), filepath.Base(path)
		for {
			resolved, err := filepath.EvalSymlinks(parent)
			if err == nil {
				path = filepath.Join(resolved, tail)
				break
			}
			if !os.IsNotExist(err) {
				return Asset{}, err
			}
			tail = filepath.Join(filepath.Base(parent), tail)
			parent = filepath.Dir(parent)
		}
		// An existing destination may itself be a symlink.
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		} else if !os.IsNotExist(err) {
			return Asset{}, err
		}
		assetRoot, err := filepath.EvalSymlinks(s.root)
		if err != nil {
			return Asset{}, err
		}
		rel, err := filepath.Rel(assetRoot, path)
		if err != nil || !filepath.IsLocal(rel) || filepath.Ext(path) != ".json" {
			return Asset{}, errors.New("CREDENTIAL_PUBLICATION_PATH_INVALID")
		}
		after := Asset{Platform: "chatgpt", Email: result.Email, Payload: item.After}
		left, right := credentialScope(source), credentialScope(after)
		sameUser := left[1] != "" && left[1] == right[1]
		compatible := sameUser || ((left[0] == "" || right[0] == "" || left[0] == right[0]) && (left[1] == "" || right[1] == "" || left[1] == right[1]))
		if !compatible || !credentialEqual(after, Asset{Email: result.Email, Payload: result.Document}) || credentialString(item.After, "credential_updated_at") != result.UpdatedAt {
			return Asset{}, errors.New("CREDENTIAL_RESULT_MISMATCH")
		}
		var before map[string]any
		raw, err := os.ReadFile(path)
		if err == nil {
			if err = json.Unmarshal(raw, &before); err != nil {
				return Asset{}, errors.New("CREDENTIAL_PUBLICATION_INVALID")
			}
		} else if !os.IsNotExist(err) {
			return Asset{}, err
		}
		if !reflect.DeepEqual(before, item.Before) && !reflect.DeepEqual(before, item.After) {
			return Asset{}, errors.New("CREDENTIAL_CHANGED_BEFORE_SAVE")
		}
		paths[i] = path
	}
	for i, item := range journal.Files {
		if err := os.MkdirAll(filepath.Dir(paths[i]), 0700); err != nil {
			return Asset{}, err
		}
		raw, err := json.Marshal(item.After)
		if err != nil {
			return Asset{}, err
		}
		if err = atomicWrite(paths[i], raw); err != nil {
			return Asset{}, err
		}
	}
	return s.confirmCredentialResult(result)
}

// AcknowledgeCredentialTask retires only the temporary publication journal.
// Work's original receipt remains stable even when the account is reused later.
func (s *Store) AcknowledgeCredentialTask(id string) error {
	result, err := s.readCredentialTask(id)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if result.PublicationID != "" {
		idBytes, err := hex.DecodeString(result.PublicationID)
		if err != nil || len(idBytes) != 32 || result.PublicationID != strings.ToLower(result.PublicationID) {
			return errors.New("CREDENTIAL_PUBLICATION_ID_INVALID")
		}
		path := filepath.Join(filepath.Dir(s.root), "oauth-results", result.PublicationID+".json")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if !strings.HasPrefix(id, "work-") {
		err = os.Remove(s.credentialTaskPath(id))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	result.PublicationID = ""
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return atomicWrite(s.credentialTaskPath(id), raw)
}
