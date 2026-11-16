package http

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"office/internal/service"
)

// discordAPI is the Discord REST base URL (overridden in tests).
var discordAPI = "https://discord.com/api/v10"

const (
	sessionCookie = "office_session"
	stateCookie   = "office_oauth_state"
	sessionMaxAge = 30 * 24 * time.Hour
	memberTTL     = time.Minute
)

// WebConfig enables the signed-in website (booking calendar and admin pages).
type WebConfig struct {
	PublicURL     string // e.g. https://office-tracker.ieeeuottawa.ca
	ClientID      string // Discord application ID
	ClientSecret  string // Discord OAuth2 client secret
	SessionSecret string // HMAC key for session cookies
	BotToken      string // used to read guild membership and roles
	GuildID       string // exec server
	APIKey        string // still accepted on management endpoints for scripts
	Booking       *service.BookingService
	// OnBookingChange runs after a booking changes so Discord updates quickly.
	OnBookingChange func()
}

// webSession is what the signed cookie carries.
type webSession struct {
	UserID  string `json:"u"`
	Name    string `json:"n"`
	Avatar  string `json:"a,omitempty"`
	Expires int64  `json:"e"`
	Nonce   string `json:"x"` // per-session CSRF secret
}

// webUser is a signed-in exec-server member and what they may do.
type webUser struct {
	ID, Name, Avatar string
	Roles            []string
	Book, Approve    bool
	Admin            bool
	csrf             string
}

type cachedMember struct {
	member  *guildMember
	fetched time.Time
}

type guildMember struct {
	Nick  string   `json:"nick"`
	Roles []string `json:"roles"`
	User  struct {
		ID         string `json:"id"`
		Username   string `json:"username"`
		GlobalName string `json:"global_name"`
		Avatar     string `json:"avatar"`
	} `json:"user"`
}

func (m *guildMember) displayName() string {
	switch {
	case m.Nick != "":
		return m.Nick
	case m.User.GlobalName != "":
		return m.User.GlobalName
	}
	return m.User.Username
}

type guildInfo struct {
	OwnerID string `json:"owner_id"`
	Roles   []struct {
		ID          string `json:"id"`
		Permissions string `json:"permissions"`
	} `json:"roles"`
}

type webAuth struct {
	cfg    WebConfig
	client *http.Client

	mu      sync.Mutex
	members map[string]cachedMember
	guild   *guildInfo
	guildAt time.Time
}

func newWebAuth(cfg WebConfig) *webAuth {
	return &webAuth{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, members: map[string]cachedMember{}}
}

func (a *webAuth) sign(payload []byte) string {
	mac := hmac.New(sha256.New, []byte(a.cfg.SessionSecret))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *webAuth) encodeSession(s webSession) string {
	payload, _ := json.Marshal(s)
	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + a.sign([]byte(body))
}

func (a *webAuth) decodeSession(value string) (*webSession, bool) {
	body, sig, ok := strings.Cut(value, ".")
	if !ok || subtle.ConstantTimeCompare([]byte(sig), []byte(a.sign([]byte(body)))) != 1 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, false
	}
	var s webSession
	if json.Unmarshal(payload, &s) != nil || s.UserID == "" || time.Now().Unix() > s.Expires {
		return nil, false
	}
	return &s, true
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *webAuth) secureCookies() bool { return strings.HasPrefix(a.cfg.PublicURL, "https://") }

func (a *webAuth) redirectURI() string {
	return strings.TrimRight(a.cfg.PublicURL, "/") + "/auth/discord/callback"
}

