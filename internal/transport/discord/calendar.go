package discord

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"office/internal/repository"
	"office/internal/service"
	"office/internal/transport/discord/calendarimage"

	"github.com/bwmarrin/discordgo"
)

// Calendar owns only office booking interactions, not attendance dashboards.
type Calendar struct {
	defaultChannelID string
	notifyMu         sync.Mutex
	service          *service.BookingService
	guildID          string
	mu               sync.Mutex // serialize shared message rendering and navigation
}

func NewCalendar(svc *service.BookingService, guildID string) *Calendar {
	return &Calendar{service: svc, guildID: guildID}
}

func calendarCommandDefinitions() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{Name: "calendar", Description: "Create or update this channel's shared office booking calendar", Options: []*discordgo.ApplicationCommandOption{
			{Name: "date", Description: "A date in the week to display, YYYY-MM-DD", Type: discordgo.ApplicationCommandOptionString},
		}},
		{Name: "book", Description: "Request office time for a Signing Officer to approve"},
		{Name: "bookings", Description: "View your booking requests and cancel upcoming reservations"},
		{Name: "booking-review", Description: "Approve or decline office requests (Signing Officer role required)"},
	}
}

func (c *Calendar) allowed(i *discordgo.InteractionCreate) bool {
	return c != nil && c.service != nil && i != nil && i.GuildID != "" && i.GuildID == c.guildID && i.Member != nil && i.Member.User != nil
}

func bookingRoles(i *discordgo.InteractionCreate) []string {
	if i.Member == nil {
		return nil
	}
	return i.Member.Roles
}

func calendarReply(s *discordgo.Session, i *discordgo.InteractionCreate, message string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Content: message, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}}}); err != nil {
		log.Error("calendar reply failed", "err", err)
	}
}

func calendarDefer(s *discordgo.Session, i *discordgo.InteractionCreate, update bool) bool {
	response := &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral}}
	if update {
		response.Type = discordgo.InteractionResponseDeferredMessageUpdate
		response.Data = nil
	}
	if err := s.InteractionRespond(i.Interaction, response); err != nil {
		log.Error("calendar acknowledgement failed", "err", err)
		return false
	}
	return true
}

func calendarFinish(s *discordgo.Session, i *discordgo.InteractionCreate, message string) {
	embeds := []*discordgo.MessageEmbed{}
	components := []discordgo.MessageComponent{}
	if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &message, Embeds: &embeds, Components: &components, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}}); err != nil {
		log.Error("calendar response edit failed", "err", err)
	}
}

func calendarError(err error) string {
	log.Error("calendar operation failed", "err", err)
	if errors.Is(err, sql.ErrNoRows) {
		return "That booking no longer exists in this server."
	}
	return service.BookingErrorMessage(err)
}

func (c *Calendar) HandleCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !c.allowed(i) {
		calendarReply(s, i, "Office bookings are available in the exec server.")
		return
	}
	switch i.ApplicationCommandData().Name {
	case "book":
		c.showForm(s, i)
	case "bookings":
		if calendarDefer(s, i, false) {
			c.showList(s, i, false, 0, "")
		}
	case "booking-review":
		if !service.IsSigningOfficer(bookingRoles(i)) {
			calendarReply(s, i, service.ErrBookingPermission.Error())
			return
		}
		if calendarDefer(s, i, false) {
			c.showList(s, i, true, 0, "")
		}
	case "calendar":
		week := service.BookingWeek(time.Now())
		for _, opt := range i.ApplicationCommandData().Options {
			if opt.Name == "date" {
				date, err := time.ParseInLocation("2006-01-02", opt.StringValue(), service.BookingLocation)
				if err != nil || date.Year() < 2000 || date.Year() > 2100 {
					calendarReply(s, i, "Use a date in YYYY-MM-DD format (2000–2100).")
					return
				}
				week = service.BookingWeek(date)
			}
		}
		if !calendarDefer(s, i, false) {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		boards, err := c.service.Repo.Calendars(c.guildID)
		if err != nil {
			calendarFinish(s, i, calendarError(err))
			return
		}
		board := repository.BookingCalendar{GuildID: c.guildID, ChannelID: i.ChannelID, Week: week.Format("2006-01-02")}
		for _, existing := range boards {
			if existing.ChannelID == i.ChannelID {
				board.MessageID = existing.MessageID
			}
		}
		msg, err := c.renderBoard(s, board)
		if isMissingDiscordMessage(err) {
			board.MessageID = ""
			msg, err = c.renderBoard(s, board)
		}
		if err != nil {
			calendarFinish(s, i, "Couldn't publish the calendar. Check that the bot can view this channel, send messages, embed links and attach files.")
			log.Error("publish calendar failed", "err", err)
			return
		}
		board.MessageID = msg.ID
		if err = c.service.Repo.SaveCalendar(board); err != nil {
			calendarFinish(s, i, "The calendar was posted, but saving it failed. Run /calendar again to retry.")
			log.Error("save calendar failed", "err", err)
			return
		}
		calendarFinish(s, i, fmt.Sprintf("Office calendar ready: https://discord.com/channels/%s/%s/%s", i.GuildID, i.ChannelID, msg.ID))
	}
}

