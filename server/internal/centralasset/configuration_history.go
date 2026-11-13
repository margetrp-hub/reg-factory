package centralasset

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Connection history is local recovery material, just like the existing private
// env file. It is never exposed by configuration APIs or used for a new task.
func history(directory string) ([]Config, error) {
	raw, err := os.ReadFile(filepath.Join(directory, "connections.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var result []Config
	if len(raw) > 1024*1024 || json.Unmarshal(raw, &result) != nil {
		return nil, errors.New("ASSET_CONFIGURATION_HISTORY_INVALID")
	}
	return result, nil
}

// PreserveConfig must finish before publishing the new configuration. A failed
// save can leave an extra recovery entry; it cannot strand an existing result.
func PreserveConfig(c Config) error {
	if c.JournalKey == "" && c.AssetKey == "" {
		return nil
	}
	previous, err := history(c.Directory)
	if err != nil {
		return err
	}
	for _, old := range previous {
		if old == c {
			return nil
		}
	}
	previous = append(previous, c)
	raw, err := json.Marshal(previous)
	if err != nil {
		return err
	}
	if len(raw) > 1024*1024 {
		return errors.New("ASSET_CONFIGURATION_HISTORY_FULL")
	}
	if err := os.MkdirAll(c.Directory, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.Directory, ".connections-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), filepath.Join(c.Directory, "connections.json")); err != nil {
		return err
	}
	return (journal{directory: c.Directory}).sync()
}

func forScope(c Config, scope map[string]any) (Config, error) {
	params, _ := scope["parameters"].(map[string]any)
	ref, _ := params["asset_ref"].(map[string]any)
	if c.NodeID == scope["node_id"] && c.CenterID == ref["center_id"] && c.Validate() == nil {
		return c, nil
	}
	// Only an already admitted local intent/result may use a previous binding.
	id, _ := scope["attempt_id"].(string)
	local := false
	for _, name := range []string{"intents", "results"} {
		j := journal{directory: filepath.Join(c.Directory, name)}
		path, err := j.path(id)
		if err != nil {
			return c, err
		}
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			local = true
		}
	}
	if !local {
		return c, nil
	}
	previous, err := history(c.Directory)
	if err != nil {
		return c, err
	}
	for i := len(previous) - 1; i >= 0; i-- {
		old := previous[i]
		if old.NodeID == scope["node_id"] && old.CenterID == ref["center_id"] && old.Validate() == nil {
			return old, nil
		}
	}
	return c, nil
}
