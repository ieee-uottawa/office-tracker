package discord

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"office/internal/service"

	"github.com/bwmarrin/discordgo"
)

var manageServerPermission = int64(discordgo.PermissionManageGuild)

func calendarCommandDefinitions() []*discordgo.ApplicationCommand {
	text := func(name, description string, required, autocomplete bool) *discordgo.ApplicationCommandOption {
		return &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: name, Description: description, Required: required, Autocomplete: autocomplete}
	}
	purpose := text("purpose", "What the office is for, e.g. WIE committee meeting", true, false)
	purpose.MaxLength = 80
	capability := &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionString, Name: "capability", Description: "What the role can do", Required: true,
		Choices: []*discordgo.ApplicationCommandOptionChoice{{Name: "book — request office time", Value: service.CapabilityBook}, {Name: "approve — approve and decline requests", Value: service.CapabilityApprove}}}
	role := &discordgo.ApplicationCommandOption{Type: discordgo.ApplicationCommandOptionRole, Name: "role", Description: "Server role", Required: true}
	return []*discordgo.ApplicationCommand{
		{Name: "book", Description: "Request office time", Options: []*discordgo.ApplicationCommandOption{
			text("date", "Day, e.g. tomorrow, fri, nov 20", true, true),
			text("start", "Start time (only free times are suggested)", true, true),
			text("end", "End time (only times that fit are suggested)", true, true),
			purpose,
			{Type: discordgo.ApplicationCommandOptionString, Name: "repeat", Description: "Repeat the booking", Choices: []*discordgo.ApplicationCommandOptionChoice{{Name: "Every week", Value: "weekly"}, {Name: "Every 2 weeks", Value: "biweekly"}}},
			text("until", "Last date of a repeating booking", false, true),
		}},
		{Name: "bookings", Description: "See, change or cancel your office bookings"},
		{Name: "calendar", Description: "See the office calendar privately", Options: []*discordgo.ApplicationCommandOption{
			text("date", "A day in the week to show, e.g. next fri or dec 3", false, true),
		}},
		{Name: "office-review", Description: "Approve, decline or revoke office bookings (approvers only)"},
		{Name: "office-admin", Description: "Configure office booking roles and channels", DefaultMemberPermissions: &manageServerPermission, Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionSubCommandGroup, Name: "roles", Description: "Who can book and approve", Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "add", Description: "Let a role book or approve", Options: []*discordgo.ApplicationCommandOption{role, capability}},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "remove", Description: "Stop a role booking or approving", Options: []*discordgo.ApplicationCommandOption{role, capability}},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "list", Description: "Show which roles can book and approve"},
			}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "channel", Description: "Set where the calendar or the officer request cards go", Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "type", Description: "Which channel", Required: true, Choices: []*discordgo.ApplicationCommandOptionChoice{{Name: "calendar — shared office calendar", Value: "calendar"}, {Name: "officers — request cards for approvers", Value: "officers"}}},
				{Type: discordgo.ApplicationCommandOptionChannel, Name: "channel", Description: "Text channel", Required: true, ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText}},
			}},
			{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status", Description: "Show the booking setup and check the bot's channel permissions"},
		}},
	}
}

// IsBookingCommand reports whether the calendar handles a slash command.
func IsBookingCommand(name string) bool {
	switch name {
	case "book", "bookings", "calendar", "office-review", "office-admin":
		return true
	}
	return false
}

func (c *Calendar) HandleCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	a, err := c.actor(s, i)
	if err != nil {
		notice(s, i, bookingError(err))
		return
	}
	data := i.ApplicationCommandData()
	switch data.Name {
	case "book":
		c.handleBookCommand(s, i, a)
	case "bookings":
		showNew(s, i, c.myBookingsView(a, ""))
	case "calendar":
		week := c.now()
		for _, opt := range data.Options {
			if opt.Name == "date" {
				day, err := service.ParseBookingDate(opt.StringValue(), c.now())
				if err != nil {
					notice(s, i, bookingError(err))
					return
				}
				week = day
			}
		}
		c.showWeek(s, i, week)
	case "office-review":
		if !a.access.Approve {
			notice(s, i, bookingError(service.ErrBookingPermission))
			return
		}
		showNew(s, i, c.reviewView(""))
	case "office-admin":
		c.handleAdmin(s, i, a)
	}
}

func parseID(text string) (int64, bool) {
	id, err := strconv.ParseInt(text, 10, 64)
	return id, err == nil && id > 0
}

