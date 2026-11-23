package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrBookingConflict = errors.New("that time is already taken")
	ErrBookingState    = errors.New("this booking has already changed; refresh to see its current status")
	ErrBookingPast     = errors.New("this booking has already started")
	ErrBookingOwner    = errors.New("you can only change your own bookings")
	ErrSelfApproval    = errors.New("another officer must approve your request")
	ErrChangePending   = errors.New("a time change for this booking is already waiting for approval; cancel it in /bookings first")
)

// Booking times are UTC Unix seconds; display and input use Ottawa local time.
// A series shares SeriesID (the ID of its first booking); a time change is a
// new request whose ReplacesID points at the booking it would replace.
type Booking struct {
	ID                           int64
	GuildID, UserID, Name, Title string
	Start, End                   time.Time
	Status, ReviewedBy, Reason   string
	SeriesID, ReplacesID         int64
	CreatedAt, ReviewedAt        time.Time
}

// Lead is the booking that represents its request (the first of a series).
func (b Booking) Lead() int64 {
	if b.SeriesID > 0 {
		return b.SeriesID
	}
	return b.ID
}

func (b Booking) Active() bool { return b.Status == "pending" || b.Status == "approved" }

type BookingCalendar struct {
	GuildID, ChannelID, MessageID, Week string
}

type BookingRepo struct{ DB *sql.DB }