func (c *Calendar) showForm(s *discordgo.Session, i *discordgo.InteractionCreate) {
	fields := []struct {
		id, label, placeholder, value string
		max                           int
	}{
		{"title", "What are you using the office for?", "e.g. Committee meeting", "", 80},
		{"date", "Date (YYYY-MM-DD)", "YYYY-MM-DD", time.Now().In(service.BookingLocation).Format("2006-01-02"), 10},
		{"start", "Start time (24-hour)", "14:00", "", 5},
		{"end", "End time (24-hour)", "16:00 (or 24:00 for midnight)", "", 5},
	}
	components := []discordgo.MessageComponent{}
	for _, f := range fields {
		components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{CustomID: f.id, Label: f.label, Style: discordgo.TextInputShort, Placeholder: f.placeholder, Value: f.value, Required: true, MaxLength: f.max}}})
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseModal, Data: &discordgo.InteractionResponseData{CustomID: "office:request", Title: "Book office time", Components: components}}); err != nil {
		log.Error("booking modal failed", "err", err)
	}
}

func bookingFormValues(components []discordgo.MessageComponent) map[string]string {
	values := map[string]string{}
	for _, component := range components {
		row, ok := component.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, child := range row.Components {
			if input, ok := child.(*discordgo.TextInput); ok {
				values[input.CustomID] = input.Value
			}
		}
	}
	return values
}

func (c *Calendar) HandleModal(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.ModalSubmitData().CustomID != "office:request" {
		return
	}
	if !c.allowed(i) {
		calendarReply(s, i, "Office bookings are available in the exec server.")
		return
	}
	if !calendarDefer(s, i, false) {
		return
	}
	values := bookingFormValues(i.ModalSubmitData().Components)
	user := i.Member.User
	name := i.Member.Nick
	if name == "" {
		name = user.GlobalName
	}
	if name == "" {
		name = user.Username
	}
	b, err := c.service.Request(i.ID, i.GuildID, user.ID, name, values["title"], values["date"], values["start"], values["end"], time.Now())
	if err != nil {
		calendarFinish(s, i, calendarError(err)+" Reopen Book office time to correct your request.")
		return
	}
	calendarFinish(s, i, fmt.Sprintf("Booking request #%d · **%s**\n%s", b.ID, b.Status, bookingWhen(*b)))
	c.Refresh(s)
}

