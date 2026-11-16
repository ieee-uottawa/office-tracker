package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"office/internal/repository"
	"office/internal/service"

	"github.com/bwmarrin/discordgo"
)

// sendWithNonce posts a message with Discord's duplicate protection, using
// discordgo's authenticated, rate-limited request helper (v0.29 predates the
// nonce fields).
func sendWithNonce(s *discordgo.Session, channelID, nonce string, message *discordgo.MessageSend) (*discordgo.Message, error) {
	payload := struct {
		*discordgo.MessageSend
		Nonce        string `json:"nonce"`
		EnforceNonce bool   `json:"enforce_nonce"`
	}{message, nonce, true}
	endpoint := discordgo.EndpointChannelMessages(channelID)
	data, err := s.RequestWithBucketID("POST", endpoint, payload, endpoint)
	if err != nil {
		return nil, err
	}
	var sent discordgo.Message
	if err = json.Unmarshal(data, &sent); err != nil {
		return nil, err
	}
	if sent.ID == "" {
		return nil, errors.New("discord returned no message ID")
	}
	return &sent, nil
}

func discordErrorCode(err error) int {
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Message != nil {
		return rest.Message.Code
	}
	return 0
}

func roleMentions(roles []string) string {
	mentions := make([]string, 0, len(roles))
	for _, role := range roles {
		mentions = append(mentions, "<@&"+role+">")
	}
	return strings.Join(mentions, " ")
}

// updateRequestCard redraws a request's officer card in place, without pings.
func (c *Calendar) updateRequestCard(s *discordgo.Session, b repository.Booking) error {
	channel, message, err := c.service.Repo.RequestNotificationMessage(b.Lead())
	if err != nil || message == "" {
		return err
	}
	embed, buttons, err := c.requestCard(b)
	if err != nil {
		return err
	}
	content := fmt.Sprintf("Office request from <@%s>", b.UserID)
	embeds := []*discordgo.MessageEmbed{embed}
	components := []discordgo.MessageComponent{}
	if len(buttons) > 0 {
		components = append(components, row(buttons...))
	}
	_, err = s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: message, Channel: channel, Content: &content, Embeds: &embeds, Components: &components, AllowedMentions: noMentions()})
	if isMissingDiscordMessage(err) {
		return nil
	}
	return err
}

