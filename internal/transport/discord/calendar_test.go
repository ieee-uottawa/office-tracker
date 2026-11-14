package discord

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"office/internal/database"
	"office/internal/repository"
	"office/internal/service"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
)

type calendarRoundTripper func(*http.Request) (*http.Response, error)

func (f calendarRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type calendarRequest struct {
	path, method, contentType string
	body                      []byte
}

func newCalendarTest(t *testing.T) (*Calendar, *discordgo.Session, *[]calendarRequest) {
	t.Helper()
	db := database.Open(filepath.Join(t.TempDir(), "discord-bookings.db"))
	t.Cleanup(func() { db.Close() })
	require.NoError(t, database.Migrate(db))
	c := NewCalendar(&service.BookingService{Repo: &repository.BookingRepo{DB: db}}, "exec")
	s, err := discordgo.New("Bot test-token")
	require.NoError(t, err)
	requests := []calendarRequest{}
	s.Client = &http.Client{Transport: calendarRoundTripper(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		requests = append(requests, calendarRequest{r.URL.Path, r.Method, r.Header.Get("Content-Type"), body})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"calendar-message","channel_id":"channel"}`))}, nil
	})}
	return c, s, &requests
}

func calendarInteraction(user string, typ discordgo.InteractionType, data discordgo.InteractionData) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "interaction-" + user, AppID: "app", Token: "test", Type: typ, GuildID: "exec", ChannelID: "channel", Member: &discordgo.Member{User: &discordgo.User{ID: user, Username: user}}, Data: data}}
}

func TestCalendarBookingInteractionPermissions(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	c.SetDefaultChannel("channel")
	c.Refresh(s)
	uploadCount := func() int {
		n := 0
		for _, r := range *requests {
			if strings.HasPrefix(r.contentType, "multipart/form-data") {
				n++
			}
		}
		return n
	}
	date := time.Now().In(service.BookingLocation).AddDate(0, 0, 1).Format("2006-01-02")
	components := []discordgo.MessageComponent{}
	for key, value := range map[string]string{"title": "Team meeting", "date": date, "start": "14:00", "end": "16:00"} {
		components = append(components, &discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: key, Value: value}}})
	}
	i := calendarInteraction("alice", discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{CustomID: "office:request", Components: components})
	c.HandleModal(s, i)
	require.Equal(t, 2, uploadCount(), "new request refreshes the managed calendar")
	pending, err := c.service.Repo.Pending("exec", time.Now())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "alice", pending[0].Name)
	click := calendarInteraction("officer", discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "office:approve:1:0"})
	c.HandleButton(s, click)
	b, err := c.service.Repo.Get("exec", pending[0].ID)
	require.NoError(t, err)
	require.Equal(t, "pending", b.Status)
	require.Contains(t, string((*requests)[len(*requests)-1].body), "Signing Officer")
	click.Member.Roles = []string{service.SigningOfficerRoleID}
	c.HandleButton(s, click)
	b, err = c.service.Repo.Get("exec", b.ID)
	require.NoError(t, err)
	require.Equal(t, "approved", b.Status)
	require.Equal(t, 3, uploadCount(), "approval refreshes the managed calendar")
	// A forged cancel button must not let another user cancel Alice's booking.
	click.Data = discordgo.MessageComponentInteractionData{CustomID: "office:cancel:1:0"}
	c.HandleButton(s, click)
	b, err = c.service.Repo.Get("exec", b.ID)
	require.NoError(t, err)
	require.Equal(t, "approved", b.Status)
	click.Member.User.ID = "alice"
	c.HandleButton(s, click)
	b, err = c.service.Repo.Get("exec", b.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", b.Status)
	require.Equal(t, 4, uploadCount(), "cancellation refreshes the managed calendar")
	// A matching role ID in a different guild cannot access this calendar.
	click.GuildID = "other"
	click.Data = discordgo.MessageComponentInteractionData{CustomID: "office:review"}
	c.HandleButton(s, click)
	require.Contains(t, string((*requests)[len(*requests)-1].body), "exec server")
}

func TestCalendarPublishingNavigationAndFiles(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	i := calendarInteraction("alice", discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "calendar"})
	c.HandleCommand(s, i)
	boards, err := c.service.Repo.Calendars("exec")
	require.NoError(t, err)
	require.Len(t, boards, 1)
	week := service.BookingWeek(time.Now()).AddDate(0, 0, 7)
	click := calendarInteraction("alice", discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "office:week:" + week.Format("2006-01-02")})
	click.Message = &discordgo.Message{ID: "calendar-message"}
	c.HandleButton(s, click)
	boards, err = c.service.Repo.Calendars("exec")
	require.NoError(t, err)
	require.Equal(t, week.Format("2006-01-02"), boards[0].Week)
	// Saved navigation survives a new controller instance (the state is in SQLite).
	NewCalendar(c.service, "exec").Refresh(s)
	uploads := 0
	for _, request := range *requests {
		if !strings.HasPrefix(request.contentType, "multipart/form-data") {
			continue
		}
		uploads++
		_, params, err := mime.ParseMediaType(request.contentType)
		require.NoError(t, err)
		reader := multipart.NewReader(bytes.NewReader(request.body), params["boundary"])
		foundPNG, foundPayload := false, false
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
			data, err := io.ReadAll(part)
			require.NoError(t, err)
			if part.FileName() == "office-calendar.png" {
				foundPNG = true
				require.True(t, bytes.HasPrefix(data, []byte("\x89PNG")))
			}
			if part.FormName() == "payload_json" {
				foundPayload = true
				var payload map[string]any
				require.NoError(t, json.Unmarshal(data, &payload))
				require.Contains(t, string(data), "attachment://office-calendar.png")
			}
		}
		require.True(t, foundPNG)
		require.True(t, foundPayload)
	}
	require.Equal(t, 3, uploads)
}

func TestCalendarButtonsAndModalPayload(t *testing.T) {
	seen := map[string]bool{}
	for _, component := range calendarComponents(service.BookingWeek(time.Now()), 2) {
		row := component.(discordgo.ActionsRow)
		require.LessOrEqual(t, len(row.Components), 5)
		for _, component := range row.Components {
			button := component.(discordgo.Button)
			require.False(t, seen[button.CustomID], "duplicate component ID: %s", button.CustomID)
			seen[button.CustomID] = true
			require.LessOrEqual(t, len(button.CustomID), 100)
		}
	}
	c, s, requests := newCalendarTest(t)
	i := calendarInteraction("alice", discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "book"})
	c.HandleCommand(s, i)
	var response struct {
		Type discordgo.InteractionResponseType `json:"type"`
		Data struct {
			Components []json.RawMessage `json:"components"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal((*requests)[0].body, &response))
	require.Equal(t, discordgo.InteractionResponseModal, response.Type)
	require.Len(t, response.Data.Components, 4)
}

func TestManagedCalendarRestoresWithoutDuplicates(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	c.SetDefaultChannel("channel")
	c.Refresh(s)
	boards, err := c.service.Repo.Calendars("exec")
	require.NoError(t, err)
	require.Len(t, boards, 1)
	firstID := boards[0].MessageID
	restored := NewCalendar(c.service, "exec")
	restored.SetDefaultChannel("channel")
	restored.Refresh(s)
	posts := 0
	for _, r := range *requests {
		if r.method == "POST" && strings.HasPrefix(r.contentType, "multipart/form-data") {
			posts++
		}
	}
	require.Equal(t, 1, posts, "restart must edit the saved message")
	original := s.Client.Transport
	deleted := true
	s.Client.Transport = calendarRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PATCH" && deleted {
			deleted = false
			return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":10008,"message":"Unknown Message"}`))}, nil
		}
		response, err := original.RoundTrip(r)
		if err == nil && r.Method == "POST" {
			response.Body = io.NopCloser(strings.NewReader(`{"id":"replacement-calendar","channel_id":"channel"}`))
		}
		return response, err
	})
	restored.Refresh(s)
	boards, err = c.service.Repo.Calendars("exec")
	require.NoError(t, err)
	require.Len(t, boards, 1)
	require.NotEqual(t, firstID, boards[0].MessageID)
	require.Equal(t, "replacement-calendar", boards[0].MessageID)
}

func TestPresenceFinderIgnoresCalendarAndTemporaryPresenceReplies(t *testing.T) {
	bot := &discordgo.User{ID: "bot"}
	presence := &discordgo.Message{Author: bot, Embeds: []*discordgo.MessageEmbed{{Title: "🏢 Office Presence"}}, Components: []discordgo.MessageComponent{&discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.Button{CustomID: "refresh_btn"}}}}}
	require.True(t, isPresenceDashboardMessage(presence, "bot"))
	require.False(t, isPresenceDashboardMessage(presence, "someone-else"))
	calendar := &discordgo.Message{Author: bot, Embeds: []*discordgo.MessageEmbed{{Title: "September 28 – October 4, 2026"}}, Components: calendarComponents(service.BookingWeek(time.Now()), 0)}
	require.False(t, isPresenceDashboardMessage(calendar, "bot"))
	reply := &discordgo.Message{Author: bot, Embeds: presence.Embeds}
	require.False(t, isPresenceDashboardMessage(reply, "bot"))
}
