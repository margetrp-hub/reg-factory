package nodeassets

import (
	"regexp"
	"strings"
)

var stateCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type ResultObservation struct {
	SourceStatus   string  `json:"source_status"`
	SourceLiveness string  `json:"source_liveness"`
	CheckedAt      *string `json:"checked_at"`
}

// ObserveResult preserves explicit node facts. Only known structured codes
// supply a missing fact; HTTP 401 and error text never establish a ban.
// Task outcome and credential revision remain the original result's facts.
func ObserveResult(result map[string]any) (ResultObservation, error) {
	if result == nil {
		return ResultObservation{}, ErrInvalid
	}
	out := ResultObservation{SourceStatus: "unknown", SourceLiveness: "unknown"}
	for key, target := range map[string]*string{"source_status": &out.SourceStatus, "liveness": &out.SourceLiveness} {
		if value, present := result[key]; present && value != nil {
			s, ok := value.(string)
			if !ok {
				return ResultObservation{}, ErrInvalid
			}
			if s != "" {
				*target = s
			}
		}
	}
	code := ""
	if value := result["error_code"]; value != nil {
		var ok bool
		code, ok = value.(string)
		if !ok {
			return ResultObservation{}, ErrInvalid
		}
	}
	if value := result["checked_at"]; value != nil {
		s, ok := value.(string)
		if !ok {
			return ResultObservation{}, ErrInvalid
		}
		out.CheckedAt = &s
	}
	if out.SourceStatus == "unknown" && out.SourceLiveness == "unknown" {
		switch strings.ToLower(code) {
		case "account_deactivated", "account_banned":
			out.SourceStatus = "banned"
		case "workspace_deactivated", "deactivated_workspace":
			out.SourceStatus = "workspace_deactivated"
		}
	}
	if _, err := ProjectAccountState(out.SourceStatus, out.SourceLiveness, out.CheckedAt); err != nil {
		return ResultObservation{}, err
	}
	return out, nil
}

type AccountState struct {
	SourceStatus            string  `json:"source_status"`
	SourceLiveness          string  `json:"source_liveness"`
	Account                 string  `json:"account"`
	Workspace               string  `json:"workspace"`
	Credential              string  `json:"credential"`
	Health                  string  `json:"health"`
	CheckedAt               *string `json:"checked_at"`
	AutomaticRefreshAllowed bool    `json:"automatic_refresh_allowed"`
}

func ProjectAccountState(status, liveness string, checkedAt *string) (AccountState, error) {
	if !stateCode.MatchString(status) || !stateCode.MatchString(liveness) {
		return AccountState{}, ErrInvalid
	}
	has := func(values ...string) bool {
		for _, v := range values {
			if status == v || liveness == v {
				return true
			}
		}
		return false
	}
	s := AccountState{status, liveness, "unknown", "unknown", "unknown", "unknown", checkedAt, true}
	if has("banned") {
		s.Account = "banned"
	}
	if has("workspace_deactivated") {
		s.Workspace = "deactivated"
	}
	if has("email_otp_required") {
		s.Credential = "email_otp_required"
	} else if has("needs_refresh", "expired", "relogin_required") {
		s.Credential = "needs_refresh"
	}
	if liveness == "alive" && checkedAt != nil && *checkedAt != "" {
		s.Health = "alive"
	}
	if has("banned", "workspace_deactivated", "invalid", "dead") {
		s.Health = "unavailable"
	} else if s.Credential != "unknown" {
		s.Health = "needs_attention"
	}
	s.AutomaticRefreshAllowed = !has("banned", "workspace_deactivated", "email_otp_required")
	return s, nil
}