// discordGet calls the Discord API with a bot or bearer token.
func (a *webAuth) discordGet(ctx context.Context, path, authorization string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discordAPI+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("User-Agent", "DiscordBot (office-tracker, 1)")
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return errNotMember
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("discord %s: %s %s", path, res.Status, body)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

var errNotMember = errors.New("not a member of the exec server")

// member returns the exec-server member, cached briefly so role changes apply within a minute.
func (a *webAuth) member(ctx context.Context, userID string) (*guildMember, error) {
	a.mu.Lock()
	cached, ok := a.members[userID]
	a.mu.Unlock()
	if ok && time.Since(cached.fetched) < memberTTL {
		if cached.member == nil {
			return nil, errNotMember
		}
		return cached.member, nil
	}
	var m guildMember
	err := a.discordGet(ctx, "/guilds/"+a.cfg.GuildID+"/members/"+userID, "Bot "+a.cfg.BotToken, &m)
	if err != nil && !errors.Is(err, errNotMember) {
		if ok && cached.member != nil {
			return cached.member, nil // Discord hiccup: keep the last known membership
		}
		return nil, err
	}
	a.mu.Lock()
	if errors.Is(err, errNotMember) {
		a.members[userID] = cachedMember{fetched: time.Now()}
	} else {
		a.members[userID] = cachedMember{member: &m, fetched: time.Now()}
	}
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// memberName resolves a display name for history entries, falling back to a mention-like ID.
func (a *webAuth) memberName(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	if m, err := a.member(ctx, userID); err == nil {
		return m.displayName()
	}
	return "a former member"
}

func (a *webAuth) guildInfo(ctx context.Context) (*guildInfo, error) {
	a.mu.Lock()
	if a.guild != nil && time.Since(a.guildAt) < 5*time.Minute {
		g := a.guild
		a.mu.Unlock()
		return g, nil
	}
	a.mu.Unlock()
	var g guildInfo
	if err := a.discordGet(ctx, "/guilds/"+a.cfg.GuildID, "Bot "+a.cfg.BotToken, &g); err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.guild, a.guildAt = &g, time.Now()
	a.mu.Unlock()
	return &g, nil
}

const (
	permAdministrator = 1 << 3
	permManageGuild   = 1 << 5
)

// isAdmin mirrors /office-admin: Administrator or Manage Server.
func (a *webAuth) isAdmin(ctx context.Context, userID string, roles []string) bool {
	g, err := a.guildInfo(ctx)
	if err != nil {
		return false
	}
	if g.OwnerID == userID {
		return true
	}
	var perms uint64
	held := map[string]bool{a.cfg.GuildID: true} // @everyone
	for _, r := range roles {
		held[r] = true
	}
	for _, r := range g.Roles {
		if held[r.ID] {
			p, _ := strconv.ParseUint(r.Permissions, 10, 64)
			perms |= p
		}
	}
	return perms&(permAdministrator|permManageGuild) != 0
}

// user returns the signed-in member, or nil when signed out. errNotMember means
// a valid session for someone who is not (or no longer) in the exec server.
func (a *webAuth) user(r *http.Request) (*webUser, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, nil
	}
	s, ok := a.decodeSession(c.Value)
	if !ok {
		return nil, nil
	}
	m, err := a.member(r.Context(), s.UserID)
	if err != nil {
		return nil, err
	}
	u := &webUser{ID: s.UserID, Name: m.displayName(), Avatar: s.Avatar, Roles: m.Roles, csrf: a.sign([]byte("csrf:" + s.Nonce))}
	access, err := a.cfg.Booking.Access(a.cfg.GuildID, service.MemberGrants(u.ID, m.Roles))
	if err != nil {
		return nil, err
	}
	u.Book, u.Approve = access.Book, access.Approve
	u.Admin = a.isAdmin(r.Context(), u.ID, m.Roles)
	return u, nil
}

func (a *webAuth) validCSRF(r *http.Request, u *webUser) bool {
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		token = r.FormValue("csrf")
	}
	return u != nil && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(u.csrf)) == 1
}

// ---- OAuth2 flow ----

func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.Contains(next, "\\") {
		return "/"
	}
	return next
}

func (a *webAuth) startLogin(w http.ResponseWriter, r *http.Request) {
	state := randomToken()
	next := safeNext(r.URL.Query().Get("next"))
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state + "|" + url.QueryEscape(next), Path: "/auth", MaxAge: 600, HttpOnly: true, Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode})
	q := url.Values{"client_id": {a.cfg.ClientID}, "redirect_uri": {a.redirectURI()}, "response_type": {"code"}, "scope": {"identify"}, "state": {state}, "prompt": {"none"}}
	http.Redirect(w, r, "https://discord.com/oauth2/authorize?"+q.Encode(), http.StatusFound)
}

func (a *webAuth) callback(w http.ResponseWriter, r *http.Request) (next string, err error) {
	c, cerr := r.Cookie(stateCookie)
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/auth", MaxAge: -1})
	if cerr != nil {
		return "/", errors.New("sign-in expired")
	}
	state, nextEsc, _ := strings.Cut(c.Value, "|")
	next, _ = url.QueryUnescape(nextEsc)
	next = safeNext(next)
	if r.URL.Query().Get("error") != "" {
		return next, errors.New("sign-in was cancelled")
	}
	if state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(r.URL.Query().Get("state"))) != 1 {
		return next, errors.New("sign-in expired")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {a.redirectURI()}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, discordAPI+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return next, err
	}
	req.SetBasicAuth(a.cfg.ClientID, a.cfg.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := a.client.Do(req)
	if err != nil {
		return next, err
	}
	defer res.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&token) != nil || token.AccessToken == "" {
		return next, fmt.Errorf("discord token exchange failed: %s", res.Status)
	}
	var me struct {
		ID, Username, Avatar string
		GlobalName           string `json:"global_name"`
	}
	if err := a.discordGet(r.Context(), "/users/@me", "Bearer "+token.AccessToken, &me); err != nil {
		return next, err
	}
	name := me.GlobalName
	if name == "" {
		name = me.Username
	}
	avatar := ""
	if me.Avatar != "" {
		avatar = "https://cdn.discordapp.com/avatars/" + me.ID + "/" + me.Avatar + ".png?size=64"
	}
	s := webSession{UserID: me.ID, Name: name, Avatar: avatar, Expires: time.Now().Add(sessionMaxAge).Unix(), Nonce: randomToken()}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: a.encodeSession(s), Path: "/", MaxAge: int(sessionMaxAge.Seconds()), HttpOnly: true, Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode})
	return next, nil
}

func (a *webAuth) logout(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode})
}

// icsToken is the unguessable part of the calendar feed URL.
func (a *webAuth) icsToken() string {
	return a.sign([]byte("ics-feed"))[:32]
}
