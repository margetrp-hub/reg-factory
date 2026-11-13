package mail

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

type mailTransport func(*http.Request) (*http.Response, error)

func (f mailTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestGraphSourceRotatesTokenRetainsMessagesAndRecovers(t *testing.T) {
	a := poolApp(t)
	src, err := a.DB.UpsertSource(Source{Name: "Graph", Username: "owner@example.test", Transport: "graph", ClientID: "fixture-client", RefreshToken: "first-rt", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.DB.ImportMailboxes(context.Background(), src.ID, []string{"owner@example.test"}); err != nil {
		t.Fatal(err)
	}
	previous := graphHTTP
	t.Cleanup(func() { graphHTTP = previous })
	tokens, reads := 0, 0
	unauthorized := false
	graphHTTP = &http.Client{Transport: mailTransport(func(req *http.Request) (*http.Response, error) {
		status := 200
		body := ""
		if req.URL.Host == "login.microsoftonline.com" {
			if err := req.ParseForm(); err != nil {
				t.Fatal(err)
			}
			want := "first-rt"
			if tokens > 0 {
				want = "rotated-rt"
			}
			if req.Form.Get("refresh_token") != want {
				t.Fatal("did not use latest RT")
			}
			tokens++
			body = `{"access_token":"fixture-at","refresh_token":"rotated-rt","expires_in":3600}`
		} else {
			reads++
			if req.URL.Host != "graph.microsoft.com" || req.Header.Get("Authorization") != "Bearer fixture-at" {
				t.Fatal("wrong Graph request")
			}
			body = fmt.Sprintf(`{"value":[{"id":"graph-1","subject":"Verification code","from":{"emailAddress":{"address":"noreply@openai.com"}},"toRecipients":[{"emailAddress":{"address":"owner@example.test"}}],"receivedDateTime":%q,"body":{"contentType":"text","content":"code: 123456"}}]}`, time.Now().UTC().Format(time.RFC3339))
			if unauthorized {
				status = 401
				body = `{"error":{"code":"InvalidAuthenticationToken"}}`
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	r := a.newRunner(src)
	if _, err = a.DB.ex.Exec(`UPDATE mail_pool SET state='auth_failed' WHERE email=?`, src.Username); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = r.pollTransport(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	current, err := a.DB.getSource(src.ID)
	if err != nil || current.RefreshToken != "rotated-rt" || tokens != 1 || reads != 2 {
		t.Fatal("token/cache not persisted", tokens, reads, err)
	}
	mailboxes, _, err := a.DB.PoolPage(context.Background(), src.Username, "", src.ID, 25, 0)
	if err != nil || len(mailboxes) != 1 || mailboxes[0].State != "active" {
		t.Fatal("successful mailbox login did not clear authentication failure", mailboxes, err)
	}
	items, total, err := a.DB.MessagePage(context.Background(), src.Username, "", src.ID, 25, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].BodyText != "code: 123456" {
		t.Fatal("mail replay or contents", total, err)
	}
	unauthorized = true
	if err = r.pollTransport(context.Background()); err == nil {
		t.Fatal("401 reported as successful empty inbox")
	}
	unauthorized = false
	if err = r.pollTransport(context.Background()); err != nil || tokens != 2 {
		t.Fatal("401 did not recover through current RT", tokens, err)
	}
	current.RefreshToken = ""
	current.ClientID = ""
	if current, err = a.DB.UpsertSource(current); err != nil || current.RefreshToken != "rotated-rt" {
		t.Fatal("blank save lost RT", err)
	}
	current.ClientID = "replacement-app"
	if current, err = a.DB.UpsertSource(current); err != nil {
		t.Fatal("re-authorizing the same mailbox must preserve its messages", err)
	}
	if err = r.pollTransport(context.Background()); err != ErrSourceChanged {
		t.Fatal("previous application continued writing", err)
	}
	if err = a.DB.SetSourceEnabled(context.Background(), src.ID, false); err != nil {
		t.Fatal(err)
	}
	if err = r.pollTransport(context.Background()); err != ErrSourceChanged {
		t.Fatal("disabled source continued", err)
	}
}

func TestPOP3SourceReadsWithoutDeletingAndDeduplicatesUIDL(t *testing.T) {
	a := poolApp(t)
	certificate := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serverTLS := certificate.TLS.Clone()
	clientTLS := certificate.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	certificate.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	previous := pop3TLS
	pop3TLS = clientTLS
	t.Cleanup(func() { pop3TLS = previous })
	serverErr := make(chan error, 1)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				serverErr <- err
				return
			}
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			p := textproto.NewConn(conn)
			p.PrintfLine("+OK fixture")
			for {
				line, e := p.ReadLine()
				if e == io.EOF {
					break
				}
				if e != nil {
					conn.Close()
					serverErr <- e
					return
				}
				switch {
				case strings.HasPrefix(line, "USER "), strings.HasPrefix(line, "PASS "):
					p.PrintfLine("+OK")
				case strings.TrimSpace(line) == "UIDL":
					p.PrintfLine("+OK")
					p.PrintfLine("1 fixture-uid")
					p.PrintfLine(".")
				case line == "RETR 1":
					p.PrintfLine("+OK")
					p.PrintfLine("To: owner@example.test")
					p.PrintfLine("From: noreply@example.test")
					p.PrintfLine("Subject: Code")
					p.PrintfLine("")
					p.PrintfLine("123456")
					p.PrintfLine(".")
				default:
					conn.Close()
					serverErr <- fmt.Errorf("unexpected or destructive POP3 command: %s", line)
					return
				}
			}
			conn.Close()
		}
		serverErr <- nil
	}()
	address := listener.Addr().(*net.TCPAddr)
	src, err := a.DB.UpsertSource(Source{Name: "POP3", Host: "127.0.0.1", Port: address.Port, Username: "owner@example.test", Password: "fixture-password", Transport: "pop3", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	r := a.newRunner(src)
	for i := 0; i < 2; i++ {
		if err = r.pollTransport(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
	_, total, err := a.DB.MessagePage(context.Background(), src.Username, "", src.ID, 25, 0)
	if err != nil || total != 1 {
		t.Fatal("POP3 delivery duplicated or missing", total, err)
	}
}
