package service

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"office/internal/database"
	"office/internal/repository"

	"github.com/stretchr/testify/require"
)

func bookingTestService(t *testing.T) *BookingService {
	t.Helper()
	db := database.Open(filepath.Join(t.TempDir(), "bookings.db"))
	t.Cleanup(func() { db.Close() })
	require.NoError(t, database.Migrate(db))
	require.NoError(t, database.Migrate(db)) // upgrades are safe to repeat
	return &BookingService{Repo: &repository.BookingRepo{DB: db}}
}

// Thursday, October 1 2026, 09:00 in Ottawa.
func bookingTestNow() time.Time { return time.Date(2026, 10, 1, 9, 0, 0, 0, BookingLocation) }

var officer = []string{SigningOfficerRoleID}

func testDraft(day string, start, end int) Draft {
	d, err := time.ParseInLocation("2006-01-02", day, BookingLocation)
	if err != nil {
		panic(err)
	}
	return Draft{Title: "Committee planning", Day: d, StartMin: start * 60, EndMin: end * 60}
}

func submit(t *testing.T, s *BookingService, request, user string, d Draft) []repository.Booking {
	t.Helper()
	created, _, err := s.Submit(request, "exec", user, user, nil, d, bookingTestNow())
	require.NoError(t, err)
	return created
}

func status(t *testing.T, s *BookingService, id int64) string {
	t.Helper()
	b, err := s.Repo.Get("exec", id)
	require.NoError(t, err)
	return b.Status
}

func TestPendingRequestsHoldTheSlot(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	a := submit(t, s, "a", "alice", testDraft("2026-10-02", 14, 16))[0]
	_, _, err := s.Submit("b", "exec", "bob", "Bob", nil, testDraft("2026-10-02", 15, 17), now)
	require.ErrorIs(t, err, repository.ErrBookingConflict, "a pending request holds its time")
	submit(t, s, "c", "carol", testDraft("2026-10-02", 16, 17)) // adjacent is fine
	require.NoError(t, s.Decline(a.ID, now))
	submit(t, s, "b2", "bob", testDraft("2026-10-02", 15, 16)) // declined time is free again
}

// Decline is a test helper for an officer declining without a reason.
func (s *BookingService) Decline(id int64, now time.Time) error {
	return s.Decide("exec", id, "officer", officer, false, "", now)
}

func TestApprovalPermissionsAndSelfApproval(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	a := submit(t, s, "a", "alice", testDraft("2026-10-02", 14, 16))[0]
	for _, roles := range [][]string{nil, {"administrator"}, {"wrong-role"}} {
		require.ErrorIs(t, s.Decide("exec", a.ID, "officer", roles, true, "", now), ErrBookingPermission)
	}
	require.ErrorIs(t, s.Decide("other-guild", a.ID, "officer", officer, true, "", now), sql.ErrNoRows)
	require.ErrorIs(t, s.Decide("exec", a.ID, "alice", officer, true, "", now), repository.ErrSelfApproval)
	require.NoError(t, s.Decide("exec", a.ID, "officer", officer, true, "", now))
	require.ErrorIs(t, s.Decide("exec", a.ID, "officer", officer, false, "", now), repository.ErrBookingState)
	b, err := s.Repo.Get("exec", a.ID)
	require.NoError(t, err)
	require.Equal(t, "approved", b.Status)
	require.Equal(t, "officer", b.ReviewedBy)
}

