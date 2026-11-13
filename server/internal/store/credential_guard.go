package store

import (
	"encoding/json"
	"errors"
	"time"
)

// CredentialExecutionGuard reads the same database that owns task settlement.
// Python must never infer permission to replay an RT from a missing SQLite file.
func (s *Store) CredentialExecutionGuard(task Task) (string, error) {
	ref := CredentialTaskRef(task)
	if ref == "" {
		return "", nil
	}
	aliases := map[string]bool{ref: true}
	rows, err := s.exRO.Query(`SELECT ref FROM accounts WHERE platform='chatgpt'
 AND lower(trim(identity))<>'' AND lower(trim(identity)) IN
 (SELECT lower(trim(identity)) FROM accounts WHERE ref=? AND platform='chatgpt')`, ref)
	if err != nil {
		return "", errors.New("账号执行历史不可读；未执行平台操作，可稍后重试")
	}
	for rows.Next() {
		var alias string
		if err = rows.Scan(&alias); err != nil {
			rows.Close()
			return "", err
		}
		aliases[alias] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", err
	}
	var args []string
	if json.Unmarshal([]byte(task.ArgsJSON), &args) != nil {
		return "", errors.New("凭据任务参数无效")
	}
	authorize := false
	for _, arg := range args {
		if arg == "--allow-relogin" {
			authorize = true
		}
	}
	rows, err = s.exRO.Query(`SELECT t.id,t.args_json,t.status,t.exit_code,t.started_at,t.finished_at,
 COALESCE((SELECT MIN(e.id) FROM events e WHERE e.task_id=t.id AND e.stream='stage'
 AND `+s.ex.JSONText("e.line", "stage")+`='execution' AND `+s.ex.JSONText("e.line", "status")+`='running'),0)
 FROM tasks t
 WHERE script='asset_ops' AND id<>? AND status IN ('running','stopping','succeeded','failed','unknown','interrupted','stopped')
 ORDER BY finished_at DESC,created_at DESC,id DESC`, task.ID)
	if err != nil {
		return "", errors.New("账号执行历史不可读；未执行平台操作，可稍后重试")
	}
	defer rows.Close()
	type execution struct {
		at       time.Time
		sequence int64
	}
	var success execution
	var uncertain []execution
	for rows.Next() {
		prior := Task{Script: "asset_ops"}
		var evidence execution
		if err = rows.Scan(&prior.ID, &prior.ArgsJSON, &prior.Status, &prior.ExitCode, &prior.StartedAt, &prior.FinishedAt, &evidence.sequence); err != nil {
			return "", err
		}
		if !aliases[CredentialTaskRef(prior)] {
			continue
		}
		if prior.Status == "running" || prior.Status == "stopping" {
			return "busy", nil
		}
		if authorize {
			continue
		}
		evidence.at, _ = time.Parse(time.RFC3339Nano, prior.FinishedAt)
		if evidence.at.IsZero() {
			evidence.at, _ = time.Parse(time.RFC3339Nano, prior.StartedAt)
		}
		if prior.Status == "succeeded" {
			if evidence.at.After(success.at) || (evidence.at.Equal(success.at) && evidence.sequence > success.sequence) {
				success = evidence
			}
			continue
		}
		if (prior.Status == "failed" && prior.ExitCode == 75) ||
			((prior.Status == "unknown" || prior.Status == "interrupted" || prior.Status == "stopped") && prior.StartedAt != "") {
			uncertain = append(uncertain, evidence)
		}
	}
	if err = rows.Err(); err != nil {
		return "", err
	}
	for _, prior := range uncertain {
		// Old timestamps have one-second precision. Never use random task IDs
		// to decide which RT was consumed: retained execution events break ties.
		if !prior.at.IsZero() && (success.at.After(prior.at) ||
			(success.at.Equal(prior.at) && prior.sequence > 0 && success.sequence > prior.sequence)) {
			continue
		}
		return "relogin_required", nil
	}
	return "", nil
}
