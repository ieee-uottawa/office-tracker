package database

import (
	"database/sql"
	"strings"
)

// bookingsSchema is the current shape of the bookings table. Existing tables
// are rebuilt into it once (SQLite cannot alter CHECK constraints), keeping
// every row and ID.
const bookingsSchema = `CREATE TABLE bookings (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 request_id TEXT NOT NULL UNIQUE,
 guild_id TEXT NOT NULL,
 user_id TEXT NOT NULL,
 display_name TEXT NOT NULL,
 title TEXT NOT NULL,
 starts_at INTEGER NOT NULL,
 ends_at INTEGER NOT NULL CHECK (ends_at > starts_at),
 status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','declined','cancelled','revoked','expired','replaced')),
 series_id INTEGER NOT NULL DEFAULT 0,
 replaces_id INTEGER NOT NULL DEFAULT 0,
 decision_reason TEXT NOT NULL DEFAULT '',
 reviewed_by TEXT NOT NULL DEFAULT '',
 reviewed_at INTEGER,
 created_at INTEGER NOT NULL
)`

// migrateBookings only adds or reshapes booking tables, preserving attendance data.
func migrateBookings(db *sql.DB) error {
	var existing string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='bookings'`).Scan(&existing)
	switch {
	case err == sql.ErrNoRows:
		if _, err = db.Exec(bookingsSchema); err != nil {
			return err
		}
	case err != nil:
		return err
	case !strings.Contains(existing, "replaces_id"):
		if err = rebuildBookings(db); err != nil {
			return err
		}
	}
	_, err = db.Exec(`
CREATE INDEX IF NOT EXISTS idx_bookings_calendar ON bookings(guild_id,status,starts_at,ends_at);
CREATE INDEX IF NOT EXISTS idx_bookings_user ON bookings(guild_id,user_id,starts_at);
CREATE INDEX IF NOT EXISTS idx_bookings_series ON bookings(series_id);
CREATE TABLE IF NOT EXISTS booking_calendars (
 channel_id TEXT PRIMARY KEY,
 guild_id TEXT NOT NULL,
 message_id TEXT NOT NULL,
 week_start TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS booking_role_capabilities (
 guild_id TEXT NOT NULL,
 role_id TEXT NOT NULL,
 capability TEXT NOT NULL CHECK (capability IN ('book','approve')),
 PRIMARY KEY (guild_id,role_id,capability)
);
CREATE TABLE IF NOT EXISTS booking_settings (
 guild_id TEXT NOT NULL,
 key TEXT NOT NULL,
 value TEXT NOT NULL,
 PRIMARY KEY (guild_id,key)
);`)
	if err != nil {
		return err
	}
	return migrateBookingNotifications(db)
}

func rebuildBookings(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS booking_request_notification`,
		`DROP TRIGGER IF EXISTS booking_decision_notification`,
		`ALTER TABLE bookings RENAME TO bookings_old`,
		bookingsSchema,
		`INSERT INTO bookings(id,request_id,guild_id,user_id,display_name,title,starts_at,ends_at,status,reviewed_by,reviewed_at,created_at)
 SELECT id,request_id,guild_id,user_id,display_name,title,starts_at,ends_at,status,reviewed_by,reviewed_at,created_at FROM bookings_old`,
		`DROP TABLE bookings_old`,
		// Requests left undecided past their start used to linger as pending;
		// close them quietly instead of DMing people about old requests.
		`UPDATE bookings SET status='expired' WHERE status='pending' AND starts_at <= unixepoch()`,
	} {
		if _, err = tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
