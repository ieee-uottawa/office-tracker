package database

import "database/sql"

// Triggers enqueue notifications in the same transaction as each booking change.
func migrateBookingNotifications(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS booking_notifications (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 booking_id INTEGER NOT NULL,
 event TEXT NOT NULL CHECK(event IN ('pending','approved','declined','cancelled')),
 actor_id TEXT NOT NULL,
 channel_id TEXT NOT NULL DEFAULT '',
 message_id TEXT NOT NULL DEFAULT '',
 delivered_at INTEGER,
 attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt_at INTEGER NOT NULL DEFAULT 0,
 UNIQUE(booking_id,event)
);
CREATE INDEX IF NOT EXISTS idx_booking_notifications_delivery ON booking_notifications(delivered_at,next_attempt_at);
CREATE TRIGGER IF NOT EXISTS booking_request_notification AFTER INSERT ON bookings
BEGIN
 INSERT OR IGNORE INTO booking_notifications(booking_id,event,actor_id)
 VALUES(NEW.id,'pending',NEW.user_id);
END;
CREATE TRIGGER IF NOT EXISTS booking_decision_notification AFTER UPDATE OF status ON bookings
WHEN NEW.status<>OLD.status
BEGIN
 INSERT OR IGNORE INTO booking_notifications(booking_id,event,actor_id)
 VALUES(NEW.id,NEW.status,NEW.reviewed_by);
END;
INSERT OR IGNORE INTO booking_notifications(booking_id,event,actor_id)
 SELECT id,'pending',user_id FROM bookings WHERE status='pending' AND starts_at>unixepoch();
`)
	return err
}
