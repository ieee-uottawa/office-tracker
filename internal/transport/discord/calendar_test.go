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

// Thursday, October 1 2026, 09:00 in Ottawa.
var calendarTestNow = time.Date(2026, 10, 1, 9, 0, 0, 0, service.BookingLocation)

func okResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func newCalendarTest(t *testing.T) (*Calendar, *discordgo.Session, *[]calendarRequest) {
	t.Helper()
	db := database.Open(filepath.Join(t.TempDir(), "discord-bookings.db"))
	t.Cleanup(func() { db.Close() })
	require.NoError(t, database.Migrate(db))
	c := NewCalendar(&service.BookingService{Repo: &repository.BookingRepo{DB: db}}, "exec")
	c.now = func() time.Time { return calendarTestNow }
	s, err := discordgo.New("Bot test-token")
	require.NoError(t, err)
	requests := []calendarRequest{}
	s.Client = &http.Client{Transport: calendarRoundTripper(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, err = io.ReadAll(r.Body)
			require.NoError(t, err)
		}
		requests = append(requests, calendarRequest{r.URL.Path, r.Method, r.Header.Get("Content-Type"), body})
		if strings.HasSuffix(r.URL.Path, "/users/@me/channels") {
			return okResponse(`{"id":"dm-channel","type":1}`), nil
		}
		return okResponse(`{"id":"calendar-message","channel_id":"channel"}`), nil
	})}
	return c, s, &requests
}

func calendarInteraction(user string, typ discordgo.InteractionType, data discordgo.InteractionData, roles ...string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "interaction-" + user, AppID: "app", Token: "test", Type: typ, GuildID: "exec", ChannelID: "channel",
		Member: &discordgo.Member{User: &discordgo.User{ID: user, Username: user}, Roles: roles}, Data: data}}
}

func click(user, customID string, values ...string) *discordgo.InteractionCreate {
	i := calendarInteraction(user, discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: customID, Values: values})
	i.Message = &discordgo.Message{ID: "private", Flags: discordgo.MessageFlagsEphemeral}
	return i
}

type callback struct {
	Type discordgo.InteractionResponseType `json:"type"`
	Data struct {
		Content    string                       `json:"content"`
		CustomID   string                       `json:"custom_id"`
		Flags      int                          `json:"flags"`
		Embeds     []discordgo.MessageEmbed     `json:"embeds"`
		Components []json.RawMessage            `json:"components"`
		Choices    []map[string]json.RawMessage `json:"choices"`
	} `json:"data"`
	raw string
}

func lastCallback(t *testing.T, requests []calendarRequest) callback {
	t.Helper()
	for n := len(requests) - 1; n >= 0; n-- {
		if strings.HasSuffix(requests[n].path, "/callback") {
			body := requests[n].body
			if strings.HasPrefix(requests[n].contentType, "multipart/") {
				body = payloadJSON(t, requests[n])
			}
			var cb callback
			require.NoError(t, json.Unmarshal(body, &cb))
			cb.raw = string(body)
			return cb
		}
	}
	t.Fatal("no interaction response")
	return callback{}
}

func payloadJSON(t *testing.T, r calendarRequest) []byte {
	_, params, err := mime.ParseMediaType(r.contentType)
	require.NoError(t, err)
	reader := multipart.NewReader(bytes.NewReader(r.body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return nil
		}
		require.NoError(t, err)
		if part.FormName() == "payload_json" {
			data, err := io.ReadAll(part)
			require.NoError(t, err)
			return data
		}
	}
}

func pending(t *testing.T, c *Calendar) []repository.Booking {
	t.Helper()
	rows, err := c.service.Repo.Pending("exec", calendarTestNow)
	require.NoError(t, err)
	return rows
}

