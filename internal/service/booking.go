package service

import (
	"errors"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"

	"office/internal/repository"
)

const SigningOfficerRoleID = "1103393189202763855"

var ErrBookingPermission = errors.New("only members with the Signing Officer role can review booking requests")

type BookingInputError string

func (e BookingInputError) Error() string { return string(e) }

func BookingErrorMessage(err error) string {
	var input BookingInputError
	if errors.As(err, &input) {
		return input.Error()
	}
	for _, known := range []error{ErrBookingPermission, repository.ErrBookingConflict, repository.ErrBookingPast, repository.ErrBookingState} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "Couldn't save the change. Please try again."
}

var BookingLocation = func() *time.Location {
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		panic(err)
	}
	return loc
}()

type BookingService struct{ Repo *repository.BookingRepo }

func IsSigningOfficer(roles []string) bool {
	for _, role := range roles {
		if role == SigningOfficerRoleID {
			return true
		}
	}
	return false
}

func BookingWeek(t time.Time) time.Time {
	t = t.In(BookingLocation)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, BookingLocation)
	return day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
}

// ParseBookingTime rejects missing and repeated wall times at DST transitions
// instead of silently assigning a different instant than the user entered.
func ParseBookingTime(date, clock string) (time.Time, error) {
	input := strings.TrimSpace(date) + " " + strings.TrimSpace(clock)
	t, err := time.ParseInLocation("2006-01-02 15:04", input, BookingLocation)
	if err != nil || t.Format("2006-01-02 15:04") != input {
		return time.Time{}, BookingInputError("use a valid date and time: YYYY-MM-DD and HH:MM (24-hour)")
	}
	for _, delta := range []time.Duration{-time.Hour, time.Hour} {
		if t.Add(delta).Format("2006-01-02 15:04") == input {
			return time.Time{}, BookingInputError("that local time occurs twice when clocks change; choose a time outside the repeated hour")
		}
	}
	return t, nil
}

func CleanBookingText(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)), " ")
}

func (s *BookingService) Request(requestID, guild, user, name, title, date, startClock, endClock string, now time.Time) (*repository.Booking, error) {
	if requestID == "" || guild == "" || user == "" {
		return nil, BookingInputError("unable to identify the booking request")
	}
	title = CleanBookingText(title)
	name = CleanBookingText(name)
	if title == "" || utf8.RuneCountInString(title) > 80 {
		return nil, BookingInputError("enter a booking purpose between 1 and 80 characters")
	}
	if name == "" {
		name = user
	}
	start, err := ParseBookingTime(date, startClock)
	if err != nil {
		return nil, err
	}
	var end time.Time
	if strings.TrimSpace(endClock) == "24:00" {
		end = time.Date(start.Year(), start.Month(), start.Day()+1, 0, 0, 0, 0, BookingLocation)
	} else {
		end, err = ParseBookingTime(date, endClock)
		if err != nil {
			return nil, err
		}
	}
	if !start.After(now) {
		return nil, BookingInputError("choose a start time in the future")
	}
	if start.After(now.AddDate(1, 0, 0)) {
		return nil, BookingInputError("bookings can be requested up to one year ahead")
	}
	if end.Sub(start) < 15*time.Minute {
		return nil, BookingInputError("the end must be at least 15 minutes after the start, on the same date (use 24:00 for midnight)")
	}
	conflicts, err := s.Repo.Approved(guild, start, end)
	if err != nil {
		return nil, err
	}
	if len(conflicts) > 0 {
		return nil, repository.ErrBookingConflict
	}
	return s.Repo.Create(requestID, repository.Booking{GuildID: guild, UserID: user, Name: name, Title: title, Start: start, End: end}, now)
}

func (s *BookingService) Decide(guild string, id int64, officer string, roles []string, approve bool, now time.Time) error {
	if !IsSigningOfficer(roles) || officer == "" {
		return ErrBookingPermission
	}
	return s.Repo.Decide(guild, id, officer, approve, now)
}

func (s *BookingService) Cancel(guild string, id int64, user string, now time.Time) error {
	b, err := s.Repo.Get(guild, id)
	if err != nil {
		return err
	}
	if b.UserID != user {
		return BookingInputError("you can only cancel your own bookings")
	}
	if !b.Start.After(now) {
		return repository.ErrBookingPast
	}
	return s.Repo.Cancel(guild, id, user, now)
}
