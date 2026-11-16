package discord

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"office/internal/repository"
	"office/internal/service"

	"github.com/bwmarrin/discordgo"
)

func statusLabel(b repository.Booking, now time.Time) string {
	switch b.Status {
	case "pending":
		if b.ReplacesID > 0 {
			return "⏳ Time change waiting for approval"
		}
		if !b.Start.After(now) {
			return "⌛ Expired"
		}
		return "⏳ Waiting for approval"
	case "approved":
		return "✅ Approved"
	case "declined":
		return "❌ Declined"
	case "cancelled":
		return "🚫 Cancelled"
	case "revoked":
		return "⛔ Revoked"
	case "expired":
		return "⌛ Expired"
	}
	return b.Status
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---- my bookings ----

func (c *Calendar) myBookingsView(a *bookingActor, message string) view {
	now := c.now()
	rows, err := c.service.Repo.Mine(c.guildID, a.ID, now)
	if err != nil {
		return view{content: bookingError(err)}
	}
	var upcoming, recent []repository.Booking
	for _, b := range rows {
		if b.Active() && b.Start.After(now) {
			upcoming = append(upcoming, b)
		} else {
			recent = append(recent, b)
		}
	}
	var out strings.Builder
	line := func(b repository.Booking) string {
		series := ""
		if b.SeriesID > 0 {
			series = "🔁 "
		}
		return fmt.Sprintf("%s`%s` **%s** — %s\n", series, bookingWhen(b), escapeBookingText(b.Title), statusLabel(b, now))
	}
	if len(upcoming) == 0 {
		out.WriteString("You have no upcoming bookings.\n")
	}
	for n, b := range upcoming {
		if n == 25 {
			fmt.Fprintf(&out, "…and %d more\n", len(upcoming)-25)
			break
		}
		out.WriteString(line(b))
	}
	if len(recent) > 0 {
		out.WriteString("\n**Past week**\n")
		for n := len(recent) - 1; n >= 0 && n >= len(recent)-10; n-- {
			out.WriteString(line(recent[n]))
		}
	}
	embed := &discordgo.MessageEmbed{Title: "My bookings", Color: 0x2D6AE3, Description: truncate(out.String(), 4000)}
	components := []discordgo.MessageComponent{}
	if len(upcoming) > 0 {
		options := []discordgo.SelectMenuOption{}
		for _, b := range upcoming[:min(25, len(upcoming))] {
			options = append(options, discordgo.SelectMenuOption{Label: truncate(bookingWhen(b), 100), Value: strconv.FormatInt(b.ID, 10), Description: truncate(b.Title+" · "+statusLabel(b, now), 100)})
		}
		components = append(components, row(discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: "office:minesel", Placeholder: "Change or cancel a booking…", Options: options}))
	}
	components = append(components, row(button("Book office time", "office:book", discordgo.SuccessButton), button("Refresh", "office:mine", discordgo.SecondaryButton)))
	return view{content: message, embeds: []*discordgo.MessageEmbed{embed}, components: components}
}

func (c *Calendar) myBookingView(a *bookingActor, id int64) view {
	b, err := c.service.Repo.Get(c.guildID, id)
	if err != nil || b.UserID != a.ID {
		return c.myBookingsView(a, "That booking is no longer available.")
	}
	now := c.now()
	var out strings.Builder
	fmt.Fprintf(&out, "**%s**\n%s\n%s", escapeBookingText(b.Title), bookingWhen(*b), statusLabel(*b, now))
	if b.ReplacesID > 0 {
		if original, err := c.service.Repo.Get(c.guildID, b.ReplacesID); err == nil {
			fmt.Fprintf(&out, "\nReplaces %s, which you keep until this is approved.", bookingWhen(*original))
		}
	}
	buttons := []discordgo.MessageComponent{}
	if b.Active() && b.Start.After(now) {
		if b.ReplacesID > 0 {
			buttons = append(buttons, button("Withdraw time change", fmt.Sprintf("office:cancel:%d", b.ID), discordgo.DangerButton))
		} else {
			change, _ := c.service.Repo.PendingChange(c.guildID, b.ID)
			if change != nil {
				fmt.Fprintf(&out, "\nA change to %s is waiting for approval.", bookingWhen(*change))
				buttons = append(buttons, button("Withdraw time change", fmt.Sprintf("office:cancel:%d", change.ID), discordgo.SecondaryButton))
			} else {
				buttons = append(buttons, button("Change time", fmt.Sprintf("office:change:%d", b.ID), discordgo.PrimaryButton))
			}
			buttons = append(buttons, button("Cancel booking", fmt.Sprintf("office:cancel:%d", b.ID), discordgo.DangerButton))
			if b.SeriesID > 0 {
				buttons = append(buttons, button("Cancel this & later dates", fmt.Sprintf("office:cancelrest:%d", b.ID), discordgo.DangerButton))
			}
		}
	}
	buttons = append(buttons, button("Back", "office:mine", discordgo.SecondaryButton))
	return view{content: out.String(), components: []discordgo.MessageComponent{row(buttons...)}}
}

