package assetstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	current, err := s.Get(ref, result.Email)
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
