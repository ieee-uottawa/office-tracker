package http

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"office/internal/repository"
	"office/internal/service"
)

//go:embed web/templates/*.html web/assets
var webFiles embed.FS

// webApp serves the signed-in website: the booking calendar and the admin pages.
type webApp struct {
	auth         *webAuth
	cfg          WebConfig
	pages        map[string]*template.Template
	assets       http.Handler
	assetVersion string
	now          func() time.Time
}

func newWebApp(cfg WebConfig) (*webApp, error) {
	app := &webApp{auth: newWebAuth(cfg), cfg: cfg, pages: map[string]*template.Template{}, now: time.Now}
	for _, page := range []string{"booking", "admin", "login", "message"} {
		t, err := template.ParseFS(webFiles, "web/templates/layout.html", "web/templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		app.pages[page] = t
	}
	assets, err := fs.Sub(webFiles, "web/assets")
	if err != nil {
		return nil, err
	}
	css, err := fs.ReadFile(assets, "app.css")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(css)
	for _, name := range []string{"js/common.js", "js/booking.js", "js/admin.js"} {
		js, _ := fs.ReadFile(assets, name)
		h.Write(js)
	}
	sum := h.Sum(nil)
	app.assetVersion = hex.EncodeToString(sum[:])[:10]
	files := http.StripPrefix("/assets/", http.FileServer(http.FS(assets)))
	app.assets = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		if strings.HasPrefix(r.URL.Path, "/assets/js/") || strings.HasSuffix(r.URL.Path, ".css") {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		files.ServeHTTP(w, r)
	})
	return app, nil
}

type pageData struct {
	Title, Page, CSRF, AssetVersion string
	Year                            int
	User                            *webUser
	Scripts                         []string
	ClientData                      map[string]any
	Heading, Message                string
	Action                          string
	ActionURL                       string
}

