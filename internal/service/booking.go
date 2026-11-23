package service

import (
	"errors"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"

	"office/internal/repository"
)

// SigningOfficerRoleID approves bookings until /office-admin assigns approver roles.
const SigningOfficerRoleID = "1103393189202763855"

// DefaultOfficerChannelID receives request cards until /office-admin sets another.
const DefaultOfficerChannelID = "1555060913147486269"

const (
	CapabilityBook    = "book"
	CapabilityApprove = "approve"

	SettingOfficerChannel  = "officer_channel"
	SettingCalendarChannel = "calendar_channel"

	MaxSeriesLength = 26
)

var (
	ErrBookingPermission = errors.New("only booking approvers can review office requests")
	ErrBookPermission    = errors.New("you don't have a role that can book the office")
)

type BookingInputError string

func (e BookingInputError) Error() string { return string(e) }

func BookingErrorMessage(err error) string {
	var input BookingInputError
	if errors.As(err, &input) {
		return capitalize(input.Error())
	}
	for _, known := range []error{ErrBookingPermission, ErrBookPermission, repository.ErrBookingConflict, repository.ErrBookingPast,
		repository.ErrBookingState, repository.ErrBookingOwner, repository.ErrSelfApproval, repository.ErrChangePending} {
		if errors.Is(err, known) {
			return capitalize(known.Error()) + "."
		}
	}
	return "Couldn't save the change. Please try again."
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

var BookingLocation = func() *time.Location {
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		panic(err)
	}
	return loc
}()

type BookingService struct{ Repo *repository.BookingRepo }

// Access is what a member may do with office bookings.
type Access struct{ Book, Approve bool }

// Access resolves a member's capabilities from their roles. Until roles are
// assigned for a capability, anyone in the server may book and the Signing
// Officer role approves.
func (s *BookingService) Access(guild string, roles []string) (Access, error) {
	mapping, err := s.Repo.RoleCapabilities(guild)
	if err != nil {
		return Access{}, err
	}
	has := func(capability string, fallback []string) bool {
		allowed := mapping[capability]
		if len(allowed) == 0 {
			if fallback == nil {
				return true
			}
			allowed = fallback
		}
		for _, role := range roles {
			if slices.Contains(allowed, role) {
				return true
			}
		}
		return false
	}
	return Access{Book: has(CapabilityBook, nil), Approve: has(CapabilityApprove, []string{SigningOfficerRoleID})}, nil
}

// UserGrantPrefix marks a capability granted to one member instead of a role
// (stored as "user:<discord user id>").
const UserGrantPrefix = "user:"

// MemberGrants is what a member's capabilities are matched against: their
// roles plus their own user grant.
func MemberGrants(userID string, roles []string) []string {
	return append(slices.Clone(roles), UserGrantPrefix+userID)
}

// ApproverRoles lists the roles pinged for new requests (user grants are not pinged).
func (s *BookingService) ApproverRoles(guild string) []string {
	mapping, err := s.Repo.RoleCapabilities(guild)
	if err != nil || len(mapping[CapabilityApprove]) == 0 {
		return []string{SigningOfficerRoleID}
	}
	roles := []string{}
	for _, id := range mapping[CapabilityApprove] {
		if !strings.HasPrefix(id, UserGrantPrefix) {
			roles = append(roles, id)
		}
	}
	return roles
}

// Setting returns a stored setting or its default.
func (s *BookingService) Setting(guild, key, fallback string) string {
	value, err := s.Repo.Setting(guild, key)
	if err != nil || value == "" {
		return fallback
	}
	return value
}

func BookingWeek(t time.Time) time.Time {
	day := BookingDay(t)
	return day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
}

func CleanBookingText(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)), " ")
}

// Draft is a booking request being filled in.
type Draft struct {
	Title            string
	Day              time.Time // local midnight
	StartMin, EndMin int       // minutes after midnight; an end <= start is the next day
	Repeat           string    // "", "weekly" or "biweekly"
	Until            time.Time // last date for a repeat
	Replaces         int64     // booking whose time is being changed
}

// Slot is one occurrence of a draft and whether it is free.
type Slot struct {
	Start, End time.Time
	Taken      bool
}

func RepeatStep(repeat string) int {
	switch repeat {
	case "weekly":
		return 7
	case "biweekly":
		return 14
	}
	return 0
}

