// Package calendarimage renders the actual reservation data into a Discord PNG.
package calendarimage

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"time"

	"office/internal/repository"
	"office/internal/service"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

var regularFont = mustFont(goregular.TTF)
var boldFont = mustFont(gobold.TTF)

func mustFont(data []byte) *opentype.Font {
	f, err := opentype.Parse(data)
	if err != nil {
		panic(err)
	}
	return f
}

type painter struct {
	im    *image.RGBA
	faces map[string]font.Face
}

func (p *painter) face(size int, bold bool) font.Face {
	key := fmt.Sprintf("%d:%t", size, bold)
	if face, ok := p.faces[key]; ok {
		return face
	}
	f := regularFont
	if bold {
		f = boldFont
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err)
	}
	p.faces[key] = face
	return face
}

func (p *painter) box(x, y, w, h int, c color.RGBA) {
	draw.Draw(p.im, image.Rect(x, y, x+w, y+h), &image.Uniform{C: c}, image.Point{}, draw.Src)
}

func (p *painter) text(x, y, size, width int, bold bool, c color.RGBA, text string) {
	face := p.face(size, bold)
	if font.MeasureString(face, text).Ceil() > width {
		runes := []rune(text)
		for len(runes) > 0 && font.MeasureString(face, string(runes)+"…").Ceil() > width {
			runes = runes[:len(runes)-1]
		}
		text = string(runes) + "…"
	}
	d := font.Drawer{Dst: p.im, Src: &image.Uniform{C: c}, Face: face, Dot: fixed.P(x, y)}
	d.DrawString(text)
}

func rgb(r, g, b uint8) color.RGBA { return color.RGBA{R: r, G: g, B: b, A: 255} }

type segment struct {
	day, from, to int // day of week, minutes after local midnight
	booking       repository.Booking
	first         bool // the segment where the booking starts
}

// segments splits bookings into per-day pieces so overnight bookings appear
// on both days.
func segments(week time.Time, bookings []repository.Booking) []segment {
	result := []segment{}
	for _, b := range bookings {
		for day := 0; day < 7; day++ {
			dayStart := week.AddDate(0, 0, day)
			dayEnd := week.AddDate(0, 0, day+1)
			a, z := b.Start, b.End
			if !a.Before(dayEnd) || !z.After(dayStart) {
				continue
			}
			if a.Before(dayStart) {
				a = dayStart
			}
			if z.After(dayEnd) {
				z = dayEnd
			}
			// Use wall-clock minutes so rows stay aligned across DST changes.
			la := a.In(service.BookingLocation)
			from, to := la.Hour()*60+la.Minute(), 24*60
			if !z.Equal(dayEnd) {
				lz := z.In(service.BookingLocation)
				to = lz.Hour()*60 + lz.Minute()
			}
			result = append(result, segment{day: day, from: from, to: to, booking: b, first: !b.Start.Before(dayStart)})
		}
	}
	return result
}

