package discord

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"office/internal/repository"
	"office/internal/service"
	"office/internal/transport/discord/calendarimage"

	"github.com/bwmarrin/discordgo"
)

// Calendar owns office booking interactions, not attendance dashboards.
type Calendar struct {
	defaultChannelID string
	notifyMu         sync.Mutex
	service          *service.BookingService
	guildID          string
	mu               sync.Mutex // serialize shared message rendering
	draftsMu         sync.Mutex
	drafts           map[string]*bookingDraft
	now              func() time.Time
}

func NewCalendar(svc *service.BookingService, guildID string) *Calendar {
	return &Calendar{service: svc, guildID: guildID, drafts: map[string]*bookingDraft{}, now: time.Now}
}

// bookingActor is the member behind an interaction, resolved from the exec
// server even when they click a button in a DM.
type bookingActor struct {
	ID, Name string
	Roles    []string
	Perms    int64
	access   service.Access
}

func memberName(m *discordgo.Member) string {
	if m.Nick != "" {
		return m.Nick
	}
	if m.User.GlobalName != "" {
		return m.User.GlobalName
	}
	return m.User.Username
}

func (c *Calendar) actor(s *discordgo.Session, i *discordgo.InteractionCreate) (*bookingActor, error) {
	if c == nil || c.service == nil || i == nil {
		return nil, errNotExecServer
	}
	member := i.Member
	switch {
	case i.GuildID == c.guildID && c.guildID != "" && member != nil && member.User != nil:
	case i.GuildID == "" && i.User != nil:
		var err error
		if member, err = s.State.Member(c.guildID, i.User.ID); err != nil || member == nil {
			if member, err = s.GuildMember(c.guildID, i.User.ID); err != nil {
				return nil, errNotExecServer
			}
		}
		if member.User == nil {
			member.User = i.User
		}
	default:
		return nil, errNotExecServer
	}
	a := &bookingActor{ID: member.User.ID, Name: memberName(member), Roles: service.MemberGrants(member.User.ID, member.Roles), Perms: member.Permissions}
	var err error
	a.access, err = c.service.Access(c.guildID, a.Roles)
	return a, err
}

var errNotExecServer = errors.New("office bookings are available to members of the exec server")

func (a *bookingActor) isAdmin() bool {
	return a.Perms&(discordgo.PermissionAdministrator|discordgo.PermissionManageGuild) != 0
}

func noMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

// view is a private (ephemeral) bot response.
type view struct {
	content    string
	embeds     []*discordgo.MessageEmbed
	components []discordgo.MessageComponent
	files      []*discordgo.File
}

func isPrivateBookingMessage(i *discordgo.InteractionCreate) bool {
	return i.Message != nil && i.Message.Flags&discordgo.MessageFlagsEphemeral != 0
}

// show replaces the private message the member clicked on, or otherwise sends
// a new private message (from the shared calendar, an officer card or a DM).
func show(s *discordgo.Session, i *discordgo.InteractionCreate, v view) {
	typ := discordgo.InteractionResponseChannelMessageWithSource
	if (i.Type == discordgo.InteractionMessageComponent || i.Type == discordgo.InteractionModalSubmit) && isPrivateBookingMessage(i) {
		typ = discordgo.InteractionResponseUpdateMessage
	}
	respond(s, i, typ, v)
}

// showNew always sends a new private message.
func showNew(s *discordgo.Session, i *discordgo.InteractionCreate, v view) {
	respond(s, i, discordgo.InteractionResponseChannelMessageWithSource, v)
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, typ discordgo.InteractionResponseType, v view) {
	if v.embeds == nil {
		v.embeds = []*discordgo.MessageEmbed{}
	}
	if v.components == nil {
		v.components = []discordgo.MessageComponent{}
	}
	data := &discordgo.InteractionResponseData{Content: v.content, Embeds: v.embeds, Components: v.components, Files: v.files, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions()}
	if typ == discordgo.InteractionResponseUpdateMessage {
		attachments := []*discordgo.MessageAttachment{}
		data.Attachments = &attachments
		data.Flags = 0
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: typ, Data: data}); err != nil {
		log.Error("booking response failed", "err", err)
	}
}

func notice(s *discordgo.Session, i *discordgo.InteractionCreate, message string) {
	showNew(s, i, view{content: message})
}