type sqlRunner interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// write runs fn in an IMMEDIATE transaction so overlap checks and inserts are
// serialized against every other booking writer. busy_timeout belongs to a
// SQLite connection, so it is applied to each borrowed connection.
func (r *BookingRepo) write(fn func(ctx context.Context, q sqlRunner) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := r.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return err
	}
	if _, err = conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	if err = fn(ctx, conn); err != nil {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		return err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

func (r *BookingRepo) exec(query string, args ...any) error {
	return r.write(func(ctx context.Context, q sqlRunner) error {
		_, err := q.ExecContext(ctx, query, args...)
		return err
	})
}

const bookingColumns = `id,guild_id,user_id,display_name,title,starts_at,ends_at,status,reviewed_by,decision_reason,series_id,replaces_id,created_at,COALESCE(reviewed_at,0)`

type bookingScanner interface{ Scan(...any) error }

func scanBooking(row bookingScanner) (*Booking, error) {
	b := &Booking{}
	var start, end, created, reviewed int64
	err := row.Scan(&b.ID, &b.GuildID, &b.UserID, &b.Name, &b.Title, &start, &end, &b.Status, &b.ReviewedBy, &b.Reason, &b.SeriesID, &b.ReplacesID, &created, &reviewed)
	b.Start, b.End, b.CreatedAt = time.Unix(start, 0), time.Unix(end, 0), time.Unix(created, 0)
	if reviewed > 0 {
		b.ReviewedAt = time.Unix(reviewed, 0)
	}
	return b, err
}

func listBookings(ctx context.Context, q sqlRunner, where string, args ...any) ([]Booking, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+bookingColumns+` FROM bookings `+where, args...)
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

func (r *BookingRepo) list(where string, args ...any) ([]Booking, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := r.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return listBookings(ctx, conn, where, args...)
}

func getBooking(ctx context.Context, q sqlRunner, guild string, id int64) (*Booking, error) {
	return scanBooking(q.QueryRowContext(ctx, `SELECT `+bookingColumns+` FROM bookings WHERE guild_id=? AND id=?`, guild, id))
}

func (r *BookingRepo) Get(guild string, id int64) (*Booking, error) {
	return scanBooking(r.DB.QueryRow(`SELECT `+bookingColumns+` FROM bookings WHERE guild_id=? AND id=?`, guild, id))
}

func notify(ctx context.Context, q sqlRunner, bookingID int64, event, actor string) error {
	_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO booking_notifications(booking_id,event,actor_id) VALUES(?,?,?)`, bookingID, event, actor)
	return err
}

// overlaps reports whether [start,end) intersects a held (pending) or approved
// booking. Half-open intervals let one booking begin exactly when another ends.
func overlaps(ctx context.Context, q sqlRunner, guild string, start, end time.Time, ignore ...int64) (bool, error) {
	query := `SELECT COUNT(*) FROM bookings WHERE guild_id=? AND status IN ('pending','approved') AND starts_at < ? AND ends_at > ?`
	args := []any{guild, end.Unix(), start.Unix()}
	for _, id := range ignore {
		if id > 0 {
			query += ` AND id<>?`
			args = append(args, id)
		}
	}
	var n int
	err := q.QueryRowContext(ctx, query, args...).Scan(&n)
	return n > 0, err
}

// Busy lists held and approved bookings that intersect [start,end).
func (r *BookingRepo) Busy(guild string, start, end time.Time) ([]Booking, error) {
	return r.list(`WHERE guild_id=? AND status IN ('pending','approved') AND starts_at < ? AND ends_at > ? ORDER BY starts_at,id`, guild, end.Unix(), start.Unix())
}

// Approved lists approved bookings that intersect [start,end).
func (r *BookingRepo) Approved(guild string, start, end time.Time) ([]Booking, error) {
	return r.list(`WHERE guild_id=? AND status='approved' AND starts_at < ? AND ends_at > ? ORDER BY starts_at,id`, guild, end.Unix(), start.Unix())
}

// Pending lists upcoming bookings still waiting for a decision.
func (r *BookingRepo) Pending(guild string, now time.Time) ([]Booking, error) {
	return r.list(`WHERE guild_id=? AND status='pending' AND starts_at > ? ORDER BY starts_at,id`, guild, now.Unix())
}

// ApprovedUpcoming lists approved bookings that have not started.
func (r *BookingRepo) ApprovedUpcoming(guild string, now time.Time) ([]Booking, error) {
	return r.list(`WHERE guild_id=? AND status='approved' AND starts_at > ? ORDER BY starts_at,id`, guild, now.Unix())
}

// Mine lists a member's bookings from the last week onwards, without the
// bookings that were superseded by an approved time change.
func (r *BookingRepo) Mine(guild, user string, now time.Time) ([]Booking, error) {
	return r.list(`WHERE guild_id=? AND user_id=? AND status<>'replaced' AND ends_at > ? ORDER BY starts_at,id`, guild, user, now.AddDate(0, 0, -7).Unix())
}

// Series lists every booking that belongs to the same request as lead.
func (r *BookingRepo) Series(guild string, lead int64) ([]Booking, error) {
	return r.list(`WHERE guild_id=? AND (id=? OR series_id=?) ORDER BY starts_at,id`, guild, lead, lead)
}

// PendingChange returns the pending time change for a booking, if any.
func (r *BookingRepo) PendingChange(guild string, id int64) (*Booking, error) {
	rows, err := r.list(`WHERE guild_id=? AND replaces_id=? AND status='pending' ORDER BY id LIMIT 1`, guild, id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// Create inserts one request: a single booking or a series. Occurrences that
// overlap a held or approved booking are skipped; a single booking that
// overlaps returns ErrBookingConflict. A time change (replaces > 0) keeps the
// original until it is approved, except a pending original, which is
// replaced immediately. Discord retries of the same interaction are no-ops.
func (r *BookingRepo) Create(requestID string, items []Booking, replaces int64, now time.Time) (created []Booking, skipped int, err error) {
	if len(items) == 0 {
		return nil, 0, errors.New("no booking times")
	}
	guild, user := items[0].GuildID, items[0].UserID
	err = r.write(func(ctx context.Context, q sqlRunner) error {
		existing, err := listBookings(ctx, q, `WHERE guild_id=? AND user_id=? AND (request_id=? OR request_id LIKE ?) ORDER BY starts_at,id`, guild, user, requestID, requestID+":%")
		if err != nil || len(existing) > 0 {
			created = existing
			return err
		}
		var original *Booking
		if replaces > 0 {
			if original, err = getBooking(ctx, q, guild, replaces); err != nil {
				return err
			}
			if original.UserID != user {
				return ErrBookingOwner
			}
			if !original.Active() {
				return ErrBookingState
			}
			if !original.Start.After(now) {
				return ErrBookingPast
			}
			var n int
			if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM bookings WHERE replaces_id=? AND status='pending'`, replaces).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				return ErrChangePending
			}
		}
		var lead int64
		for n, b := range items {
			busy, err := overlaps(ctx, q, guild, b.Start, b.End, replaces)
			if err != nil {
				return err
			}
			if busy {
				skipped++
				continue
			}
			id := requestID
			if len(items) > 1 {
				id = fmt.Sprintf("%s:%d", requestID, n)
			}
			result, err := q.ExecContext(ctx, `INSERT INTO bookings(request_id,guild_id,user_id,display_name,title,starts_at,ends_at,series_id,replaces_id,created_at)
 VALUES(?,?,?,?,?,?,?,?,?,?)`, id, guild, user, b.Name, b.Title, b.Start.Unix(), b.End.Unix(), lead, replaces, now.Unix())
			if err != nil {
				return err
			}
			b.ID, err = result.LastInsertId()
			if err != nil {
				return err
			}
			b.Status, b.SeriesID, b.ReplacesID = "pending", lead, replaces
			if len(items) > 1 && lead == 0 {
				lead = b.ID
				b.SeriesID = lead
				if _, err = q.ExecContext(ctx, `UPDATE bookings SET series_id=? WHERE id=?`, lead, lead); err != nil {
					return err
				}
			}
			created = append(created, b)
		}
		if len(created) == 0 {
			return ErrBookingConflict
		}
		if original != nil && original.Status == "pending" {
			if _, err = q.ExecContext(ctx, `UPDATE bookings SET status='replaced',reviewed_by=?,reviewed_at=? WHERE id=?`, user, now.Unix(), original.ID); err != nil {
				return err
			}
			if err = notify(ctx, q, original.ID, "replaced", user); err != nil {
				return err
			}
		}
		return notify(ctx, q, created[0].ID, "pending", user)
	})
	return created, skipped, err
}

