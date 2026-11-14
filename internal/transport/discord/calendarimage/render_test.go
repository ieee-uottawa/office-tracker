package calendarimage

import (
	"bytes"
	"image/png"
	"os"
	"testing"
	"time"

	"office/internal/repository"
	"office/internal/service"

	"github.com/stretchr/testify/require"
)

func TestRenderWeeklyCalendar(t *testing.T) {
	loc := service.BookingLocation
	week := service.BookingWeek(time.Date(2026, 10, 1, 12, 0, 0, 0, loc))
	bookings := []repository.Booking{}
	for index, item := range []struct {
		day, hour, minute, duration int
		name, title                 string
	}{
		{0, 10, 0, 90, "Alice Martin", "Committee planning"},
		{1, 13, 0, 120, "Waaberi Ibrahim", "Project meeting"},
		{2, 9, 0, 60, "Émilie Dubois", "Study session"},
		{2, 14, 0, 180, "Sam Chen", "Workshop preparation"},
		{3, 11, 0, 15, "Maya Patel", "Quick meeting"},
		{3, 11, 15, 45, "Alex Thompson", "Office hours"},
		{4, 16, 30, 90, "A very long display name that must fit", "Team discussion with a longer purpose"},
		{6, 20, 0, 120, "Jordan Lee", "Evening planning"},
	} {
		start := week.AddDate(0, 0, item.day).Add(time.Duration(item.hour)*time.Hour + time.Duration(item.minute)*time.Minute)
		bookings = append(bookings, repository.Booking{ID: int64(index + 1), UserID: item.name, Name: item.name, Title: item.title, Start: start, End: start.Add(time.Duration(item.duration) * time.Minute), Status: "approved"})
	}
	data, err := Render(week, bookings, week.AddDate(0, 0, 3))
	require.NoError(t, err)
	im, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	require.Equal(t, 1680, im.Bounds().Dx())
	require.Less(t, len(data), 1024*1024)
	// A real fixture can be exported for visual inspection without posting to Discord.
	if output := os.Getenv("CALENDAR_PREVIEW_PATH"); output != "" {
		require.NoError(t, os.WriteFile(output, data, 0600))
	}
	for _, times := range [][2]int{{0, 1}, {23, 24}} {
		b := repository.Booking{UserID: "night", Name: "Night booking", Start: week.Add(time.Duration(times[0]) * time.Hour), End: week.Add(time.Duration(times[1]) * time.Hour)}
		data, err := Render(week, []repository.Booking{b}, week)
		require.NoError(t, err)
		_, err = png.Decode(bytes.NewReader(data))
		require.NoError(t, err)
	}
	_, err = Render(week, nil, week)
	require.NoError(t, err)
}