// HandleButton routes buttons and select menus whose custom ID starts with "office:".
func (c *Calendar) HandleButton(s *discordgo.Session, i *discordgo.InteractionCreate) {
	a, err := c.actor(s, i)
	if err != nil {
		notice(s, i, bookingError(err))
		return
	}
	parts := strings.Split(i.MessageComponentData().CustomID, ":")
	if len(parts) < 2 {
		return
	}
	arg := func(n int) string {
		if len(parts) > n {
			return parts[n]
		}
		return ""
	}
	id, hasID := parseID(arg(2))
	approverOnly := map[string]bool{"review": true, "revsel": true, "approve": true, "decline": true, "revokelist": true, "revokesel": true, "revoke": true}
	if approverOnly[parts[1]] && !a.access.Approve {
		notice(s, i, bookingError(service.ErrBookingPermission))
		return
	}
	switch parts[1] {
	case "book":
		c.startBooking(s, i, a)
	case "d":
		c.handleDraftComponent(s, i, a, arg(2), arg(3))
	case "mine":
		show(s, i, c.myBookingsView(a, ""))
	case "minesel":
		if id, ok := parseID(selectedValue(i)); ok {
			show(s, i, c.myBookingView(a, id))
		}
	case "change":
		if hasID {
			c.startChange(s, i, a, id)
		}
	case "cancel", "cancelrest":
		if hasID {
			show(s, i, c.cancelConfirmView(a, id, parts[1] == "cancelrest"))
		}
	case "cancelyes":
		if !hasID {
			return
		}
		message := "Cancelled."
		if err := c.service.Cancel(c.guildID, id, a.ID, arg(3) == "1", c.now()); err != nil {
			message = bookingError(err)
		}
		show(s, i, c.myBookingsView(a, message))
		if err == nil {
			c.Refresh(s)
		}
	case "weeks", "week":
		c.showWeek(s, i, c.now())
	case "view":
		if week, err := time.ParseInLocation("2006-01-02", arg(2), service.BookingLocation); err == nil && week.Year() >= 2000 && week.Year() <= 2100 {
			c.showWeek(s, i, week)
		}
	case "review":
		show(s, i, c.reviewView(""))
	case "revsel":
		if id, ok := parseID(selectedValue(i)); ok {
			show(s, i, c.requestDetailView(a, id))
		}
	case "approve":
		if hasID {
			c.decide(s, i, a, id, true, "")
		}
	case "decline":
		if hasID {
			reasonModal(s, i, fmt.Sprintf("office:declinem:%d", id), "Decline request")
		}
	case "revokelist":
		show(s, i, c.revokeListView(""))
	case "revokesel":
		if id, ok := parseID(selectedValue(i)); ok {
			show(s, i, c.revokeDetailView(id))
		}
	case "revoke":
		if hasID {
			reasonModal(s, i, fmt.Sprintf("office:revokem:%d:%s", id, arg(3)), "Revoke booking")
		}
	default:
		notice(s, i, "That button is out of date. Use `/bookings` or the office calendar.")
	}
}

func (c *Calendar) HandleModal(s *discordgo.Session, i *discordgo.InteractionCreate) {
	parts := strings.Split(i.ModalSubmitData().CustomID, ":")
	if len(parts) < 3 || parts[0] != "office" {
		return
	}
	a, err := c.actor(s, i)
	if err != nil {
		notice(s, i, bookingError(err))
		return
	}
	id, hasID := parseID(parts[2])
	switch parts[1] {
	case "dm":
		c.handleDraftModal(s, i, a, parts[2])
	case "declinem":
		if hasID {
			c.decide(s, i, a, id, false, modalValues(i)["reason"])
		}
	case "revokem":
		if !hasID {
			return
		}
		rest := len(parts) > 3 && parts[3] == "1"
		err := c.service.Revoke(c.guildID, id, a.ID, a.Roles, rest, modalValues(i)["reason"], c.now())
		message := "Revoked. The booker will get a DM."
		if err != nil {
			message = bookingError(err)
		}
		show(s, i, c.revokeListView(message))
		if err == nil {
			c.Refresh(s)
		}
	}
}

// ---- autocomplete ----

func commandOptions(options []*discordgo.ApplicationCommandInteractionDataOption) (map[string]string, string) {
	values, focused := map[string]string{}, ""
	for _, opt := range options {
		if opt.Type == discordgo.ApplicationCommandOptionString {
			values[opt.Name] = opt.StringValue()
		}
		if opt.Focused {
			focused = opt.Name
		}
	}
	return values, focused
}

func (c *Calendar) HandleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	choices := []*discordgo.ApplicationCommandOptionChoice{}
	if a, err := c.actor(s, i); err == nil && a.access.Book {
		values, focused := commandOptions(i.ApplicationCommandData().Options)
		choices = c.suggest(values, focused)
	}
	if len(choices) > 25 {
		choices = choices[:25]
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionApplicationCommandAutocompleteResult, Data: &discordgo.InteractionResponseData{Choices: choices}}); err != nil {
		log.Error("booking autocomplete failed", "err", err)
	}
}