func (c *Calendar) cancelConfirmView(a *bookingActor, id int64, rest bool) view {
	b, err := c.service.Repo.Get(c.guildID, id)
	if err != nil || b.UserID != a.ID {
		return view{content: "That booking is no longer available."}
	}
	question := fmt.Sprintf("Cancel **%s** on %s?", escapeBookingText(b.Title), bookingWhen(*b))
	if b.ReplacesID > 0 {
		question = fmt.Sprintf("Withdraw your time change to %s? Your current booking stays as it is.", bookingWhen(*b))
	}
	if rest {
		count := 0
		if series, err := c.service.Repo.Series(c.guildID, b.SeriesID); err == nil {
			for _, other := range series {
				if other.Active() && !other.Start.Before(b.Start) {
					count++
				}
			}
		}
		question = fmt.Sprintf("Cancel **%s** on %s and every later date in this series (%d dates)?", escapeBookingText(b.Title), service.FormatDay(b.Start), count)
	}
	return view{content: question, components: []discordgo.MessageComponent{row(
		button("Yes, cancel", fmt.Sprintf("office:cancelyes:%d:%d", id, boolDigit(rest)), discordgo.DangerButton),
		button("Keep it", "office:mine", discordgo.SecondaryButton),
	)}}
}

func boolDigit(v bool) int {
	if v {
		return 1
	}
	return 0
}

// ---- officer review ----

type requestSummary struct {
	first repository.Booking // next pending date
	count int                // pending dates
}

// pendingLeads groups upcoming pending bookings by request, oldest request first.
func (c *Calendar) pendingLeads() ([]requestSummary, error) {
	rows, err := c.service.Repo.Pending(c.guildID, c.now())
	if err != nil {
		return nil, err
	}
	index := map[int64]int{}
	result := []requestSummary{}
	for _, b := range rows {
		if n, ok := index[b.Lead()]; ok {
			result[n].count++
			continue
		}
		index[b.Lead()] = len(result)
		result = append(result, requestSummary{first: b, count: 1})
	}
	return result, nil
}

func (c *Calendar) reviewView(message string) view {
	leads, err := c.pendingLeads()
	if err != nil {
		return view{content: bookingError(err)}
	}
	var out strings.Builder
	if len(leads) == 0 {
		out.WriteString("Nothing is waiting for approval.")
	}
	options := []discordgo.SelectMenuOption{}
	for n, r := range leads {
		kind := ""
		switch {
		case r.first.ReplacesID > 0:
			kind = " · time change"
		case r.count > 1:
			kind = fmt.Sprintf(" · %d dates", r.count)
		}
		if n < 25 {
			fmt.Fprintf(&out, "`%s` **%s** — %s%s\n", bookingWhen(r.first), escapeBookingText(r.first.Title), escapeBookingText(r.first.Name), kind)
			options = append(options, discordgo.SelectMenuOption{Label: truncate(r.first.Name+" · "+bookingWhen(r.first), 100), Value: strconv.FormatInt(r.first.Lead(), 10), Description: truncate(r.first.Title+kind, 100)})
		}
	}
	embed := &discordgo.MessageEmbed{Title: fmt.Sprintf("Waiting for approval (%d)", len(leads)), Color: 0xE3A72D, Description: truncate(out.String(), 4000)}
	components := []discordgo.MessageComponent{}
	if len(options) > 0 {
		components = append(components, row(discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: "office:revsel", Placeholder: "Review a request…", Options: options}))
	}
	components = append(components, row(button("Revoke an approved booking", "office:revokelist", discordgo.SecondaryButton), button("Refresh", "office:review", discordgo.SecondaryButton)))
	return view{content: message, embeds: []*discordgo.MessageEmbed{embed}, components: components}
}

