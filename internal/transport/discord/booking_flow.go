package discord

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"office/internal/repository"
	"office/internal/service"

	"github.com/bwmarrin/discordgo"
)

// bookingDraft is a request being filled in through the picker or /book.
// Drafts live in memory; after a restart the member starts again.
type bookingDraft struct {
	id, user                 string
	d                        service.Draft
	hasDay, hasStart, hasEnd bool
	original                 *repository.Booking
	created                  time.Time
	result                   *view // set once submitted, so a double click shows the same result
}

const draftLifetime = 2 * time.Hour

var lengthChoices = []int{30, 60, 90, 120, 150, 180, 240, 300, 360, 480, 600, 720, 960}

func newDraftID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Calendar) newDraft(user string) *bookingDraft {
	d := &bookingDraft{id: newDraftID(), user: user, created: c.now()}
	c.draftsMu.Lock()
	c.drafts[d.id] = d
	c.draftsMu.Unlock()
	return d
}

func (c *Calendar) draft(id, user string) *bookingDraft {
	c.draftsMu.Lock()
	defer c.draftsMu.Unlock()
	d := c.drafts[id]
	if d == nil || d.user != user || c.now().Sub(d.created) > draftLifetime {
		return nil
	}
	return d
}

func (c *Calendar) pruneDrafts() {
	c.draftsMu.Lock()
	defer c.draftsMu.Unlock()
	for id, d := range c.drafts {
		if c.now().Sub(d.created) > draftLifetime {
			delete(c.drafts, id)
		}
	}
}

var expiredDraft = view{content: "This booking form expired. Click **Book office time** or use `/book` to start again."}

// ---- free time ----

// busyAround lists held/approved bookings from the day's start to two days later.
func (c *Calendar) busyAround(day time.Time) []repository.Booking {
	day = service.BookingDay(day)
	busy, err := c.service.Repo.Busy(c.guildID, day, day.AddDate(0, 0, 2))
	if err != nil {
		log.Error("load busy times failed", "err", err)
	}
	return busy
}

func isFree(busy []repository.Booking, start, end time.Time, ignore int64) bool {
	for _, b := range busy {
		if b.ID != ignore && b.Start.Before(end) && b.End.After(start) {
			return false
		}
	}
	return true
}

// freeStarts lists minutes (30-minute steps) where at least 30 minutes are free.
func (c *Calendar) freeStarts(day time.Time, busy []repository.Booking, ignore int64) []int {
	now := c.now()
	starts := []int{}
	for m := 0; m < 24*60; m += 30 {
		start, end, err := service.BookingRange(day, m, m+30)
		if err != nil || !start.After(now) || !isFree(busy, start, end, ignore) {
			continue
		}
		starts = append(starts, m)
	}
	return starts
}

// fitsUntil returns how many minutes are free from start (capped at 24h).
func fitsUntil(busy []repository.Booking, start time.Time, ignore int64) time.Duration {
	limit := start.Add(24 * time.Hour)
	for _, b := range busy {
		if b.ID != ignore && b.End.After(start) && b.Start.Before(limit) {
			if !b.Start.After(start) {
				return 0
			}
			limit = b.Start
		}
	}
	return limit.Sub(start)
}

func endMinute(startMin, length int) int {
	end := startMin + length
	if end > 24*60 {
		end -= 24 * 60
	}
	return end
}

func clockValue(minute int) string { return fmt.Sprintf("%02d:%02d", minute/60, minute%60) }

func minuteLabel(minute int) string {
	t := time.Date(2000, 1, 1, minute/60, minute%60, 0, 0, time.UTC)
	if minute == 24*60 {
		return "midnight"
	}
	if t.Minute() == 0 {
		return t.Format("3 PM")
	}
	return t.Format("3:04 PM")
}

func dayLabel(day, today time.Time) string {
	label := service.FormatDay(day)
	switch service.BookingDay(day).Sub(today) / (24 * time.Hour) {
	case 0:
		return "Today · " + label
	case 1:
		return "Tomorrow · " + label
	}
	return label
}