func choice(name, value string) *discordgo.ApplicationCommandOptionChoice {
	return &discordgo.ApplicationCommandOptionChoice{Name: truncate(name, 100), Value: value}
}

func (c *Calendar) suggest(values map[string]string, focused string) []*discordgo.ApplicationCommandOptionChoice {
	now := c.now()
	today := service.BookingDay(now)
	typed := strings.TrimSpace(values[focused])
	out := []*discordgo.ApplicationCommandOptionChoice{}
	switch focused {
	case "date":
		busy, _ := c.service.Repo.Busy(c.guildID, today, today.AddDate(0, 0, 61))
		seen := map[string]bool{}
		add := func(day time.Time) {
			value := day.Format("2006-01-02")
			if seen[value] || day.Before(today) {
				return
			}
			seen[value] = true
			name := dayLabel(day, today)
			if len(c.freeStarts(day, busy, 0)) == 0 {
				name += " · no free time"
			}
			out = append(out, choice(name, value))
		}
		if day, err := service.ParseBookingDate(typed, now); err == nil {
			add(day)
		}
		for n := 0; n < 60 && len(out) < 25; n++ {
			day := today.AddDate(0, 0, n)
			if typed == "" || strings.Contains(strings.ToLower(dayLabel(day, today)+" "+day.Format("January 2006-01-02")), strings.ToLower(typed)) {
				add(day)
			}
		}
	case "start":
		day, err := service.ParseBookingDate(values["date"], now)
		if err != nil {
			return out
		}
		busy := c.busyAround(day)
		free := c.freeStarts(day, busy, 0)
		anchor := 8 * 60
		if day.Equal(today) {
			local := now.In(service.BookingLocation)
			anchor = max(anchor, local.Hour()*60+local.Minute())
		}
		if m, err := service.ParseBookingClock(typed); err == nil && m < 24*60 {
			if start, err := service.BookingAt(day, m); err == nil && start.After(now) && fitsUntil(busy, start, 0) >= 15*time.Minute {
				out = append(out, choice(minuteLabel(m), clockValue(m)))
			}
			anchor = m
		} else if typed != "" {
			return out
		}
		for _, m := range window(free, anchor, 25, false, 0) {
			if len(out) == 0 || out[0].Value != clockValue(m) {
				out = append(out, choice(minuteLabel(m), clockValue(m)))
			}
		}
	case "end":
		day, err := service.ParseBookingDate(values["date"], now)
		if err != nil {
			return out
		}
		startMin, err := service.ParseBookingClock(values["start"])
		if err != nil || startMin >= 24*60 {
			return out
		}
		start, err := service.BookingAt(day, startMin)
		if err != nil {
			return out
		}
		fits := fitsUntil(c.busyAround(day), start, 0)
		label := func(end time.Time) string {
			text := service.FormatClock(end)
			if end.Equal(day.AddDate(0, 0, 1)) {
				text = "midnight"
			} else if service.BookingDay(end) != service.BookingDay(start) {
				text += " (next day)"
			}
			return text + " · " + service.FormatDuration(end.Sub(start))
		}
		endValue := func(end time.Time) string {
			if end.Equal(day.AddDate(0, 0, 1)) {
				return "24:00"
			}
			local := end.In(service.BookingLocation)
			return clockValue(local.Hour()*60 + local.Minute())
		}
		if m, err := service.ParseBookingClock(typed); err == nil {
			if _, end, err := service.BookingRange(day, startMin, m); err == nil && end.Sub(start) >= 15*time.Minute && end.Sub(start) <= fits {
				out = append(out, choice(label(end), endValue(end)))
			}
		} else if typed != "" {
			return out
		}
		for step := 30; len(out) < 25 && time.Duration(step)*time.Minute <= fits && step < 24*60; step += 30 {
			end := start.Add(time.Duration(step) * time.Minute)
			if len(out) == 0 || out[0].Value != endValue(end) {
				out = append(out, choice(label(end), endValue(end)))
			}
		}
	case "until":
		day, err := service.ParseBookingDate(values["date"], now)
		step := service.RepeatStep(values["repeat"])
		if err != nil || step == 0 {
			return out
		}
		for n := 1; n < service.MaxSeriesLength && len(out) < 25; n++ {
			last := day.AddDate(0, 0, n*step)
			out = append(out, choice(fmt.Sprintf("%s · %d dates", service.FormatDay(last), n+1), last.Format("2006-01-02")))
		}
	}
	return out
}

// ---- /office-admin ----