func (c *Calendar) requestDetailView(a *bookingActor, lead int64) view {
	b, err := c.service.Repo.Get(c.guildID, lead)
	if err != nil {
		return c.reviewView("That request no longer exists.")
	}
	embed, buttons, err := c.requestCard(*b)
	if err != nil {
		return c.reviewView(bookingError(err))
	}
	components := []discordgo.MessageComponent{}
	if len(buttons) > 0 && b.UserID == a.ID {
		buttons = []discordgo.MessageComponent{}
		embed.Footer = &discordgo.MessageEmbedFooter{Text: "This is your request; another officer must decide it."}
	}
	buttons = append(buttons, button("Back", "office:review", discordgo.SecondaryButton))
	components = append(components, row(buttons...))
	return view{embeds: []*discordgo.MessageEmbed{embed}, components: components}
}

func (c *Calendar) revokeListView(message string) view {
	rows, err := c.service.Repo.ApprovedUpcoming(c.guildID, c.now())
	if err != nil {
		return view{content: bookingError(err)}
	}
	if len(rows) == 0 {
		return view{content: strings.TrimSpace(message + "\nThere are no upcoming approved bookings."), components: []discordgo.MessageComponent{row(button("Back", "office:review", discordgo.SecondaryButton))}}
	}
	options := []discordgo.SelectMenuOption{}
	for _, b := range rows[:min(25, len(rows))] {
		options = append(options, discordgo.SelectMenuOption{Label: truncate(b.Name+" · "+bookingWhen(b), 100), Value: strconv.FormatInt(b.ID, 10), Description: truncate(b.Title, 100)})
	}
	return view{content: strings.TrimSpace(message + "\nPick the approved booking to revoke. The booker gets a DM."), components: []discordgo.MessageComponent{
		row(discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: "office:revokesel", Placeholder: "Approved booking…", Options: options}),
		row(button("Back", "office:review", discordgo.SecondaryButton)),
	}}
}

func (c *Calendar) revokeDetailView(id int64) view {
	b, err := c.service.Repo.Get(c.guildID, id)
	if err != nil || b.Status != "approved" {
		return c.revokeListView("That booking is no longer approved.")
	}
	buttons := []discordgo.MessageComponent{button("Revoke", fmt.Sprintf("office:revoke:%d:0", b.ID), discordgo.DangerButton)}
	if b.SeriesID > 0 {
		buttons = append(buttons, button("Revoke this & later dates", fmt.Sprintf("office:revoke:%d:1", b.ID), discordgo.DangerButton))
	}
	buttons = append(buttons, button("Back", "office:revokelist", discordgo.SecondaryButton))
	return view{content: fmt.Sprintf("**%s** — %s (<@%s>)\n%s", escapeBookingText(b.Title), escapeBookingText(b.Name), b.UserID, bookingWhen(*b)), components: []discordgo.MessageComponent{row(buttons...)}}
}

func reasonModal(s *discordgo.Session, i *discordgo.InteractionCreate, customID, title string) {
	input := discordgo.TextInput{CustomID: "reason", Label: "Reason (optional, sent to the booker)", Style: discordgo.TextInputParagraph, Required: false, MaxLength: 300}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseModal, Data: &discordgo.InteractionResponseData{
		CustomID: customID, Title: title, Components: []discordgo.MessageComponent{row(input)}}}); err != nil {
		log.Error("reason modal failed", "err", err)
	}
}

// decide approves or declines a request from an officer card or the review list.
func (c *Calendar) decide(s *discordgo.Session, i *discordgo.InteractionCreate, a *bookingActor, lead int64, approve bool, reason string) {
	err := c.service.Decide(c.guildID, lead, a.ID, a.Roles, approve, reason, c.now())
	message := map[bool]string{true: "Approved. The requester will get a DM.", false: "Declined. The requester will get a DM."}[approve]
	if err != nil {
		message = bookingError(err)
	}
	if isPrivateBookingMessage(i) {
		show(s, i, c.reviewView(message))
	} else {
		notice(s, i, message)
	}
	if err == nil {
		c.Refresh(s)
	}
}

// ---- request card (officer channel and review detail) ----

func requestSummaryText(b repository.Booking, rows []repository.Booking, original *repository.Booking) string {
	title := escapeBookingText(b.Title)
	switch {
	case original != nil:
		return fmt.Sprintf("**Time change** for **%s**\nFrom: %s\nTo: %s", title, bookingWhen(*original), bookingWhen(b))
	case len(rows) > 1:
		first, last := rows[0], rows[len(rows)-1]
		every := "Every week"
		if len(rows) > 1 && rows[1].Start.Sub(first.Start) > 8*24*time.Hour {
			every = "Every 2 weeks"
		}
		return fmt.Sprintf("**%s**\n%s on %s · %s – %s\n%s → %s (%d dates)", title, every, first.Start.In(service.BookingLocation).Format("Monday"),
			service.FormatClock(first.Start), service.FormatClock(first.End), service.FormatDay(first.Start), service.FormatDay(last.Start), len(rows))
	}
	return fmt.Sprintf("**%s**\n%s", title, bookingWhen(b))
}

