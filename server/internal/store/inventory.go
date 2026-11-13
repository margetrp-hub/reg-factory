package store

import (
	"database/sql"
	"fmt"
	"time"
)

// inventoryRelaySchema is deliberately small.  Work owns the durable relay
// lease; the node only needs an idempotency record and the source-side binding
// that protects a stock account from being offered twice.
const inventoryRelaySchema = `
CREATE TABLE IF NOT EXISTS inventory_relay_events (
  event_id TEXT PRIMARY KEY,
  request_sha256 TEXT NOT NULL,
  response_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS inventory_relay_bindings (
  team_asset_uuid TEXT PRIMARY KEY,
  node_id TEXT NOT NULL,
  account_ref TEXT NOT NULL UNIQUE,
  revision INTEGER NOT NULL,
  state TEXT NOT NULL,
  resource_version INTEGER NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_inventory_relay_bindings_account
  ON inventory_relay_bindings(account_ref);
`

// EnsureInventoryRelay creates the node-local idempotency tables. It is safe
// to call on every process start and works with databases created before the
// inventory relay existed.
func (s *Store) EnsureInventoryRelay() error {
	for _, column := range []string{"import_batch", "imported_at"} {
		if err := s.ex.EnsureColumn("accounts", column, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	// Older SG databases predate the reference/binding columns. Add them before
	// the first relay request so production can be upgraded in place.
	s.ensureColumns("accounts", map[string]string{
		"version": "INTEGER NOT NULL DEFAULT 1", "oauth_status": "TEXT NOT NULL DEFAULT ''",
		"join_status": "TEXT NOT NULL DEFAULT ''", "liveness_status": "TEXT NOT NULL DEFAULT ''",
		"liveness_checked_at": "TEXT NOT NULL DEFAULT ''",
	})
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS inventory_relay_events (
          event_id TEXT PRIMARY KEY,
          request_sha256 TEXT NOT NULL,
          response_json TEXT NOT NULL,
          created_at TEXT NOT NULL
        )`,
		`CREATE TABLE IF NOT EXISTS inventory_relay_bindings (
          team_asset_uuid TEXT PRIMARY KEY,
          node_id TEXT NOT NULL,
          account_ref TEXT NOT NULL UNIQUE,
          revision INTEGER NOT NULL,
          state TEXT NOT NULL,
          resource_version INTEGER NOT NULL,
          updated_at TEXT NOT NULL
        )`,
		`CREATE INDEX IF NOT EXISTS idx_inventory_relay_bindings_account ON inventory_relay_bindings(account_ref)`,
	} {
		if _, err := s.ex.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// InventoryEvent returns a saved terminal response. The boolean is false if
// this event has not been seen before.
func (s *Store) InventoryEvent(eventID string) (requestSHA, response string, found bool, err error) {
	err = s.ex.QueryRow(`SELECT request_sha256,response_json FROM inventory_relay_events WHERE event_id=?`, eventID).Scan(&requestSHA, &response)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	return requestSHA, response, err == nil, err
}

// InventoryBinding is the source-side business binding used to make event
// retries idempotent and to enforce monotonic revisions.
type InventoryBinding struct {
	TeamAssetUUID   string
	NodeID          string
	AccountRef      string
	Revision        int
	State           string
	ResourceVersion int
}

func (s *Store) InventoryBinding(teamAssetUUID string) (InventoryBinding, bool, error) {
	var b InventoryBinding
	err := s.ex.QueryRow(`SELECT team_asset_uuid,node_id,account_ref,revision,state,resource_version
        FROM inventory_relay_bindings WHERE team_asset_uuid=?`, teamAssetUUID).Scan(
		&b.TeamAssetUUID, &b.NodeID, &b.AccountRef, &b.Revision, &b.State, &b.ResourceVersion)
	if err == sql.ErrNoRows {
		return InventoryBinding{}, false, nil
	}
	return b, err == nil, err
}

// CommitInventoryEvent atomically advances the source account, records the
// monotonic binding and saves the exact signed response. A replay returns the
// previously saved response without touching account state again.
func (s *Store) CommitInventoryEvent(eventID, requestSHA, response, nodeID, teamAssetUUID,
	accountRef, state string, revision, expectedVersion int) (saved string, replay bool, err error) {
	if err = s.EnsureInventoryRelay(); err != nil {
		return "", false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()

	var oldSHA, oldResponse string
	err = s.ex.QueryRowTx(tx, `SELECT request_sha256,response_json FROM inventory_relay_events WHERE event_id=?`, eventID).Scan(&oldSHA, &oldResponse)
	if err == nil {
		if oldSHA != requestSHA {
			return "", false, fmt.Errorf("inventory event %s changed", eventID)
		}
		return oldResponse, true, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}

	var current int
	if err = s.ex.QueryRowTx(tx, `SELECT version FROM accounts WHERE ref=?`, accountRef).Scan(&current); err != nil {
		return "", false, err
	}
	if current != expectedVersion {
		return "", false, fmt.Errorf("inventory account version conflict: got %d want %d", current, expectedVersion)
	}

	var old InventoryBinding
	err = s.ex.QueryRowTx(tx, `SELECT team_asset_uuid,node_id,account_ref,revision,state,resource_version
        FROM inventory_relay_bindings WHERE team_asset_uuid=?`, teamAssetUUID).Scan(
		&old.TeamAssetUUID, &old.NodeID, &old.AccountRef, &old.Revision, &old.State, &old.ResourceVersion)
	if err == nil {
		if old.NodeID != nodeID || old.AccountRef != accountRef || revision != old.Revision+1 ||
			state == "prepared" || old.State == "retired" {
			return "", false, fmt.Errorf("inventory binding conflict")
		}
	} else if err != sql.ErrNoRows {
		return "", false, err
	} else if revision != 1 || state != "prepared" {
		return "", false, fmt.Errorf("inventory binding requires prepared revision 1")
	}

	now := time.Now().Format(time.RFC3339)
	// claimed is the existing node-side custody bit. join_status is a stable
	// projection for the reference endpoint; neither is cleared by retries.
	if _, err = s.ex.ExecTxArgs(tx, `UPDATE accounts SET claimed=1,claimed_at=?,join_status='joined',version=version+1,updated_at=? WHERE ref=? AND version=?`,
		now, now, accountRef, expectedVersion); err != nil {
		return "", false, err
	}
	if state == "prepared" {
		_, err = s.ex.ExecTxArgs(tx, `INSERT INTO inventory_relay_bindings(team_asset_uuid,node_id,account_ref,revision,state,resource_version,updated_at)
            VALUES(?,?,?,?,?,?,?)`, teamAssetUUID, nodeID, accountRef, revision, state, expectedVersion+1, now)
	} else {
		_, err = s.ex.ExecTxArgs(tx, `UPDATE inventory_relay_bindings SET revision=?,state=?,resource_version=?,updated_at=? WHERE team_asset_uuid=?`,
			revision, state, expectedVersion+1, now, teamAssetUUID)
	}
	if err != nil {
		return "", false, err
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO inventory_relay_events(event_id,request_sha256,response_json,created_at) VALUES(?,?,?,?)`,
		eventID, requestSHA, response, now); err != nil {
		return "", false, err
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	return response, false, nil
}

// CommitCoreInventoryEvent stores a local-intake response without claiming a
// production source. Core intake uses the Team asset UUID as the stable local
// account reference, so a later refresh can address it directly.
func (s *Store) CommitCoreInventoryEvent(eventID, requestSHA, response string) (saved string, replay bool, err error) {
	if err = s.EnsureInventoryRelay(); err != nil {
		return "", false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var oldSHA, oldResponse string
	err = s.ex.QueryRowTx(tx, `SELECT request_sha256,response_json FROM inventory_relay_events WHERE event_id=?`, eventID).Scan(&oldSHA, &oldResponse)
	if err == nil {
		if oldSHA != requestSHA {
			return "", false, fmt.Errorf("inventory event %s changed", eventID)
		}
		return oldResponse, true, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	now := time.Now().Format(time.RFC3339)
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO inventory_relay_events(event_id,request_sha256,response_json,created_at) VALUES(?,?,?,?)`,
		eventID, requestSHA, response, now); err != nil {
		return "", false, err
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	return response, false, nil
}

// CommitCoreInventoryEventForAccount is the atomic form used by SG intake:
// advance the newly imported source's revision and save the receipt together.
// A lost response can therefore be replayed without incrementing the version
// a second time.
func (s *Store) CommitCoreInventoryEventForAccount(eventID, requestSHA, response, accountRef string, expectedVersion int) (saved string, replay bool, err error) {
	if err = s.EnsureInventoryRelay(); err != nil {
		return "", false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var oldSHA, oldResponse string
	err = s.ex.QueryRowTx(tx, `SELECT request_sha256,response_json FROM inventory_relay_events WHERE event_id=?`, eventID).Scan(&oldSHA, &oldResponse)
	if err == nil {
		if oldSHA != requestSHA {
			return "", false, fmt.Errorf("inventory event %s changed", eventID)
		}
		return oldResponse, true, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	now := time.Now().Format(time.RFC3339)
	result, err := s.ex.ExecTxArgs(tx, `UPDATE accounts SET version=version+1,updated_at=? WHERE ref=? AND version=?`, now, accountRef, expectedVersion)
	if err != nil {
		return "", false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return "", false, fmt.Errorf("inventory account version conflict")
	}
	if _, err = s.ex.ExecTxArgs(tx, `INSERT INTO inventory_relay_events(event_id,request_sha256,response_json,created_at) VALUES(?,?,?,?)`,
		eventID, requestSHA, response, now); err != nil {
		return "", false, err
	}
	if err = tx.Commit(); err != nil {
		return "", false, err
	}
	return response, false, nil
}

// AdvanceAccountVersion moves a source account to the version returned by a
// successful core-intake receipt. It is idempotent at the caller boundary: a
// replay is detected from inventory_relay_events before this method is called.
func (s *Store) AdvanceAccountVersion(ref string, expected int) error {
	result, err := s.ex.Exec(`UPDATE accounts SET version=version+1,updated_at=? WHERE ref=? AND version=?`,
		time.Now().Format(time.RFC3339), ref, expected)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("inventory account version conflict")
	}
	return nil
}
