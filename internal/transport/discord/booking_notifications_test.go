package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"office/internal/repository"
	"office/internal/service"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
)

var officerRoles = []string{service.SigningOfficerRoleID}

func notificationTestBooking(t *testing.T, c *Calendar, request string) *repository.Booking {
	t.Helper()
	d := service.Draft{Title: "Meeting @everyone <@&other-role>", Day: time.Date(2026, 10, 2, 0, 0, 0, 0, service.BookingLocation), StartMin: 14 * 60, EndMin: 15 * 60}
	created, _, err := c.service.Submit(request, c.guildID, "requester", "Alice", nil, d, calendarTestNow)
	require.NoError(t, err)
	return &created[0]
}

type post struct {
	channel string
	payload map[string]json.RawMessage
}

func posts(t *testing.T, requests []calendarRequest) []post {
	t.Helper()
	result := []post{}
	for _, r := range requests {
		if r.method != "POST" || !strings.HasPrefix(r.path, "/api/v9/channels/") || !strings.HasSuffix(r.path, "/messages") || strings.HasPrefix(r.contentType, "multipart/") {
			continue
		}
		var payload map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(r.body, &payload))
		channel := strings.TrimSuffix(strings.TrimPrefix(r.path, "/api/v9/channels/"), "/messages")
		result = append(result, post{channel, payload})
	}
	return result
}

func mentions(t *testing.T, p post) discordgo.MessageAllowedMentions {
	var m discordgo.MessageAllowedMentions
	require.NoError(t, json.Unmarshal(p.payload["allowed_mentions"], &m))
	return m
}

func TestBookingNotificationsLifecycle(t *testing.T) {
	for _, decision := range []string{"approved", "declined", "cancelled", "revoked"} {
		t.Run(decision, func(t *testing.T) {
			c, s, requests := newCalendarTest(t)
			b := notificationTestBooking(t, c, "request")
			notificationTestBooking(t, c, "request") // a retried interaction adds nothing
			require.NoError(t, c.DeliverNotifications(s))
			sent := posts(t, *requests)
			require.Len(t, sent, 1)
			require.Equal(t, service.DefaultOfficerChannelID, sent[0].channel)
			m := mentions(t, sent[0])
			require.Equal(t, officerRoles, m.Roles)
			require.Empty(t, m.Users)
			require.Empty(t, m.Parse)
			require.Equal(t, "true", string(sent[0].payload["enforce_nonce"]))

			switch decision {
			case "cancelled":
				require.NoError(t, c.service.Cancel(c.guildID, b.ID, "requester", false, calendarTestNow))
			case "revoked":
				require.NoError(t, c.service.Decide(c.guildID, b.ID, "officer", officerRoles, true, "", calendarTestNow))
				require.NoError(t, c.DeliverNotifications(s))
				*requests = (*requests)[:0]
				require.NoError(t, c.service.Revoke(c.guildID, b.ID, "officer", officerRoles, false, "Interviews", calendarTestNow))
			default:
				require.NoError(t, c.service.Decide(c.guildID, b.ID, "officer", officerRoles, decision == "approved", "Room is closed", calendarTestNow))
			}
			require.NoError(t, c.DeliverNotifications(s))
			require.NoError(t, c.DeliverNotifications(s))

			// Nothing new is posted in the officer channel: the card is edited in place.
			officerPosts := 0
			var dms []post
			for _, p := range posts(t, *requests) {
				if p.channel == service.DefaultOfficerChannelID {
					officerPosts++
				}
				if p.channel == "dm-channel" {
					dms = append(dms, p)
				}
			}
			require.Equal(t, map[bool]int{true: 0, false: 1}[decision == "revoked"], officerPosts)
			var edit map[string]json.RawMessage
			for _, r := range *requests {
				if r.method == "PATCH" {
					require.NoError(t, json.Unmarshal(r.body, &edit))
				}
			}
			require.NotNil(t, edit)
			require.JSONEq(t, "[]", string(edit["components"]), "decided cards have no buttons")
			if decision == "cancelled" {
				require.Empty(t, dms, "the requester cancelled it themselves")
				require.Contains(t, string(edit["embeds"]), "Cancelled by the requester")
				return
			}
			require.Len(t, dms, 1, "the decision goes to the requester by DM")
			content := string(dms[0].payload["content"])
			require.Contains(t, content, map[string]string{"approved": "approved", "declined": "declined", "revoked": "revoked"}[decision])
			require.NotContains(t, content, "@everyone")
			if decision == "declined" {
				require.Contains(t, content, "Room is closed")
			}
			if decision == "revoked" {
				require.Contains(t, content, "Interviews")
			}
			require.Empty(t, mentions(t, dms[0]).Parse)
			pendingNotes, err := c.service.Repo.PendingNotifications(c.guildID, calendarTestNow.Add(time.Hour))
			require.NoError(t, err)
			require.Empty(t, pendingNotes)
		})
	}
}

func TestClosedDMsFallBackToCalendarChannel(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	c.SetDefaultChannel("calendar-channel")
	b := notificationTestBooking(t, c, "request")
	require.NoError(t, c.service.Decide(c.guildID, b.ID, "officer", officerRoles, true, "", calendarTestNow))
	original := s.Client.Transport
	s.Client.Transport = calendarRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/channels/dm-channel/messages") {
			return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":50007,"message":"Cannot send messages to this user"}`))}, nil
		}
		return original.RoundTrip(r)
	})
	require.NoError(t, c.DeliverNotifications(s))
	var fallback *post
	for _, p := range posts(t, *requests) {
		if p.channel == "calendar-channel" {
			fallback = &p
		}
	}
	require.NotNil(t, fallback)
	require.Equal(t, []string{"requester"}, mentions(t, *fallback).Users)
	require.Contains(t, string(fallback.payload["content"]), "can't DM you")
}

