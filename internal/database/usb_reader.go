package database

import "database/sql"

func migrateUSBReader(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS reader_requests (
 request_id TEXT PRIMARY KEY,
 payload TEXT NOT NULL,
 response TEXT NOT NULL,
 created_at INTEGER NOT NULL
 )`)
	return err
}
