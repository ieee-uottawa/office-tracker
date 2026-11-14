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

// Render uses wall-clock hours so rows remain consistent across weeks with DST.
func Render(week time.Time, bookings []repository.Booking, _ time.Time) ([]byte, error) {
	week = service.BookingWeek(week)
	loc := service.BookingLocation
	minHour, maxHour := 8, 22
	for _, b := range bookings {
		a, z := b.Start.In(loc), b.End.In(loc)
		if a.Hour() < minHour {
			minHour = a.Hour()
		}
		endHour := z.Hour()
		if z.Minute() > 0 {
			endHour++
		}
		if z.Format("2006-01-02") != a.Format("2006-01-02") {
			endHour = 24
		}
		if endHour > maxHour {
			maxHour = endHour
		}
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
	for day := 0; day < 7; day++ {
		d := week.AddDate(0, 0, day)
		x := left + day*col
		p.text(x+14, 30, 19, col-28, true, ink, d.Format("Mon"))
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
	accent, fill := rgb(52, 86, 130), rgb(226, 236, 248)
	for _, b := range bookings {
		a, z := b.Start.In(loc), b.End.In(loc)
		day := -1
		for j := 0; j < 7; j++ {
			if a.Format("2006-01-02") == week.AddDate(0, 0, j).Format("2006-01-02") {
				day = j
				break
			}
		}
		if day < 0 {
			continue
		}
		startMinute := a.Hour()*60 + a.Minute()
		endMinute := z.Hour()*60 + z.Minute()
		if a.Format("2006-01-02") != z.Format("2006-01-02") {
			endMinute = 1440
		}
		y := top + (startMinute-minHour*60)*row/60
		h := (endMinute - startMinute) * row / 60
		if h < 1 {
			h = 1
		}
		x := left + day*col + 5
		w := col - 10
		p.box(x, y, w, h-1, fill)
		p.box(x, y, 3, h-1, accent)
		if h < 40 {
			p.text(x+10, y+min(h-3, 19), 13, w-18, true, accent, b.Name)
		} else {
			p.text(x+12, y+22, 18, w-23, true, accent, b.Name)
			if h >= 63 {
				p.text(x+12, y+45, 16, w-23, false, ink, b.Title)
			}
			if h >= 87 {
				p.text(x+12, y+68, 14, w-23, false, muted, a.Format("15:04")+"–"+endLabel(z, a))
			}
		}
	}
	var buf bytes.Buffer
	err := png.Encode(&buf, p.im)
	return buf.Bytes(), err
}

func endLabel(end, start time.Time) string {
	if end.Format("2006-01-02") != start.Format("2006-01-02") {
		return "24:00"
	}
	return end.Format("15:04")
}