// Render draws the week's held (pending, hatched) and approved bookings.
// Rows use wall-clock hours so they stay consistent across weeks with DST.
func Render(week time.Time, bookings []repository.Booking, now time.Time) ([]byte, error) {
	week = service.BookingWeek(week)
	loc := service.BookingLocation
	pieces := segments(week, bookings)
	minHour, maxHour := 8, 22
	for _, seg := range pieces {
		minHour = min(minHour, seg.from/60)
		maxHour = max(maxHour, (seg.to+59)/60)
	}
	const width, left, top, col, row = 1680, 100, 48, 220, 68
	gridHeight := (maxHour - minHour) * row
	p := &painter{im: image.NewRGBA(image.Rect(0, 0, width, top+gridHeight+20)), faces: map[string]font.Face{}}
	defer func() {
		for _, face := range p.faces {
			face.Close()
		}
	}()
	ink, muted, line := rgb(35, 38, 42), rgb(105, 110, 116), rgb(224, 227, 231)
	p.box(0, 0, width, p.im.Bounds().Dy(), rgb(255, 255, 255))
	today := service.BookingDay(now)
	for day := 0; day < 7; day++ {
		d := week.AddDate(0, 0, day)
		x := left + day*col
		if d.Before(today) {
			p.box(x, top, col, gridHeight, rgb(246, 247, 249))
		}
		label, color := d.Format("Mon 2"), ink
		if d.Before(today) {
			color = muted
		}
		if d.Equal(today) {
			p.box(x+1, 0, col-1, top, rgb(232, 240, 252))
			color = rgb(32, 84, 160)
		}
		p.text(x+14, 30, 19, col-28, true, color, label)
		p.box(x, top, 1, gridHeight, line)
	}
	p.box(left+7*col, top, 1, gridHeight, line)
	for hour := minHour; hour <= maxHour; hour++ {
		y := top + (hour-minHour)*row
		p.box(left, y, 7*col, 1, line)
		p.text(36, y+5, 15, 58, false, muted, fmt.Sprintf("%02d:00", hour))
		if hour < maxHour {
			p.box(left, y+row/2, 7*col, 1, rgb(241, 244, 249))
		}
	}
	for _, seg := range pieces {
		b := seg.booking
		y := top + (seg.from-minHour*60)*row/60
		h := max(1, (seg.to-seg.from)*row/60)
		x := left + seg.day*col + 5
		w := col - 10
		accent, fill := rgb(52, 86, 130), rgb(226, 236, 248)
		if b.Status == "pending" {
			accent, fill = rgb(150, 105, 20), rgb(253, 246, 228)
		}
		if b.End.Before(now) || b.End.Equal(now) {
			accent, fill = rgb(140, 146, 154), rgb(238, 240, 243)
		}
		p.box(x, y, w, h-1, fill)
		if b.Status == "pending" {
			// Diagonal hatching marks a held request that is not approved yet.
			stripe := rgb(245, 228, 186)
			for yy := y; yy < y+h-1; yy++ {
				for xx := x; xx < x+w; xx++ {
					if (xx+yy)%14 < 3 {
						p.im.SetRGBA(xx, yy, stripe)
					}
				}
			}
		}
		p.box(x, y, 3, h-1, accent)
		name := b.Name
		if b.Status == "pending" {
			name = "Pending · " + name
		}
		if !seg.first {
			name += " (cont.)"
		}
		a, z := b.Start.In(loc), b.End.In(loc)
		if h < 40 {
			p.text(x+10, y+min(h-3, 19), 13, w-18, true, accent, name)
		} else {
			p.text(x+12, y+22, 18, w-23, true, accent, name)
			if h >= 63 {
				p.text(x+12, y+45, 16, w-23, false, ink, b.Title)
			}
			if h >= 87 {
				p.text(x+12, y+68, 14, w-23, false, muted, a.Format("15:04")+"–"+endLabel(z, a))
			}
		}
	}
	if !now.Before(week) && now.Before(week.AddDate(0, 0, 7)) {
		local := now.In(loc)
		minute := local.Hour()*60 + local.Minute()
		if minute >= minHour*60 && minute <= maxHour*60 {
			day := int(today.Sub(week).Hours() / 24)
			if day >= 0 && day < 7 {
				y := top + (minute-minHour*60)*row/60
				red := rgb(214, 48, 49)
				p.box(left+day*col, y-1, col, 3, red)
				p.box(left+day*col-4, y-5, 10, 10, red)
			}
		}
	}
	var buf bytes.Buffer
	err := png.Encode(&buf, p.im)
	return buf.Bytes(), err
}

func endLabel(end, start time.Time) string {
	if service.BookingDay(end) != service.BookingDay(start) {
		if end.Equal(service.BookingDay(start).AddDate(0, 0, 1)) {
			return "24:00"
		}
		return end.Format("15:04") + " +1d"
	}
	return end.Format("15:04")
}