func (c *Calendar) HandleButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !c.allowed(i) {
		calendarReply(s, i, "Office bookings are available in the exec server.")
		return
	}
	parts := strings.Split(i.MessageComponentData().CustomID, ":")
	if len(parts) < 2 {
		return
	}
	action := parts[1]
	if action == "book" {
		c.showForm(s, i)
		return
	}
	if action == "week" {
		if len(parts) != 3 && (len(parts) != 4 || (parts[3] != "today" && parts[3] != "refresh")) {
			calendarReply(s, i, "Invalid calendar date.")
			return
		}
		week, err := time.ParseInLocation("2006-01-02", parts[2], service.BookingLocation)
		if err != nil || week.Year() < 2000 || week.Year() > 2100 {
			calendarReply(s, i, "Invalid calendar date.")
			return
		}
		if !calendarDefer(s, i, true) {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		boards, err := c.service.Repo.Calendars(c.guildID)
		if err == nil {
			for _, board := range boards {
				if i.Message != nil && board.ChannelID == i.ChannelID && board.MessageID == i.Message.ID {
					board.Week = service.BookingWeek(week).Format("2006-01-02")
					if _, err = c.renderBoard(s, board); err == nil {
						err = c.service.Repo.SaveCalendar(board)
					}
					if err != nil {
						c.followup(s, i, "Couldn't refresh the calendar. Try again.")
						log.Error("calendar navigation failed", "err", err)
					}
					return
				}
			}
		}
		c.followup(s, i, "This calendar is no longer tracked. Run /calendar in this channel to restore it.")
		return
	}
	review := action == "review" || action == "approve" || action == "decline"
	if review && !service.IsSigningOfficer(bookingRoles(i)) {
		calendarReply(s, i, service.ErrBookingPermission.Error())
		return
	}
	if action != "review" && action != "mine" && action != "approve" && action != "decline" && action != "cancel" {
		calendarReply(s, i, "Unknown booking action.")
		return
	}
	offset := 0
	if action == "review" || action == "mine" {
		if len(parts) > 3 {
			calendarReply(s, i, "Invalid booking page.")
			return
		}
		if len(parts) == 3 {
			var err error
			offset, err = strconv.Atoi(parts[2])
			if err != nil || offset < 0 {
				calendarReply(s, i, "Invalid booking page.")
				return
			}
		}
		// Bare buttons live on the public calendar; pagination lives on private cards.
		update := len(parts) == 3 && isPrivateBookingMessage(i)
		if calendarDefer(s, i, update) {
			c.showList(s, i, review, offset, "")
		}
		return
	}
	if len(parts) != 4 {
		calendarReply(s, i, "Invalid booking action.")
		return
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	offset, offsetErr := strconv.Atoi(parts[3])
	if err != nil || offsetErr != nil || id <= 0 || offset < 0 {
		calendarReply(s, i, "Invalid booking action.")
		return
	}
	if !calendarDefer(s, i, isPrivateBookingMessage(i)) {
		return
	}
	user := i.Member.User.ID
	if action == "cancel" {
		err = c.service.Cancel(i.GuildID, id, user, time.Now())
	} else {
		err = c.service.Decide(i.GuildID, id, user, bookingRoles(i), action == "approve", time.Now())
	}
	notice := fmt.Sprintf("Booking #%d %s.", id, map[string]string{"approve": "approved", "decline": "declined", "cancel": "cancelled"}[action])
	if err != nil {
		notice = calendarError(err)
	}
	c.showList(s, i, review, offset, notice)
	if err == nil {
		c.Refresh(s)
	}
}

func isPrivateBookingMessage(i *discordgo.InteractionCreate) bool {
	return i.Message != nil && i.Message.Flags&discordgo.MessageFlagsEphemeral != 0
}

func (c *Calendar) followup(s *discordgo.Session, i *discordgo.InteractionCreate, message string) {
	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{Content: message, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}}); err != nil {
		log.Error("calendar followup failed", "err", err)
	}
}

func calendarButton(label, id string, style discordgo.ButtonStyle, disabled bool) discordgo.MessageComponent {
	return discordgo.Button{Label: label, CustomID: id, Style: style, Disabled: disabled}
}

