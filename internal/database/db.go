// Package database provides functions for interacting with the SQLite database used by the office tracker application.
// It includes functions for opening a connection to the database and performing basic operations.
package database

import (
	"database/sql"

	"office/internal/logging"

	_ "modernc.org/sqlite"
)

var log = logging.Component("database")

// Open opens a connection to the SQLite database at the specified path and returns a *sql.DB instance.
func Open(path string) *sql.DB {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		log.Error("failed to open database", "err", err)
		panic(err)
	}

	if err := db.Ping(); err != nil {
		log.Error("failed to ping database", "err", err)
		panic(err)
	}

	return db
}

// Migrate creates the necessary tables in the database if they do not already exist.
func Migrate(db *sql.DB) error {
	queries := []string{
		// These PRAGMA statements configure the SQLite database for better performance and reliability in our use case.
		// - `journal_mode = WAL` enables Write-Ahead Logging, which allows for better concurrency and performance.
		// - `busy_timeout = 5000` sets a timeout of 5 seconds for database operations that are blocked by locks, reducing the likelihood of "database is locked" errors.
		// - `foreign_keys = ON` enables foreign key constraints, ensuring referential integrity between tables.
		`PRAGMA journal_mode = WAL; PRAGMA busy_timeout = 5000; PRAGMA foreign_keys = ON;`,

		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			rfid_uid TEXT UNIQUE NOT NULL,
			discord_id TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);`,

		`CREATE TABLE IF NOT EXISTS sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			check_in DATETIME NOT NULL,
			check_out DATETIME,
			check_out_method TEXT,
			CHECK (check_out_method IS NULL OR check_out_method IN ('rfid', 'discord', 'api', 'auto')),
			CHECK ((check_out IS NULL AND check_out_method IS NULL) OR (check_out IS NOT NULL AND check_out_method IS NOT NULL)),
			FOREIGN KEY(user_id) REFERENCES users(id)
		);`,

		// This index ensures that a user cannot have more than one active session (i.e., a session with a NULL check_out).
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_one_active_session ON sessions(user_id) WHERE check_out IS NULL;`,
		// This index improves query performance for session statistics and leaderboards that filter by check_out_method.
		`CREATE INDEX IF NOT EXISTS idx_sessions_checkout_method ON sessions(check_out_method);`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}

	if err := addColumnIfMissing(db, "users", "jingle", "TEXT"); err != nil {
		return err
	}

	return nil
}

// addColumnIfMissing adds a column to a table if it doesn't already exist.
// SQLite has no "ALTER TABLE ... ADD COLUMN IF NOT EXISTS", so existing
// columns are checked via PRAGMA table_info first to keep this idempotent
// across repeated startups, same as the CREATE TABLE IF NOT EXISTS calls above.
func addColumnIfMissing(db *sql.DB, table, column, sqlType string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + sqlType)
	return err
}