func TestBookCommandConfirmAndSubmit(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	options := []*discordgo.ApplicationCommandInteractionDataOption{
		{Name: "date", Type: discordgo.ApplicationCommandOptionString, Value: "fri"},
		{Name: "start", Type: discordgo.ApplicationCommandOptionString, Value: "2pm"},
		{Name: "end", Type: discordgo.ApplicationCommandOptionString, Value: "16:00"},
		{Name: "purpose", Type: discordgo.ApplicationCommandOptionString, Value: "Board meeting"},
	}
	c.HandleCommand(s, calendarInteraction("alice", discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "book", Options: options}))
	cb := lastCallback(t, *requests)
	require.Equal(t, discordgo.InteractionResponseChannelMessageWithSource, cb.Type)
	require.Equal(t, int(discordgo.MessageFlagsEphemeral), cb.Data.Flags)
	require.Contains(t, cb.Data.Content, "Confirm your request")
	require.Contains(t, cb.Data.Content, "Fri, Oct 2 · 2 PM – 4 PM")
	require.Empty(t, pending(t, c), "nothing is requested before Submit")

	submitID := findCustomID(t, cb, ":submit")
	c.HandleButton(s, click("alice", submitID))
	cb = lastCallback(t, *requests)
	require.Equal(t, discordgo.InteractionResponseUpdateMessage, cb.Type)
	require.Contains(t, cb.Data.Content, "Request sent")
	require.Len(t, pending(t, c), 1)
	// A double click shows the same result instead of a second request.
	c.HandleButton(s, click("alice", submitID))
	require.Contains(t, lastCallback(t, *requests).Data.Content, "Request sent")
	require.Len(t, pending(t, c), 1)

	// Someone else gets the clash at confirm time and can pick another time.
	c.HandleCommand(s, calendarInteraction("bob", discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "book", Options: options}))
	cb = lastCallback(t, *requests)
	require.Contains(t, cb.Data.Content, "already taken")
	require.NotContains(t, cb.raw, ":submit")
	findCustomID(t, cb, ":edit")
}

func findCustomID(t *testing.T, cb callback, suffix string) string {
	t.Helper()
	var rows []struct {
		Components []struct {
			CustomID string `json:"custom_id"`
		} `json:"components"`
	}
	data, _ := json.Marshal(cb.Data.Components)
	require.NoError(t, json.Unmarshal(data, &rows))
	for _, r := range rows {
		for _, component := range r.Components {
			if strings.HasSuffix(component.CustomID, suffix) {
				return component.CustomID
			}
		}
	}
	t.Fatalf("no component ending in %q in %s", suffix, cb.raw)
	return ""
}

func TestPickerFlow(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	c.HandleButton(s, calendarInteraction("alice", discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "office:book"}))
	cb := lastCallback(t, *requests)
	require.Equal(t, discordgo.InteractionResponseChannelMessageWithSource, cb.Type, "the shared calendar opens a private picker")
	requireValidComponents(t, cb)
	prefix := strings.TrimSuffix(findCustomID(t, cb, ":day"), "day")
	c.HandleButton(s, click("alice", prefix+"day", "2026-10-02"))
	c.HandleButton(s, click("alice", prefix+"start", "14:00"))
	c.HandleButton(s, click("alice", prefix+"len", "120"))
	c.HandleButton(s, click("alice", prefix+"repeat", "weekly"))
	cb = lastCallback(t, *requests)
	requireValidComponents(t, cb)
	require.Contains(t, cb.Data.Content, "is free")
	c.HandleButton(s, click("alice", prefix+"next"))
	cb = lastCallback(t, *requests)
	require.Equal(t, discordgo.InteractionResponseModal, cb.Type)
	require.Contains(t, cb.raw, `"until"`)

	modal := calendarInteraction("alice", discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{CustomID: cb.Data.CustomID, Components: []discordgo.MessageComponent{
		&discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: "title", Value: "WIE meetings"}}},
		&discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: "until", Value: "oct 23"}}},
	}})
	modal.Message = &discordgo.Message{Flags: discordgo.MessageFlagsEphemeral}
	c.HandleModal(s, modal)
	cb = lastCallback(t, *requests)
	require.Contains(t, cb.Data.Content, "4 of 4 dates free")
	c.HandleButton(s, click("alice", findCustomID(t, cb, ":submit")))
	require.Len(t, pending(t, c), 4)

	// Another member's picker only offers times that are still free.
	c.HandleButton(s, click("bob", "office:book"))
	prefix = strings.TrimSuffix(findCustomID(t, lastCallback(t, *requests), ":day"), "day")
	c.HandleButton(s, click("bob", prefix+"day", "2026-10-02"))
	cb = lastCallback(t, *requests)
	require.NotContains(t, cb.raw, `"value":"14:00"`)
	require.NotContains(t, cb.raw, `"value":"15:30"`)
	require.Contains(t, cb.raw, `"value":"16:00"`)
	c.HandleButton(s, click("bob", prefix+"start", "13:00"))
	cb = lastCallback(t, *requests)
	require.Contains(t, cb.raw, `"value":"60"`)
	require.NotContains(t, cb.raw, `"value":"90"`, "a length that runs into a held booking is not offered")

	// Someone else's draft ID does nothing for them.
	c.HandleButton(s, click("mallory", prefix+"cancel"))
	require.Contains(t, lastCallback(t, *requests).Data.Content, "expired")
}

