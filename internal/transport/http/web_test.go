package http

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"office/internal/database"
	"office/internal/repository"
	"office/internal/service"

	"github.com/stretchr/testify/require"
)

// fakeDiscord answers the few Discord API calls the website makes.
type fakeDiscord struct {
	mu      sync.Mutex
	members map[string][]string // user ID -> role IDs
	owner   string
	login   string // user returned by /users/@me
}

func (f *fakeDiscord) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/oauth2/token":
		id, secret, _ := r.BasicAuth()
		if id != "client" || secret != "secret" || r.FormValue("code") != "good-code" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"user-token"}`))
	case r.URL.Path == "/users/@me":
		if r.Header.Get("Authorization") != "Bearer user-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": f.login, "username": f.login, "global_name": strings.ToUpper(f.login[:1]) + f.login[1:]})
	case r.URL.Path == "/guilds/exec":
		_ = json.NewEncoder(w).Encode(map[string]any{"owner_id": f.owner, "roles": []map[string]string{
			{"id": "exec", "permissions": "0"},
			{"id": "managers", "permissions": "32"},
			{"id": service.SigningOfficerRoleID, "permissions": "0"},
		}})
	case strings.HasPrefix(r.URL.Path, "/guilds/exec/members/"):
		id := strings.TrimPrefix(r.URL.Path, "/guilds/exec/members/")
		roles, ok := f.members[id]
		if !ok {
			http.Error(w, `{"message":"Unknown Member"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"nick": "", "roles": roles, "user": map[string]string{"id": id, "username": id, "global_name": strings.ToUpper(id[:1]) + id[1:]}})
	default:
		http.NotFound(w, r)
	}
}

type webHarness struct {
	t       *testing.T
	server  *httptest.Server
	discord *fakeDiscord
	booking *service.BookingService
	changes int
	mu      sync.Mutex
}

func newWebHarness(t *testing.T) *webHarness {
	t.Helper()
	discord := &fakeDiscord{members: map[string][]string{
		"alice":   {"exec"},
		"officer": {"exec", service.SigningOfficerRoleID},
		"admin":   {"exec", "managers"},
	}, owner: "owner"}
	stub := httptest.NewServer(discord)
	t.Cleanup(stub.Close)
	previous := discordAPI
	discordAPI = stub.URL
	t.Cleanup(func() { discordAPI = previous })

	db := database.Open(filepath.Join(t.TempDir(), "web.db"))
	t.Cleanup(func() { db.Close() })
	require.NoError(t, database.Migrate(db))
	userRepo, sessionRepo := &repository.UserRepo{DB: db}, &repository.SessionRepo{DB: db}
	srv := New("0", service.NewAttendanceService(userRepo, sessionRepo), &service.UserService{Users: userRepo}, &service.SessionService{Sessions: sessionRepo},
		&service.OfficeStatsService{Sessions: sessionRepo}, service.NewEnvironmentService(time.Minute), nil, MiddlewareConfig{})
	h := &webHarness{t: t, discord: discord, booking: &service.BookingService{Repo: &repository.BookingRepo{DB: db}}}
	require.NoError(t, srv.EnableWeb(WebConfig{
		PublicURL: "http://office.test", ClientID: "client", ClientSecret: "secret", SessionSecret: "session-secret",
		BotToken: "bot", GuildID: "exec", APIKey: "script-key", Booking: h.booking,
		OnBookingChange: func() { h.mu.Lock(); h.changes++; h.mu.Unlock() },
	}))
	h.server = httptest.NewServer(srv.Handler())
	t.Cleanup(h.server.Close)
	return h
}

// client signs a member in through the real OAuth callback.
func (h *webHarness) client(user string) (*http.Client, string) {
	h.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Get(h.server.URL + "/login?next=/admin")
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusFound, res.StatusCode)
	loc, err := url.Parse(res.Header.Get("Location"))
	require.NoError(h.t, err)
	require.Equal(h.t, "http://office.test/auth/discord/callback", loc.Query().Get("redirect_uri"))
	h.discord.mu.Lock()
	h.discord.login = user
	h.discord.mu.Unlock()
	res, err = c.Get(h.server.URL + "/auth/discord/callback?code=good-code&state=" + loc.Query().Get("state"))
	require.NoError(h.t, err)
	require.Equal(h.t, http.StatusFound, res.StatusCode)
	require.Equal(h.t, "/admin", res.Header.Get("Location"), "returns to the page that asked for sign-in")
	// The CSRF token is embedded in the page for scripts.
	res, err = c.Get(h.server.URL + "/")
	require.NoError(h.t, err)
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	csrf := between(string(body), `"csrf":"`, `"`)
	return c, csrf
}

func between(s, a, b string) string {
	_, rest, ok := strings.Cut(s, a)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, b)
	return v
}

