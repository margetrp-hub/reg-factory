package mail

// CloudMail sources are HTTP mailboxes: every address carries its own JWT and
// the panel keeps that material in mail_pool.meta. One source row can front
// many addresses, so we poll only the ones currently in use and let thousands
// of dormant candidates cost nothing.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cloudmailWorkers    = 4
	cloudmailPerAddress = 20
	cloudmailAddressCap = 200
	cloudmailBodyLimit  = 8 << 20
)

type cloudAddress struct {
	Email        string
	JWT          string
	SitePassword string
	BaseURL      string
}

// cloudmailAddresses returns the addresses this source should poll, oldest
// check first so a slow rotation still eventually covers everything.
func (d *DB) cloudmailAddresses(src Source, limit int) ([]cloudAddress, error) {
	if limit <= 0 {
		limit = cloudmailAddressCap
	}
	rows, err := d.ex.Query(`
SELECT email, meta FROM mail_pool
WHERE provider='cloudmail'
  AND source_id = ?
  AND COALESCE(meta,'') LIKE '%"jwt"%'
  AND (state IN ('ready','claimed') OR COALESCE(assigned_to,'') <> '')
ORDER BY COALESCE(last_checked_at, 0) ASC
LIMIT ?`, src.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []cloudAddress
	for rows.Next() {
		var email, meta string
		if err := rows.Scan(&email, &meta); err != nil {
			return nil, err
		}
		var m struct {
			JWT          string `json:"jwt"`
			BaseURL      string `json:"base_url"`
			SitePassword string `json:"site_password"`
		}
		if meta != "" {
			_ = json.Unmarshal([]byte(meta), &m)
		}
		if strings.TrimSpace(m.JWT) == "" {
			continue
		}
		base := strings.TrimSpace(m.BaseURL)
		if base == "" {
			base = strings.TrimSpace(src.Endpoint)
		}
		if base == "" {
			continue
		}
		out = append(out, cloudAddress{Email: email, JWT: m.JWT, SitePassword: m.SitePassword, BaseURL: base})
	}
	return out, rows.Err()
}

// touchCloudAddresses records the check time so the next tick rotates onward.
func (d *DB) touchCloudAddresses(addrs []cloudAddress) {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	for _, a := range addrs {
		_, _ = d.ex.Exec("UPDATE mail_pool SET last_checked_at=? WHERE email=?", now, a.Email)
	}
}

type cloudmailMessage struct {
	ID        json.Number `json:"id"`
	MessageID string      `json:"message_id"`
	Address   string      `json:"address"`
	Source    string      `json:"source"`
	Subject   string      `json:"subject"`
	Raw       string      `json:"raw"`
}

// pollCloudmail fetches every in-use address bound to this source.
func (r *Runner) pollCloudmail(ctx context.Context) error {
	addrs, err := r.App.DB.cloudmailAddresses(r.Src, cloudmailAddressCap)
	if err != nil {
		return err
	}
	if len(addrs) == 0 {
		return r.markOK(0)
	}
	aliases, err := r.App.DB.SourceAliases(r.Src.ID)
	if err != nil {
		return err
	}
	sem := make(chan struct{}, cloudmailWorkers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	stored := 0
	var firstErr error
	for _, addr := range addrs {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(a cloudAddress) {
			defer wg.Done()
			defer func() { <-sem }()
			n, err := r.fetchCloudAddress(ctx, a, aliases)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			stored += n
			r.lastBatch = stored
		}(addr)
	}
	wg.Wait()
	r.App.DB.touchCloudAddresses(addrs)
	if firstErr != nil && stored == 0 {
		return firstErr
	}
	return r.markOK(stored)
}

func (r *Runner) fetchCloudAddress(ctx context.Context, a cloudAddress, aliases []string) (int, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(cloudmailPerAddress))
	q.Set("offset", "0")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(a.BaseURL, "/")+"/api/mails?"+q.Encode(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.JWT)
	req.Header.Set("Accept", "application/json")
	if a.SitePassword != "" {
		req.Header.Set("x-custom-auth", a.SitePassword)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cloudmail 请求失败（%s）：%w", a.Email, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, cloudmailBodyLimit))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return 0, fmt.Errorf("cloudmail 地址凭证无效（%s）：%s", a.Email, truncateText(string(body), 160))
	default:
		return 0, fmt.Errorf("cloudmail 返回 HTTP %d（%s）：%s", resp.StatusCode, a.Email, truncateText(string(body), 160))
	}
	var payload struct {
		Results []cloudmailMessage `json:"results"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("cloudmail 响应无法解析（%s）：%w", a.Email, err)
	}
	count := 0
	for _, item := range payload.Results {
		uid := strings.TrimSpace(item.ID.String())
		if uid == "" || uid == "0" {
			uid = strings.TrimSpace(item.MessageID)
		}
		if uid == "" {
			continue
		}
		decoded := Parse([]byte(item.Raw))
		verdict := Classify(decoded.Subject, decoded.From, decoded.Text)
		rec := MessageRecord{
			Recipients: decoded.Recipients,
			Mailbox:    r.Src.Folder,
			UID:        uid,
			SourceID:   r.Src.ID,
			AliasHME:   matchAlias(decoded.Recipients, aliases),
			FromAddr:   decoded.From,
			ToHeader:   decoded.To,
			Subject:    decoded.Subject,
			MessageID:  item.MessageID,
			Tags:       verdict.Tags,
			PrimaryTag: verdict.PrimaryTag,
			Score:      verdict.Score,
			Snippet:    decoded.Snippet,
			BodyText:   decoded.Text,
		}
		if decoded.SentAt != nil {
			ts := float64(decoded.SentAt.Unix())
			rec.SentAt = &ts
		}
		isNew, err := r.App.DB.StoreSourceMessage(r.Src, rec)
		if err != nil {
			return count, err
		}
		if isNew {
			count++
		}
	}
	return count, nil
}

func truncateText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