func (b *bookingDraft) length() int {
	start, end, err := service.BookingRange(b.d.Day, b.d.StartMin, b.d.EndMin)
	if err != nil {
		return 0
	}
	return int(end.Sub(start).Minutes())
}

func (b *bookingDraft) ignore() int64 { return b.d.Replaces }

// ---- picker ----

func (c *Calendar) pickerView(d *bookingDraft, problem string) view {
	today := service.BookingDay(c.now())
	var header strings.Builder
	if d.original != nil {
		fmt.Fprintf(&header, "**Change time** for **%s**\nCurrently %s\n", escapeBookingText(d.original.Title), bookingWhen(*d.original))
	} else {
		header.WriteString("**Book office time**\n")
	}
	if problem != "" {
		header.WriteString("⚠️ " + problem + "\n")
	}
	horizon := today.AddDate(0, 0, 27)
	if d.hasDay && !d.d.Day.Before(horizon) {
		horizon = d.d.Day.AddDate(0, 0, 2)
	}
	busy, err := c.service.Repo.Busy(c.guildID, today, horizon)
	if err != nil {
		log.Error("load busy times failed", "err", err)
	}
	if d.hasDay && d.hasStart && d.hasEnd {
		start, end, err := service.BookingRange(d.d.Day, d.d.StartMin, d.d.EndMin)
		if err == nil && isFree(busy, start, end, d.ignore()) {
			fmt.Fprintf(&header, "✅ %s is free.", service.FormatRange(start, end))
		} else if err == nil {
			fmt.Fprintf(&header, "❌ %s overlaps another booking.", service.FormatRange(start, end))
		}
	} else {
		header.WriteString("Pick a day, a start time and a length. Only free times are listed. Need something else? Use `/book`.")
	}
	id := func(action string) string { return "office:d:" + d.id + ":" + action }

	// Day
	days := []discordgo.SelectMenuOption{}
	if d.hasDay && d.d.Day.Sub(today) >= 25*24*time.Hour {
		days = append(days, discordgo.SelectMenuOption{Label: dayLabel(d.d.Day, today), Value: d.d.Day.Format("2006-01-02"), Default: true})
	}
	for n := 0; len(days) < 25; n++ {
		day := today.AddDate(0, 0, n)
		option := discordgo.SelectMenuOption{Label: dayLabel(day, today), Value: day.Format("2006-01-02"), Default: d.hasDay && day.Equal(d.d.Day)}
		if len(c.freeStarts(day, busy, d.ignore())) == 0 {
			option.Description = "No free time"
		}
		days = append(days, option)
	}
	rows := []discordgo.MessageComponent{row(discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: id("day"), Placeholder: "Day", Options: days})}

	// Start
	starts := []discordgo.SelectMenuOption{}
	if d.hasDay {
		free := c.freeStarts(d.d.Day, busy, d.ignore())
		if d.hasStart && !containsInt(free, d.d.StartMin) {
			free = insertSorted(free, d.d.StartMin)
		}
		anchor := 8 * 60
		if d.d.Day.Equal(today) {
			now := c.now().In(service.BookingLocation)
			anchor = max(anchor, now.Hour()*60+now.Minute())
		}
		if d.hasStart {
			anchor = min(anchor, d.d.StartMin)
		}
		for _, m := range window(free, anchor, 25, d.hasStart, d.d.StartMin) {
			starts = append(starts, discordgo.SelectMenuOption{Label: minuteLabel(m), Value: clockValue(m), Default: d.hasStart && m == d.d.StartMin})
		}
	}
	startMenu := discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: id("start"), Placeholder: "Start time", Options: starts}
	if len(starts) == 0 {
		startMenu.Disabled = true
		startMenu.Placeholder = "Start time (pick a day first)"
		if d.hasDay {
			startMenu.Placeholder = "No free time on this day"
		}
		startMenu.Options = []discordgo.SelectMenuOption{{Label: "—", Value: "none"}}
	}
	rows = append(rows, row(startMenu))

	// Length
	lengths := []discordgo.SelectMenuOption{}
	if d.hasDay && d.hasStart {
		start, err := service.BookingAt(d.d.Day, d.d.StartMin)
		if err == nil {
			fits := fitsUntil(busy, start, d.ignore())
			choices := append([]int{}, lengthChoices...)
			if d.hasEnd && !containsInt(choices, d.length()) {
				choices = insertSorted(choices, d.length())
			}
			for _, length := range choices {
				if time.Duration(length)*time.Minute > fits && !(d.hasEnd && length == d.length()) {
					continue
				}
				end := start.Add(time.Duration(length) * time.Minute)
				until := service.FormatClock(end)
				if service.BookingDay(end) != service.BookingDay(start) {
					until += " (next day)"
				}
				if end.Equal(service.BookingDay(start).AddDate(0, 0, 1)) {
					until = "midnight"
				}
				lengths = append(lengths, discordgo.SelectMenuOption{Label: service.FormatDuration(time.Duration(length)*time.Minute) + " · until " + until, Value: fmt.Sprint(length), Default: d.hasEnd && length == d.length()})
				if len(lengths) == 25 {
					break
				}
			}
		}
	}
	lengthMenu := discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: id("len"), Placeholder: "Length", Options: lengths}
	if len(lengths) == 0 {
		lengthMenu.Disabled = true
		lengthMenu.Placeholder = "Length (pick a start time first)"
		lengthMenu.Options = []discordgo.SelectMenuOption{{Label: "—", Value: "none"}}
	}
	rows = append(rows, row(lengthMenu))

	if d.original == nil {
		repeat := []discordgo.SelectMenuOption{
			{Label: "Doesn't repeat", Value: "none", Default: d.d.Repeat == ""},
			{Label: "Every week", Value: "weekly", Default: d.d.Repeat == "weekly"},
			{Label: "Every 2 weeks", Value: "biweekly", Default: d.d.Repeat == "biweekly"},
		}
		rows = append(rows, row(discordgo.SelectMenu{MenuType: discordgo.StringSelectMenu, CustomID: id("repeat"), Placeholder: "Repeat", Options: repeat}))
	}
	next := button("Next", id("next"), discordgo.PrimaryButton)
	next.Disabled = !(d.hasDay && d.hasStart && d.hasEnd)
	rows = append(rows, row(next, button("Cancel", id("cancel"), discordgo.SecondaryButton)))
	return view{content: strings.TrimSpace(header.String()), components: rows}
}

