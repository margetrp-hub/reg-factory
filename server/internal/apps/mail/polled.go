package mail

// POP3 and Graph own stable message identifiers, not IMAP sequence numbers.
// All transports feed the same message/recipient transaction and source owner.
import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var graphHTTP = &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{
	Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
}}

var pop3TLS = &tls.Config{MinVersion: tls.VersionTLS12}

func (r *Runner) pollTransport(ctx context.Context) error {
	var err error
	switch r.Src.Transport {
	case TransportGraph:
		err = r.pollGraph(ctx)
	case TransportPOP3:
		err = r.pollPOP3(ctx)
	}
	if err != nil {
		return err
	}
	return r.markOK(r.lastBatch)
}

func (r *Runner) storePolled(uid string, decoded Decoded) error {
	verdict := Classify(decoded.Subject, decoded.From, decoded.Text)
	rec := MessageRecord{SourceID: r.Src.ID, Mailbox: r.Src.Folder, UID: uid,
		Recipients: decoded.Recipients, FromAddr: decoded.From, ToHeader: decoded.To,
		Subject: decoded.Subject, MessageID: decoded.MessageID, BodyText: decoded.Text,
		Snippet: decoded.Snippet, Tags: verdict.Tags, PrimaryTag: verdict.PrimaryTag, Score: verdict.Score}
	if decoded.SentAt != nil {
		stamp := float64(decoded.SentAt.Unix())
		rec.SentAt = &stamp
	}
	added, err := r.App.DB.StoreSourceMessage(r.Src, rec)
	if added {
		r.lastBatch++
	}
	return err
}

// Graph issues a new RT during a normal refresh. Persist that exact result
// before reading messages, conditional on the original configured credentials.
func (r *Runner) pollGraph(ctx context.Context) error {
	src, err := r.App.DB.getSource(r.Src.ID)
	if err != nil {
		return err
	}
	if !sameSource(src, r.Src) || !src.Enabled {
		return ErrSourceChanged
	}
	if src.RefreshToken != r.graphRefresh || time.Now().After(r.graphExpiry) {
		form := url.Values{"client_id": {src.ClientID}, "grant_type": {"refresh_token"},
			"refresh_token": {src.RefreshToken}, "scope": {"https://graph.microsoft.com/Mail.Read offline_access"}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://login.microsoftonline.com/consumers/oauth2/v2.0/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := graphHTTP.Do(req)
		if err != nil {
			return fmt.Errorf("Microsoft 授权连接失败：%w", err)
		}
		var token struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			Expires int    `json:"expires_in"`
			Code    string `json:"error"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			if token.Code == "invalid_grant" {
				return errors.New("Microsoft 邮箱授权已失效，请更新邮箱授权资料")
			}
			return fmt.Errorf("Microsoft 邮箱授权被拒绝（HTTP %d）", resp.StatusCode)
		}
		if err != nil || token.Access == "" {
			return errors.New("Microsoft 未返回有效邮箱访问凭据")
		}
		if token.Refresh != "" && token.Refresh != src.RefreshToken {
			// Share the configuration writer lock: a blank form save must not
			// read the old RT while this successful rotation is being committed.
			r.App.DB.mu.Lock()
			result, err := r.App.DB.ex.ExecContext(ctx, `UPDATE mail_sources SET refresh_token=? WHERE id=? AND refresh_token=? AND client_id=? AND transport='graph'`, token.Refresh, src.ID, src.RefreshToken, src.ClientID)
			r.App.DB.mu.Unlock()
			if err != nil {
				return fmt.Errorf("邮箱授权结果保存失败：%w", err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if count != 1 {
				return ErrSourceChanged
			}
			src.RefreshToken = token.Refresh
		}
		r.graphRefresh, r.graphToken = src.RefreshToken, token.Access
		r.graphExpiry = time.Now().Add(time.Duration(token.Expires)*time.Second - 30*time.Second)
	}
	params := url.Values{"$top": {"50"}, "$orderby": {"receivedDateTime desc"},
		"$select": {"id,internetMessageId,subject,from,toRecipients,ccRecipients,receivedDateTime,body,bodyPreview"}}
	if src.LastOKAt != nil {
		params.Set("$filter", "receivedDateTime ge "+src.LastOKAt.Add(-time.Minute).UTC().Format(time.RFC3339))
	}
	folder := src.Folder
	if strings.EqualFold(folder, "INBOX") {
		folder = "inbox"
	}
	next := "https://graph.microsoft.com/v1.0/me/mailFolders/" + url.PathEscape(folder) + "/messages?" + params.Encode()
	count := 0
	limit := defaultInt(r.App.Cfg.BootstrapLimit, 300)
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		// Only Microsoft's documented nextLink receives mailbox authorization.
		if req.URL.Scheme != "https" || req.URL.Host != "graph.microsoft.com" {
			return errors.New("Microsoft 返回无效分页地址")
		}
		req.Header.Set("Authorization", "Bearer "+r.graphToken)
		req.Header.Set("Prefer", `outlook.body-content-type="text"`)
		resp, err := graphHTTP.Do(req)
		if err != nil {
			return fmt.Errorf("Microsoft 取信连接失败：%w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				r.graphToken = ""
				r.graphExpiry = time.Time{}
			}
			return fmt.Errorf("Microsoft 取信失败（HTTP %d）", resp.StatusCode)
		}
		type address struct {
			Email struct {
				Address string `json:"address"`
			} `json:"emailAddress"`
		}
		var page struct {
			Next  string `json:"@odata.nextLink"`
			Items []struct {
				ID        string    `json:"id"`
				MessageID string    `json:"internetMessageId"`
				Subject   string    `json:"subject"`
				From      address   `json:"from"`
				To        []address `json:"toRecipients"`
				CC        []address `json:"ccRecipients"`
				Received  time.Time `json:"receivedDateTime"`
				Preview   string    `json:"bodyPreview"`
				Body      struct {
					Content string `json:"content"`
					Type    string `json:"contentType"`
				} `json:"body"`
			} `json:"value"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return errors.New("Microsoft 邮件响应无法解析")
		}
		for _, item := range page.Items {
			if item.ID == "" {
				return errors.New("Microsoft 邮件缺少标识")
			}
			body := item.Body.Content
			if strings.EqualFold(item.Body.Type, "html") {
				body = html.UnescapeString(htmlTag.ReplaceAllString(tagStripper.ReplaceAllString(body, " "), " "))
			}
			decoded := Decoded{Subject: item.Subject, From: item.From.Email.Address, Text: body, Snippet: item.Preview, SentAt: &item.Received, MessageID: item.MessageID}
			for _, recipient := range append(item.To, item.CC...) {
				decoded.Recipients = append(decoded.Recipients, recipient.Email.Address)
			}
			decoded.To = strings.Join(decoded.Recipients, ", ")
			if err := r.storePolled(item.ID, decoded); err != nil {
				return err
			}
			count++
		}
		next = page.Next
		if src.LastOKAt == nil && count >= limit {
			break
		}
	}
	return nil
}