func (a *webApp) render(w http.ResponseWriter, status int, page string, d pageData) {
	d.AssetVersion, d.Year, d.Page = a.assetVersion, a.now().Year(), firstNonEmpty(d.Page, page)
	if d.User != nil {
		d.CSRF = d.User.csrf
	}
	if d.ClientData == nil {
		d.ClientData = map[string]any{}
	}
	d.ClientData["csrf"] = d.CSRF
	if d.User != nil {
		d.ClientData["user"] = map[string]any{"id": d.User.ID, "name": d.User.Name, "book": d.User.Book, "approve": d.User.Approve, "admin": d.User.Admin}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.WriteHeader(status)
	if err := a.pages[page].ExecuteTemplate(w, "layout", d); err != nil {
		log.Error("render page failed", "page", page, "err", err)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// currentUser renders the right page for signed-out people and non-members.
func (a *webApp) currentUser(w http.ResponseWriter, r *http.Request) *webUser {
	u, err := a.auth.user(r)
	switch {
	case errors.Is(err, errNotMember):
		a.auth.logout(w)
		a.render(w, http.StatusForbidden, "message", pageData{Title: "Exec members only", Heading: "Exec members only", Message: "The office calendar is for members of the IEEE uOttawa exec Discord server. Ask an exec to add you, then sign in again.", Action: "Sign in again", ActionURL: "/login"})
		return nil
	case err != nil:
		log.Error("load web user failed", "err", err)
		a.render(w, http.StatusServiceUnavailable, "message", pageData{Title: "Discord is unavailable", Heading: "Discord is unavailable", Message: "We couldn't check your Discord roles. Try again in a minute.", Action: "Try again", ActionURL: r.URL.RequestURI()})
		return nil
	case u == nil:
		a.render(w, http.StatusOK, "login", pageData{Title: "Sign in", ClientData: map[string]any{"next": safeNext(r.URL.RequestURI())}, ActionURL: "/login?next=" + urlQueryEscape(safeNext(r.URL.RequestURI()))})
		return nil
	}
	return u
}

func urlQueryEscape(s string) string {
	return strings.NewReplacer("%", "%25", "&", "%26", "?", "%3F", "#", "%23", "+", "%2B", " ", "%20").Replace(s)
}

// ServeHTTP routes the website's own paths.
func (a *webApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; {
	case strings.HasPrefix(p, "/assets/"):
		a.assets.ServeHTTP(w, r)
	case p == "/login":
		a.auth.startLogin(w, r)
	case p == "/auth/discord/callback":
		next, err := a.auth.callback(w, r)
		if err != nil {
			log.Warn("discord sign-in failed", "err", err)
			a.render(w, http.StatusBadRequest, "message", pageData{Title: "Sign-in failed", Heading: "Sign-in didn't finish", Message: "Discord didn't complete the sign-in. Try again.", Action: "Sign in with Discord", ActionURL: "/login?next=" + urlQueryEscape(next)})
			return
		}
		http.Redirect(w, r, next, http.StatusFound)
	case p == "/logout":
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if u, _ := a.auth.user(r); u != nil && !a.auth.validCSRF(r, u) {
			http.Error(w, "invalid request", http.StatusForbidden)
			return
		}
		a.auth.logout(w)
		http.Redirect(w, r, "/", http.StatusFound)
	case p == "/ui" || strings.HasPrefix(p, "/ui/"):
		http.Redirect(w, r, "/admin", http.StatusMovedPermanently)
	case p == "/admin":
		u := a.currentUser(w, r)
		if u == nil {
			return
		}
		if !u.Admin {
			a.render(w, http.StatusForbidden, "message", pageData{Title: "Admins only", User: u, Heading: "Admins only", Message: "The admin pages are for members with Manage Server in the exec Discord server.", Action: "Back to the calendar", ActionURL: "/"})
			return
		}
		a.render(w, http.StatusOK, "admin", pageData{Title: "Admin", User: u, Scripts: []string{"admin.js"}})
	case p == "/":
		u := a.currentUser(w, r)
		if u == nil {
			return
		}
		a.render(w, http.StatusOK, "booking", pageData{Title: "Office booking", Page: "calendar", User: u, Scripts: []string{"booking.js"},
			ClientData: map[string]any{"ics": strings.TrimRight(a.cfg.PublicURL, "/") + "/calendar/" + a.auth.icsToken() + ".ics", "view": r.URL.Query().Get("view"), "today": service.BookingDay(a.now()).Format("2006-01-02")}})
	case strings.HasPrefix(p, "/calendar/") && strings.HasSuffix(p, ".ics"):
		a.serveICS(w, r)
	case strings.HasPrefix(p, "/api/web/"):
		a.serveAPI(w, r)
	default:
		a.render(w, http.StatusNotFound, "message", pageData{Title: "Not found", Heading: "Page not found", Message: "That page doesn't exist.", Action: "Back to the calendar", ActionURL: "/"})
	}
}

// isWebPath reports the paths the website handles itself.
func isWebPath(p string) bool {
	return p == "/" || p == "/login" || p == "/logout" || p == "/admin" || p == "/ui" || strings.HasPrefix(p, "/ui/") ||
		strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/auth/") || strings.HasPrefix(p, "/api/web/") || strings.HasPrefix(p, "/calendar/")
}

// isManagementPath lists the admin API used by the admin pages and scripts.
// Device endpoints (RFID scans, sensor posts, the reader relay) are not included.
func isManagementPath(r *http.Request) bool {
	p := r.URL.Path
	for _, prefix := range []string{"/api/users", "/api/sessions", "/api/statistics", "/api/reports", "/api/rfid/scans", "/api/presence"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return r.Method == http.MethodGet && (p == "/api/environment" || p == "/api/devices/health")
}

// gate protects the management API with an admin session or the API key, and
// keeps the existing API-key rule for everything else.
func (a *webApp) gate(apiKeyEnabled bool, next http.Handler) http.Handler {
	keyOK := func(r *http.Request) bool {
		if a.cfg.APIKey == "" {
			return false
		}
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key, _ = strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		return key != "" && key == a.cfg.APIKey
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isWebPath(r.URL.Path) || r.URL.Path == "/health" || r.URL.Path == "/api/reader/request":
		case isManagementPath(r):
			if !keyOK(r) {
				u, err := a.auth.user(r)
				if err != nil || u == nil || !u.Admin {
					writeJSONError(w, http.StatusUnauthorized, "Sign in as an admin to use this.")
					return
				}
				if r.Method != http.MethodGet && r.Method != http.MethodHead && !a.auth.validCSRF(r, u) {
					writeJSONError(w, http.StatusForbidden, "Refresh the page and try again.")
					return
				}
			}
		case apiKeyEnabled && !keyOK(r):
			writeJSONError(w, http.StatusUnauthorized, "Unauthorized: invalid or missing API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeWebJSON(w, status, map[string]string{"error": message})
}

func writeWebJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---- booking API ----

type bookingJSON struct {
	ID       int64  `json:"id"`
	Lead     int64  `json:"lead"`
	SeriesID int64  `json:"series_id,omitempty"`
	Replaces int64  `json:"replaces,omitempty"`
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Date     string `json:"date"`  // local start date
	Start    int    `json:"start"` // minutes after local midnight of Date
	End      int    `json:"end"`   // may exceed 1440 for overnight bookings
	Mine     bool   `json:"mine"`
}

func toJSON(b repository.Booking, me string) bookingJSON {
	start, end := b.Start.In(service.BookingLocation), b.End.In(service.BookingLocation)
	day := service.BookingDay(start)
	endMin := end.Hour()*60 + end.Minute()
	if days := int(service.BookingDay(end).Sub(day).Hours()/24 + 0.5); days > 0 {
		endMin += days * 24 * 60
	}
	return bookingJSON{ID: b.ID, Lead: b.Lead(), SeriesID: b.SeriesID, Replaces: b.ReplacesID, UserID: b.UserID, Name: b.Name, Title: b.Title, Status: b.Status, Reason: b.Reason,
		Date: day.Format("2006-01-02"), Start: start.Hour()*60 + start.Minute(), End: endMin, Mine: b.UserID == me}
}

type draftJSON struct {
	RequestID string `json:"request_id"`
	Title     string `json:"title"`
	Date      string `json:"date"`
	Start     int    `json:"start"`
	End       int    `json:"end"`
	Repeat    string `json:"repeat"`
	Until     string `json:"until"`
	Replaces  int64  `json:"replaces"`
}

func (d draftJSON) draft() (service.Draft, error) {
	day, err := time.ParseInLocation("2006-01-02", d.Date, service.BookingLocation)
	if err != nil {
		return service.Draft{}, service.BookingInputError("choose a day")
	}
	if d.Start < 0 || d.Start >= 24*60 || d.End <= d.Start || d.End-d.Start > 24*60 {
		return service.Draft{}, service.BookingInputError("choose a start and end time")
	}
	out := service.Draft{Title: d.Title, Day: day, StartMin: d.Start, EndMin: d.End % (24 * 60), Repeat: d.Repeat, Replaces: d.Replaces}
	if d.End == 24*60 {
		out.EndMin = 24 * 60
	}
	if d.Repeat != "" {
		if out.Until, err = time.ParseInLocation("2006-01-02", d.Until, service.BookingLocation); err != nil {
			return service.Draft{}, service.BookingInputError("choose when the repeat ends")
		}
	}
	return out, nil
}

func (a *webApp) serveAPI(w http.ResponseWriter, r *http.Request) {
	u, err := a.auth.user(r)
	if err != nil && !errors.Is(err, errNotMember) {
		writeJSONError(w, http.StatusServiceUnavailable, "Discord is unavailable. Try again in a minute.")
		return
	}
	if u == nil {
		writeJSONError(w, http.StatusUnauthorized, "Sign in to continue.")
		return
	}
	if r.Method != http.MethodGet && !a.auth.validCSRF(r, u) {
		writeJSONError(w, http.StatusForbidden, "Refresh the page and try again.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/web/")
	parts := strings.Split(path, "/")
	guild, now := a.cfg.GuildID, a.now()
	repo := a.cfg.Booking.Repo
	roles := service.MemberGrants(u.ID, u.Roles)
	fail := func(err error) {
		if errors.Is(err, errBadRequest) {
			writeJSONError(w, http.StatusBadRequest, "Invalid request.")
			return
		}
		msg := service.BookingErrorMessage(err)
		status := http.StatusBadRequest
		if msg == "Couldn't save the change. Please try again." {
			if strings.Contains(err.Error(), "no rows") {
				writeJSONError(w, http.StatusNotFound, "That booking no longer exists.")
				return
			}
			log.Error("web booking action failed", "err", err)
			status = http.StatusInternalServerError
		}
		writeJSONError(w, status, msg)
	}
	changed := func() {
		if a.cfg.OnBookingChange != nil {
			go a.cfg.OnBookingChange()
		}
	}
	readBody := func(v any) bool {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid request.")
			return false
		}
		return true
	}
	list := func(rows []repository.Booking) []bookingJSON {
		out := make([]bookingJSON, 0, len(rows))
		for _, b := range rows {
			out = append(out, toJSON(b, u.ID))
		}
		return out
	}

	switch {
	case r.Method == http.MethodGet && path == "me":
		writeWebJSON(w, http.StatusOK, map[string]any{"id": u.ID, "name": u.Name, "book": u.Book, "approve": u.Approve, "admin": u.Admin})

	case r.Method == http.MethodGet && path == "bookings":
		from, err1 := time.ParseInLocation("2006-01-02", r.URL.Query().Get("from"), service.BookingLocation)
		to, err2 := time.ParseInLocation("2006-01-02", r.URL.Query().Get("to"), service.BookingLocation)
		if err1 != nil || err2 != nil || !to.After(from) || to.Sub(from) > 62*24*time.Hour {
			writeJSONError(w, http.StatusBadRequest, "Invalid date range.")
			return
		}
		rows, err := repo.Busy(guild, from, to.AddDate(0, 0, 1))
		if err != nil {
			fail(err)
			return
		}
		writeWebJSON(w, http.StatusOK, map[string]any{"bookings": list(rows), "today": service.BookingDay(now).Format("2006-01-02"), "now": now.In(service.BookingLocation).Hour()*60 + now.In(service.BookingLocation).Minute()})

	case r.Method == http.MethodGet && path == "mine":
		rows, err := repo.Mine(guild, u.ID, now)
		if err != nil {
			fail(err)
			return
		}
		writeWebJSON(w, http.StatusOK, map[string]any{"bookings": list(rows)})

	case r.Method == http.MethodGet && path == "requests":
		if !u.Approve {
			writeJSONError(w, http.StatusForbidden, service.BookingErrorMessage(service.ErrBookingPermission))
			return
		}
		rows, err := repo.Pending(guild, now)
		if err != nil {
			fail(err)
			return
		}
		writeWebJSON(w, http.StatusOK, map[string]any{"bookings": list(rows)})

	case r.Method == http.MethodGet && len(parts) == 2 && parts[0] == "bookings":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		b, err := repo.Get(guild, id)
		if err != nil {
			fail(err)
			return
		}
		writeWebJSON(w, http.StatusOK, a.detail(r, *b, u))

	case r.Method == http.MethodPost && path == "plan":
		var d draftJSON
		if !readBody(&d) {
			return
		}
		draft, err := d.draft()
		if err == nil {
			if strings.TrimSpace(draft.Title) == "" {
				draft.Title = "-" // the purpose is checked on submit; plan only the times
			}
			var slots []service.Slot
			if slots, err = a.cfg.Booking.Plan(guild, draft, now); err == nil {
				out := []map[string]any{}
				for _, s := range slots {
					out = append(out, map[string]any{"date": service.BookingDay(s.Start).Format("2006-01-02"), "taken": s.Taken})
				}
				writeWebJSON(w, http.StatusOK, map[string]any{"slots": out})
				return
			}
		}
		writeWebJSON(w, http.StatusOK, map[string]any{"error": service.BookingErrorMessage(err), "slots": []any{}})

	case r.Method == http.MethodPost && path == "bookings":
		var d draftJSON
		if !readBody(&d) {
			return
		}
		draft, err := d.draft()
		if err != nil {
			fail(err)
			return
		}
		request := "web:" + u.ID + ":" + d.RequestID
		if d.RequestID == "" || len(d.RequestID) > 64 {
			request = "web:" + randomToken()
		}
		created, skipped, err := a.cfg.Booking.Submit(request, guild, u.ID, u.Name, roles, draft, now)
		if err != nil {
			fail(err)
			return
		}
		changed()
		writeWebJSON(w, http.StatusOK, map[string]any{"bookings": list(created), "skipped": skipped})

	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "bookings":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		var body struct {
			Rest    bool    `json:"rest"`
			Reason  string  `json:"reason"`
			Approve []int64 `json:"approve"`
		}
		if !readBody(&body) {
			return
		}
		switch parts[2] {
		case "cancel":
			err = a.cfg.Booking.Cancel(guild, id, u.ID, body.Rest, now)
		case "decide":
			err = a.cfg.Booking.DecideDates(guild, id, u.ID, roles, body.Approve, body.Reason, now)
		case "revoke":
			err = a.cfg.Booking.Revoke(guild, id, u.ID, roles, body.Rest, body.Reason, now)
		default:
			err = errBadRequest
		}
		if err != nil {
			fail(err)
			return
		}
		changed()
		b, err := repo.Get(guild, id)
		if err != nil {
			fail(err)
			return
		}
		writeWebJSON(w, http.StatusOK, a.detail(r, *b, u))

	default:
		writeJSONError(w, http.StatusNotFound, "Not found.")
	}
}

var errBadRequest = errors.New("bad request")

type historyItem struct {
	At   string `json:"at"`
	Text string `json:"text"`
}

func (a *webApp) detail(r *http.Request, b repository.Booking, u *webUser) map[string]any {
	repo := a.cfg.Booking.Repo
	rows, _ := repo.Series(a.cfg.GuildID, b.Lead())
	series := []bookingJSON{}
	for _, row := range rows {
		series = append(series, toJSON(row, u.ID))
	}
	out := map[string]any{"booking": toJSON(b, u.ID), "series": series}
	if b.ReplacesID > 0 {
		if o, err := repo.Get(a.cfg.GuildID, b.ReplacesID); err == nil {
			out["original"] = toJSON(*o, u.ID)
		}
	}
	if change, err := repo.PendingChange(a.cfg.GuildID, b.ID); err == nil && change != nil {
		out["change"] = toJSON(*change, u.ID)
	}
	who := func(id string) string {
		if id == u.ID {
			return "you"
		}
		return a.auth.memberName(r.Context(), id)
	}
	stamp := func(t time.Time) string { return service.FormatDay(t) + ", " + service.FormatClock(t) }
	history := []historyItem{{At: stamp(b.CreatedAt), Text: "Requested by " + who(b.UserID)}}
	if !b.ReviewedAt.IsZero() {
		text := ""
		switch b.Status {
		case "approved":
			text = "Approved by " + who(b.ReviewedBy)
		case "declined":
			text = "Declined by " + who(b.ReviewedBy)
		case "revoked":
			text = "Revoked by " + who(b.ReviewedBy)
		case "cancelled":
			text = "Cancelled by " + who(b.ReviewedBy)
		case "expired":
			text = "Expired before anyone decided"
		case "replaced":
			text = "Moved to a new time"
		}
		if text != "" {
			history = append(history, historyItem{At: stamp(b.ReviewedAt), Text: text})
		}
	}
	out["history"] = history
	return out
}

// ---- calendar feed (one-way sync to Google Calendar and other apps) ----

func (a *webApp) serveICS(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/calendar/"), ".ics")
	if token != a.auth.icsToken() {
		http.NotFound(w, r)
		return
	}
	now := a.now()
	rows, err := a.cfg.Booking.Repo.Approved(a.cfg.GuildID, now.AddDate(0, -1, 0), now.AddDate(1, 0, 0))
	if err != nil {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	esc := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`, "\r", "")
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\r\n") }
	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:-//IEEE uOttawa//Office Tracker//EN")
	line("CALSCALE:GREGORIAN")
	line("X-WR-CALNAME:IEEE uOttawa Office")
	line("X-WR-TIMEZONE:America/Toronto")
	line("REFRESH-INTERVAL;VALUE=DURATION:PT15M")
	line("X-PUBLISHED-TTL:PT15M")
	for _, bk := range rows {
		line("BEGIN:VEVENT")
		line(fmt.Sprintf("UID:booking-%d@office-tracker.ieeeuottawa.ca", bk.ID))
		line("DTSTAMP:" + now.UTC().Format("20060102T150405Z"))
		line("DTSTART:" + bk.Start.UTC().Format("20060102T150405Z"))
		line("DTEND:" + bk.End.UTC().Format("20060102T150405Z"))
		line("SUMMARY:" + esc.Replace(bk.Title+" ("+bk.Name+")"))
		line("LOCATION:IEEE uOttawa office")
		line("END:VEVENT")
	}
	line("END:VCALENDAR")
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=300")
	_, _ = w.Write([]byte(b.String()))
}
