package service

import (
	"database/sql"
	"errors"
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

func bookingTestNow() time.Time { return time.Date(2026, 10, 1, 9, 0, 0, 0, BookingLocation) }

func requestTestBooking(t *testing.T, s *BookingService, request, user, start, end string) *repository.Booking {
	t.Helper()
	b, err := s.Request(request, "exec", user, "Alice Example", "Committee planning", "2026-10-02", start, end, bookingTestNow())
	require.NoError(t, err)
	return b
}

func TestBookingApprovalPermissionsAndConflicts(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	roles := []string{SigningOfficerRoleID}
	a := requestTestBooking(t, s, "first", "alice", "14:00", "16:00")
	b := requestTestBooking(t, s, "second", "bob", "15:00", "17:00")
	for _, badRoles := range [][]string{nil, {"administrator"}, {"wrong-role"}} {
		require.ErrorIs(t, s.Decide("exec", a.ID, "officer", badRoles, true, now), ErrBookingPermission)
		require.ErrorIs(t, s.Decide("exec", a.ID, "officer", badRoles, false, now), ErrBookingPermission)
	}
	require.ErrorIs(t, s.Decide("other-guild", a.ID, "officer", roles, true, now), sql.ErrNoRows)
	require.NoError(t, s.Decide("exec", a.ID, "officer", roles, true, now))
	require.ErrorIs(t, s.Decide("exec", b.ID, "officer", roles, true, now), repository.ErrBookingConflict)
	require.ErrorIs(t, s.Decide("exec", a.ID, "officer", roles, false, now), repository.ErrBookingState)
	require.NoError(t, s.Decide("exec", b.ID, "officer", roles, false, now))
	approved, err := s.Repo.Get("exec", a.ID)
	require.NoError(t, err)
	require.Equal(t, "approved", approved.Status)
	require.Equal(t, "officer", approved.ReviewedBy)
	// Adjacent reservations do not overlap.
	c := requestTestBooking(t, s, "third", "charlie", "16:00", "17:00")
	require.NoError(t, s.Decide("exec", c.ID, "officer", roles, true, now))
}

func TestBookingCancellationAndReplay(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	a := requestTestBooking(t, s, "same-interaction", "alice", "14:00", "16:00")
	replay := requestTestBooking(t, s, "same-interaction", "alice", "14:00", "16:00")
	require.Equal(t, a.ID, replay.ID)
	require.Error(t, s.Cancel("exec", a.ID, "bob", now))
	require.ErrorIs(t, s.Cancel("different-guild", a.ID, "alice", now), sql.ErrNoRows)
	require.NoError(t, s.Decide("exec", a.ID, "officer", []string{SigningOfficerRoleID}, true, now))
	_, err := s.Request("overlap", "exec", "bob", "Bob", "Meeting", "2026-10-02", "15:00", "16:00", now)
	require.ErrorIs(t, err, repository.ErrBookingConflict)
	require.NoError(t, s.Cancel("exec", a.ID, "alice", now))
	b := requestTestBooking(t, s, "available-again", "bob", "14:00", "16:00")
	require.NoError(t, s.Decide("exec", b.ID, "officer", []string{SigningOfficerRoleID}, true, now))
	require.ErrorIs(t, s.Cancel("exec", b.ID, "bob", b.Start), repository.ErrBookingPast)
}

func TestBookingDatesAndTimezones(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	for _, tc := range []struct{ name, date, start, end string }{
		{"invalid date", "2026-02-30", "14:00", "15:00"},
		{"past", "2026-09-30", "14:00", "15:00"},
		{"reversed", "2026-10-02", "15:00", "14:00"},
		{"too short", "2026-10-02", "14:00", "14:10"},
		{"too far", "2028-10-02", "14:00", "15:00"},
		{"spring gap", "2027-03-14", "02:30", "04:00"},
		{"fall repeated hour", "2026-11-01", "01:30", "03:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Request(tc.name, "exec", "u", "Name", "Purpose", tc.date, tc.start, tc.end, now)
			require.Error(t, err)
		})
	}
	b := requestTestBooking(t, s, "midnight", "alice", "23:00", "24:00")
	require.Equal(t, "2026-10-03 00:00", b.End.In(BookingLocation).Format("2006-01-02 15:04"))
	summer, err := ParseBookingTime("2026-10-02", "14:00")
	require.NoError(t, err)
	winter, err := ParseBookingTime("2027-01-02", "14:00")
	require.NoError(t, err)
	require.Equal(t, 18, summer.UTC().Hour())
	require.Equal(t, 19, winter.UTC().Hour())
	week := BookingWeek(time.Date(2026, 11, 1, 12, 0, 0, 0, BookingLocation))
	require.Equal(t, "2026-10-26", week.Format("2006-01-02"))
	require.Equal(t, 169*time.Hour, week.AddDate(0, 0, 7).Sub(week))
}

func TestBookingConcurrentApproval(t *testing.T) {
	s := bookingTestService(t)
	now := bookingTestNow()
	a := requestTestBooking(t, s, "race-a", "alice", "14:00", "16:00")
	b := requestTestBooking(t, s, "race-b", "bob", "15:00", "17:00")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []int64{a.ID, b.ID} {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			<-start
			results <- s.Decide("exec", id, "officer", []string{SigningOfficerRoleID}, true, now)
		}(id)
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
	bookings, err := s.Repo.Approved("exec", a.Start, b.End)
	require.NoError(t, err)
	require.Len(t, bookings, 1)
}

func TestBookingExpiredAndSavedCalendar(t *testing.T) {
	s := bookingTestService(t)
	b := requestTestBooking(t, s, "expires", "alice", "14:00", "16:00")
	require.ErrorIs(t, s.Decide("exec", b.ID, "officer", []string{SigningOfficerRoleID}, true, b.Start), repository.ErrBookingPast)
	pending, err := s.Repo.Pending("exec", b.Start)
	require.NoError(t, err)
	require.Empty(t, pending)
	board := repository.BookingCalendar{GuildID: "exec", ChannelID: "channel", MessageID: "message", Week: "2026-09-28"}
	require.NoError(t, s.Repo.SaveCalendar(board))
	board.Week = "2026-10-05"
	require.NoError(t, s.Repo.SaveCalendar(board))
	boards, err := s.Repo.Calendars("exec")
	require.NoError(t, err)
	require.Equal(t, []repository.BookingCalendar{board}, boards)
	other, err := s.Repo.Calendars("other")
	require.NoError(t, err)
	require.Empty(t, other)
}
