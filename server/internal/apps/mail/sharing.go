package mail

import (
	"context"
	"errors"
	"sort"
	"strings"
)

func (a *App) ShareCandidates(ctx context.Context, q string, limit, offset int) ([]map[string]any, int, error) {
	// Sources can exist before their first message. Include their root mailbox
	// as an explicit choice, never secretly attach it when sharing an alias.
	rows, _, err := a.DB.PoolPage(ctx, q, "", 0, 100, 0)
	if err != nil {
		return nil, 0, err
	}
	all := rows
	for start := 100; len(rows) == 100; start += 100 {
		rows, _, err = a.DB.PoolPage(ctx, q, "", 0, 100, start)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, rows...)
	}
	sources, err := a.DB.ListSources(false)
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	for _, b := range all {
		seen[b.Email] = true
	}
	for _, src := range sources {
		email := strings.ToLower(strings.TrimSpace(src.Username))
		moved, e := a.DB.MailboxTransferred(email)
		if e != nil {
			return nil, 0, e
		}
		if moved {
			continue
		}
		if !seen[email] && looksLikeEmail(email) && strings.Contains(email, strings.ToLower(q)) {
			all = append(all, Mailbox{Email: email, SourceID: src.ID, Provider: "mail", Transport: src.Transport, Role: "parent", State: "active"})
			seen[email] = true
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Email < all[j].Email })
	total := len(all)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	out := []map[string]any{}
	for _, box := range all[offset:end] {
		out = append(out, map[string]any{"ref": "mail:" + box.Email, "email": box.Email,
			"platform": box.Provider, "kind": box.Transport, "role": box.Role, "status": box.State, "parent": box.ParentID})
	}
	return out, total, err
}

func (a *App) shareMailbox(email string) (*Mailbox, error) {
	if moved, e := a.DB.MailboxTransferred(email); e != nil {
		return nil, e
	} else if moved {
		return nil, errors.New("ASSET_SOURCE_TRANSFERRED")
	}
	rows, _, err := a.DB.PoolPage(context.Background(), email, "", 0, 100, 0)
	if err != nil {
		return nil, err
	}
	for _, box := range rows {
		if box.Email == email {
			return &box, nil
		}
	}
	sources, err := a.DB.ListSources(false)
	if err != nil {
		return nil, err
	}
	var box *Mailbox
	for _, src := range sources {
		if strings.EqualFold(strings.ToLower(strings.TrimSpace(src.Username)), email) {
			if box != nil {
				return nil, errors.New("ASSET_MAILBOX_AMBIGUOUS")
			}
			box = &Mailbox{Email: email, SourceID: src.ID, Provider: "mail", Transport: src.Transport, Role: "parent", State: "active"}
		}
	}
	if box == nil {
		return nil, errors.New("ASSET_MAILBOX_NOT_FOUND")
	}
	return box, nil
}

func (d *DB) MailboxTransferred(email string) (bool, error) {
	var n int
	err := d.ex.QueryRow(`SELECT count(*) FROM mail_transferred WHERE email=?`, strings.ToLower(strings.TrimSpace(email))).Scan(&n)
	return n > 0, err
}

func (a *App) ShareSnapshot(ref string) (email, platform, kind, role, status string, doc map[string]any, err error) {
	email = strings.TrimPrefix(ref, "mail:")
	box, e := a.shareMailbox(email)
	if e != nil {
		err = e
		return
	}
	if box.SourceID == 0 {
		doc = map[string]any{"email": email}
		platform, kind, role, status = box.Provider, "material", "address", box.State
		return
	}
	src, e := a.DB.getSource(box.SourceID)
	if e != nil {
		err = e
		return
	}
	platform, kind, role, status = box.Provider, src.Transport, box.Role, box.State

	if platform == "" {
		platform = "mail"
	}
	if kind == "" {
		kind = "imap"
	}
	if role == "" {
		role = "address"
	}
	doc = map[string]any{"email": email}
	// An alias gets a dependency, never its unselected parent's password.
	if role == "alias" || !strings.EqualFold(email, strings.ToLower(strings.TrimSpace(src.Username))) {
		kind, role = "alias", "alias"
		doc["parent_source_ref"] = "mail:" + strings.ToLower(strings.TrimSpace(src.Username))
		return
	}
	doc["username"] = src.Username
	doc["source_enabled"] = src.Enabled
	switch kind {
	case TransportIMAP, TransportPOP3:
		doc["host"], doc["port"], doc["password"], doc["folder"], doc["ssl"] = src.Host, src.Port, src.Password, src.Folder, true
	case "graph", "outlook":
		kind = "graph"
		doc["client_id"], doc["refresh_token"] = src.ClientID, src.RefreshToken
	case "luckmail":
		kind = "api"
		doc["adapter"] = "luckmail"
		doc["endpoint"], doc["api_key"] = src.Endpoint, src.APIKey
	default:
		doc["adapter"] = kind
		kind = "material"
	}
	return
}

func (a *App) WithdrawShared(ref string) error {
	email := strings.TrimPrefix(ref, "mail:")
	a.DB.mu.Lock()
	defer a.DB.mu.Unlock()
	tx, err := a.DB.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = a.DB.ex.ExecTxArgs(tx, `INSERT INTO mail_transferred(email) VALUES(?) ON CONFLICT(email) DO NOTHING`, email); err != nil {
		return err
	}
	if _, err = a.DB.ex.ExecTxArgs(tx, `UPDATE mail_sources SET enabled=0 WHERE lower(username)=lower(?)`, email); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.notifySources()
	return nil
}