func TestShortNoticeReminderPingsOnce(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	notificationTestBooking(t, c, "request") // Oct 2 14:00
	require.NoError(t, c.DeliverNotifications(s))
	c.now = func() time.Time { return time.Date(2026, 10, 2, 12, 30, 0, 0, service.BookingLocation) }
	c.Maintain(s)
	c.Maintain(s)
	reminders := 0
	for _, p := range posts(t, *requests) {
		if strings.Contains(string(p.payload["content"]), "still needs a decision") {
			reminders++
			require.Equal(t, officerRoles, mentions(t, p).Roles)
			require.Contains(t, string(p.payload["message_reference"]), "calendar-message")
		}
	}
	require.Equal(t, 1, reminders)
	// At the start time the request expires and the requester is told.
	c.now = func() time.Time { return time.Date(2026, 10, 2, 14, 0, 0, 0, service.BookingLocation) }
	c.Maintain(s)
	require.Equal(t, "expired", mustStatus(t, c, 1))
	expiredDM := false
	for _, p := range posts(t, *requests) {
		expiredDM = expiredDM || (p.channel == "dm-channel" && strings.Contains(string(p.payload["content"]), "expired"))
	}
	require.True(t, expiredDM)
}

func TestBookingNotificationsRetryAfterRestart(t *testing.T) {
	c, s, _ := newCalendarTest(t)
	notificationTestBooking(t, c, "request")
	fail := true
	sent := 0
	var nonces []string
	s.Client = &http.Client{Transport: calendarRoundTripper(func(r *http.Request) (*http.Response, error) {
		sent++
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var payload struct {
			Nonce string `json:"nonce"`
		}
		require.NoError(t, json.Unmarshal(body, &payload))
		nonces = append(nonces, payload.Nonce)
		if fail {
			return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":50013,"message":"Missing Permissions"}`))}, nil
		}
		return okResponse(`{"id":"request-message"}`), nil
	})}
	require.Error(t, c.DeliverNotifications(s))
	queued, err := c.service.Repo.PendingNotifications(c.guildID, calendarTestNow.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, 1, queued[0].Attempts)
	_, err = c.service.Repo.DB.Exec(`UPDATE booking_notifications SET next_attempt_at=0`)
	require.NoError(t, err)
	fail = false
	restarted := NewCalendar(c.service, c.guildID)
	restarted.now = c.now
	require.NoError(t, restarted.DeliverNotifications(s))
	require.NoError(t, restarted.DeliverNotifications(s))
	require.Equal(t, 2, sent)
	require.Equal(t, nonces[0], nonces[1])
}

func TestDecisionEditRetryDoesNotRepeatDM(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	b := notificationTestBooking(t, c, "request")
	require.NoError(t, c.DeliverNotifications(s))
	require.NoError(t, c.service.Decide(c.guildID, b.ID, "officer", officerRoles, true, "", calendarTestNow))
	original := s.Client.Transport
	failEdit := true
	s.Client.Transport = calendarRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PATCH" && failEdit {
			return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":50013,"message":"Missing Permissions"}`))}, nil
		}
		return original.RoundTrip(r)
	})
	require.Error(t, c.DeliverNotifications(s))
	_, err := c.service.Repo.DB.Exec(`UPDATE booking_notifications SET next_attempt_at=0`)
	require.NoError(t, err)
	failEdit = false
	require.NoError(t, c.DeliverNotifications(s))
	dms := 0
	for _, p := range posts(t, *requests) {
		if p.channel == "dm-channel" {
			dms++
		}
	}
	require.Equal(t, 1, dms)
}

func TestResolvedRequestDoesNotPingOfficersLate(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	b := notificationTestBooking(t, c, "request")
	require.NoError(t, c.service.Decide(c.guildID, b.ID, "officer", officerRoles, true, "", calendarTestNow))
	require.NoError(t, c.DeliverNotifications(s))
	for _, p := range posts(t, *requests) {
		require.NotEqual(t, service.DefaultOfficerChannelID, p.channel)
	}
}

func TestBookingConcurrentDeliverySendsOnce(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	notificationTestBooking(t, c, "request")
	results := make(chan error, 8)
	for j := 0; j < 8; j++ {
		go func() { results <- c.DeliverNotifications(s) }()
	}
	for j := 0; j < 8; j++ {
		require.NoError(t, <-results)
	}
	require.Len(t, posts(t, *requests), 1)
}

func TestApproverRolesAndOfficerChannelFromSettings(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	require.NoError(t, c.service.Repo.SetRoleCapability("exec", "chairs", service.CapabilityApprove, true))
	require.NoError(t, c.service.Repo.SetSetting("exec", service.SettingOfficerChannel, "officers"))
	notificationTestBooking(t, c, "request")
	require.NoError(t, c.DeliverNotifications(s))
	sent := posts(t, *requests)
	require.Len(t, sent, 1)
	require.Equal(t, "officers", sent[0].channel)
	require.Equal(t, []string{"chairs"}, mentions(t, sent[0]).Roles)
	var content string
	require.NoError(t, json.Unmarshal(sent[0].payload["content"], &content))
	require.True(t, strings.HasPrefix(content, "<@&chairs> <@requester>"), content)
}
