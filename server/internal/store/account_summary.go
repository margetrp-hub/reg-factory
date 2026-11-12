package store

// AccountSummary reports the full indexed population, independently of a list's
// page size. It does not read credential files or mutate account health.
type AccountSummary struct {
	Platform  string `json:"platform"`
	Total     int    `json:"total"`
	Available int    `json:"available"`
	Abnormal  int    `json:"abnormal"`
	Unknown   int    `json:"unknown"`
	Used      int    `json:"used"`
	Leased    int    `json:"leased"`
	Sources   int    `json:"source_count"`
}

func (s *Store) AccountSummaries(platform string) ([]AccountSummary, error) {
	query := accountProjectionSQL + `SELECT platform, COUNT(*),
		SUM(CASE WHEN projected_library_status='available' THEN 1 ELSE 0 END),
		SUM(CASE WHEN projected_library_status NOT IN ('used','leased','available') AND (projected_library_status='invalid' OR status IN ('banned','invalid','expired','relogin_required','needs_refresh')) THEN 1 ELSE 0 END),
		SUM(CASE WHEN projected_library_status='pending' AND status NOT IN ('banned','invalid','expired','relogin_required','needs_refresh') THEN 1 ELSE 0 END),
		SUM(CASE WHEN projected_library_status='used' THEN 1 ELSE 0 END),
		SUM(CASE WHEN projected_library_status='leased' THEN 1 ELSE 0 END), SUM(source_count)
		FROM account_projection`
	var args []any
	if platform != "" {
		query += " WHERE platform=?"
		args = append(args, platform)
	}
	rows, err := s.exRO.Query(query+" GROUP BY platform ORDER BY platform", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []AccountSummary{}
	for rows.Next() {
		var item AccountSummary
		if err := rows.Scan(&item.Platform, &item.Total, &item.Available, &item.Abnormal, &item.Unknown, &item.Used, &item.Leased, &item.Sources); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