func (h *webHarness) do(c *http.Client, method, path, csrf string, body any) (int, map[string]any) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = strings.NewReader(string(data))
	}
	req, _ := http.NewRequest(method, h.server.URL+path, reader)
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	res, err := c.Do(req)
	require.NoError(h.t, err)
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func nextWeekday(day time.Weekday) string {
	d := service.BookingDay(time.Now()).AddDate(0, 0, 1)
	for d.Weekday() != day {
		d = d.AddDate(0, 0, 1)
	}
	return d.Format("2006-01-02")
}

func TestWebSignInAndPages(t *testing.T) {
	h := newWebHarness(t)
	anon := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := anon.Get(h.server.URL + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(res.Body)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Contains(t, string(body), "Sign in with Discord")
	require.NotContains(t, string(body), "Enter API Key")

	// A forged state is rejected.
	jar, _ := cookiejar.New(nil)
	forged := &http.Client{Jar: jar, CheckRedirect: anon.CheckRedirect}
	_, _ = forged.Get(h.server.URL + "/login")
	res, err = forged.Get(h.server.URL + "/auth/discord/callback?code=good-code&state=wrong")
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	alice, _ := h.client("alice")
	res, err = alice.Get(h.server.URL + "/")
	require.NoError(t, err)
	body, _ = io.ReadAll(res.Body)
	require.Contains(t, string(body), "Office booking")
	require.Contains(t, string(body), "/assets/js/booking.js")
	require.NotContains(t, string(body), `href="/admin"`, "members don't see the admin link")
	res, err = alice.Get(h.server.URL + "/admin")
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, res.StatusCode)

	admin, _ := h.client("admin")
	res, err = admin.Get(h.server.URL + "/admin")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	res, err = admin.Get(h.server.URL + "/ui")
	require.NoError(t, err)
	require.Equal(t, "/admin", res.Header.Get("Location"))

	// Someone outside the exec server can't use the site.
	jar, _ = cookiejar.New(nil)
	guest := &http.Client{Jar: jar, CheckRedirect: anon.CheckRedirect}
	res, _ = guest.Get(h.server.URL + "/login")
	loc, _ := url.Parse(res.Header.Get("Location"))
	h.discord.mu.Lock()
	h.discord.login = "guest"
	h.discord.mu.Unlock()
	_, _ = guest.Get(h.server.URL + "/auth/discord/callback?code=good-code&state=" + loc.Query().Get("state"))
	res, err = guest.Get(h.server.URL + "/")
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, res.StatusCode)

	for _, asset := range []string{"/assets/app.css", "/assets/js/booking.js", "/assets/js/common.js", "/assets/fonts/Geist-Variable.woff2", "/assets/logo-dark.svg"} {
		res, err = anon.Get(h.server.URL + asset)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, res.StatusCode, asset)
	}
}

func TestWebBookingFlow(t *testing.T) {
	h := newWebHarness(t)
	alice, aliceCSRF := h.client("alice")
	officer, officerCSRF := h.client("officer")
	day := nextWeekday(time.Wednesday)
	request := map[string]any{"request_id": "r1", "title": "WIE meeting", "date": day, "start": 17 * 60, "end": 19 * 60, "repeat": "weekly", "until": service.BookingDay(mustDate(day)).AddDate(0, 0, 14).Format("2006-01-02")}

	status, _ := h.do(alice, "POST", "/api/web/bookings", "", request)
	require.Equal(t, http.StatusForbidden, status, "mutations need the CSRF token")
	status, plan := h.do(alice, "POST", "/api/web/plan", aliceCSRF, request)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, plan["slots"], 3)
	status, out := h.do(alice, "POST", "/api/web/bookings", aliceCSRF, request)
	require.Equal(t, http.StatusOK, status, out)
	created := out["bookings"].([]any)
	require.Len(t, created, 3)
	lead := int64(created[0].(map[string]any)["id"].(float64))
	// A retried submit doesn't create a second request.
	_, out = h.do(alice, "POST", "/api/web/bookings", aliceCSRF, request)
	require.Len(t, out["bookings"], 3)

	status, _ = h.do(alice, "GET", "/api/web/requests", "", nil)
	require.Equal(t, http.StatusForbidden, status)
	status, out = h.do(officer, "GET", "/api/web/requests", "", nil)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, out["bookings"], 3)

	// The requester can't approve their own request.
	status, _ = h.do(alice, "POST", "/api/web/bookings/"+itoa(lead)+"/decide", aliceCSRF, map[string]any{"approve": []int64{lead}})
	require.Equal(t, http.StatusBadRequest, status)

	// Approve two of three dates; the third is declined with the reason.
	second := int64(created[1].(map[string]any)["id"].(float64))
	third := int64(created[2].(map[string]any)["id"].(float64))
	status, out = h.do(officer, "POST", "/api/web/bookings/"+itoa(lead)+"/decide", officerCSRF, map[string]any{"approve": []int64{lead, second}, "reason": "Exam week"})
	require.Equal(t, http.StatusOK, status, out)
	for id, want := range map[int64]string{lead: "approved", second: "approved", third: "declined"} {
		b, err := h.booking.Repo.Get("exec", id)
		require.NoError(t, err)
		require.Equal(t, want, b.Status)
	}
	declined, _ := h.booking.Repo.Get("exec", third)
	require.Equal(t, "Exam week", declined.Reason)
	notes, err := h.booking.Repo.PendingNotifications("exec", time.Now().Add(time.Hour))
	require.NoError(t, err)
	events := []string{}
	for _, n := range notes {
		events = append(events, n.Event)
	}
	require.Contains(t, events, "approved")
	require.NotContains(t, events, "declined", "a partial approval sends one message")

	// The detail view shows history with names.
	status, out = h.do(alice, "GET", "/api/web/bookings/"+itoa(lead), "", nil)
	require.Equal(t, http.StatusOK, status)
	history, _ := json.Marshal(out["history"])
	require.Contains(t, string(history), "Requested by you")
	require.Contains(t, string(history), "Approved by Officer")

	// Calendar range and cancelling the rest of the series.
	status, out = h.do(alice, "GET", "/api/web/bookings?from="+day+"&to="+mustDate(day).AddDate(0, 0, 13).Format("2006-01-02"), "", nil)
	require.Equal(t, http.StatusOK, status)
	require.Len(t, out["bookings"], 2)
	status, _ = h.do(alice, "POST", "/api/web/bookings/"+itoa(lead)+"/cancel", aliceCSRF, map[string]any{"rest": true})
	require.Equal(t, http.StatusOK, status)
	b, _ := h.booking.Repo.Get("exec", second)
	require.Equal(t, "cancelled", b.Status)
	h.mu.Lock()
	require.GreaterOrEqual(t, h.changes, 2, "Discord is told about website changes")
	h.mu.Unlock()
}