func TestRoleCapabilities(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	access, err := s.Access("exec", nil)
	require.NoError(t, err)
	require.Equal(t, Access{Book: true}, access, "anyone books and Signing Officers approve by default")
	require.NoError(t, s.Repo.SetRoleCapability("exec", "exec-role", CapabilityBook, true))
	require.NoError(t, s.Repo.SetRoleCapability("exec", "chair", CapabilityApprove, true))
	_, _, err = s.Submit("x", "exec", "guest", "Guest", []string{"alumni"}, testDraft("2026-10-02", 14, 15), now)
	require.ErrorIs(t, err, ErrBookPermission)
	created, _, err := s.Submit("y", "exec", "member", "Member", []string{"exec-role"}, testDraft("2026-10-02", 14, 15), now)
	require.NoError(t, err)
	require.ErrorIs(t, s.Decide("exec", created[0].ID, "old-officer", officer, true, "", now), ErrBookingPermission, "assigned approver roles replace the default")
	require.NoError(t, s.Decide("exec", created[0].ID, "chair-person", []string{"chair"}, true, "", now))
	require.Equal(t, []string{"chair"}, s.ApproverRoles("exec"))
	require.NoError(t, s.Repo.SetRoleCapability("exec", "exec-role", CapabilityBook, false))
	access, err = s.Access("exec", []string{"alumni"})
	require.NoError(t, err)
	require.True(t, access.Book)
	// A single member can be granted a capability without a role, and is not pinged as a role.
	require.NoError(t, s.Repo.SetRoleCapability("exec", UserGrantPrefix+"tester", CapabilityApprove, true))
	access, err = s.Access("exec", MemberGrants("tester", []string{"alumni"}))
	require.NoError(t, err)
	require.True(t, access.Approve)
	access, err = s.Access("exec", MemberGrants("someone-else", []string{"alumni"}))
	require.NoError(t, err)
	require.False(t, access.Approve)
	require.Equal(t, []string{"chair"}, s.ApproverRoles("exec"))
	require.NoError(t, s.Repo.SetSetting("exec", SettingOfficerChannel, "new-channel"))
	require.Equal(t, "new-channel", s.Setting("exec", SettingOfficerChannel, DefaultOfficerChannelID))
	require.Equal(t, "fallback", s.Setting("exec", SettingCalendarChannel, "fallback"))
}

func TestCancellationRevocationAndReplay(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	a := submit(t, s, "same-interaction", "alice", testDraft("2026-10-02", 14, 16))
	replay := submit(t, s, "same-interaction", "alice", testDraft("2026-10-02", 14, 16))
	require.Equal(t, a[0].ID, replay[0].ID, "a retried interaction does not create a second request")
	require.ErrorIs(t, s.Cancel("exec", a[0].ID, "bob", false, now), repository.ErrBookingOwner)
	require.ErrorIs(t, s.Cancel("different-guild", a[0].ID, "alice", false, now), sql.ErrNoRows)
	require.NoError(t, s.Decide("exec", a[0].ID, "officer", officer, true, "", now))
	require.ErrorIs(t, s.Revoke("exec", a[0].ID, "someone", nil, false, "", now), ErrBookingPermission)
	require.NoError(t, s.Revoke("exec", a[0].ID, "officer", officer, false, "Room needed for interviews", now))
	b, err := s.Repo.Get("exec", a[0].ID)
	require.NoError(t, err)
	require.Equal(t, "revoked", b.Status)
	require.Equal(t, "Room needed for interviews", b.Reason)
	c := submit(t, s, "available-again", "bob", testDraft("2026-10-02", 14, 16))[0]
	require.ErrorIs(t, s.Cancel("exec", c.ID, "bob", false, c.Start), repository.ErrBookingPast)
	require.NoError(t, s.Cancel("exec", c.ID, "bob", false, now))
	require.Equal(t, "cancelled", status(t, s, c.ID))
}

func TestRepeatingBookings(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	// Someone already holds Oct 16, the third Friday.
	blocker := submit(t, s, "blocker", "bob", testDraft("2026-10-16", 17, 18))[0]
	d := testDraft("2026-10-02", 17, 19)
	d.Repeat, d.Until = "weekly", time.Date(2026, 10, 30, 0, 0, 0, 0, BookingLocation)
	slots, err := s.Plan("exec", d, now)
	require.NoError(t, err)
	require.Len(t, slots, 5)
	require.True(t, slots[2].Taken)
	created, skipped, err := s.Submit("series", "exec", "stuti", "Stuti", nil, d, now)
	require.NoError(t, err)
	require.Equal(t, 1, skipped)
	require.Len(t, created, 4)
	lead := created[0].ID
	for _, b := range created {
		require.Equal(t, lead, b.Lead())
	}
	// One approval covers every pending date.
	require.NoError(t, s.Decide("exec", lead, "officer", officer, true, "", now))
	approved, err := s.Repo.Approved("exec", now, now.AddDate(0, 1, 0))
	require.NoError(t, err)
	require.Len(t, approved, 4)
	// Cancel one date, then revoke the rest from the third date.
	require.NoError(t, s.Cancel("exec", created[1].ID, "stuti", false, now))
	require.NoError(t, s.Revoke("exec", created[2].ID, "officer", officer, true, "", now))
	statuses := []string{}
	for _, b := range created {
		statuses = append(statuses, status(t, s, b.ID))
	}
	require.Equal(t, []string{"approved", "cancelled", "revoked", "revoked"}, statuses)
	require.Equal(t, "pending", status(t, s, blocker.ID))

	biweekly := testDraft("2026-10-05", 10, 11)
	biweekly.Repeat, biweekly.Until = "biweekly", time.Date(2026, 11, 2, 0, 0, 0, 0, BookingLocation)
	slots, err = s.Plan("exec", biweekly, now)
	require.NoError(t, err)
	require.Len(t, slots, 3)
	tooLong := testDraft("2026-10-05", 10, 11)
	tooLong.Repeat, tooLong.Until = "weekly", time.Date(2027, 6, 1, 0, 0, 0, 0, BookingLocation)
	_, err = s.Plan("exec", tooLong, now)
	require.Error(t, err)
}

