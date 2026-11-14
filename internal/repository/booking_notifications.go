package repository

import (
	"database/sql"
	"errors"
	"time"
)

type BookingNotification struct {
	ID                                   int64
	Event, ActorID, ChannelID, MessageID string
	Attempts                             int
	Booking                              Booking
}

func (r *BookingRepo) PendingNotifications(guild string, now time.Time) ([]BookingNotification, error) {
	rows, err := r.DB.Query(`SELECT n.id,n.event,n.actor_id,n.channel_id,n.message_id,n.attempts,
 b.id,b.guild_id,b.user_id,b.display_name,b.title,b.starts_at,b.ends_at,b.status,b.reviewed_by
 FROM booking_notifications n JOIN bookings b ON b.id=n.booking_id
 WHERE b.guild_id=? AND n.delivered_at IS NULL AND n.next_attempt_at<=?
 ORDER BY n.id LIMIT 50`, guild, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	notices := []BookingNotification{}
	for rows.Next() {
		var n BookingNotification
		var start, end int64
		b := &n.Booking
		if err := rows.Scan(&n.ID, &n.Event, &n.ActorID, &n.ChannelID, &n.MessageID, &n.Attempts, &b.ID, &b.GuildID, &b.UserID, &b.Name, &b.Title, &start, &end, &b.Status, &b.ReviewedBy); err != nil {
			return nil, err
		}
		b.Start, b.End = time.Unix(start, 0), time.Unix(end, 0)
		notices = append(notices, n)
	}
	return notices, rows.Err()
}

func (r *BookingRepo) SaveNotificationMessage(id int64, channel, message string) error {
	_, err := r.exec(`UPDATE booking_notifications SET channel_id=?,message_id=? WHERE id=?`, channel, message, id)
	return err
}

func (r *BookingRepo) CompleteNotification(id int64, now time.Time) error {
	_, err := r.exec(`UPDATE booking_notifications SET delivered_at=? WHERE id=?`, now.Unix(), id)
	return err
}

func (r *BookingRepo) RetryNotification(id int64, attempts int, now time.Time) error {
	delay := min(300, 5*(1<<min(attempts, 6)))
	_, err := r.exec(`UPDATE booking_notifications SET attempts=attempts+1,next_attempt_at=? WHERE id=?`, now.Add(time.Duration(delay)*time.Second).Unix(), id)
	return err
}

func (r *BookingRepo) RequestNotificationMessage(bookingID int64) (channel, message string, err error) {
	err = r.DB.QueryRow(`SELECT channel_id,message_id FROM booking_notifications WHERE booking_id=? AND event='pending'`, bookingID).Scan(&channel, &message)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}
