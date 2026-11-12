package nodeassets

import (
	"encoding/json"
	"errors"
	"math"
)

var ErrAcknowledgement = errors.New("ASSET_ACK_SCOPE_INVALID")

type ResultExpectation struct {
	TaskID           string `json:"task_id"`
	AccountID        string `json:"account_id"`
	ResultID         string `json:"result_id"`
	Outcome          string `json:"outcome"`
	ExpectedRevision int64  `json:"expected_revision"`
	Refresh          bool   `json:"refresh"`
}

func revisionNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		value, err := n.Int64()
		return value, err == nil && value > 0
	case int64:
		return n, n > 0
	case int:
		return int64(n), n > 0
	case float64:
		return int64(n), n > 0 && n <= 9007199254740991 && math.Trunc(n) == n
	default:
		return 0, false
	}
}

// ValidateResultReceipt is the same acknowledgement rule as Python. Only a
// scoped completed/failed result or explicit closure permits journal disposal.
// Unknown is durable evidence to reconcile, not a successful delivery closure.
func ValidateResultReceipt(receipt map[string]any, expected ResultExpectation) (string, error) {
	state := map[string]string{"success": "completed", "failed": "failed", "unknown": "unknown"}[expected.Outcome]
	if state == "" || expected.ExpectedRevision < 1 || expected.ExpectedRevision >= math.MaxInt64 ||
		!identifier.MatchString(expected.TaskID) || !identifier.MatchString(expected.AccountID) || !identifier.MatchString(expected.ResultID) ||
		receipt["task_id"] != expected.TaskID || receipt["account_id"] != expected.AccountID {
		return "", ErrAcknowledgement
	}
	if receipt["state"] == "closed" {
		revision, ok := revisionNumber(receipt["expected_revision"])
		if !ok || revision != expected.ExpectedRevision {
			return "", ErrAcknowledgement
		}
		return "closed", nil
	}
	resultID, present := receipt["result_id"]
	if !present {
		resultID = receipt["last_result_id"]
	}
	if receipt["state"] != state || resultID != expected.ResultID {
		return "", ErrAcknowledgement
	}
	if expected.Outcome != "unknown" {
		revision, ok := revisionNumber(receipt["credential_revision"])
		maximum := expected.ExpectedRevision
		if expected.Refresh && expected.Outcome == "success" {
			maximum++
		}
		if !ok || revision < expected.ExpectedRevision || revision > maximum {
			return "", ErrAcknowledgement
		}
	}
	return state, nil
}
