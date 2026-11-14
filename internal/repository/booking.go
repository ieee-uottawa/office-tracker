package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var (
	ErrBookingConflict = errors.New("that time overlaps an approved booking")
	ErrBookingState    = errors.New("this booking has already changed; refresh to see its current status")
	ErrBookingPast     = errors.New("this booking has already started")
)

// Booking times are UTC Unix seconds; display and input use Ottawa local time.
type Booking struct {
	ID                           int64
	GuildID, UserID, Name, Title string
	Start, End                   time.Time
	Status, ReviewedBy           string
}

type BookingCalendar struct {
	GuildID, ChannelID, MessageID, Week string
}

type BookingRepo struct{ DB *sql.DB }

// busy_timeout belongs to a SQLite connection, so apply it to each borrowed
// writer rather than relying on the one connection used during migration.
func (r *BookingRepo) exec(query string, args ...any) (sql.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := r.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return nil, err
	}
	return conn.ExecContext(ctx, query, args...)
}

const bookingColumns = `id,guild_id,user_id,display_name,title,starts_at,ends_at,status,reviewed_by`

type bookingScanner interface{ Scan(...any) error }

func scanBooking(row bookingScanner) (*Booking, error) {
	b := &Booking{}
	var start, end int64
	err := row.Scan(&b.ID, &b.GuildID, &b.UserID, &b.Name, &b.Title, &start, &end, &b.Status, &b.ReviewedBy)
	b.Start, b.End = time.Unix(start, 0), time.Unix(end, 0)
	return b, err
}

func (r *BookingRepo) Get(guild string, id int64) (*Booking, error) {
	return scanBooking(r.DB.QueryRow(`SELECT `+bookingColumns+` FROM bookings WHERE guild_id=? AND id=?`, guild, id))
}

func (r *BookingRepo) Create(requestID string, b Booking, now time.Time) (*Booking, error) {
	// Discord retries of the same interaction cannot create duplicate requests.
	_, err := r.exec(`INSERT INTO bookings(request_id,guild_id,user_id,display_name,title,starts_at,ends_at,created_at)
 VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(request_id) DO NOTHING`, requestID, b.GuildID, b.UserID, b.Name, b.Title, b.Start.Unix(), b.End.Unix(), now.Unix())
	if err != nil {
		return nil, err
	}
	return scanBooking(r.DB.QueryRow(`SELECT `+bookingColumns+` FROM bookings WHERE request_id=? AND guild_id=? AND user_id=?`, requestID, b.GuildID, b.UserID))
}

func (r *BookingRepo) list(query string, args ...any) ([]Booking, error) {
	rows, err := r.DB.Query(`SELECT `+bookingColumns+` FROM bookings `+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Booking{}
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *b)
	}
	return result, rows.Err()
}

func (r *BookingRepo) Approved(guild string, start, end time.Time) ([]Booking, error) {
	return r.list(`WHERE guild_id=? AND status='approved' AND starts_at < ? AND ends_at > ? ORDER BY starts_at,id`, guild, end.Unix(), start.Unix())
}

func (r *BookingRepo) Pending(guild string, now time.Time) ([]Booking, error) {
	return r.list(`WHERE guild_id=? AND status='pending' AND starts_at > ? ORDER BY starts_at,id`, guild, now.Unix())
}

func (r *BookingRepo) Mine(guild, user string, now time.Time) ([]Booking, error) {
	return r.list(`WHERE guild_id=? AND user_id=? AND ends_at > ? ORDER BY starts_at,id`, guild, user, now.AddDate(0, -1, 0).Unix())
}

func (r *BookingRepo) Decide(guild string, id int64, officer string, approve bool, now time.Time) error {
	status := "declined"
	if approve {
		status = "approved"
	}
	// A single SQL write serializes competing approvals. Half-open intervals
	// allow one reservation to begin exactly when another ends.
	result, err := r.exec(`UPDATE bookings SET status=?,reviewed_by=?,reviewed_at=?
 WHERE id=? AND guild_id=? AND status='pending' AND starts_at > ?
 AND (?='declined' OR NOT EXISTS (
 SELECT 1 FROM bookings AS other WHERE other.guild_id=bookings.guild_id
 AND other.status='approved' AND other.id<>bookings.id
 AND other.starts_at < bookings.ends_at AND other.ends_at > bookings.starts_at))`,
		status, officer, now.Unix(), id, guild, now.Unix(), status)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n > 0 {
		return err
	}
	b, err := r.Get(guild, id)
	if err != nil {
		return err
	}
	if b.Status != "pending" {
		return ErrBookingState
	}
	if !b.Start.After(now) {
		return ErrBookingPast
	}
	return ErrBookingConflict
}

func (r *BookingRepo) Cancel(guild string, id int64, user string, now time.Time) error {
	result, err := r.exec(`UPDATE bookings SET status='cancelled',reviewed_by=?,reviewed_at=?
 WHERE id=? AND guild_id=? AND user_id=? AND status IN ('pending','approved') AND starts_at > ?`, user, now.Unix(), id, guild, user, now.Unix())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBookingState
	}
	return nil
}

func (r *BookingRepo) SaveCalendar(c BookingCalendar) error {
	_, err := r.exec(`INSERT INTO booking_calendars(channel_id,guild_id,message_id,week_start) VALUES(?,?,?,?)
 ON CONFLICT(channel_id) DO UPDATE SET guild_id=excluded.guild_id,message_id=excluded.message_id,week_start=excluded.week_start`, c.ChannelID, c.GuildID, c.MessageID, c.Week)
	return err
}

func (r *BookingRepo) Calendars(guild string) ([]BookingCalendar, error) {
	rows, err := r.DB.Query(`SELECT guild_id,channel_id,message_id,week_start FROM booking_calendars WHERE guild_id=?`, guild)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []BookingCalendar{}
	for rows.Next() {
		var c BookingCalendar
		if err := rows.Scan(&c.GuildID, &c.ChannelID, &c.MessageID, &c.Week); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (r *BookingRepo) DeleteCalendar(channel, message string) error {
	_, err := r.exec(`DELETE FROM booking_calendars WHERE channel_id=? AND message_id=?`, channel, message)
	return err
}