// Decide approves or declines every upcoming pending booking of a request.
// Approving a time change replaces the booking it was made for.
func (r *BookingRepo) Decide(guild string, lead int64, officer string, approve bool, reason string, now time.Time) error {
	return r.decide(guild, lead, officer, func(Booking) bool { return approve }, reason, now)
}

// DecideSelected approves the listed pending dates of a request and declines
// the rest. One notification goes out: approved when any date was approved.
func (r *BookingRepo) DecideSelected(guild string, lead int64, officer string, approveIDs []int64, reason string, now time.Time) error {
	keep := map[int64]bool{}
	for _, id := range approveIDs {
		keep[id] = true
	}
	return r.decide(guild, lead, officer, func(b Booking) bool { return keep[b.ID] }, reason, now)
}

func (r *BookingRepo) decide(guild string, lead int64, officer string, approve func(Booking) bool, reason string, now time.Time) error {
	return r.write(func(ctx context.Context, q sqlRunner) error {
		b, err := getBooking(ctx, q, guild, lead)
		if err != nil {
			return err
		}
		lead = b.Lead()
		if b.UserID == officer {
			return ErrSelfApproval
		}
		rows, err := listBookings(ctx, q, `WHERE guild_id=? AND (id=? OR series_id=?) AND status='pending' AND starts_at > ?`, guild, lead, lead, now.Unix())
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			if b.Status == "pending" {
				return ErrBookingPast
			}
			return ErrBookingState
		}
		approved := 0
		for _, row := range rows {
			status, rowReason := "declined", reason
			if approve(row) {
				status, rowReason = "approved", ""
				approved++
				// Holds normally prevent this; older overlapping requests may remain.
				var n int
				if err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM bookings WHERE guild_id=? AND status='approved' AND id<>? AND id<>? AND starts_at < ? AND ends_at > ?`,
					guild, row.ID, row.ReplacesID, row.End.Unix(), row.Start.Unix()).Scan(&n); err != nil {
					return err
				}
				if n > 0 {
					return ErrBookingConflict
				}
			}
			if _, err = q.ExecContext(ctx, `UPDATE bookings SET status=?,reviewed_by=?,reviewed_at=?,decision_reason=? WHERE id=?`, status, officer, now.Unix(), rowReason, row.ID); err != nil {
				return err
			}
			if status == "approved" && row.ReplacesID > 0 {
				result, err := q.ExecContext(ctx, `UPDATE bookings SET status='replaced',reviewed_by=?,reviewed_at=? WHERE id=? AND status IN ('pending','approved')`, officer, now.Unix(), row.ReplacesID)
				if err != nil {
					return err
				}
				if n, _ := result.RowsAffected(); n > 0 {
					if err = notify(ctx, q, row.ReplacesID, "replaced", officer); err != nil {
						return err
					}
				}
			}
		}
		event := "declined"
		if approved > 0 {
			event = "approved"
		}
		return notify(ctx, q, lead, event, officer)
	})
}

// end closes upcoming bookings (one, or it and the rest of its series) and any
// time changes waiting on them.
func (r *BookingRepo) end(guild string, id int64, actor string, owner bool, rest bool, from []string, status, reason string, now time.Time) error {
	return r.write(func(ctx context.Context, q sqlRunner) error {
		b, err := getBooking(ctx, q, guild, id)
		if err != nil {
			return err
		}
		if owner && b.UserID != actor {
			return ErrBookingOwner
		}
		if !b.Start.After(now) {
			return ErrBookingPast
		}
		statuses := "'" + strings.Join(from, "','") + "'"
		where := `WHERE guild_id=? AND id=? AND status IN (` + statuses + `) AND starts_at > ?`
		args := []any{guild, b.ID, now.Unix()}
		if rest && b.SeriesID > 0 {
			where = `WHERE guild_id=? AND series_id=? AND starts_at >= ? AND status IN (` + statuses + `) AND starts_at > ?`
			args = []any{guild, b.SeriesID, b.Start.Unix(), now.Unix()}
		}
		rows, err := listBookings(ctx, q, where+` ORDER BY starts_at,id`, args...)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return ErrBookingState
		}
		for _, row := range rows {
			if _, err = q.ExecContext(ctx, `UPDATE bookings SET status=?,reviewed_by=?,reviewed_at=?,decision_reason=? WHERE id=?`, status, actor, now.Unix(), reason, row.ID); err != nil {
				return err
			}
			changes, err := listBookings(ctx, q, `WHERE replaces_id=? AND status='pending'`, row.ID)
			if err != nil {
				return err
			}
			for _, change := range changes {
				if _, err = q.ExecContext(ctx, `UPDATE bookings SET status='cancelled',reviewed_by=?,reviewed_at=? WHERE id=?`, actor, now.Unix(), change.ID); err != nil {
					return err
				}
				if err = notify(ctx, q, change.ID, "cancelled", actor); err != nil {
					return err
				}
			}
		}
		return notify(ctx, q, rows[0].ID, status, actor)
	})
}

// Cancel lets a member cancel their own upcoming booking(s).
func (r *BookingRepo) Cancel(guild string, id int64, user string, rest bool, now time.Time) error {
	return r.end(guild, id, user, true, rest, []string{"pending", "approved"}, "cancelled", "", now)
}

// Revoke lets an officer withdraw approved upcoming booking(s).
func (r *BookingRepo) Revoke(guild string, id int64, officer string, rest bool, reason string, now time.Time) error {
	return r.end(guild, id, officer, false, rest, []string{"approved"}, "revoked", reason, now)
}

// Expire frees requests that reached their start time without a decision.
func (r *BookingRepo) Expire(guild string, now time.Time) (int, error) {
	count := 0
	err := r.write(func(ctx context.Context, q sqlRunner) error {
		rows, err := listBookings(ctx, q, `WHERE guild_id=? AND status='pending' AND starts_at <= ?`, guild, now.Unix())
		if err != nil {
			return err
		}
		for _, row := range rows {
			if _, err = q.ExecContext(ctx, `UPDATE bookings SET status='expired',reviewed_at=? WHERE id=?`, now.Unix(), row.ID); err != nil {
				return err
			}
			// Only tell people about requests that expired recently (the bot
			// may have been offline for a while).
			if row.Start.After(now.Add(-24 * time.Hour)) {
				if err = notify(ctx, q, row.ID, "expired", ""); err != nil {
					return err
				}
			}
		}
		count = len(rows)
		return nil
	})
	return count, err
}

// QueueReminders queues one officer reminder per pending booking starting
// within the window, if the request has been waiting at least minWait (so a
// last-minute request is not pinged twice in a row).
func (r *BookingRepo) QueueReminders(guild string, now time.Time, window, minWait time.Duration) error {
	return r.exec(`INSERT OR IGNORE INTO booking_notifications(booking_id,event,actor_id)
 SELECT id,'reminder','' FROM bookings WHERE guild_id=? AND status='pending' AND starts_at > ? AND starts_at <= ? AND created_at <= ?`,
		guild, now.Unix(), now.Add(window).Unix(), now.Add(-minWait).Unix())
}

func (r *BookingRepo) SaveCalendar(c BookingCalendar) error {
	return r.exec(`INSERT INTO booking_calendars(channel_id,guild_id,message_id,week_start) VALUES(?,?,?,?)
 ON CONFLICT(channel_id) DO UPDATE SET guild_id=excluded.guild_id,message_id=excluded.message_id,week_start=excluded.week_start`, c.ChannelID, c.GuildID, c.MessageID, c.Week)
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
	return r.exec(`DELETE FROM booking_calendars WHERE channel_id=? AND message_id=?`, channel, message)
}

// RoleCapabilities maps each capability ("book", "approve") to its role IDs.
func (r *BookingRepo) RoleCapabilities(guild string) (map[string][]string, error) {
	rows, err := r.DB.Query(`SELECT capability,role_id FROM booking_role_capabilities WHERE guild_id=? ORDER BY capability,role_id`, guild)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string][]string{}
	for rows.Next() {
		var capability, role string
		if err := rows.Scan(&capability, &role); err != nil {
			return nil, err
		}
		result[capability] = append(result[capability], role)
	}
	return result, rows.Err()
}

func (r *BookingRepo) SetRoleCapability(guild, role, capability string, enabled bool) error {
	if enabled {
		return r.exec(`INSERT OR IGNORE INTO booking_role_capabilities(guild_id,role_id,capability) VALUES(?,?,?)`, guild, role, capability)
	}
	return r.exec(`DELETE FROM booking_role_capabilities WHERE guild_id=? AND role_id=? AND capability=?`, guild, role, capability)
}

func (r *BookingRepo) Setting(guild, key string) (string, error) {
	var value string
	err := r.DB.QueryRow(`SELECT value FROM booking_settings WHERE guild_id=? AND key=?`, guild, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (r *BookingRepo) SetSetting(guild, key, value string) error {
	return r.exec(`INSERT INTO booking_settings(guild_id,key,value) VALUES(?,?,?) ON CONFLICT(guild_id,key) DO UPDATE SET value=excluded.value`, guild, key, value)
}

// RefreshNames updates the requester name on upcoming bookings (keyed by Discord ID).
func (r *BookingRepo) RefreshNames(guild string, names map[string]string, now time.Time) error {
	for user, name := range names {
		if err := r.exec(`UPDATE bookings SET display_name=? WHERE guild_id=? AND user_id=? AND ends_at > ? AND display_name<>?`, name, guild, user, now.Unix(), name); err != nil {
			return err
		}
	}
	return nil
}