// Plan validates a draft and lists its occurrences, marking those already held.
func (s *BookingService) Plan(guild string, d Draft, now time.Time) ([]Slot, error) {
	title := CleanBookingText(d.Title)
	if title == "" || utf8.RuneCountInString(title) > 80 {
		return nil, BookingInputError("enter a purpose between 1 and 80 characters")
	}
	start, end, err := BookingRange(d.Day, d.StartMin, d.EndMin)
	if err != nil {
		return nil, err
	}
	if !start.After(now) {
		return nil, BookingInputError("choose a start time in the future")
	}
	if start.After(now.AddDate(1, 0, 0)) {
		return nil, BookingInputError("bookings can start up to one year ahead")
	}
	if end.Sub(start) < 15*time.Minute {
		return nil, BookingInputError("bookings must be at least 15 minutes long")
	}
	days := []time.Time{BookingDay(d.Day)}
	if step := RepeatStep(d.Repeat); step > 0 {
		if d.Replaces > 0 {
			return nil, BookingInputError("a time change can't repeat")
		}
		until := BookingDay(d.Until)
		if until.Before(days[0].AddDate(0, 0, step)) {
			return nil, BookingInputError("choose a repeat end date at least one repeat after the first date")
		}
		for day := days[0].AddDate(0, 0, step); !day.After(until); day = day.AddDate(0, 0, step) {
			days = append(days, day)
		}
		if len(days) > MaxSeriesLength {
			return nil, BookingInputError("a repeating booking can have at most 26 dates")
		}
		if days[len(days)-1].After(now.AddDate(1, 0, 0)) {
			return nil, BookingInputError("repeating bookings must end within one year")
		}
	} else if d.Repeat != "" {
		return nil, BookingInputError("repeat must be weekly or every 2 weeks")
	}
	slots := make([]Slot, 0, len(days))
	for _, day := range days {
		a, z, err := BookingRange(day, d.StartMin, d.EndMin)
		if err != nil {
			return nil, BookingInputError(FormatDay(day) + ": " + err.Error())
		}
		busy, err := s.Repo.Busy(guild, a, z)
		if err != nil {
			return nil, err
		}
		taken := false
		for _, b := range busy {
			if b.ID != d.Replaces {
				taken = true
			}
		}
		slots = append(slots, Slot{Start: a, End: z, Taken: taken})
	}
	return slots, nil
}

// Submit creates the request for a draft, skipping occurrences that are taken.
func (s *BookingService) Submit(requestID, guild, user, name string, roles []string, d Draft, now time.Time) ([]repository.Booking, int, error) {
	if requestID == "" || guild == "" || user == "" {
		return nil, 0, BookingInputError("unable to identify the booking request")
	}
	access, err := s.Access(guild, roles)
	if err != nil {
		return nil, 0, err
	}
	if !access.Book {
		return nil, 0, ErrBookPermission
	}
	slots, err := s.Plan(guild, d, now)
	if err != nil {
		return nil, 0, err
	}
	if name = CleanBookingText(name); name == "" {
		name = user
	}
	items := make([]repository.Booking, 0, len(slots))
	for _, slot := range slots {
		items = append(items, repository.Booking{GuildID: guild, UserID: user, Name: name, Title: CleanBookingText(d.Title), Start: slot.Start, End: slot.End})
	}
	return s.Repo.Create(requestID, items, d.Replaces, now)
}

func (s *BookingService) requireApprover(guild string, roles []string) error {
	access, err := s.Access(guild, roles)
	if err != nil {
		return err
	}
	if !access.Approve {
		return ErrBookingPermission
	}
	return nil
}

// Decide approves or declines a request (all pending dates of a series).
func (s *BookingService) Decide(guild string, id int64, officer string, roles []string, approve bool, reason string, now time.Time) error {
	if officer == "" {
		return ErrBookingPermission
	}
	if err := s.requireApprover(guild, roles); err != nil {
		return err
	}
	return s.Repo.Decide(guild, id, officer, approve, cleanReason(reason), now)
}

// DecideDates approves the listed dates of a request and declines its other
// pending dates (an empty list declines the whole request).
func (s *BookingService) DecideDates(guild string, id int64, officer string, roles []string, approveIDs []int64, reason string, now time.Time) error {
	if officer == "" {
		return ErrBookingPermission
	}
	if err := s.requireApprover(guild, roles); err != nil {
		return err
	}
	return s.Repo.DecideSelected(guild, id, officer, approveIDs, cleanReason(reason), now)
}

// Cancel cancels a member's own booking, or it and the rest of its series.
func (s *BookingService) Cancel(guild string, id int64, user string, rest bool, now time.Time) error {
	return s.Repo.Cancel(guild, id, user, rest, now)
}

// Revoke withdraws an approved booking, or it and the rest of its series.
func (s *BookingService) Revoke(guild string, id int64, officer string, roles []string, rest bool, reason string, now time.Time) error {
	if err := s.requireApprover(guild, roles); err != nil {
		return err
	}
	return s.Repo.Revoke(guild, id, officer, rest, cleanReason(reason), now)
}

// Maintain expires undecided requests and queues short-notice reminders.
// It reports whether any booking changed.
func (s *BookingService) Maintain(guild string, now time.Time) (bool, error) {
	expired, err := s.Repo.Expire(guild, now)
	if err != nil {
		return false, err
	}
	return expired > 0, s.Repo.QueueReminders(guild, now, 2*time.Hour, 30*time.Minute)
}

func cleanReason(reason string) string {
	reason = CleanBookingText(reason)
	if utf8.RuneCountInString(reason) > 300 {
		reason = string([]rune(reason)[:300])
	}
	return reason
}