func requireValidComponents(t *testing.T, cb callback) {
	t.Helper()
	var rows []struct {
		Components []struct {
			CustomID string            `json:"custom_id"`
			Options  []json.RawMessage `json:"options"`
		} `json:"components"`
	}
	data, _ := json.Marshal(cb.Data.Components)
	require.NoError(t, json.Unmarshal(data, &rows))
	require.LessOrEqual(t, len(rows), 5)
	seen := map[string]bool{}
	for _, r := range rows {
		require.LessOrEqual(t, len(r.Components), 5)
		for _, component := range r.Components {
			require.False(t, seen[component.CustomID], "duplicate custom ID %s", component.CustomID)
			seen[component.CustomID] = true
			require.LessOrEqual(t, len(component.CustomID), 100)
			require.LessOrEqual(t, len(component.Options), 25)
		}
	}
}

func TestAutocompleteOffersOnlyFreeTimes(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	_, _, err := c.service.Submit("held", "exec", "bob", "Bob", nil, service.Draft{Title: "Held", Day: time.Date(2026, 10, 2, 0, 0, 0, 0, service.BookingLocation), StartMin: 12 * 60, EndMin: 15 * 60}, calendarTestNow)
	require.NoError(t, err)
	complete := func(focused string, values map[string]string) []string {
		options := []*discordgo.ApplicationCommandInteractionDataOption{}
		for name, value := range values {
			options = append(options, &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value, Focused: name == focused})
		}
		c.HandleAutocomplete(s, calendarInteraction("alice", discordgo.InteractionApplicationCommandAutocomplete, discordgo.ApplicationCommandInteractionData{Name: "book", Options: options}))
		cb := lastCallback(t, *requests)
		require.Equal(t, discordgo.InteractionApplicationCommandAutocompleteResult, cb.Type)
		values2 := []string{}
		for _, choice := range cb.Data.Choices {
			var v string
			require.NoError(t, json.Unmarshal(choice["value"], &v))
			values2 = append(values2, v)
		}
		return values2
	}
	dates := complete("date", map[string]string{"date": ""})
	require.Equal(t, "2026-10-01", dates[0])
	require.Len(t, dates, 25)
	require.Equal(t, []string{"2026-11-20"}, complete("date", map[string]string{"date": "nov 20"})[:1])
	starts := complete("start", map[string]string{"date": "2026-10-02", "start": ""})
	require.Contains(t, starts, "11:00")
	require.NotContains(t, starts, "12:00")
	require.NotContains(t, starts, "14:30")
	require.Contains(t, starts, "15:00")
	ends := complete("end", map[string]string{"date": "2026-10-02", "start": "10:00", "end": ""})
	require.Equal(t, []string{"10:30", "11:00", "11:30", "12:00"}, ends)
	require.Equal(t, []string{"23:00", "23:30", "24:00"}, complete("end", map[string]string{"date": "2026-10-02", "start": "22:30", "end": ""})[:3])
	require.Contains(t, complete("end", map[string]string{"date": "2026-10-02", "start": "22:30", "end": ""}), "02:00", "bookings can run past midnight")
	until := complete("until", map[string]string{"date": "2026-10-02", "repeat": "weekly", "until": ""})
	require.Equal(t, "2026-10-09", until[0])
}

