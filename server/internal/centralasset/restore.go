package centralasset

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"aishop-next/execution-go/nodeassets"
	"github.com/tiantianGPU/reg-factory/server/internal/fernet"
)

// Receiving is not production and never makes a private node pool entry.
// Task executors continue to read their assigned version from the center.
func (s *Sharing) Restore(ctx context.Context, scope, material map[string]any) (map[string]any, error) {
	entry, ok := material["snapshot"].(map[string]any)
	if !ok || len(nodeassets.MissingSnapshotFields(entry)) != 0 || material["account_id"] != scope["subject_id"] ||
		entry["source_ref"] != "central:"+str(scope["subject_id"]) || number(entry["source_revision"]) != number(scope["resource_version"]) {
		return failure("ASSET_RECEIVE_ENVELOPE_INVALID", false), nil
	}
	cfg, _ := s.Config()
	cfg, err := forScope(cfg, scope)
	if err != nil {
		return nil, err
	}
	if err = PreserveConfig(cfg); err != nil {
		return nil, err
	}
	// Immutable encrypted input keyed by the original Work task. It is usable
	// even if the operator sent only partial materials or opaque JSON.
	raw, err := json.Marshal(map[string]any{"scope": scope, "entry": entry})
	if err != nil {
		return nil, err
	}
	sealed, err := fernet.Encrypt([]byte(cfg.JournalKey), raw)
	if err != nil {
		return nil, err
	}
	_, err = s.Store.Execer().ExecContext(ctx, `INSERT INTO central_received_assets(task_id,account_id,revision,record_ciphertext,received_at)
      VALUES(?,?,?,?,?) ON CONFLICT(task_id) DO NOTHING`, scope["attempt_id"], scope["subject_id"], number(scope["resource_version"]), sealed, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	var stored string
	err = s.Store.Execer().QueryRowContext(ctx, `SELECT record_ciphertext FROM central_received_assets WHERE task_id=?`, scope["attempt_id"]).Scan(&stored)
	if err != nil {
		return nil, err
	}
	existing, e := fernet.Decrypt([]byte(cfg.JournalKey), []byte(stored), 0)
	if e != nil || !bytes.Equal(existing, raw) {
		return failure("ASSET_RECEIVE_REQUEST_CONFLICT", false), nil
	}
	return map[string]any{"status": "completed", "document": map[string]any{"source_ref": entry["source_ref"]}, "error_code": ""}, nil
}
func (s *Sharing) Received() ([]map[string]any, error) {
	rows, err := s.Store.Execer().Query(`SELECT task_id,account_id,revision,received_at FROM central_received_assets ORDER BY received_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []map[string]any{}
	for rows.Next() {
		var id, account, stamp string
		var revision int
		if err = rows.Scan(&id, &account, &revision, &stamp); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"task_id": id, "account_id": account, "revision": revision, "received_at": stamp})
	}
	return result, rows.Err()
}