func TestTimeChanges(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	original := submit(t, s, "original", "alice", testDraft("2026-10-02", 14, 16))[0]
	require.NoError(t, s.Decide("exec", original.ID, "officer", officer, true, "", now))

	// Overlapping its own original slot is allowed; the original stays booked.
	change := testDraft("2026-10-02", 15, 17)
	change.Replaces = original.ID
	_, _, err := s.Submit("change-by-bob", "exec", "bob", "Bob", nil, change, now)
	require.ErrorIs(t, err, repository.ErrBookingOwner)
	moved := submit(t, s, "change-1", "alice", change)[0]
	require.Equal(t, "approved", status(t, s, original.ID))
	change2 := change
	_, _, err = s.Submit("change-2", "exec", "alice", "Alice", nil, change2, now)
	require.ErrorIs(t, err, repository.ErrChangePending)

	// Declining keeps the original.
	require.NoError(t, s.Decline(moved.ID, now))
	require.Equal(t, "approved", status(t, s, original.ID))

	// Approving replaces it.
	moved = submit(t, s, "change-3", "alice", change)[0]
	require.NoError(t, s.Decide("exec", moved.ID, "officer", officer, true, "", now))
	require.Equal(t, "replaced", status(t, s, original.ID))
	require.Equal(t, "approved", status(t, s, moved.ID))

	// A still-pending request is replaced immediately.
	pending := submit(t, s, "pending", "carol", testDraft("2026-10-03", 9, 10))[0]
	update := testDraft("2026-10-03", 11, 12)
	update.Replaces = pending.ID
	updated := submit(t, s, "update", "carol", update)[0]
	require.Equal(t, "replaced", status(t, s, pending.ID))
	require.Equal(t, "pending", status(t, s, updated.ID))

	// Cancelling a booking withdraws its waiting change.
	another := submit(t, s, "another", "dan", testDraft("2026-10-04", 9, 10))[0]
	require.NoError(t, s.Decide("exec", another.ID, "officer", officer, true, "", now))
	waiting := testDraft("2026-10-04", 12, 13)
	waiting.Replaces = another.ID
	waitingChange := submit(t, s, "waiting", "dan", waiting)[0]
	require.NoError(t, s.Cancel("exec", another.ID, "dan", false, now))
	require.Equal(t, "cancelled", status(t, s, waitingChange.ID))
}

func TestExpiryAndReminders(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	b := submit(t, s, "soon", "alice", testDraft("2026-10-01", 10, 11))[0]
	later := submit(t, s, "later", "alice", testDraft("2026-10-02", 10, 11))[0]
	// Too fresh for a reminder.
	changed, err := s.Maintain("exec", now)
	require.NoError(t, err)
	require.False(t, changed)
	countEvents := func(event string) int {
		var n int
		require.NoError(t, s.Repo.DB.QueryRow(`SELECT COUNT(*) FROM booking_notifications WHERE event=?`, event).Scan(&n))
		return n
	}
	require.Equal(t, 0, countEvents("reminder"))
	_, err = s.Maintain("exec", now.Add(31*time.Minute))
	require.NoError(t, err)
	_, err = s.Maintain("exec", now.Add(40*time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, countEvents("reminder"), "one reminder per request")
	require.ErrorIs(t, s.Decide("exec", b.ID, "officer", officer, true, "", b.Start), repository.ErrBookingPast)
	changed, err = s.Maintain("exec", b.Start)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "expired", status(t, s, b.ID))
	require.Equal(t, "pending", status(t, s, later.ID))
	require.Equal(t, 1, countEvents("expired"))
	// The expired slot is free again.
	_, err = s.Plan("exec", testDraft("2026-10-02", 10, 11), now)
	require.NoError(t, err)
}