func TestReviewPermissionsAndDecisions(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	created, _, err := c.service.Submit("req", "exec", "alice", "Alice", nil, service.Draft{Title: "Meeting", Day: time.Date(2026, 10, 2, 0, 0, 0, 0, service.BookingLocation), StartMin: 14 * 60, EndMin: 16 * 60}, calendarTestNow)
	require.NoError(t, err)
	id := created[0].ID
	approve := calendarInteraction("bob", discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "office:approve:1"})
	c.HandleButton(s, approve)
	require.Contains(t, lastCallback(t, *requests).Data.Content, "approvers")
	require.Equal(t, "pending", mustStatus(t, c, id))
	// The requester can't approve their own request even as an approver.
	self := calendarInteraction("alice", discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "office:approve:1"}, service.SigningOfficerRoleID)
	c.HandleButton(s, self)
	require.Contains(t, lastCallback(t, *requests).Data.Content, "Another officer")
	// Old card buttons (office:approve:<id>:0) keep working.
	officer := calendarInteraction("officer", discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "office:approve:1:0"}, service.SigningOfficerRoleID)
	c.HandleButton(s, officer)
	cb := lastCallback(t, *requests)
	require.Equal(t, int(discordgo.MessageFlagsEphemeral), cb.Data.Flags)
	require.Contains(t, cb.Data.Content, "Approved")
	require.Equal(t, "approved", mustStatus(t, c, id))

	// Revoke with a reason through the review screens.
	c.HandleButton(s, calendarInteraction("officer", discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "office:revoke:1:0"}, service.SigningOfficerRoleID))
	require.Equal(t, discordgo.InteractionResponseModal, lastCallback(t, *requests).Type)
	modal := calendarInteraction("officer", discordgo.InteractionModalSubmit, discordgo.ModalSubmitInteractionData{CustomID: "office:revokem:1:0", Components: []discordgo.MessageComponent{
		&discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: "reason", Value: "Interviews"}}}}}, service.SigningOfficerRoleID)
	c.HandleModal(s, modal)
	require.Equal(t, "revoked", mustStatus(t, c, id))

	// Another guild can't use the calendar, even with a matching role ID.
	other := calendarInteraction("officer", discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "office:review"}, service.SigningOfficerRoleID)
	other.GuildID = "other"
	c.HandleButton(s, other)
	require.Contains(t, lastCallback(t, *requests).Data.Content, "exec server")
}

func mustStatus(t *testing.T, c *Calendar, id int64) string {
	t.Helper()
	b, err := c.service.Repo.Get("exec", id)
	require.NoError(t, err)
	return b.Status
}

func TestMyBookingsCancelAndChange(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	created, _, err := c.service.Submit("req", "exec", "alice", "Alice", nil, service.Draft{Title: "Meeting", Day: time.Date(2026, 10, 2, 0, 0, 0, 0, service.BookingLocation), StartMin: 14 * 60, EndMin: 16 * 60}, calendarTestNow)
	require.NoError(t, err)
	id := created[0].ID
	require.NoError(t, c.service.Decide("exec", id, "officer", []string{service.SigningOfficerRoleID}, true, "", calendarTestNow))

	c.HandleCommand(s, calendarInteraction("alice", discordgo.InteractionApplicationCommand, discordgo.ApplicationCommandInteractionData{Name: "bookings"}))
	cb := lastCallback(t, *requests)
	require.Contains(t, cb.Data.Embeds[0].Description, "Approved")
	c.HandleButton(s, click("alice", "office:minesel", "1"))
	cb = lastCallback(t, *requests)
	findCustomID(t, cb, "office:change:1")

	// Change time: the picker opens with the current time filled in.
	c.HandleButton(s, click("alice", "office:change:1"))
	cb = lastCallback(t, *requests)
	require.Contains(t, cb.Data.Content, "Change time")
	prefix := strings.TrimSuffix(findCustomID(t, cb, ":start"), "start")
	c.HandleButton(s, click("alice", prefix+"start", "17:00"))
	c.HandleButton(s, click("alice", prefix+"next"))
	cb = lastCallback(t, *requests)
	require.Contains(t, cb.Data.Content, "You keep your current time")
	c.HandleButton(s, click("alice", findCustomID(t, cb, ":submit")))
	require.Equal(t, "approved", mustStatus(t, c, id))
	change, err := c.service.Repo.PendingChange("exec", id)
	require.NoError(t, err)
	require.Equal(t, "17:00", change.Start.In(service.BookingLocation).Format("15:04"))

	// Another member can't cancel or change it.
	c.HandleButton(s, click("bob", "office:cancelyes:1:0"))
	require.Equal(t, "approved", mustStatus(t, c, id))
	c.HandleButton(s, click("bob", "office:change:1"))
	require.Contains(t, lastCallback(t, *requests).Data.Content, "your own")

	c.HandleButton(s, click("alice", "office:cancel:1"))
	c.HandleButton(s, click("alice", findCustomID(t, lastCallback(t, *requests), "office:cancelyes:1:0")))
	require.Equal(t, "cancelled", mustStatus(t, c, id))
	require.Equal(t, "cancelled", mustStatus(t, c, change.ID), "the waiting change goes with it")
}

