package service

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "tues": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

var months = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September, "sept": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
}

// BookingDay returns local midnight of t's date in Ottawa.
func BookingDay(t time.Time) time.Time {
	t = t.In(BookingLocation)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, BookingLocation)
}

// ParseBookingDate accepts 2026-11-20, today, tomorrow, weekday names
// (the next one, including today) and "nov 20" / "20 nov" (the next one).
func ParseBookingDate(text string, now time.Time) (time.Time, error) {
	s := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(text, ",", " ")))
	today := BookingDay(now)
	if d, err := time.ParseInLocation("2006-01-02", s, BookingLocation); err == nil {
		return d, nil
	}
	switch s {
	case "today", "tod":
		return today, nil
	case "tomorrow", "tmr", "tmrw", "tom":
		return today.AddDate(0, 0, 1), nil
	}
	fields := strings.Fields(s)
	if len(fields) == 1 || (len(fields) == 2 && fields[0] == "next") {
		word := fields[len(fields)-1]
		for name, day := range weekdays {
			if strings.HasPrefix(word, name) && len(word) <= len(name)+6 {
				offset := (int(day) - int(today.Weekday()) + 7) % 7
				if len(fields) == 2 && offset == 0 {
					offset = 7
				}
				return today.AddDate(0, 0, offset), nil
			}
		}
	}
	if len(fields) == 2 || len(fields) == 3 {
		monthWord, dayWord := fields[0], fields[1]
		if _, err := strconv.Atoi(monthWord); err == nil {
			monthWord, dayWord = dayWord, monthWord
		}
		day, err := strconv.Atoi(strings.TrimRight(dayWord, "stndrh"))
		if month, ok := monthPrefix(monthWord); ok && err == nil {
			year := today.Year()
			if len(fields) == 3 {
				if year, err = strconv.Atoi(fields[2]); err != nil {
					return time.Time{}, bookingDateError
				}
			}
			d := time.Date(year, month, day, 0, 0, 0, 0, BookingLocation)
			if d.Day() != day {
				return time.Time{}, bookingDateError
			}
			if len(fields) == 2 && d.Before(today) {
				d = d.AddDate(1, 0, 0)
			}
			return d, nil
		}
	}
	return time.Time{}, bookingDateError
}

var bookingDateError = BookingInputError("enter a date like \"tomorrow\", \"fri\", \"nov 20\" or 2026-11-20")

func monthPrefix(word string) (time.Month, bool) {
	for name, month := range months {
		if strings.HasPrefix(word, name) {
			return month, true
		}
	}
	return 0, false
}

var clockPattern = regexp.MustCompile(`^(\d{1,2})(?::?(\d{2}))?\s*(am|pm|a|p)?$`)

// ParseBookingClock returns minutes after midnight for 14:00, 1400, 14, 2pm,
// 2:30 pm, noon or midnight (1440, the end of the day).
func ParseBookingClock(text string) (int, error) {
	s := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(text, ".", "")))
	switch s {
	case "noon":
		return 12 * 60, nil
	case "midnight", "24:00", "2400":
		return 24 * 60, nil
	}
	m := clockPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, bookingClockError
	}
	hour, _ := strconv.Atoi(m[1])
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	if suffix := m[3]; suffix != "" {
		if hour < 1 || hour > 12 {
			return 0, bookingClockError
		}
		hour %= 12
		if suffix[0] == 'p' {
			hour += 12
		}
	}
	if hour > 23 || minute > 59 {
		return 0, bookingClockError
	}
	return hour*60 + minute, nil
}

var bookingClockError = BookingInputError("enter a time like 14:00 or 2pm")

// BookingAt returns the instant for a local date and minute of day, rejecting
// wall times that do not exist or happen twice when clocks change.
func BookingAt(day time.Time, minute int) (time.Time, error) {
	day = BookingDay(day)
	if minute == 24*60 {
		return day.AddDate(0, 0, 1), nil
	}
	t := time.Date(day.Year(), day.Month(), day.Day(), minute/60, minute%60, 0, 0, BookingLocation)
	if t.Hour()*60+t.Minute() != minute || BookingDay(t) != day {
		return time.Time{}, BookingInputError("that time is skipped when clocks change; choose another time")
	}
	for _, delta := range []time.Duration{-time.Hour, time.Hour} {
		other := t.Add(delta)
		if other.Hour() == t.Hour() && other.Minute() == t.Minute() && BookingDay(other) == day {
			return time.Time{}, BookingInputError("that time happens twice when clocks change; choose a time outside the repeated hour")
		}
	}
	return t, nil
}

// BookingRange builds a booking interval; an end at or before the start means
// the next day (e.g. 22:00–02:00).
func BookingRange(day time.Time, startMinute, endMinute int) (time.Time, time.Time, error) {
	if startMinute >= 24*60 {
		return time.Time{}, time.Time{}, BookingInputError("start the booking before midnight")
	}
	start, err := BookingAt(day, startMinute)
	if err != nil {
		return start, start, err
	}
	endDay := BookingDay(day)
	if endMinute <= startMinute {
		endDay = endDay.AddDate(0, 0, 1)
	}
	end, err := BookingAt(endDay, endMinute)
	return start, end, err
}

// FormatClock renders 14:30 as "2:30 PM".
func FormatClock(t time.Time) string {
	t = t.In(BookingLocation)
	if t.Minute() == 0 {
		return t.Format("3 PM")
	}
	return t.Format("3:04 PM")
}

// FormatDay renders "Thu, Nov 19".
func FormatDay(t time.Time) string { return t.In(BookingLocation).Format("Mon, Jan 2") }

// FormatRange renders "Thu, Nov 19 · 2 PM – 4 PM" (with the end day when it differs).
func FormatRange(start, end time.Time) string {
	endText := FormatClock(end)
	if BookingDay(end) != BookingDay(start) {
		if end.Equal(BookingDay(start).AddDate(0, 0, 1)) {
			endText = "midnight"
		} else {
			endText = FormatDay(end) + " " + endText
		}
	}
	return FormatDay(start) + " · " + FormatClock(start) + " – " + endText
}

// FormatDuration renders 90 minutes as "1h 30m".
func FormatDuration(d time.Duration) string {
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}
