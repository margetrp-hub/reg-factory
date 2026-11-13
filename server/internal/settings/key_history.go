package settings

import (
	"encoding/json"
	"errors"
	"os"
)

func KeyHistory(path, name string) ([]string, error) {
	raw, err := os.ReadFile(path + ".keys")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys map[string][]string
	if len(raw) > 1024*1024 || json.Unmarshal(raw, &keys) != nil {
		return nil, errors.New("CONFIG_KEY_HISTORY_INVALID")
	}
	return keys[name], nil
}

// Keep old local artifact keys before changing the active key. They are only
// used to decrypt already persisted material, never to authorize new requests.
func RetainArtifactKey(path, value string) error {
	if value == "" {
		return nil
	}
	const key = "REG_FACTORY_NODE_ARTIFACT_KEY"
	previous, err := KeyHistory(path, key)
	if err != nil {
		return err
	}
	for _, v := range previous {
		if value == v {
			return nil
		}
	}
	previous = append(previous, value)
	raw, err := json.Marshal(map[string][]string{key: previous})
	if err != nil || len(raw) > 1024*1024 {
		return errors.New("CONFIG_KEY_HISTORY_FULL")
	}
	return writePrivate(path+".keys", raw)
}