func TestConcurrentRequestsForOneSlot(t *testing.T) {
	s := bookingTestService(t)
	start := make(chan struct{})
	results := make(chan error, 6)
	var wg sync.WaitGroup
	for n := 0; n < 6; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			_, _, err := s.Submit(fmt.Sprintf("race-%d", n), "exec", fmt.Sprintf("user-%d", n), "User", nil, testDraft("2026-10-02", 14, 16), bookingTestNow())
			results <- err
		}(n)
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else {
			require.True(t, errors.Is(err, repository.ErrBookingConflict), "unexpected error: %v", err)
		}
	}
	require.Equal(t, 1, success)
}

func TestBookingValidation(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	for name, d := range map[string]Draft{
		"past":        testDraft("2026-09-30", 14, 15),
		"too short":   {Title: "x", Day: testDraft("2026-10-02", 0, 0).Day, StartMin: 14 * 60, EndMin: 14*60 + 10},
		"too far":     testDraft("2028-10-02", 14, 15),
		"spring gap":  {Title: "x", Day: testDraft("2027-03-14", 0, 0).Day, StartMin: 150, EndMin: 240},
		"fall repeat": {Title: "x", Day: testDraft("2026-11-01", 0, 0).Day, StartMin: 90, EndMin: 180},
		"no purpose":  {Day: testDraft("2026-10-02", 0, 0).Day, StartMin: 600, EndMin: 660},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.Plan("exec", d, now)
			require.Error(t, err)
		})
	}
	// An end at or before the start runs past midnight.
	overnight := submit(t, s, "overnight", "events", testDraft("2026-10-02", 22, 2))[0]
	require.Equal(t, "2026-10-03 02:00", overnight.End.In(BookingLocation).Format("2006-01-02 15:04"))
	midnight := submit(t, s, "midnight", "events", Draft{Title: "Late", Day: testDraft("2026-10-05", 0, 0).Day, StartMin: 23 * 60, EndMin: 24 * 60})[0]
	require.Equal(t, "2026-10-06 00:00", midnight.End.In(BookingLocation).Format("2006-01-02 15:04"))
	week := BookingWeek(time.Date(2026, 11, 1, 12, 0, 0, 0, BookingLocation))
	require.Equal(t, "2026-10-26", week.Format("2006-01-02"))
	require.Equal(t, 169*time.Hour, week.AddDate(0, 0, 7).Sub(week))
}

func TestParseBookingDateAndClock(t *testing.T) {
	now := bookingTestNow() // Thursday Oct 1 2026
	for text, want := range map[string]string{
		"2026-11-20": "2026-11-20", "today": "2026-10-01", "Tomorrow": "2026-10-02", "fri": "2026-10-02",
		"friday": "2026-10-02", "thu": "2026-10-01", "next thu": "2026-10-08", "nov 20": "2026-11-20",
		"20 Nov": "2026-11-20", "Sep 3": "2027-09-03", "dec 3rd": "2026-12-03", "jan 5 2027": "2027-01-05",
	} {
		got, err := ParseBookingDate(text, now)
		require.NoError(t, err, text)
		require.Equal(t, want, got.Format("2006-01-02"), text)
	}
	for _, bad := range []string{"", "someday", "feb 30", "13/45"} {
		_, err := ParseBookingDate(bad, now)
		require.Error(t, err, bad)
	}
	for text, want := range map[string]int{
		"14:00": 840, "1400": 840, "14": 840, "2pm": 840, "2:30 PM": 870, "12am": 0, "12pm": 720, "noon": 720,
		"midnight": 1440, "9": 540, "9:05": 545, "11 p.m.": 1380,
	} {
		got, err := ParseBookingClock(text)
		require.NoError(t, err, text)
		require.Equal(t, want, got, text)
	}
	for _, bad := range []string{"", "25:00", "13pm", "2:75", "abc"} {
		_, err := ParseBookingClock(bad)
		require.Error(t, err, bad)
	}
	summer, err := BookingAt(time.Date(2026, 10, 2, 0, 0, 0, 0, BookingLocation), 14*60)
	require.NoError(t, err)
	winter, err := BookingAt(time.Date(2027, 1, 2, 0, 0, 0, 0, BookingLocation), 14*60)
	require.NoError(t, err)
	require.Equal(t, 18, summer.UTC().Hour())
	require.Equal(t, 19, winter.UTC().Hour())
	start, _ := BookingAt(time.Date(2026, 11, 19, 0, 0, 0, 0, BookingLocation), 14*60)
	require.Equal(t, "Thu, Nov 19 · 2 PM – 4:30 PM", FormatRange(start, start.Add(150*time.Minute)))
	require.Equal(t, "Thu, Nov 19 · 2 PM – midnight", FormatRange(start, start.Add(10*time.Hour)))
}