func bookingError(err error) string {
	if errors.Is(err, errNotExecServer) {
		return "Office bookings are available to members of the exec server."
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "That booking no longer exists."
	}
	if msg := service.BookingErrorMessage(err); msg != "Couldn't save the change. Please try again." {
		return msg
	}
	log.Error("booking operation failed", "err", err)
	return "Couldn't save the change. Please try again."
}

func button(label, id string, style discordgo.ButtonStyle) discordgo.Button {
	return discordgo.Button{Label: label, CustomID: id, Style: style}
}

func row(components ...discordgo.MessageComponent) discordgo.ActionsRow {
	return discordgo.ActionsRow{Components: components}
}

func escapeBookingText(s string) string {
	return strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`", "~", "\\~", "|", "\\|", "<", "‹", ">", "›", "@", "@\u200b").Replace(s)
}

func bookingWhen(b repository.Booking) string { return service.FormatRange(b.Start, b.End) }

// ---- shared calendar ----

func (c *Calendar) calendarChannel() string {
	return c.service.Setting(c.guildID, service.SettingCalendarChannel, c.defaultChannelID)
}

func (c *Calendar) officerChannel() string {
	return c.service.Setting(c.guildID, service.SettingOfficerChannel, service.DefaultOfficerChannelID)
}

func calendarComponents(pending int) []discordgo.MessageComponent {
	review := "Review requests"
	if pending > 0 {
		review = fmt.Sprintf("Review requests (%d)", pending)
	}
	return []discordgo.MessageComponent{row(
		button("Book office time", "office:book", discordgo.SuccessButton),
		button("My bookings", "office:mine", discordgo.SecondaryButton),
		button("Other weeks", "office:weeks", discordgo.SecondaryButton),
		button(review, "office:review", discordgo.SecondaryButton),
	)}
}

// weekView renders a week's image and a text agenda of held and approved bookings.
func (c *Calendar) weekView(week time.Time) (*discordgo.MessageEmbed, *discordgo.File, error) {
	week = service.BookingWeek(week)
	bookings, err := c.service.Repo.Busy(c.guildID, week, week.AddDate(0, 0, 7))
	if err != nil {
		return nil, nil, err
	}
	png, err := calendarimage.Render(week, bookings, c.now())
	if err != nil {
		return nil, nil, err
	}
	title := week.Format("Jan 2") + " – " + week.AddDate(0, 0, 6).Format("Jan 2, 2006")
	embed := &discordgo.MessageEmbed{Title: title, Color: 0x2D6AE3, Description: weekAgenda(bookings), Image: &discordgo.MessageEmbedImage{URL: "attachment://office-calendar.png"}}
	return embed, &discordgo.File{Name: "office-calendar.png", ContentType: "image/png", Reader: bytes.NewReader(png)}, nil
}

func weekAgenda(bookings []repository.Booking) string {
	if len(bookings) == 0 {
		return "The office is free all week."
	}
	var out strings.Builder
	day := ""
	for _, b := range bookings {
		if d := service.FormatDay(b.Start); d != day {
			day = d
			fmt.Fprintf(&out, "\n**%s**\n", d)
		}
		when := service.FormatClock(b.Start) + " – " + service.FormatClock(b.End)
		if b.End.Equal(service.BookingDay(b.Start).AddDate(0, 0, 1)) {
			when = service.FormatClock(b.Start) + " – midnight"
		}
		line := fmt.Sprintf("%s · %s — %s", when, escapeBookingText(b.Name), escapeBookingText(b.Title))
		if b.Status == "pending" {
			line += " *(pending)*"
		}
		if out.Len()+len(line) > 3800 {
			out.WriteString("…")
			break
		}
		out.WriteString(line + "\n")
	}
	return strings.TrimSpace(out.String())
}

func (c *Calendar) renderBoard(s *discordgo.Session, board repository.BookingCalendar) (*discordgo.Message, error) {
	embed, file, err := c.weekView(c.now())
	if err != nil {
		return nil, err
	}
	pending, err := c.pendingLeads()
	if err != nil {
		return nil, err
	}
	embeds := []*discordgo.MessageEmbed{embed}
	components := calendarComponents(len(pending))
	files := []*discordgo.File{file}
	if board.MessageID == "" {
		return s.ChannelMessageSendComplex(board.ChannelID, &discordgo.MessageSend{Embeds: embeds, Components: components, Files: files, AllowedMentions: noMentions()})
	}
	attachments := []*discordgo.MessageAttachment{}
	return s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: board.MessageID, Channel: board.ChannelID, Embeds: &embeds, Components: &components, Files: files, Attachments: &attachments, AllowedMentions: noMentions()})
}

func isMissingDiscordMessage(err error) bool {
	var rest *discordgo.RESTError
	return errors.As(err, &rest) && rest.Message != nil && rest.Message.Code == discordgo.ErrCodeUnknownMessage
}

// SetDefaultChannel sets the calendar channel used until /office-admin picks one.
func (c *Calendar) SetDefaultChannel(channelID string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultChannelID = channelID
}

// Refresh delivers notifications and redraws the one shared calendar,
// removing calendars left in other channels.
func (c *Calendar) Refresh(s *discordgo.Session) {
	if c == nil || c.service == nil || c.guildID == "" {
		return
	}
	if err := c.DeliverNotifications(s); err != nil {
		log.Error("booking notification delivery incomplete", "err", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	channel := c.calendarChannel()
	boards, err := c.service.Repo.Calendars(c.guildID)
	if err != nil {
		log.Error("load booking calendars failed", "err", err)
		return
	}
	board := repository.BookingCalendar{GuildID: c.guildID, ChannelID: channel}
	for _, existing := range boards {
		if existing.ChannelID == channel {
			board.MessageID = existing.MessageID
			continue
		}
		if err := s.ChannelMessageDelete(existing.ChannelID, existing.MessageID); err != nil && !isMissingDiscordMessage(err) {
			log.Warn("remove old booking calendar failed", "channel", existing.ChannelID, "err", err)
		}
		if err := c.service.Repo.DeleteCalendar(existing.ChannelID, existing.MessageID); err != nil {
			log.Error("forget old booking calendar failed", "err", err)
		}
	}
	if channel == "" {
		return
	}
	board.Week = service.BookingWeek(c.now()).Format("2006-01-02")
	msg, err := c.renderBoard(s, board)
	if isMissingDiscordMessage(err) {
		board.MessageID = ""
		msg, err = c.renderBoard(s, board)
	}
	if err != nil {
		log.Error("refresh booking calendar failed", "channel", channel, "err", err)
		return
	}
	board.MessageID = msg.ID
	if err = c.service.Repo.SaveCalendar(board); err != nil {
		log.Error("save booking calendar failed", "err", err)
	}
}

// Maintain expires undecided requests, queues reminders and delivers
// notifications; it redraws the calendar when bookings changed.
func (c *Calendar) Maintain(s *discordgo.Session) {
	if c == nil || c.service == nil || c.guildID == "" {
		return
	}
	changed, err := c.service.Maintain(c.guildID, c.now())
	if err != nil {
		log.Error("booking maintenance failed", "err", err)
	}
	c.pruneDrafts()
	if changed {
		c.Refresh(s)
		return
	}
	if err := c.DeliverNotifications(s); err != nil {
		log.Error("booking notification retry failed", "err", err)
	}
}

// showWeek shows a private, navigable copy of the calendar.
func (c *Calendar) showWeek(s *discordgo.Session, i *discordgo.InteractionCreate, week time.Time) {
	week = service.BookingWeek(week)
	embed, file, err := c.weekView(week)
	if err != nil {
		notice(s, i, bookingError(err))
		return
	}
	id := func(t time.Time) string { return "office:view:" + t.Format("2006-01-02") }
	this := service.BookingWeek(c.now())
	thisButton := button("This week", id(this)+":now", discordgo.PrimaryButton)
	thisButton.Disabled = week.Equal(this)
	show(s, i, view{embeds: []*discordgo.MessageEmbed{embed}, files: []*discordgo.File{file}, components: []discordgo.MessageComponent{row(
		button("← Previous", id(week.AddDate(0, 0, -7)), discordgo.SecondaryButton),
		thisButton,
		button("Next →", id(week.AddDate(0, 0, 7)), discordgo.SecondaryButton),
		button("Book office time", "office:book", discordgo.SuccessButton),
	)}})
}