// requestCard describes a request and its state, with decision buttons while it waits.
func (c *Calendar) requestCard(b repository.Booking) (*discordgo.MessageEmbed, []discordgo.MessageComponent, error) {
	lead := b.Lead()
	if lead != b.ID {
		leadRow, err := c.service.Repo.Get(c.guildID, lead)
		if err != nil {
			return nil, nil, err
		}
		b = *leadRow
	}
	rows, err := c.service.Repo.Series(c.guildID, lead)
	if err != nil {
		return nil, nil, err
	}
	var original *repository.Booking
	if b.ReplacesID > 0 {
		original, _ = c.service.Repo.Get(c.guildID, b.ReplacesID)
	}
	now := c.now()
	counts := map[string]int{}
	waiting := 0
	var decided *repository.Booking
	for n, r := range rows {
		status := r.Status
		if status == "pending" && !r.Start.After(now) {
			status = "expired"
		}
		if status == "pending" {
			waiting++
		}
		counts[status]++
		if decided == nil && r.ReviewedBy != "" && (r.Status == "approved" || r.Status == "declined" || r.Status == "revoked") {
			decided = &rows[n]
		}
	}
	embed := &discordgo.MessageEmbed{Title: "Office request", Description: requestSummaryText(b, rows, original) + fmt.Sprintf("\nRequested by <@%s>", b.UserID)}
	if original != nil {
		embed.Title = "Office time change"
	}
	var status string
	buttons := []discordgo.MessageComponent{}
	switch {
	case waiting > 0:
		embed.Color = 0xE3A72D
		status = "⏳ Waiting for approval"
		if len(rows) > 1 {
			status = fmt.Sprintf("⏳ %d dates waiting for approval", waiting)
		}
		approve := "Approve"
		if original != nil {
			approve = "Approve change"
		} else if waiting > 1 {
			approve = fmt.Sprintf("Approve %d dates", waiting)
		}
		buttons = append(buttons,
			button(approve, fmt.Sprintf("office:approve:%d", lead), discordgo.SuccessButton),
			button("Decline", fmt.Sprintf("office:decline:%d", lead), discordgo.DangerButton))
	case len(rows) == 1:
		r := rows[0]
		switch r.Status {
		case "approved":
			embed.Color, status = 0x2DA44E, fmt.Sprintf("✅ Approved by <@%s>", r.ReviewedBy)
		case "declined":
			embed.Color, status = 0xCF222E, fmt.Sprintf("❌ Declined by <@%s>", r.ReviewedBy)
		case "revoked":
			embed.Color, status = 0xCF222E, fmt.Sprintf("⛔ Revoked by <@%s>", r.ReviewedBy)
		case "cancelled":
			embed.Color, status = 0x8C959F, "🚫 Cancelled by the requester"
			if r.ReviewedBy != r.UserID {
				status = "🚫 Cancelled"
			}
		case "replaced":
			embed.Color, status = 0x8C959F, "↪️ Replaced by a time change"
		default:
			embed.Color, status = 0x8C959F, "⌛ Expired — no decision before it started"
		}
	default:
		parts := []string{}
		for _, s := range []struct{ key, label string }{{"approved", "✅ %d approved"}, {"declined", "❌ %d declined"}, {"revoked", "⛔ %d revoked"}, {"cancelled", "🚫 %d cancelled"}, {"replaced", "↪️ %d moved"}, {"expired", "⌛ %d expired"}} {
			if counts[s.key] > 0 {
				parts = append(parts, fmt.Sprintf(s.label, counts[s.key]))
			}
		}
		status = strings.Join(parts, " · ")
		embed.Color = 0x8C959F
		if counts["approved"] > 0 {
			embed.Color = 0x2DA44E
		}
		if decided != nil {
			status += fmt.Sprintf("\nDecided by <@%s>", decided.ReviewedBy)
		}
	}
	if decided != nil && decided.Reason != "" {
		status += "\nReason: " + escapeBookingText(decided.Reason)
	}
	embed.Fields = []*discordgo.MessageEmbedField{{Name: "Status", Value: status}}
	return embed, buttons, nil
}
