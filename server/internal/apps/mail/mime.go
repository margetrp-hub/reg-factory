// Package mailmime extracts the fields the index needs from a *partially*
// fetched message: subject, participants, a readable snippet and the send time.
//
// Two realities shape this code:
//   - QQ (and most Chinese providers) still send GBK/GB2312 bodies, so every
//     charset has to go through a real decoder instead of being assumed UTF-8.
//   - The body arrives truncated (IMAP servers may ignore a partial fetch and
//     return everything, or honour it and cut a MIME part in half), so parsing
//     is best-effort with a plain-text fallback rather than fatal.
package mail

import (
	"bytes"
	"encoding/base64"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strings"
	"time"

	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/transform"
)

const (
	snippetLimit = 240
	bodyLimit    = 4000
)

var (
	// Go's RE2 has no backreferences, so script/style are matched explicitly
	// instead of with a (<tag>...</\1>) pattern.
	tagStripper = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>`)
	htmlTag     = regexp.MustCompile(`(?s)<[^>]+>`)
	spaceRun    = regexp.MustCompile(`\s+`)
	addressRe   = regexp.MustCompile(`[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`)
)

var recipientHeaders = []string{
	"To", "Cc", "Delivered-To", "X-Original-To", "Envelope-To", "Resent-To", "Apparently-To",
}

// Decoded is what the indexer stores per message.
type Decoded struct {
	Subject    string
	From       string
	To         string
	Recipients []string
	Snippet    string
	Text       string
	SentAt     *time.Time
	MessageID  string
}

// Parse decodes a complete raw message (headers + body in one buffer), which is
// the shape BODY[] returns.
func Parse(raw []byte) Decoded {
	if len(raw) == 0 {
		return Decoded{}
	}
	return Decode(raw, nil)
}

// Decode parses one message given its raw header block and (possibly truncated) body.
//
// Some servers ignore a partial/parts fetch and return the whole message as the
// body section; in that case the header block is embedded at the front of
// rawBody. Rather than trusting the caller we detect that shape and split it,
// otherwise Subject/Content-Transfer-Encoding are lost and every body looks like
// an undecoded base64 blob.
func Decode(rawHeaders, rawBody []byte) Decoded {
	if !hasHeaderBlock(rawHeaders) && hasHeaderBlockPrefix(rawBody) {
		headerPart, bodyPart := splitAtHeaderEnd(rawBody)
		rawHeaders, rawBody = headerPart, bodyPart
	}

	combined := make([]byte, 0, len(rawHeaders)+len(rawBody)+4)
	combined = append(combined, bytes.TrimRight(rawHeaders, "\r\n")...)
	combined = append(combined, '\r', '\n', '\r', '\n')
	combined = append(combined, rawBody...)

	msg, err := mail.ReadMessage(bytes.NewReader(combined))
	if err != nil {
		return Decoded{Snippet: Collapse(StripHTML(string(rawBody)))}
	}

	dec := &mime.WordDecoder{CharsetReader: charsetReader}
	header := func(name string) string {
		raw := msg.Header.Get(name)
		if raw == "" {
			return ""
		}
		if v, err := dec.DecodeHeader(raw); err == nil {
			return v
		}
		return raw
	}

	out := Decoded{
		Subject:   header("Subject"),
		From:      normaliseAddress(header("From")),
		To:        header("To"),
		MessageID: strings.Trim(header("Message-ID"), "<>"),
	}
	out.Recipients = collectRecipients(header)
	if date := header("Date"); date != "" {
		if t, err := mail.ParseDate(date); err == nil {
			utc := t.UTC()
			out.SentAt = &utc
		}
	}
	text := extractText(msg)
	out.Text = Truncate(text, bodyLimit)
	out.Snippet = Truncate(text, snippetLimit)
	out.Subject = strings.ToValidUTF8(out.Subject, "?")
	out.From = strings.ToValidUTF8(out.From, "?")
	out.To = strings.ToValidUTF8(out.To, "?")
	out.Text = strings.ToValidUTF8(out.Text, "?")
	out.Snippet = strings.ToValidUTF8(out.Snippet, "?")
	return out
}

func collectRecipients(header func(string) string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(addr string) {
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr == "" || seen[addr] {
			return
		}
		seen[addr] = true
		out = append(out, addr)
	}
	for _, name := range recipientHeaders {
		raw := header(name)
		if raw == "" {
			continue
		}
		if list, err := mail.ParseAddressList(raw); err == nil {
			for _, a := range list {
				add(a.Address)
			}
			continue
		}
		for _, m := range addressRe.FindAllString(raw, -1) {
			add(m)
		}
	}
	return out
}

func normaliseAddress(raw string) string {
	if list, err := mail.ParseAddressList(raw); err == nil && len(list) > 0 {
		return list[0].String()
	}
	return raw
}

// extractText walks the MIME tree and returns the most useful text it can find.
func extractText(msg *mail.Message) string {
	contentType := msg.Header.Get("Content-Type")
	mediaType, params, _ := mime.ParseMediaType(contentType)
	charset := strings.ToLower(params["charset"])

	if strings.HasPrefix(mediaType, "multipart/") {
		if boundary := params["boundary"]; boundary != "" {
			reader := multipart.NewReader(msg.Body, boundary)
			var best string
			for {
				part, err := reader.NextPart()
				if err != nil {
					break
				}
				if part.FileName() != "" {
					continue // attachments carry no snippet value
				}
				text := textFromPart(part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), part)
				if len(text) > len(best) {
					best = text
				}
			}
			if best != "" {
				return best
			}
		}
		return ""
	}

	body, _ := io.ReadAll(io.LimitReader(msg.Body, 64<<10))
	decoded := decodeTransfer(msg.Header.Get("Content-Transfer-Encoding"), body)
	text := decodeCharset(decoded, charset)
	if strings.Contains(strings.ToLower(mediaType), "html") || looksLikeHTML(text) {
		return Collapse(StripHTML(text))
	}
	return Collapse(text)
}

func textFromPart(partContentType, transferEncoding string, r io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(r, 64<<10))
	decoded := decodeTransfer(transferEncoding, body)
	mediaType, params, _ := mime.ParseMediaType(partContentType)
	text := decodeCharset(decoded, strings.ToLower(params["charset"]))
	if strings.Contains(mediaType, "html") || looksLikeHTML(text) {
		return Collapse(StripHTML(text))
	}
	return Collapse(text)
}

func decodeTransfer(encoding string, body []byte) []byte {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, body)
		if decoded, err := base64.StdEncoding.DecodeString(string(clean)); err == nil {
			return decoded
		}
	case "quoted-printable":
		if decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body))); err == nil {
			return decoded
		}
	}
	return body
}

func decodeCharset(body []byte, charset string) string {
	if charset == "" || charset == "utf-8" || charset == "us-ascii" || charset == "ascii" {
		return string(body)
	}
	enc, err := htmlindex.Get(charset)
	if err != nil || enc == nil {
		return string(body)
	}
	if decoded, _, err := transform.Bytes(enc.NewDecoder(), body); err == nil {
		return strings.ToValidUTF8(string(decoded), "\uFFFD")
	}
	return strings.ToValidUTF8(string(body), "\uFFFD")
}

func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return nil, err
	}
	return strings.NewReader(decodeCharset(raw, strings.ToLower(charset))), nil
}

func looksLikeHTML(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "<html") || strings.Contains(lower, "<div") || strings.Contains(lower, "<table")
}

// StripHTML removes tags and unescapes entities.
func StripHTML(markup string) string {
	text := tagStripper.ReplaceAllString(markup, " ")
	text = htmlTag.ReplaceAllString(text, " ")
	return html.UnescapeString(text)
}

// Collapse folds all whitespace runs into single spaces.
func Collapse(text string) string {
	return strings.TrimSpace(spaceRun.ReplaceAllString(text, " "))
}

// Truncate cuts a string to at most n runes.
func Truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// ClampBody keeps the stored text within bodyLimit runes.
func ClampBody(s string) string { return Truncate(s, bodyLimit) }

// hasHeaderBlock reports whether the bytes already look like an RFC 5322 header
// block (a known field followed by a colon).
func hasHeaderBlock(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	head := strings.ToLower(string(raw[:min(len(raw), 512)]))
	for _, field := range []string{"subject:", "from:", "to:", "content-type:", "date:", "message-id:"} {
		if strings.Contains(head, field) {
			return true
		}
	}
	return false
}

// hasHeaderBlockPrefix reports whether the bytes *start* with a header block.
func hasHeaderBlockPrefix(raw []byte) bool {
	head := strings.ToLower(string(raw[:min(len(raw), 512)]))
	if !strings.Contains(head, ":") {
		return false
	}
	first := strings.SplitN(head, "\n", 2)[0]
	for _, field := range []string{"from:", "to:", "subject:", "content-type:", "date:", "return-path:", "received:", "message-id:", "mime-version:"} {
		if strings.HasPrefix(strings.TrimSpace(first), strings.TrimSuffix(field, ":")) {
			return true
		}
	}
	return false
}

// splitAtHeaderEnd cuts a raw message into header block and body.
func splitAtHeaderEnd(raw []byte) ([]byte, []byte) {
	if idx := bytes.Index(raw, []byte("\r\n\r\n")); idx >= 0 {
		return raw[:idx], raw[idx+4:]
	}
	if idx := bytes.Index(raw, []byte("\n\n")); idx >= 0 {
		return raw[:idx], raw[idx+2:]
	}
	return raw, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
