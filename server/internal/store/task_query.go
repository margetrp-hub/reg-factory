package store

import (
	"strings"
)

// TaskQuery is a bounded history view. Status counts share its search/script
// scope, but not the selected status, so the UI can display honest filter counts.
type TaskQuery struct {
	Status, Search                       string
	Scripts                              []string
	Limit, Offset                        int
	CredentialPlatform, CredentialAction string
}

type TaskPage struct {
	Tasks  []Task         `json:"tasks"`
	Count  int            `json:"count"`
	Total  int            `json:"total"`
	Stats  map[string]int `json:"stats"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

func (s *Store) QueryTasks(q TaskQuery) (TaskPage, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	page := TaskPage{Tasks: []Task{}, Stats: map[string]int{}, Limit: q.Limit, Offset: q.Offset}
	where, args := " WHERE 1=1", []any{}
	if q.CredentialPlatform != "" {
		if q.CredentialAction == "refresh" {
			where += ` AND script='asset_ops' AND ` + s.ex.JSONText("args_json", "0") + `='refresh' AND ` + s.ex.JSONText("args_json", "4") + `=?`
		} else {
			where += ` AND script='credential.check' AND ` + s.ex.JSONText("args_json", "platform") + `=?`
		}
		args = append(args, q.CredentialPlatform)
	}
	if len(q.Scripts) > 0 {
		where += " AND script IN (" + strings.TrimSuffix(strings.Repeat("?,", len(q.Scripts)), ",") + ")"
		for _, script := range q.Scripts {
			args = append(args, script)
		}
	}
	if search := strings.TrimSpace(q.Search); search != "" {
		literal := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(search) + "%"
		where += ` AND (id LIKE ? ESCAPE '\' OR title LIKE ? ESCAPE '\' OR script LIKE ? ESCAPE '\')`
		args = append(args, literal, literal, literal)
	}
	tx, err := s.read.Begin()
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	counts, err := s.ex.QueryTx(tx, "SELECT status, COUNT(*) FROM tasks"+where+" GROUP BY status", args...)
	if err != nil {
		return page, err
	}
	for counts.Next() {
		var status string
		var count int
		if err = counts.Scan(&status, &count); err != nil {
			counts.Close()
			return page, err
		}
		page.Stats[status] = count
		if q.Status == "" || q.Status == status {
			page.Total += count
		}
	}
	err = counts.Err()
	counts.Close()
	if err != nil {
		return page, err
	}
	if q.Status != "" {
		where += " AND status=?"
		args = append(args, q.Status)
	}
	args = append(args, q.Limit, q.Offset)
	rows, err := s.ex.QueryTx(tx, `SELECT id, script, title, args_json, status, pid, created_at, started_at,
		finished_at, exit_code, error, actor FROM tasks`+where+` ORDER BY created_at DESC, id DESC LIMIT CAST(? AS BIGINT) OFFSET CAST(? AS BIGINT)`, args...)
	if err != nil {
		return page, err
	}
	for rows.Next() {
		var t Task
		if err = rows.Scan(&t.ID, &t.Script, &t.Title, &t.ArgsJSON, &t.Status, &t.PID,
			&t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.ExitCode, &t.Error, &t.Actor); err != nil {
			rows.Close()
			return page, err
		}
		page.Tasks = append(page.Tasks, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	page.Count = len(page.Tasks)
	return page, tx.Commit()
}
