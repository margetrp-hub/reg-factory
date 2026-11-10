package store

import (
	"encoding/json"
	"regexp"
	"strings"
)

var secretAssignment = regexp.MustCompile(`(?i)(["']?(?:access_token|refresh_token|id_token|oauth_access_token|password|passwd|totp|otp_secret|client_secret|api_key|session_key|sessionKey|cookie|authorization|code_verifier)["']?\s*[:=]\s*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s,;}]+)`)
var bearerSecret = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]+`)
var jwtSecret = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
var refreshSecret = regexp.MustCompile(`\brt_[A-Za-z0-9_-]{12,}\b`)
var urlUserInfo = regexp.MustCompile(`(https?://)[^\s/@]+:[^\s/@]+@`)

// RedactLog is applied at storage and read time, including old task output.
func RedactLog(line string) string {
	line = secretAssignment.ReplaceAllString(line, `${1}"[REDACTED]"`)
	line = bearerSecret.ReplaceAllString(line, "Bearer [REDACTED]")
	line = jwtSecret.ReplaceAllString(line, "[REDACTED_JWT]")
	line = refreshSecret.ReplaceAllString(line, "[REDACTED_RT]")
	return urlUserInfo.ReplaceAllString(line, `${1}[REDACTED]@`)
}

type StageEvent struct {
	Attempt    int    `json:"attempt,omitempty"`
	Stage      string `json:"stage"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Code       string `json:"error_code,omitempty"`
	Action     string `json:"next_action,omitempty"`
	ResultSHA  string `json:"result_sha256,omitempty"`
}

func stageLine(event StageEvent) string {
	raw, _ := json.Marshal(event)
	return string(raw)
}

func (s *Store) Stage(taskID string, event StageEvent) error {
	return s.AppendEvent(taskID, "stage", stageLine(event))
}

// Keep the latest occurrence of each credential stage, even after a long
// browser log or settlement retry sequence has filled the visible log tail.
func (s *Store) CredentialStages(taskID string) ([]StageEvent, error) {
	rows, err := s.read.Query(`SELECT line FROM events WHERE id IN (
		SELECT MAX(id) FROM events WHERE task_id=? AND stream='stage'
		AND json_valid(line) AND json_extract(line,'$.stage') GLOB 'credential_*'
		GROUP BY json_extract(line,'$.stage')) ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var stages []StageEvent
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			return nil, err
		}
		var stage StageEvent
		if err = json.Unmarshal([]byte(line), &stage); err != nil {
			return nil, err
		}
		stages = append(stages, stage)
	}
	return stages, rows.Err()
}

func productionActionFor(state string) string {
	switch state {
	case "completed":
		return "结果已保存，Work 自动回传；网络异常重交同一结果"
	case "failed":
		return "在生产中心查看错误；选择经确认的已有结果重新提交，不自动刷新凭证"
	case "cancelled":
		return "已停止；原结果仍保留供生产中心核对"
	default:
		return "Work 将在检查间隔后核对同一结果；可在生产中心查看或取消"
	}
}

func safeProductionCode(result string) string {
	var value struct {
		Code string `json:"error_code"`
	}
	_ = json.Unmarshal([]byte(result), &value)
	if strings.HasPrefix(value.Code, "PRODUCTION_") && len(value.Code) <= 96 {
		return value.Code
	}
	return ""
}
