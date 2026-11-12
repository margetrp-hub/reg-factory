package store

import "database/sql"

// NewProbeStore 构造一个最小 Store，给诊断程序用。
func NewProbeStore(db *sql.DB, d Dialect) *Store {
	return &Store{db: db, read: db, readIsShared: true, dialect: d, ex: NewExecer(db, d), exRO: NewExecer(db, d)}
}
