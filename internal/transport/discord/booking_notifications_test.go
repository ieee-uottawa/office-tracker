package discord

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"office/internal/database"
	"office/internal/repository"
	"office/internal/service"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
)

func notificationTestBooking(t *testing.T, c *Calendar, request string) *repository.Booking {
	t.Helper()
	date := time.Now().In(service.BookingLocation).AddDate(0, 0, 1).Format("2006-01-02")
	b, err := c.service.Request(request, c.guildID, "requester", "Alice", "Meeting @everyone <@&other-role>", date, "14:00", "15:00", time.Now())
	require.NoError(t, err)
	return b
}

type notificationPayload struct {
	discordgo.MessageSend
	Nonce        string `json:"nonce"`
	EnforceNonce bool   `json:"enforce_nonce"`
}

func notificationPosts(t *testing.T, requests []calendarRequest) []map[string]json.RawMessage {
	t.Helper()
	result := []map[string]json.RawMessage{}
	for _, request := range requests {
		if request.method != "POST" || !strings.HasSuffix(request.path, "/channels/"+BookingNotificationChannelID+"/messages") {
			continue
		}
		var payload map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(request.body, &payload))
		result = append(result, payload)
	}
	return result
}

func TestBookingNotificationsLifecycle(t *testing.T) {
	for _, decision := range []string{"approved", "declined", "cancelled"} {
		t.Run(decision, func(t *testing.T) {
			c, s, requests := newCalendarTest(t)
			b := notificationTestBooking(t, c, "request")
			// Replaying a request or a startup migration does not enqueue another ping.
			notificationTestBooking(t, c, "request")
			require.NoError(t, database.Migrate(c.service.Repo.DB))
			require.NoError(t, c.DeliverNotifications(s))
			posts := notificationPosts(t, *requests)
			require.Len(t, posts, 1)
			var mentions discordgo.MessageAllowedMentions
			require.NoError(t, json.Unmarshal(posts[0]["allowed_mentions"], &mentions))
			require.Equal(t, []string{service.SigningOfficerRoleID}, mentions.Roles)
			require.Empty(t, mentions.Users)
			require.Empty(t, mentions.Parse)
			require.Equal(t, "true", string(posts[0]["enforce_nonce"]))
			require.NotEmpty(t, posts[0]["nonce"])
			if decision == "cancelled" {
				require.NoError(t, c.service.Cancel(c.guildID, b.ID, "requester", time.Now()))
			} else {
				require.NoError(t, c.service.Decide(c.guildID, b.ID, "officer", []string{service.SigningOfficerRoleID}, decision == "approved", time.Now()))
			}
			require.NoError(t, c.DeliverNotifications(s))
			require.NoError(t, c.DeliverNotifications(s))
			posts = notificationPosts(t, *requests)
			require.Len(t, posts, 2)
			mentions = discordgo.MessageAllowedMentions{}
			require.NoError(t, json.Unmarshal(posts[1]["allowed_mentions"], &mentions))
			require.Empty(t, mentions.Roles)
			require.Empty(t, mentions.Parse)
			if decision == "cancelled" {
				require.Empty(t, mentions.Users)
			} else {
				require.Equal(t, []string{"requester"}, mentions.Users)
			}
			require.Contains(t, string(posts[1]["content"]), decision)
			var edits []calendarRequest
			for _, request := range *requests {
				if request.method == "PATCH" {
					edits = append(edits, request)
				}
			}
			require.Len(t, edits, 1)
			var edit map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(edits[0].body, &edit))
			require.JSONEq(t, "[]", string(edit["components"]))
			require.Contains(t, string(edit["content"]), decision)
			pending, err := c.service.Repo.PendingNotifications(c.guildID, time.Now().Add(time.Hour))
			require.NoError(t, err)
			require.Empty(t, pending)
		})
	}
}

func TestBookingNotificationsRetryAfterRestart(t *testing.T) {
	c, s, _ := newCalendarTest(t)
	notificationTestBooking(t, c, "request")
	fail := true
	posts := 0
	var nonces []string
	s.Client = &http.Client{Transport: calendarRoundTripper(func(r *http.Request) (*http.Response, error) {
		posts++
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
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"request-message"}`))}, nil
	})}
	require.Error(t, c.DeliverNotifications(s))
	queued, err := c.service.Repo.PendingNotifications(c.guildID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, 1, queued[0].Attempts)
	_, err = c.service.Repo.DB.Exec(`UPDATE booking_notifications SET next_attempt_at=0`)
	require.NoError(t, err)
	fail = false
	restarted := NewCalendar(c.service, c.guildID)
	require.NoError(t, restarted.DeliverNotifications(s))
	require.NoError(t, restarted.DeliverNotifications(s))
	require.Equal(t, 2, posts)
	require.Equal(t, nonces[0], nonces[1])
}

func TestBookingNotificationEditRetryDoesNotRepeatPing(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	b := notificationTestBooking(t, c, "request")
	require.NoError(t, c.DeliverNotifications(s))
	require.NoError(t, c.service.Decide(c.guildID, b.ID, "officer", []string{service.SigningOfficerRoleID}, true, time.Now()))
	original := s.Client.Transport
	failEdit := true
	s.Client.Transport = calendarRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.Method == "PATCH" && failEdit {
			return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":50013,"message":"Missing Permissions"}`))}, nil
		}
		return original.RoundTrip(r)
	})
	require.Error(t, c.DeliverNotifications(s))
	require.Len(t, notificationPosts(t, *requests), 2)
	_, err := c.service.Repo.DB.Exec(`UPDATE booking_notifications SET next_attempt_at=0`)
	require.NoError(t, err)
	failEdit = false
	require.NoError(t, NewCalendar(c.service, c.guildID).DeliverNotifications(s))
	require.Len(t, notificationPosts(t, *requests), 2)
}

func TestResolvedRequestDoesNotPingOfficersLate(t *testing.T) {
	c, s, requests := newCalendarTest(t)
	b := notificationTestBooking(t, c, "request")
	require.NoError(t, c.service.Decide(c.guildID, b.ID, "officer", []string{service.SigningOfficerRoleID}, true, time.Now()))
	require.NoError(t, c.DeliverNotifications(s))
	posts := notificationPosts(t, *requests)
	require.Len(t, posts, 1)
	require.Contains(t, string(posts[0]["content"]), "approved")
	var mentions discordgo.MessageAllowedMentions
	require.NoError(t, json.Unmarshal(posts[0]["allowed_mentions"], &mentions))
	require.Empty(t, mentions.Roles)
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
	require.Len(t, notificationPosts(t, *requests), 1)
}