func TestManagementAPIRequiresAdmin(t *testing.T) {
	h := newWebHarness(t)
	anon := &http.Client{}
	res, err := anon.Get(h.server.URL + "/api/users")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	alice, _ := h.client("alice")
	status, _ := h.do(alice, "GET", "/api/users", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
	admin, adminCSRF := h.client("admin")
	status, _ = h.do(admin, "GET", "/api/users", "", nil)
	require.Equal(t, http.StatusOK, status)
	status, _ = h.do(admin, "POST", "/api/users", "", map[string]string{"name": "Sam", "rfid_uid": "AA"})
	require.Equal(t, http.StatusForbidden, status, "admin writes need the CSRF token")
	status, _ = h.do(admin, "POST", "/api/users", adminCSRF, map[string]string{"name": "Sam", "rfid_uid": "AA"})
	require.Less(t, status, 300)
	// Scripts can still use the API key.
	req, _ := http.NewRequest("GET", h.server.URL+"/api/sessions/open", nil)
	req.Header.Set("X-API-Key", "script-key")
	res, err = anon.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	// Devices keep working without signing in.
	res, err = anon.Post(h.server.URL+"/api/rfid/scan", "application/json", strings.NewReader(`{"uid":"unknown"}`))
	require.NoError(t, err)
	require.NotEqual(t, http.StatusUnauthorized, res.StatusCode)
	res, err = anon.Get(h.server.URL + "/health")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
}

func TestCalendarFeed(t *testing.T) {
	h := newWebHarness(t)
	alice, aliceCSRF := h.client("alice")
	officer, officerCSRF := h.client("officer")
	_, out := h.do(alice, "POST", "/api/web/bookings", aliceCSRF, map[string]any{"title": "Board, meeting", "date": nextWeekday(time.Friday), "start": 600, "end": 660})
	id := int64(out["bookings"].([]any)[0].(map[string]any)["id"].(float64))
	h.do(officer, "POST", "/api/web/bookings/"+itoa(id)+"/decide", officerCSRF, map[string]any{"approve": []int64{id}})
	res, err := http.Get(h.server.URL + "/calendar/wrong.ics")
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, res.StatusCode)
	page, err := alice.Get(h.server.URL + "/")
	require.NoError(t, err)
	body, _ := io.ReadAll(page.Body)
	feed := between(string(body), `"ics":"`, `"`)
	require.Contains(t, feed, "/calendar/")
	res, err = http.Get(h.server.URL + strings.TrimPrefix(strings.ReplaceAll(feed, `\/`, "/"), "http://office.test"))
	require.NoError(t, err)
	ics, _ := io.ReadAll(res.Body)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Contains(t, string(ics), "SUMMARY:Board\\, meeting (Alice)")
	require.Contains(t, string(ics), "BEGIN:VEVENT")
}

func mustDate(s string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", s, service.BookingLocation)
	if err != nil {
		panic(err)
	}
	return d
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
