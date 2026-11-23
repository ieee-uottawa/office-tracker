package database

import (
	"database/sql"
	"strings"
)

const bookingNotificationsSchema = `CREATE TABLE booking_notifications (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 booking_id INTEGER NOT NULL,
 event TEXT NOT NULL CHECK(event IN ('pending','approved','declined','cancelled','revoked','expired','replaced','reminder')),
 actor_id TEXT NOT NULL,
 channel_id TEXT NOT NULL DEFAULT '',
 message_id TEXT NOT NULL DEFAULT '',
 delivered_at INTEGER,
 attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(booking_id,event)
)`

// Notifications are written by the booking repository in the same transaction
// as each booking change (older versions used triggers, which are removed).
func migrateBookingNotifications(db *sql.DB) error {
	var existing string
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='booking_notifications'`).Scan(&existing)
	switch {
	case err == sql.ErrNoRows:
		if _, err = db.Exec(bookingNotificationsSchema); err != nil {
			return err
		}
	case err != nil:
		return err
	case !strings.Contains(existing, "'reminder'"):
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, stmt := range []string{
			`ALTER TABLE booking_notifications RENAME TO booking_notifications_old`,
			bookingNotificationsSchema,
			`INSERT INTO booking_notifications SELECT * FROM booking_notifications_old`,
			`DROP TABLE booking_notifications_old`,
		} {
			if _, err = tx.Exec(stmt); err != nil {
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	_, err = db.Exec(`
DROP TRIGGER IF EXISTS booking_request_notification;
DROP TRIGGER IF EXISTS booking_decision_notification;
CREATE INDEX IF NOT EXISTS idx_booking_notifications_delivery ON booking_notifications(delivered_at,next_attempt_at);`)
	return err
}