func (c *Calendar) showList(s *discordgo.Session, i *discordgo.InteractionCreate, review bool, offset int, notice string) {
	var bookings []repository.Booking
	var err error
	if review {
		bookings, err = c.service.Repo.Pending(i.GuildID, time.Now())
	} else {
		bookings, err = c.service.Repo.Mine(i.GuildID, i.Member.User.ID, time.Now())
	}
	if err != nil {
		calendarFinish(s, i, calendarError(err))
		return
	}
	label, prefix := "My bookings", "mine"
	if review {
		label, prefix = "Pending office requests", "review"
	}
	embed := &discordgo.MessageEmbed{Title: label, Color: 0x2D6AE3, Description: "No bookings to show."}
	components := []discordgo.MessageComponent{}
	if len(bookings) > 0 {
		if offset >= len(bookings) {
			offset = len(bookings) - 1
		}
		if offset < 0 {
			offset = 0
		}
		b := bookings[offset]
		embed.Title = fmt.Sprintf("%s · %d of %d", label, offset+1, len(bookings))
		embed.Description = fmt.Sprintf("**%s**\n%s\n\n**Booked by:** %s (<@%s>)\n**Status:** %s\n**Booking ID:** #%d", escapeBookingText(b.Title), bookingWhen(b), escapeBookingText(b.Name), b.UserID, strings.ToUpper(b.Status), b.ID)
		if b.Status == "pending" && !b.Start.After(time.Now()) {
			embed.Description += "\nThis request expired before approval."
		}
		if b.ReviewedBy != "" {
			embed.Description += "\n**Last action by:** <@" + b.ReviewedBy + ">"
		}
		actions := []discordgo.MessageComponent{}
		if review {
			actions = append(actions, calendarButton("Approve", fmt.Sprintf("office:approve:%d:%d", b.ID, offset), discordgo.SuccessButton, false), calendarButton("Decline", fmt.Sprintf("office:decline:%d:%d", b.ID, offset), discordgo.DangerButton, false))
		} else if (b.Status == "pending" || b.Status == "approved") && b.Start.After(time.Now()) {
			actions = append(actions, calendarButton("Cancel my booking", fmt.Sprintf("office:cancel:%d:%d", b.ID, offset), discordgo.DangerButton, false))
		}
		if len(actions) > 0 {
			components = append(components, discordgo.ActionsRow{Components: actions})
		}
		components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			calendarButton("Previous", fmt.Sprintf("office:%s:%d", prefix, max(0, offset-1)), discordgo.SecondaryButton, offset == 0),
			calendarButton("Refresh", fmt.Sprintf("office:%s:%d", prefix, offset), discordgo.SecondaryButton, false),
			calendarButton("Next", fmt.Sprintf("office:%s:%d", prefix, offset+1), discordgo.SecondaryButton, offset >= len(bookings)-1),
		}})
	} else {
		components = append(components, discordgo.ActionsRow{Components: []discordgo.MessageComponent{calendarButton("Refresh", "office:"+prefix+":0", discordgo.SecondaryButton, false)}})
	}
	embeds := []*discordgo.MessageEmbed{embed}
	if _, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &notice, Embeds: &embeds, Components: &components, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}}); err != nil {
		log.Error("booking list edit failed", "err", err)
	}
}

func bookingWhen(b repository.Booking) string {
	start, end := b.Start.In(service.BookingLocation), b.End.In(service.BookingLocation)
	endText := end.Format("15:04")
	if end.Format("2006-01-02") != start.Format("2006-01-02") {
		endText = "24:00"
	}
	return start.Format("Mon, 2 Jan 2006") + " · " + start.Format("15:04") + "–" + endText
}