func containsInt(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func insertSorted(list []int, v int) []int {
	out := make([]int, 0, len(list)+1)
	added := false
	for _, x := range list {
		if !added && v < x {
			out = append(out, v)
			added = true
		}
		out = append(out, x)
	}
	if !added {
		out = append(out, v)
	}
	return out
}

// window picks up to n values starting at the first value >= anchor, shifted
// back when short, always keeping the selected value visible.
func window(values []int, anchor, n int, hasSelected bool, selected int) []int {
	if len(values) <= n {
		return values
	}
	from := len(values)
	for idx, v := range values {
		if v >= anchor {
			from = idx
			break
		}
	}
	from = min(from, len(values)-n)
	if hasSelected {
		for idx, v := range values {
			if v == selected && idx >= from+n {
				from = idx - n + 1
			}
		}
	}
	return values[from : from+n]
}

// ---- confirm & submit ----

func (c *Calendar) confirmView(d *bookingDraft) view {
	slots, err := c.service.Plan(c.guildID, d.d, c.now())
	if err != nil {
		return c.pickerView(d, bookingError(err))
	}
	id := func(action string) string { return "office:d:" + d.id + ":" + action }
	var out strings.Builder
	free, taken := []service.Slot{}, []service.Slot{}
	for _, slot := range slots {
		if slot.Taken {
			taken = append(taken, slot)
		} else {
			free = append(free, slot)
		}
	}
	title := escapeBookingText(service.CleanBookingText(d.d.Title))
	if d.original != nil {
		fmt.Fprintf(&out, "**Confirm time change** for **%s**\nFrom: %s\nTo: %s\n", title, bookingWhen(*d.original), service.FormatRange(slots[0].Start, slots[0].End))
		if d.original.Status == "approved" {
			out.WriteString("You keep your current time until an officer approves the new one.\n")
		}
	} else if len(slots) == 1 {
		fmt.Fprintf(&out, "**Confirm your request**\n**%s**\n%s\n", title, service.FormatRange(slots[0].Start, slots[0].End))
	} else {
		every := "Every week"
		if d.d.Repeat == "biweekly" {
			every = "Every 2 weeks"
		}
		fmt.Fprintf(&out, "**Confirm your request**\n**%s**\n%s on %s · %s – %s\n", title, every, slots[0].Start.In(service.BookingLocation).Format("Monday"),
			service.FormatClock(slots[0].Start), service.FormatClock(slots[0].End))
		fmt.Fprintf(&out, "%d of %d dates free: %s\n", len(free), len(slots), slotDays(free))
		if len(taken) > 0 {
			fmt.Fprintf(&out, "Already taken (left out): %s\n", slotDays(taken))
		}
	}
	if len(free) == 0 {
		out.WriteString("❌ " + map[bool]string{true: "That time is already taken.", false: "All of these dates are already taken."}[len(slots) == 1])
		return view{content: out.String(), components: []discordgo.MessageComponent{row(
			button("Pick another time", id("edit"), discordgo.PrimaryButton), button("Cancel", id("cancel"), discordgo.SecondaryButton))}}
	}
	out.WriteString("Officers are notified when you submit, and you'll get a DM with their decision.")
	label := "Submit request"
	if len(slots) > 1 {
		label = fmt.Sprintf("Submit %d dates", len(free))
	}
	return view{content: out.String(), components: []discordgo.MessageComponent{row(
		button(label, id("submit"), discordgo.SuccessButton), button("Edit", id("edit"), discordgo.SecondaryButton), button("Cancel", id("cancel"), discordgo.SecondaryButton))}}
}

func slotDays(slots []service.Slot) string {
	days := make([]string, 0, len(slots))
	for _, slot := range slots {
		days = append(days, slot.Start.In(service.BookingLocation).Format("Jan 2"))
	}
	return strings.Join(days, ", ")
}

func (c *Calendar) submit(s *discordgo.Session, i *discordgo.InteractionCreate, a *bookingActor, d *bookingDraft) {
	if d.result != nil {
		show(s, i, *d.result)
		return
	}
	created, skipped, err := c.service.Submit("draft:"+d.id, c.guildID, a.ID, a.Name, a.Roles, d.d, c.now())
	if err != nil {
		v := c.confirmView(d)
		v.content = "⚠️ " + bookingError(err) + "\n\n" + v.content
		show(s, i, v)
		return
	}
	var out strings.Builder
	switch {
	case d.original != nil && d.original.Status == "approved":
		fmt.Fprintf(&out, "**Time change requested.** You keep %s until an officer approves %s. You'll get a DM with the decision.", bookingWhen(*d.original), bookingWhen(created[0]))
	case d.original != nil:
		fmt.Fprintf(&out, "**Request updated** to %s. You'll get a DM when an officer decides.", bookingWhen(created[0]))
	case len(created) == 1:
		fmt.Fprintf(&out, "**Request sent** for %s. You'll get a DM when an officer decides.", bookingWhen(created[0]))
	default:
		fmt.Fprintf(&out, "**Request sent** for %d dates from %s. You'll get a DM when an officer decides.", len(created), service.FormatDay(created[0].Start))
	}
	if skipped > 0 {
		fmt.Fprintf(&out, "\n%d date(s) were taken just before you submitted and were left out.", skipped)
	}
	result := view{content: out.String(), components: []discordgo.MessageComponent{row(button("My bookings", "office:mine", discordgo.SecondaryButton))}}
	d.result = &result
	show(s, i, result)
	c.Refresh(s)
}

// ---- interactions ----

func (c *Calendar) startBooking(s *discordgo.Session, i *discordgo.InteractionCreate, a *bookingActor) {
	if !a.access.Book {
		notice(s, i, bookingError(service.ErrBookPermission))
		return
	}
	d := c.newDraft(a.ID)
	show(s, i, c.pickerView(d, ""))
}

func (c *Calendar) startChange(s *discordgo.Session, i *discordgo.InteractionCreate, a *bookingActor, id int64) {
	b, err := c.service.Repo.Get(c.guildID, id)
	switch {
	case err != nil:
		notice(s, i, bookingError(err))
		return
	case b.UserID != a.ID:
		notice(s, i, bookingError(repository.ErrBookingOwner))
		return
	case !b.Active() || !b.Start.After(c.now()):
		notice(s, i, "This booking can't be changed any more.")
		return
	case !a.access.Book:
		notice(s, i, bookingError(service.ErrBookPermission))
		return
	}
	if change, err := c.service.Repo.PendingChange(c.guildID, b.ID); err == nil && change != nil {
		notice(s, i, bookingError(repository.ErrChangePending))
		return
	}
	d := c.newDraft(a.ID)
	start := b.Start.In(service.BookingLocation)
	end := b.End.In(service.BookingLocation)
	endMin := end.Hour()*60 + end.Minute()
	if end.Equal(service.BookingDay(start).AddDate(0, 0, 1)) {
		endMin = 24 * 60
	}
	d.original = b
	d.d = service.Draft{Title: b.Title, Day: service.BookingDay(start), StartMin: start.Hour()*60 + start.Minute(), EndMin: endMin, Replaces: b.ID}
	d.hasDay, d.hasStart, d.hasEnd = true, true, true
	show(s, i, c.pickerView(d, ""))
}

func selectedValue(i *discordgo.InteractionCreate) string {
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (c *Calendar) handleDraftComponent(s *discordgo.Session, i *discordgo.InteractionCreate, a *bookingActor, draftID, action string) {
	d := c.draft(draftID, a.ID)
	if d == nil {
		show(s, i, expiredDraft)
		return
	}
	if d.result != nil && action != "submit" {
		show(s, i, *d.result)
		return
	}
	switch action {
	case "day":
		day, err := time.ParseInLocation("2006-01-02", selectedValue(i), service.BookingLocation)
		if err == nil {
			d.d.Day, d.hasDay = day, true
			// Keep the start and length only while they still fit.
			if d.hasStart {
				if start, err := service.BookingAt(day, d.d.StartMin); err != nil || !start.After(c.now()) || !isFree(c.busyAround(day), start, start.Add(30*time.Minute), d.ignore()) {
					d.hasStart, d.hasEnd = false, false
				}
			}
		}
	case "start":
		if m, err := service.ParseBookingClock(selectedValue(i)); err == nil {
			length := d.length()
			d.d.StartMin, d.hasStart = m, true
			if d.hasEnd {
				d.d.EndMin = endMinute(m, length)
				if start, err := service.BookingAt(d.d.Day, m); err != nil || fitsUntil(c.busyAround(d.d.Day), start, d.ignore()) < time.Duration(length)*time.Minute {
					d.hasEnd = false
				}
			}
		}
	case "len":
		var length int
		if _, err := fmt.Sscan(selectedValue(i), &length); err == nil && length > 0 && length < 24*60 {
			d.d.EndMin, d.hasEnd = endMinute(d.d.StartMin, length), true
		}
	case "repeat":
		d.d.Repeat = selectedValue(i)
		if d.d.Repeat == "none" {
			d.d.Repeat = ""
		}
	case "edit":
	case "cancel":
		c.draftsMu.Lock()
		delete(c.drafts, d.id)
		c.draftsMu.Unlock()
		show(s, i, view{content: "Cancelled — nothing was requested."})
		return
	case "next":
		if d.original != nil {
			show(s, i, c.confirmView(d))
			return
		}
		c.showDraftModal(s, i, d)
		return
	case "submit":
		c.submit(s, i, a, d)
		return
	}
	show(s, i, c.pickerView(d, ""))
}

func (c *Calendar) showDraftModal(s *discordgo.Session, i *discordgo.InteractionCreate, d *bookingDraft) {
	inputs := []discordgo.MessageComponent{row(discordgo.TextInput{CustomID: "title", Label: "What is the booking for?", Style: discordgo.TextInputShort, Placeholder: "e.g. WIE committee meeting", Value: d.d.Title, Required: true, MaxLength: 80})}
	if step := service.RepeatStep(d.d.Repeat); step > 0 {
		until := d.d.Until
		if until.IsZero() || !until.After(d.d.Day) {
			until = d.d.Day.AddDate(0, 0, 7*step)
		}
		inputs = append(inputs, row(discordgo.TextInput{CustomID: "until", Label: "Repeat until (last date)", Style: discordgo.TextInputShort, Placeholder: "e.g. dec 18 or 2026-12-18", Value: until.Format("2006-01-02"), Required: true, MaxLength: 20}))
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseModal, Data: &discordgo.InteractionResponseData{CustomID: "office:dm:" + d.id, Title: "Book office time", Components: inputs}}); err != nil {
		log.Error("booking modal failed", "err", err)
	}
}

func (c *Calendar) handleDraftModal(s *discordgo.Session, i *discordgo.InteractionCreate, a *bookingActor, draftID string) {
	d := c.draft(draftID, a.ID)
	if d == nil {
		show(s, i, expiredDraft)
		return
	}
	values := modalValues(i)
	d.d.Title = values["title"]
	if text, ok := values["until"]; ok {
		until, err := service.ParseBookingDate(text, c.now())
		if err != nil {
			show(s, i, c.pickerView(d, "Repeat until: "+bookingError(err)))
			return
		}
		d.d.Until = until
	}
	show(s, i, c.confirmView(d))
}

func modalValues(i *discordgo.InteractionCreate) map[string]string {
	values := map[string]string{}
	for _, component := range i.ModalSubmitData().Components {
		r, ok := component.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, child := range r.Components {
			if input, ok := child.(*discordgo.TextInput); ok {
				values[input.CustomID] = input.Value
			}
		}
	}
	return values
}

// ---- /book ----

func (c *Calendar) handleBookCommand(s *discordgo.Session, i *discordgo.InteractionCreate, a *bookingActor) {
	if !a.access.Book {
		notice(s, i, bookingError(service.ErrBookPermission))
		return
	}
	opts := map[string]string{}
	for _, opt := range i.ApplicationCommandData().Options {
		opts[opt.Name] = opt.StringValue()
	}
	d := c.newDraft(a.ID)
	d.d.Title = opts["purpose"]
	d.d.Repeat = opts["repeat"]
	problems := []string{}
	if day, err := service.ParseBookingDate(opts["date"], c.now()); err == nil {
		d.d.Day, d.hasDay = day, true
	} else {
		problems = append(problems, "Date: "+bookingError(err))
	}
	start, err := service.ParseBookingClock(opts["start"])
	if err == nil && start < 24*60 {
		d.d.StartMin, d.hasStart = start, true
	} else {
		problems = append(problems, "Start: enter a time like 14:00 or 2pm.")
	}
	if end, err := service.ParseBookingClock(opts["end"]); err == nil {
		d.d.EndMin, d.hasEnd = end, d.hasStart
	} else {
		problems = append(problems, "End: "+bookingError(err))
	}
	if step := service.RepeatStep(d.d.Repeat); step > 0 && d.hasDay {
		if text := opts["until"]; text != "" {
			if until, err := service.ParseBookingDate(text, c.now()); err == nil {
				d.d.Until = until
			} else {
				problems = append(problems, "Until: "+bookingError(err))
			}
		} else {
			d.d.Until = d.d.Day.AddDate(0, 0, 7*step)
		}
	}
	if len(problems) > 0 {
		showNew(s, i, c.pickerView(d, strings.Join(problems, " ")))
		return
	}
	showNew(s, i, c.confirmView(d))
}
