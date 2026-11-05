package store

// WorkEventsPage reads only this persisted task; no runner or remote work starts.
func (s *Store) WorkEventsPage(taskID string, page, size int) ([]Event, int64, error) {
	var total int64
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE task_id=?`, taskID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT id,task_id,ts,stream,substr(line,1,16384) FROM events WHERE task_id=? ORDER BY id LIMIT ? OFFSET ?`, taskID, size, (page-1)*size)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Event{}
	for rows.Next() {
		var item Event
		if err := rows.Scan(&item.ID, &item.TaskID, &item.TS, &item.Stream, &item.Line); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}