func (c *Calendar) handleAdmin(s *discordgo.Session, i *discordgo.InteractionCreate, a *bookingActor) {
	if !a.isAdmin() {
		notice(s, i, "Only server admins (Manage Server) can configure office bookings.")
		return
	}
	options := i.ApplicationCommandData().Options
	if len(options) == 0 {
		return
	}
	sub := options[0]
	switch sub.Name {
	case "roles":
		if len(sub.Options) == 0 {
			return
		}
		action := sub.Options[0]
		if action.Name != "list" {
			var role, capability string
			for _, opt := range action.Options {
				switch opt.Name {
				case "role":
					role = fmt.Sprint(opt.Value)
				case "capability":
					capability = opt.StringValue()
				}
			}
			if err := c.service.Repo.SetRoleCapability(c.guildID, role, capability, action.Name == "add"); err != nil {
				notice(s, i, bookingError(err))
				return
			}
		}
		notice(s, i, c.rolesSummary())
		c.Refresh(s)
	case "channel":
		var kind, channel string
		for _, opt := range sub.Options {
			switch opt.Name {
			case "type":
				kind = opt.StringValue()
			case "channel":
				channel = fmt.Sprint(opt.Value)
			}
		}
		key := service.SettingOfficerChannel
		if kind == "calendar" {
			key = service.SettingCalendarChannel
		}
		if err := c.service.Repo.SetSetting(c.guildID, key, channel); err != nil {
			notice(s, i, bookingError(err))
			return
		}
		message := fmt.Sprintf("New request cards will go to <#%s>.", channel)
		if kind == "calendar" {
			message = fmt.Sprintf("The office calendar now lives in <#%s>; the old one was removed.", channel)
		}
		notice(s, i, message+"\n\n"+c.channelCheck(s))
		if kind == "calendar" {
			c.Refresh(s)
		}
	case "status":
		notice(s, i, c.rolesSummary()+"\n\n"+c.channelCheck(s))
	}
}

func (c *Calendar) rolesSummary() string {
	mapping, err := c.service.Repo.RoleCapabilities(c.guildID)
	if err != nil {
		return bookingError(err)
	}
	book := "everyone in the server (no roles set)"
	if grants := mapping[service.CapabilityBook]; len(grants) > 0 {
		book = grantMentions(grants)
	}
	approve := "<@&" + service.SigningOfficerRoleID + "> (default)"
	if grants := mapping[service.CapabilityApprove]; len(grants) > 0 {
		approve = grantMentions(grants)
	}
	return fmt.Sprintf("**Can book:** %s\n**Can approve:** %s\nApprovers can't approve their own requests.", book, approve)
}

// grantMentions renders role grants as role mentions and user grants as user mentions.
func grantMentions(grants []string) string {
	out := make([]string, 0, len(grants))
	for _, id := range grants {
		if user, ok := strings.CutPrefix(id, service.UserGrantPrefix); ok {
			out = append(out, "<@"+user+">")
		} else {
			out = append(out, "<@&"+id+">")
		}
	}
	return strings.Join(out, " ")
}

func (c *Calendar) channelCheck(s *discordgo.Session) string {
	type need struct {
		name  string
		perms int64
	}
	base := int64(discordgo.PermissionViewChannel | discordgo.PermissionSendMessages | discordgo.PermissionEmbedLinks)
	checks := []struct {
		label, channel string
		needs          []need
	}{
		{"Calendar", c.calendarChannel(), []need{{"View Channel, Send Messages, Embed Links", base}, {"Attach Files", discordgo.PermissionAttachFiles}}},
		{"Officer requests", c.officerChannel(), []need{{"View Channel, Send Messages, Embed Links", base}, {"Mention roles (Mention @everyone, or make the approver roles mentionable)", discordgo.PermissionMentionEveryone}}},
	}
	lines := []string{}
	for _, check := range checks {
		if check.channel == "" {
			lines = append(lines, fmt.Sprintf("**%s:** not set", check.label))
			continue
		}
		var perms int64
		err := errors.New("no session state")
		if s.State != nil && s.State.User != nil {
			perms, err = s.State.UserChannelPermissions(s.State.User.ID, check.channel)
		}
		if err != nil {
			lines = append(lines, fmt.Sprintf("**%s:** <#%s> (couldn't check the bot's permissions)", check.label, check.channel))
			continue
		}
		missing := []string{}
		for _, n := range check.needs {
			if perms&n.perms != n.perms {
				missing = append(missing, n.name)
			}
		}
		status := "✅ bot permissions OK"
		if len(missing) > 0 {
			status = "⚠️ bot is missing: " + strings.Join(missing, "; ")
		}
		lines = append(lines, fmt.Sprintf("**%s:** <#%s> — %s", check.label, check.channel, status))
	}
	return strings.Join(lines, "\n")
}