// decisionDM is the message a requester receives, or nil when the event
// needs none.
func (c *Calendar) decisionDM(n repository.BookingNotification) *discordgo.MessageSend {
	b := n.Booking
	rows, err := c.service.Repo.Series(c.guildID, b.Lead())
	if err != nil || len(rows) == 0 {
		rows = []repository.Booking{b}
	}
	var original *repository.Booking
	if b.ReplacesID > 0 {
		original, _ = c.service.Repo.Get(c.guildID, b.ReplacesID)
	}
	title := escapeBookingText(b.Title)
	reason := ""
	if b.Reason != "" {
		reason = "\nReason: " + escapeBookingText(b.Reason)
	}
	bookAgain := row(button("Book another time", "office:book", discordgo.PrimaryButton))
	myBookings := row(button("My bookings", "office:mine", discordgo.SecondaryButton))
	manage := func(id int64) discordgo.ActionsRow {
		return row(button("Change time", fmt.Sprintf("office:change:%d", id), discordgo.SecondaryButton), button("Cancel booking", fmt.Sprintf("office:cancel:%d", id), discordgo.DangerButton))
	}
	message := &discordgo.MessageSend{AllowedMentions: noMentions()}
	switch n.Event {
	case "approved":
		switch {
		case original != nil:
			message.Content = fmt.Sprintf("✅ **Your time change was approved.**\n**%s** is now %s.", title, bookingWhen(b))
			message.Components = []discordgo.MessageComponent{manage(b.ID)}
		case len(rows) > 1:
			message.Content = "✅ **Your office booking was approved.**\n" + requestSummaryText(b, rows, nil)
			declined := []string{}
			for _, r := range rows {
				if r.Status == "declined" && r.ReviewedBy == n.ActorID {
					declined = append(declined, r.Start.In(service.BookingLocation).Format("Jan 2"))
				}
			}
			if len(declined) > 0 {
				message.Content += "\nNot approved: " + strings.Join(declined, ", ")
				for _, r := range rows {
					if r.Status == "declined" && r.Reason != "" {
						message.Content += " (" + escapeBookingText(r.Reason) + ")"
						break
					}
				}
			}
			message.Components = []discordgo.MessageComponent{myBookings}
		default:
			message.Content = fmt.Sprintf("✅ **Your office booking was approved.**\n**%s**\n%s", title, bookingWhen(b))
			message.Components = []discordgo.MessageComponent{manage(b.ID)}
		}
		message.Content += fmt.Sprintf("\nApproved by <@%s>", n.ActorID)
	case "declined":
		if original != nil {
			message.Content = fmt.Sprintf("❌ **Your time change for %s was declined.** You keep %s.%s", title, bookingWhen(*original), reason)
			message.Components = []discordgo.MessageComponent{myBookings}
		} else {
			message.Content = "❌ **Your office request was declined.**\n" + requestSummaryText(b, rows, nil) + reason
			message.Components = []discordgo.MessageComponent{bookAgain}
		}
	case "revoked":
		later := 0
		for _, r := range rows {
			if r.ID != b.ID && r.Status == "revoked" && r.Start.After(b.Start) && r.ReviewedBy == b.ReviewedBy {
				later++
			}
		}
		message.Content = fmt.Sprintf("⛔ **Your office booking was revoked.**\n**%s**\n%s", title, bookingWhen(b))
		if later > 0 {
			message.Content += fmt.Sprintf(" and %d later date(s)", later)
		}
		message.Content += reason
		message.Components = []discordgo.MessageComponent{bookAgain}
	case "expired":
		if original != nil {
			message.Content = fmt.Sprintf("⌛ **Your time change for %s wasn't decided in time.** Your original booking (%s) is unchanged.", title, bookingWhen(*original))
			message.Components = []discordgo.MessageComponent{myBookings}
		} else {
			message.Content = fmt.Sprintf("⌛ **Your office request expired** — no officer decided before it started.\n**%s**\n%s", title, bookingWhen(b))
			for _, r := range rows {
				if r.Status == "pending" {
					message.Content += "\nYour other dates are still waiting for approval."
					break
				}
			}
			message.Components = []discordgo.MessageComponent{bookAgain}
		}
	default:
		return nil
	}
	return message
}

// sendDM delivers to the member's DMs, or mentions them in the calendar
// channel when their DMs are closed.
func (c *Calendar) sendDM(s *discordgo.Session, n repository.BookingNotification, message *discordgo.MessageSend) (channel, id string, err error) {
	nonce := "bk:" + strconv.FormatInt(n.ID, 10)
	dm, err := s.UserChannelCreate(n.Booking.UserID)
	if err == nil {
		var sent *discordgo.Message
		if sent, err = sendWithNonce(s, dm.ID, nonce, message); err == nil {
			return dm.ID, sent.ID, nil
		}
	}
	if discordErrorCode(err) != discordgo.ErrCodeCannotSendMessagesToThisUser {
		return "", "", err
	}
	fallback := c.calendarChannel()
	if fallback == "" {
		return "", "", err
	}
	sent, err := sendWithNonce(s, fallback, nonce, &discordgo.MessageSend{
		Content:         fmt.Sprintf("<@%s> you have an office booking update, but I can't DM you. Allow DMs from server members (server menu ▸ Privacy Settings), then check `/bookings`.", n.Booking.UserID),
		AllowedMentions: &discordgo.MessageAllowedMentions{Users: []string{n.Booking.UserID}},
	})
	if err != nil {
		return "", "", err
	}
	return fallback, sent.ID, nil
}

