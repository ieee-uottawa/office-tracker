package discord

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"office/internal/repository"
	"office/internal/service"

	"github.com/bwmarrin/discordgo"
)

const BookingNotificationChannelID = "1555060913147486269"

func bookingNoticeEmbed(b repository.Booking, status string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("Office booking #%d · %s", b.ID, strings.ToUpper(status[:1])+status[1:]),
		Description: "**" + escapeBookingText(b.Title) + "**\n" + bookingWhen(b),
	}
}

func bookingNoticeMessage(n repository.BookingNotification) *discordgo.MessageSend {
	b := n.Booking
	message := &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{bookingNoticeEmbed(b, n.Event)},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		Components:      []discordgo.MessageComponent{},
	}
	switch n.Event {
	case "pending":
		message.Content = fmt.Sprintf("<@&%s> <@%s> requested office time.", service.SigningOfficerRoleID, b.UserID)
		message.AllowedMentions.Roles = []string{service.SigningOfficerRoleID}
		message.Components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			calendarButton("Approve", fmt.Sprintf("office:approve:%d:0", b.ID), discordgo.SuccessButton, false),
			calendarButton("Decline", fmt.Sprintf("office:decline:%d:0", b.ID), discordgo.DangerButton, false),
		}}}
	case "approved", "declined":
		message.Content = fmt.Sprintf("<@%s> Your office booking was **%s** by <@%s>.", b.UserID, n.Event, n.ActorID)
		message.AllowedMentions.Users = []string{b.UserID}
	case "cancelled":
		message.Content = fmt.Sprintf("<@%s> cancelled their office booking.", b.UserID)
	}
	return message
}

// discordgo v0.29 predates the nonce fields; use its normal authenticated,
// rate-limited request helper while including Discord's duplicate protection.
func sendBookingNotice(s *discordgo.Session, n repository.BookingNotification) (*discordgo.Message, error) {
	payload := struct {
		*discordgo.MessageSend
		Nonce        string `json:"nonce"`
		EnforceNonce bool   `json:"enforce_nonce"`
	}{bookingNoticeMessage(n), "bk:" + strconv.FormatInt(n.ID, 10), true}
	endpoint := discordgo.EndpointChannelMessages(BookingNotificationChannelID)
	data, err := s.RequestWithBucketID("POST", endpoint, payload, endpoint)
	if err != nil {
		return nil, err
	}
	var message discordgo.Message
	if err = json.Unmarshal(data, &message); err != nil {
		return nil, err
	}
	if message.ID == "" {
		return nil, errors.New("Discord returned no notification message ID")
	}
	return &message, nil
}

func (c *Calendar) updateRequestNotice(s *discordgo.Session, b repository.Booking) error {
	channel, message, err := c.service.Repo.RequestNotificationMessage(b.ID)
	if err != nil || message == "" {
		return err
	}
	status := b.Status
	if status == "pending" && !b.Start.After(time.Now()) {
		status = "expired"
	}
	content := fmt.Sprintf("Office booking by <@%s> · **%s**", b.UserID, status)
	embeds := []*discordgo.MessageEmbed{bookingNoticeEmbed(b, status)}
	components := []discordgo.MessageComponent{}
	_, err = s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		ID: message, Channel: channel, Content: &content, Embeds: &embeds, Components: &components,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
	if isMissingDiscordMessage(err) {
		return nil
	}
	return err
}

func (c *Calendar) deliverBookingNotice(s *discordgo.Session, n repository.BookingNotification) error {
	// Do not ping officers for a request that was resolved or expired while
	// Discord was unavailable. Decision notifications still go to the requester.
	if n.Event == "pending" && (n.Booking.Status != "pending" || !n.Booking.Start.After(time.Now())) {
		if err := c.updateRequestNotice(s, n.Booking); err != nil {
			return err
		}
		return c.service.Repo.CompleteNotification(n.ID, time.Now())
	}
	if n.MessageID == "" {
		message, err := sendBookingNotice(s, n)
		if err != nil {
			return err
		}
		if err = c.service.Repo.SaveNotificationMessage(n.ID, BookingNotificationChannelID, message.ID); err != nil {
			return err
		}
	}
	if n.Event != "pending" {
		// Fetch current state in case another officer/user acted during delivery.
		b, err := c.service.Repo.Get(n.Booking.GuildID, n.Booking.ID)
		if err != nil {
			return err
		}
		if err = c.updateRequestNotice(s, *b); err != nil {
			return err
		}
	}
	return c.service.Repo.CompleteNotification(n.ID, time.Now())
}

// DeliverNotifications is called immediately after booking changes and every
// 30 seconds. Sent message IDs prevent a retry of a failed edit from pinging again.
func (c *Calendar) DeliverNotifications(s *discordgo.Session) error {
	if c == nil || c.service == nil || c.guildID == "" {
		return nil
	}
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	notices, err := c.service.Repo.PendingNotifications(c.guildID, time.Now())
	if err != nil {
		return err
	}
	var failures []error
	for _, n := range notices {
		if err := c.deliverBookingNotice(s, n); err != nil {
			failures = append(failures, err)
			log.Error("booking notification queued for retry", "booking_id", n.Booking.ID, "event", n.Event, "err", err)
			if retryErr := c.service.Repo.RetryNotification(n.ID, n.Attempts, time.Now()); retryErr != nil {
				failures = append(failures, retryErr)
			}
		}
	}
	return errors.Join(failures...)
}