func TestMigrationFromOriginalBookingSchema(t *testing.T) {
	db := database.Open(filepath.Join(t.TempDir(), "old.db"))
	t.Cleanup(func() { db.Close() })
	// The schema shipped before this version, with its triggers and data.
	_, err := db.Exec(`
CREATE TABLE bookings (id INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT NOT NULL UNIQUE, guild_id TEXT NOT NULL, user_id TEXT NOT NULL,
 display_name TEXT NOT NULL, title TEXT NOT NULL, starts_at INTEGER NOT NULL, ends_at INTEGER NOT NULL CHECK (ends_at > starts_at),
 status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','declined','cancelled')),
 reviewed_by TEXT NOT NULL DEFAULT '', reviewed_at INTEGER, created_at INTEGER NOT NULL);
CREATE TABLE booking_notifications (id INTEGER PRIMARY KEY AUTOINCREMENT, booking_id INTEGER NOT NULL,
 event TEXT NOT NULL CHECK(event IN ('pending','approved','declined','cancelled')), actor_id TEXT NOT NULL,
 channel_id TEXT NOT NULL DEFAULT '', message_id TEXT NOT NULL DEFAULT '', delivered_at INTEGER,
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at INTEGER NOT NULL DEFAULT 0, UNIQUE(booking_id,event));
CREATE TRIGGER booking_request_notification AFTER INSERT ON bookings BEGIN
 INSERT OR IGNORE INTO booking_notifications(booking_id,event,actor_id) VALUES(NEW.id,'pending',NEW.user_id); END;
INSERT INTO bookings(request_id,guild_id,user_id,display_name,title,starts_at,ends_at,status,reviewed_by,created_at)
 VALUES('old','exec','stuti','Stuti','wie meetings',1790000000,1790007200,'approved','samuel',1780000000);
INSERT INTO bookings(request_id,guild_id,user_id,display_name,title,starts_at,ends_at,created_at)
 VALUES('stale','exec','sam','Sam','old request',1700000000,1700003600,1690000000);
UPDATE booking_notifications SET message_id='card-message',channel_id='requests';`)
	require.NoError(t, err)
	require.NoError(t, database.Migrate(db))
	require.NoError(t, database.Migrate(db))
	s := &BookingService{Repo: &repository.BookingRepo{DB: db}}
	b, err := s.Repo.Get("exec", 1)
	require.NoError(t, err)
	require.Equal(t, "wie meetings", b.Title)
	require.Equal(t, "approved", b.Status)
	require.Equal(t, int64(0), b.SeriesID)
	channel, message, err := s.Repo.RequestNotificationMessage(1)
	require.NoError(t, err)
	require.Equal(t, [2]string{"requests", "card-message"}, [2]string{channel, message})
	// New statuses are allowed, and the old trigger no longer writes events.
	require.NoError(t, s.Repo.Revoke("exec", 1, "officer", false, "", time.Unix(1789990000, 0)))
	stale, err := s.Repo.Get("exec", 2)
	require.NoError(t, err)
	require.Equal(t, "expired", stale.Status, "old undecided requests are closed quietly")
	var triggers int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger'`).Scan(&triggers))
	require.Equal(t, 0, triggers)
}