// POP3 uses UIDL for deduplication and RETR only. It never deletes remote mail.
func (r *Runner) pollPOP3(ctx context.Context) error {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: pop3TLS}
	conn, err := dialer.DialContext(ctx, "tcp", r.Src.Addr())
	if err != nil {
		return fmt.Errorf("POP3 连接失败：%w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	conn.SetDeadline(time.Now().Add(50 * time.Second))
	client := textproto.NewConn(conn)
	response, err := client.ReadLine()
	if err != nil || !strings.HasPrefix(response, "+OK") {
		return errors.New("POP3 服务器未完成握手")
	}
	for _, command := range []struct{ name, value string }{{"USER", r.Src.Username}, {"PASS", r.Src.Password}, {"UIDL", ""}} {
		if err = client.PrintfLine("%s %s", command.name, command.value); err != nil {
			return err
		}
		response, err = client.ReadLine()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(response, "+OK") {
			return fmt.Errorf("POP3 %s 被拒绝，请检查邮箱登录资料或协议支持", command.name)
		}
	}
	lines, err := client.ReadDotLines()
	if err != nil {
		return err
	}
	limit := r.App.Cfg.BootstrapLimit
	if limit < 1 {
		limit = 300
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return errors.New("POP3 返回无效邮件标识")
		}
		sequence, err := strconv.Atoi(parts[0])
		if err != nil {
			return errors.New("POP3 返回无效邮件序号")
		}
		var known int
		if err = r.App.DB.ex.QueryRowContext(ctx, "SELECT count(*) FROM mail_messages WHERE source_id=? AND uid=?", r.Src.ID, parts[1]).Scan(&known); err != nil {
			return err
		}
		if known > 0 {
			continue
		}
		if err = client.PrintfLine("RETR %d", sequence); err != nil {
			return err
		}
		response, err = client.ReadLine()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(response, "+OK") {
			return errors.New("POP3 邮件读取被拒绝")
		}
		raw, err := io.ReadAll(io.LimitReader(client.DotReader(), 8<<20))
		if err != nil {
			return err
		}
		if len(raw) >= 8<<20 {
			return errors.New("POP3 邮件超过收取大小限制")
		}
		if err = r.storePolled(parts[1], Parse(raw)); err != nil {
			return err
		}
	}
	return nil
}
