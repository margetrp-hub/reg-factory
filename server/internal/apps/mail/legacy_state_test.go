package mail

// Seed the previous persistent state for upgrade/readback tests only. Runtime
// state changes use SetMailbox; the alias API and its second writer are retired.
import (
	"context"
	"errors"
	"strings"
	"time"

	storepkg "github.com/tiantianGPU/reg-factory/server/internal/store"
)

func (d *DB) setLegacyAliasState(ctx context.Context, hme, state string) error {
	hme = strings.ToLower(strings.TrimSpace(hme))
	if !looksLikeEmail(hme) || (state != "active" && state != "paused" && state != "blocked") {
		return errors.New("邮箱或状态无效")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = d.ex.ExecTxArgs(tx, storepkg.UpsertIgnore(d.dialect, "INSERT INTO mail_pool(email,origin,created_at) VALUES(?,'manual',?)"), hme, float64(time.Now().Unix())); err != nil {
		return err
	}
	_, err = d.ex.ExecTxArgs(tx, `
INSERT INTO mail_alias_state(alias_hme, state, manual, updated_at)
VALUES(?,?,1,?)
ON CONFLICT(alias_hme) DO UPDATE SET state=excluded.state, manual=1, updated_at=excluded.updated_at`,
		hme, state, float64(time.Now().UnixNano())/1e9)
	if err != nil {
		return err
	}
	return tx.Commit()
}
