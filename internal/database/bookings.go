package database

import "database/sql"

// migrateBookings only adds tables and indexes, preserving attendance data.
func migrateBookings(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS bookings (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 request_id TEXT NOT NULL UNIQUE,
 guild_id TEXT NOT NULL,
 user_id TEXT NOT NULL,
 display_name TEXT NOT NULL,
 title TEXT NOT NULL,
 starts_at INTEGER NOT NULL,
 ends_at INTEGER NOT NULL CHECK (ends_at > starts_at),
 status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','declined','cancelled')),
 reviewed_by TEXT NOT NULL DEFAULT '',
 reviewed_at INTEGER,
 created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_bookings_calendar ON bookings(guild_id,status,starts_at,ends_at);
CREATE INDEX IF NOT EXISTS idx_bookings_user ON bookings(guild_id,user_id,starts_at);
CREATE TABLE IF NOT EXISTS booking_calendars (
 channel_id TEXT PRIMARY KEY,
 guild_id TEXT NOT NULL,
 message_id TEXT NOT NULL,
 week_start TEXT NOT NULL
);`)
	if err != nil {
		return err
	}
	return migrateBookingNotifications(db)
}