func (c *Calendar) deliverBookingNotice(s *discordgo.Session, n repository.BookingNotification) error {
	b := n.Booking
	now := c.now()
	switch n.Event {
	case "pending":
		waiting := b.Status == "pending" && b.Start.After(now)
		if !waiting && b.SeriesID > 0 {
			if leads, err := c.pendingLeads(); err == nil {
				for _, r := range leads {
					waiting = waiting || r.first.Lead() == b.Lead()
				}
			}
		}
		if n.MessageID == "" && waiting {
			embed, buttons, err := c.requestCard(b)
			if err != nil {
				return err
			}
			roles := c.service.ApproverRoles(c.guildID)
			channel := c.officerChannel()
			sent, err := sendWithNonce(s, channel, "bk:"+strconv.FormatInt(n.ID, 10), &discordgo.MessageSend{
				Content:         fmt.Sprintf("%s <@%s> requested office time.", roleMentions(roles), b.UserID),
				Embeds:          []*discordgo.MessageEmbed{embed},
				Components:      []discordgo.MessageComponent{row(buttons...)},
				AllowedMentions: &discordgo.MessageAllowedMentions{Roles: roles},
			})
			if err != nil {
				return err
			}
			if err = c.service.Repo.SaveNotificationMessage(n.ID, channel, sent.ID); err != nil {
				return err
			}
		} else if err := c.updateRequestCard(s, b); err != nil {
			return err
		}
	case "reminder":
		if n.MessageID == "" && b.Status == "pending" && b.Start.After(now) {
			channel, card, err := c.service.Repo.RequestNotificationMessage(b.Lead())
			if err != nil {
				return err
			}
			if channel == "" {
				channel = c.officerChannel()
			}
			roles := c.service.ApproverRoles(c.guildID)
			message := &discordgo.MessageSend{
				Content:         fmt.Sprintf("%s ⏰ <@%s>'s request for %s starts <t:%d:R> and still needs a decision.", roleMentions(roles), b.UserID, bookingWhen(b), b.Start.Unix()),
				AllowedMentions: &discordgo.MessageAllowedMentions{Roles: roles},
			}
			if card != "" {
				message.Reference = &discordgo.MessageReference{MessageID: card, ChannelID: channel, FailIfNotExists: new(bool)}
			}
			sent, err := sendWithNonce(s, channel, "bk:"+strconv.FormatInt(n.ID, 10), message)
			if err != nil {
				return err
			}
			if err = c.service.Repo.SaveNotificationMessage(n.ID, channel, sent.ID); err != nil {
				return err
			}
		}
	default:
		if dm := c.decisionDM(n); dm != nil && n.MessageID == "" {
			channel, id, err := c.sendDM(s, n, dm)
			if err != nil {
				return err
			}
			if err = c.service.Repo.SaveNotificationMessage(n.ID, channel, id); err != nil {
				return err
			}
		}
		// Fetch current state in case someone acted during delivery.
		current, err := c.service.Repo.Get(b.GuildID, b.ID)
		if err != nil {
			return err
		}
		if err = c.updateRequestCard(s, *current); err != nil {
			return err
		}
	}
	return c.service.Repo.CompleteNotification(n.ID, now)
}

// DeliverNotifications is called after booking changes and every 30 seconds.
// Saved message IDs and nonces prevent a retry from pinging or DMing twice.
func (c *Calendar) DeliverNotifications(s *discordgo.Session) error {
	if c == nil || c.service == nil || c.guildID == "" {
		return nil
	}
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	notices, err := c.service.Repo.PendingNotifications(c.guildID, c.now())
	if err != nil {
		return err
	}
	var failures []error
	for _, n := range notices {
		if err := c.deliverBookingNotice(s, n); err != nil {
			failures = append(failures, err)
			log.Error("booking notification queued for retry", "booking_id", n.Booking.ID, "event", n.Event, "err", err)
			if retryErr := c.service.Repo.RetryNotification(n.ID, n.Attempts, c.now()); retryErr != nil {
				failures = append(failures, retryErr)
			}
		}
	}
	return errors.Join(failures...)
}
