package mail

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type SyncRun struct {
	ID          int64   `json:"id"`
	SourceID    int64   `json:"sourceId"`
	SourceName  string  `json:"sourceName"`
	RequestID   string  `json:"requestId"`
	State       string  `json:"state"`
	RequestedAt float64 `json:"requestedAt"`
	StartedAt   float64 `json:"startedAt"`
	FinishedAt  float64 `json:"finishedAt"`
	Note        string  `json:"note"`
	Indexed     int     `json:"indexed"`
}

func (d *DB) ensureSync() error {
	// 列清单走方言：SQLite 用 PRAGMA table_info，PG 用 information_schema。
	cols, err := d.ex.ListColumns("mail_sync_requests")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for _, c := range cols {
		columns[c] = true
	}

	for _, field := range []struct{ name, ddl string }{
		{"request_id", "TEXT NOT NULL DEFAULT ''"},
		{"indexed", "INTEGER NOT NULL DEFAULT 0"},
		{"request_scope", "TEXT NOT NULL DEFAULT ''"},
	} {
		if !columns[field.name] {
			if _, err = d.ex.Exec("ALTER TABLE mail_sync_requests ADD COLUMN " + field.name + " " + field.ddl); err != nil {
				return err
			}
		}
	}
	_, err = d.ex.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_mail_sync_request ON mail_sync_requests(request_id,source_id) WHERE request_id!='';
CREATE INDEX IF NOT EXISTS idx_mail_sync_pending ON mail_sync_requests(source_id,state,id);`)
	return err
}

func (d *DB) QueueSync(ctx context.Context, sourceID int64, alias, requestID string) ([]SyncRun, error) {
	if len(requestID) < 8 || len(requestID) > 128 || sourceID < 0 {
		return nil, errors.New("需要有效的请求编号")
	}
	alias = strings.ToLower(strings.TrimSpace(alias))
	scope := fmt.Sprintf("%d:%s", sourceID, alias)
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Resolve the original request before looking at live source configuration.
	var count int
	if err = d.ex.QueryRowTx(tx, "SELECT count(*) FROM mail_sync_requests WHERE request_id=?", requestID).Scan(&count); err != nil {
		return nil, err
	}
	if count > 0 {
		var matches int
		if err = d.ex.QueryRowTx(tx, "SELECT count(*) FROM mail_sync_requests WHERE request_id=? AND request_scope=?", requestID, scope).Scan(&matches); err != nil {
			return nil, err
		}
		if matches != count {
			return nil, errors.New("请求编号已用于其他同步，请查看原运行记录")
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		items, _, err := d.SyncPage(ctx, requestID, 100, 0)
		return items, err
	}
	if alias != "" {
		var owned sql.NullInt64
		if err = d.ex.QueryRowTx(tx, "SELECT source_id FROM mail_pool WHERE email=?", strings.ToLower(strings.TrimSpace(alias))).Scan(&owned); err != nil || !owned.Valid {
			return nil, errors.New("邮箱尚未指定收件来源")
		}
		if sourceID > 0 && sourceID != owned.Int64 {
			return nil, errors.New("邮箱与收件来源不一致")
		}
		sourceID = owned.Int64
	}
	rows, err := d.ex.QueryTx(tx, "SELECT id FROM mail_sources WHERE enabled=1 AND (?=0 OR id=?)", sourceID, sourceID)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, errors.New("没有启用的收件来源，请先配置并启用来源")
	}
	if err = d.ex.QueryRowTx(tx, "SELECT count(*) FROM mail_sync_requests WHERE state IN ('pending','running')").Scan(&count); err != nil {
		return nil, err
	}
	if count+len(ids) > 100 {
		return nil, errors.New("同步队列已满，请等待原任务结束")
	}
	for _, id := range ids {
		if _, err = d.ex.ExecTxArgs(tx, `INSERT INTO mail_sync_requests(source_id,alias_key,state,requested_at,request_id,request_scope) VALUES(?,?,'pending',?,?,?)`, id, alias, float64(time.Now().Unix()), requestID, scope); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	items, _, err := d.SyncPage(ctx, requestID, 100, 0)
	return items, err
}

func (d *DB) SyncPage(ctx context.Context, requestID string, limit, offset int) ([]SyncRun, int, error) {
	if limit < 1 || limit > 100 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	where := ""
	args := []any{}
	if requestID != "" {
		where = " WHERE r.request_id=?"
		args = append(args, requestID)
	}
	var total int
	if err := d.ex.QueryRowContext(ctx, "SELECT count(*) FROM mail_sync_requests r"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.ex.QueryContext(ctx, `SELECT r.id,COALESCE(r.source_id,0),COALESCE(s.name,'来源已移除'),r.request_id,r.state,r.requested_at,COALESCE(r.started_at,0),COALESCE(r.finished_at,0),COALESCE(r.note,''),r.indexed
FROM mail_sync_requests r LEFT JOIN mail_sources s ON s.id=r.source_id`+where+" ORDER BY r.id DESC LIMIT CAST(? AS BIGINT) OFFSET CAST(? AS BIGINT)", append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []SyncRun{}
	for rows.Next() {
		var item SyncRun
		if err = rows.Scan(&item.ID, &item.SourceID, &item.SourceName, &item.RequestID, &item.State, &item.RequestedAt, &item.StartedAt, &item.FinishedAt, &item.Note, &item.Indexed); err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}

func (d *DB) claimSync(sourceID int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var id int64
	err := d.ex.QueryRow(`UPDATE mail_sync_requests SET state='running',started_at=? WHERE id=(SELECT id FROM mail_sync_requests WHERE source_id=? AND state='pending' AND EXISTS(SELECT 1 FROM mail_sources s WHERE s.id=source_id AND s.enabled=1) ORDER BY id LIMIT 1) RETURNING id`, float64(time.Now().Unix()), sourceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (d *DB) finishSync(id int64, state, note string, indexed int) error {
	_, err := d.ex.Exec("UPDATE mail_sync_requests SET state=?,note=?,indexed=?,finished_at=? WHERE id=? AND state='running'", state, note, indexed, float64(time.Now().Unix()), id)
	return err
}

func (d *DB) recoverSync() error {
	_, err := d.ex.Exec("UPDATE mail_sync_requests SET state='interrupted',note='服务重启，原同步结果未确认；可重新同步',finished_at=? WHERE state='running'", float64(time.Now().Unix()))
	return err
}

func (d *DB) cancelUnavailableSync() error {
	_, err := d.ex.Exec(`UPDATE mail_sync_requests SET state='cancelled',note='收件来源已停用或移除，请恢复来源后重新同步',finished_at=?
WHERE state='pending' AND NOT EXISTS(SELECT 1 FROM mail_sources s WHERE s.id=source_id AND s.enabled=1)`, float64(time.Now().Unix()))
	return err
}
