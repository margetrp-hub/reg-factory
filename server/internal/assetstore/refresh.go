package assetstore

import (
	"errors"
	"strings"
)

// RefreshedOAuth requires a new AT. Metadata changes or a still-valid old AT
// cannot establish that an uncertain RT exchange completed.
func (s *Store) RefreshedOAuth(ref, sourceSHA string) (Asset, map[string]any, error) {
	fail := errors.New("尚未找到本次保存的新凭据；保留原任务待核对")
	if sourceSHA == "" {
		return Asset{}, nil, fail
	}
	before, err := s.Result(ref, sourceSHA)
	if err != nil {
		return Asset{}, nil, fail
	}
	current, err := s.Get(ref, before.Email)
	if err != nil || current.Platform != "chatgpt" || current.ResultSHA == sourceSHA {
		return Asset{}, nil, fail
	}
	credentials, err := OAuthDocument(current.Payload, current.Email)
	if err != nil {
		return Asset{}, nil, fail
	}
	if rt, _ := credentials["refresh_token"].(string); strings.TrimSpace(rt) == "" {
		return Asset{}, nil, fail
	}
	old, _ := OAuthDocument(before.Payload, before.Email)
	if old != nil {
		if old["access_token"] == credentials["access_token"] {
			return Asset{}, nil, fail
		}
		for _, key := range []string{"account_id", "workspace_account_id", "email"} {
			if value, _ := old[key].(string); value != "" && credentials[key] != value {
				return Asset{}, nil, errors.New("凭据身份不一致，需人工核对原版本")
			}
		}
		newID, _ := credentials["id_token"].(string)
		if value, _ := old["id_token"].(string); value != "" && strings.TrimSpace(newID) == "" {
			return Asset{}, nil, fail
		}
	}
	if before.Email != "" && !strings.EqualFold(before.Email, current.Email) {
		return Asset{}, nil, errors.New("凭据账号不一致")
	}
	return current, credentials, nil
}