func escapeBookingText(s string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`", "~", "\\~", "|", "\\|", "<", "‹", ">", "›").Replace(s)
}

func calendarComponents(week time.Time, pending int) []discordgo.MessageComponent {
	weekID := func(t time.Time) string { return "office:week:" + t.Format("2006-01-02") }
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			calendarButton("← Previous week", weekID(week.AddDate(0, 0, -7)), discordgo.SecondaryButton, false),
			calendarButton("This week", weekID(service.BookingWeek(time.Now()))+":today", discordgo.PrimaryButton, false),
			calendarButton("Next week →", weekID(week.AddDate(0, 0, 7)), discordgo.SecondaryButton, false),
			calendarButton("Refresh", weekID(week)+":refresh", discordgo.SecondaryButton, false),
		}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			calendarButton("Book office time", "office:book", discordgo.SuccessButton, false),
			calendarButton("My bookings", "office:mine", discordgo.SecondaryButton, false),
			calendarButton(fmt.Sprintf("Review requests (%d)", pending), "office:review", discordgo.SecondaryButton, false),
		}},
	}
}

func (c *Calendar) renderBoard(s *discordgo.Session, board repository.BookingCalendar) (*discordgo.Message, error) {
	week, err := time.ParseInLocation("2006-01-02", board.Week, service.BookingLocation)
	if err != nil {
		return nil, err
	}
	bookings, err := c.service.Repo.Approved(c.guildID, week, week.AddDate(0, 0, 7))
	if err != nil {
		return nil, err
	}
	pending, err := c.service.Repo.Pending(c.guildID, time.Now())
	if err != nil {
		return nil, err
	}
	png, err := calendarimage.Render(week, bookings, time.Now())
	if err != nil {
		return nil, err
	}
	components := calendarComponents(week, len(pending))
	dateRange := week.Format("January 2") + " – " + week.AddDate(0, 0, 6).Format("January 2, 2006")
	embeds := []*discordgo.MessageEmbed{{Title: dateRange, Image: &discordgo.MessageEmbedImage{URL: "attachment://office-calendar.png"}}}
	files := []*discordgo.File{{Name: "office-calendar.png", ContentType: "image/png", Reader: bytes.NewReader(png)}}
	if board.MessageID == "" {
		return s.ChannelMessageSendComplex(board.ChannelID, &discordgo.MessageSend{Embeds: embeds, Components: components, Files: files, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}})
	}
	attachments := []*discordgo.MessageAttachment{}
	return s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: board.MessageID, Channel: board.ChannelID, Embeds: &embeds, Components: &components, Files: files, Attachments: &attachments, AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}})
}

func isMissingDiscordMessage(err error) bool {
	var rest *discordgo.RESTError
	return errors.As(err, &rest) && rest.Message != nil && rest.Message.Code == 10008
}

// SetDefaultChannel makes the office-tracker calendar a managed dashboard.
func (c *Calendar) SetDefaultChannel(channelID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultChannelID = channelID
}

// Refresh creates the default calendar when absent and refreshes saved boards.
func (c *Calendar) Refresh(s *discordgo.Session) {
	if c == nil || c.service == nil || c.guildID == "" {
		return
	}
	if err := c.DeliverNotifications(s); err != nil {
		log.Error("booking notification delivery incomplete", "err", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	boards, err := c.service.Repo.Calendars(c.guildID)
	if err != nil {
		log.Error("load booking calendars failed", "err", err)
		return
	}
	hasDefault := false
	for _, board := range boards {
		if board.ChannelID == c.defaultChannelID {
			hasDefault = true
		}
	}
	if c.defaultChannelID != "" && !hasDefault {
		boards = append(boards, repository.BookingCalendar{
			GuildID: c.guildID, ChannelID: c.defaultChannelID, Week: service.BookingWeek(time.Now()).Format("2006-01-02"),
		})
	}
	for _, board := range boards {
		msg, err := c.renderBoard(s, board)
		if isMissingDiscordMessage(err) {
			if board.ChannelID == c.defaultChannelID {
				board.MessageID = ""
				msg, err = c.renderBoard(s, board)
			} else {
				if err = c.service.Repo.DeleteCalendar(board.ChannelID, board.MessageID); err != nil {
					log.Error("delete missing booking calendar failed", "err", err)
				}
				continue
			}
		}
		if err != nil {
			log.Error("refresh booking calendar failed", "channel", board.ChannelID, "err", err)
			continue
		}
		if msg.ID != board.MessageID {
			board.MessageID = msg.ID
			if err = c.service.Repo.SaveCalendar(board); err != nil {
				log.Error("save automatic booking calendar failed", "err", err)
			}
		}
	}
}
