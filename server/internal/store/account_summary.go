package store

// AccountSummary reports the full indexed population, independently of a list's
// page size. It does not read credential files or mutate account health.
type AccountSummary struct {
	Platform  string `json:"platform"`
	Total     int    `json:"total"`
	Available int    `json:"available"`
	Abnormal  int    `json:"abnormal"`
	Unknown   int    `json:"unknown"`
}

func (s *Store) AccountSummaries(platform string) ([]AccountSummary, error) {
	query := `SELECT platform, COUNT(*),
		SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status IN ('banned','invalid','expired','relogin_required') THEN 1 ELSE 0 END),
		SUM(CASE WHEN status NOT IN ('ok','banned','invalid','expired','relogin_required') THEN 1 ELSE 0 END)
		FROM accounts`
	var args []any
	if platform != "" {
		query += " WHERE platform=?"
		args = append(args, platform)
	}
	rows, err := s.db.Query(query+" GROUP BY platform ORDER BY platform", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AccountSummary{}
	for rows.Next() {
		var item AccountSummary
		if err := rows.Scan(&item.Platform, &item.Total, &item.Available, &item.Abnormal, &item.Unknown); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
