package mail

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Public surface of the app: resolve the newest one-time code for a mailbox.
// It deliberately never returns subjects, bodies, senders or a listing.

var (
	codeAfterKeyword  = regexp.MustCompile(`(?i)(?:验证码|驗證碼|verification\s*code|security\s*code|login\s*code|sign[-\s]?in\s*code|one[-\s]?time\s*(?:code|password)|\bcode\b|認証コード|인증\s*번호)\D{0,24}(\d{4,8})`)
	codeBeforeKeyword = regexp.MustCompile(`(?i)(\d{4,8})\D{0,16}(?:is\s+your|验证码|驗證碼|为您的|是您的|verification\s*code|\bcode\b)`)
	standaloneSix     = regexp.MustCompile(`\b(\d{6})\b`)
)

// ExtractCode pulls a one-time code out of one message text.
func ExtractCode(text string) (string, bool) {
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	if m := codeAfterKeyword.FindStringSubmatch(text); len(m) == 2 {
		return m[1], true
	}
	if m := codeBeforeKeyword.FindStringSubmatch(text); len(m) == 2 {
		return m[1], true
	}
	// Last resort: a lone six-digit run, but only in a short message — long ones
	// are full of order ids and amounts.
	if len([]rune(text)) < 400 {
		if m := standaloneSix.FindStringSubmatch(text); len(m) == 2 {
			return m[1], true
		}
	}
	return "", false
}

// CodeResult is what the public page renders.
type CodeResult struct {
	Email      string  `json:"email"`
	Code       string  `json:"code"`
	ReceivedAt float64 `json:"receivedAt"`
	AgeSeconds float64 `json:"ageSeconds"`
	Source     string  `json:"source"`
	Matched    int     `json:"matched"`
	Scanned    int     `json:"scanned"`
}

// LookupCode scans the newest messages of one mailbox for a code.
func (a *App) LookupCode(email string, scan int) (CodeResult, error) {
	if err := a.DB.MailboxAvailable(email); err != nil {
		return CodeResult{}, err
	}
	if scan <= 0 {
		scan = 20
	}
	rows, err := a.DB.Query(email, "", scan)
	if err != nil {
		return CodeResult{}, err
	}
	now := float64(time.Now().Unix())
	res := CodeResult{Email: strings.ToLower(strings.TrimSpace(email)), Scanned: len(rows)}
	for _, m := range rows {
		code, ok := ExtractCode(m.BodyText)
		if !ok {
			code, ok = ExtractCode(m.Subject)
		}
		if !ok {
			code, ok = ExtractCode(m.Snippet)
		}
		if !ok {
			continue
		}
		res.Matched++
		if res.Code == "" {
			res.Code = code
			if m.SentAt != nil {
				res.ReceivedAt = *m.SentAt
				res.AgeSeconds = now - *m.SentAt
			} else {
				res.ReceivedAt = now
			}
			res.Source = m.PrimaryTag
		}
	}
	return res, nil
}

// --- turnstile -------------------------------------------------------------

var (
	errTurnstileNotConfigured = errors.New("TURNSTILE_NOT_CONFIGURED")
	errTurnstileUnavailable   = errors.New("TURNSTILE_UNAVAILABLE")
	errTurnstileFailed        = errors.New("TURNSTILE_FAILED")
)

type turnstileResponse struct {
	Success    bool     `json:"success"`
	Hostname   string   `json:"hostname"`
	Action     string   `json:"action"`
	ErrorCodes []string `json:"error-codes"`
}

// VerifyTurnstile checks a widget token against Cloudflare, including the
// hostname and the action the widget was rendered with.
func (a *App) VerifyTurnstile(token, remoteIP, expectedAction string) error {
	if strings.TrimSpace(a.Cfg.TurnstileSecret) == "" {
		return errTurnstileNotConfigured
	}
	if strings.TrimSpace(token) == "" {
		return errTurnstileFailed
	}
	form := url.Values{
		"secret":   {a.Cfg.TurnstileSecret},
		"response": {token},
	}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequest(http.MethodPost, defaultIfEmpty(a.Cfg.TurnstileVerifyURL,
		"https://challenges.cloudflare.com/turnstile/v0/siteverify"), strings.NewReader(form.Encode()))
	if err != nil {
		return errTurnstileUnavailable
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return errTurnstileUnavailable
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if resp.StatusCode != http.StatusOK {
		return errTurnstileUnavailable
	}
	var result turnstileResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return errTurnstileUnavailable
	}
	if !result.Success {
		return errTurnstileFailed
	}
	if a.Cfg.ExpectedHost != "" && result.Hostname != "" && !strings.EqualFold(result.Hostname, a.Cfg.ExpectedHost) {
		return errTurnstileFailed
	}
	if expectedAction != "" && result.Action != "" && result.Action != expectedAction {
		return errTurnstileFailed
	}
	return nil
}

func defaultIfEmpty(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