func TestSharedCalendarIsSingleAndPrivateNavigation(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	require.NoError(t, c.service.Repo.SaveCalendar(repository.BookingCalendar{GuildID: "exec", ChannelID: "old-channel", MessageID: "old-message", Week: "2026-09-21"}))
	c.SetDefaultChannel("channel")
	c.Refresh(s)
	boards, err := c.service.Repo.Calendars("exec")
	require.NoError(t, err)
	require.Len(t, boards, 1)
	require.Equal(t, "channel", boards[0].ChannelID)
	require.Equal(t, "2026-09-28", boards[0].Week)
	deleted := false
	for _, r := range *requests {
		deleted = deleted || (r.method == "DELETE" && strings.Contains(r.path, "/channels/old-channel/messages/old-message"))
	}
	require.True(t, deleted, "calendars left in other channels are removed")

	// Restarting edits the saved message instead of posting another.
	restored := NewCalendar(c.service, "exec")
	restored.now = c.now
	restored.SetDefaultChannel("channel")
	restored.Refresh(s)
	posts := 0
	for _, r := range *requests {
		if r.method == "POST" && strings.HasPrefix(r.contentType, "multipart/form-data") && strings.Contains(r.path, "/channels/") {
			posts++
		}
	}
	require.Equal(t, 1, posts)

	// Other weeks are private and don't move the shared calendar.
	c.HandleButton(s, calendarInteraction("alice", discordgo.InteractionMessageComponent, discordgo.MessageComponentInteractionData{CustomID: "office:weeks"}))
	cb := lastCallback(t, *requests)
	require.Equal(t, int(discordgo.MessageFlagsEphemeral), cb.Data.Flags)
	requireValidComponents(t, cb)
	c.HandleButton(s, click("alice", "office:view:2026-10-05"))
	cb = lastCallback(t, *requests)
	require.Equal(t, discordgo.InteractionResponseUpdateMessage, cb.Type)
	require.Equal(t, "Oct 5 – Oct 11, 2026", cb.Data.Embeds[0].Title)
	boards, err = c.service.Repo.Calendars("exec")
	require.NoError(t, err)
	require.Equal(t, "2026-09-28", boards[0].Week)

	// A deleted shared calendar is recreated.
	original := s.Client.Transport
	s.Client.Transport = calendarRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PATCH" {
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
	require.Equal(t, "replacement-calendar", boards[0].MessageID)
}

func TestCalendarComponentsAndPresenceFinder(t *testing.T) {
	seen := map[string]bool{}
	for _, component := range calendarComponents(2) {
		r := component.(discordgo.ActionsRow)
		require.LessOrEqual(t, len(r.Components), 5)
		for _, component := range r.Components {
			b := component.(discordgo.Button)
			require.False(t, seen[b.CustomID])
			seen[b.CustomID] = true
		}
	}
	require.True(t, seen["office:review"])
	bot := &discordgo.User{ID: "bot"}
	presence := &discordgo.Message{Author: bot, Embeds: []*discordgo.MessageEmbed{{Title: "🏢 Office Presence"}}, Components: []discordgo.MessageComponent{&discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.Button{CustomID: "refresh_btn"}}}}}
	require.True(t, isPresenceDashboardMessage(presence, "bot"))
	require.False(t, isPresenceDashboardMessage(presence, "someone-else"))
	calendar := &discordgo.Message{Author: bot, Embeds: []*discordgo.MessageEmbed{{Title: "Sep 28 – Oct 4, 2026"}}, Components: calendarComponents(0)}
	require.False(t, isPresenceDashboardMessage(calendar, "bot"))
	reply := &discordgo.Message{Author: bot, Embeds: presence.Embeds}
	require.False(t, isPresenceDashboardMessage(reply, "bot"))
	for _, cmd := range calendarCommandDefinitions() {
		require.True(t, IsBookingCommand(cmd.Name), cmd.Name)
		require.LessOrEqual(t, len(cmd.Description), 100)
	}
}
