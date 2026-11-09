package config

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/yaklabco/chainmail/internal/provider"
)

// CalendarConfig is the [calendar] table: how the daemon keeps the calendar
// cache. Every key is optional.
type CalendarConfig struct {
	// Timezone is an IANA time zone name for all-day and date-only values
	// and the default zone of created events. Empty means the host's zone.
	Timezone string `toml:"timezone"`
	// Poll is how often calendars are synced, a Go duration from 1m to
	// 24h. It defaults to "5m".
	Poll string `toml:"poll"`
	// WindowPast is how far back the cache keeps events, such as "30d":
	// 1d to 3650d.
	WindowPast string `toml:"window_past"`
	// WindowFuture is how far ahead the cache keeps events, such as
	// "365d": 1d to 3650d.
	WindowFuture string `toml:"window_future"`
}

// CalendarSettings are the resolved [calendar] values.
type CalendarSettings struct {
	// Location is the zone of all-day and date-only values.
	Location *time.Location
	// Poll is the sync interval; WindowPast and WindowFuture bound the
	// cached range around now.
	Poll, WindowPast, WindowFuture time.Duration
}

// Calendar defaults and bounds, as written in config.
const (
	defaultCalendarPoll         = "5m"
	defaultCalendarWindowPast   = "30d"
	defaultCalendarWindowFuture = "365d"
	minCalendarPoll             = time.Minute
	maxCalendarPoll             = 24 * time.Hour
	minCalendarWindow           = 24 * time.Hour
	maxCalendarWindow           = 3650 * 24 * time.Hour
)

// Calendar store values of [accounts.<name>.calendar] store.
const (
	CalendarStoreGoogle = "google"
	CalendarStoreCalDAV = "caldav"
	CalendarStoreNone   = "none"
)

// AccountCalendar is an account's [accounts.<name>.calendar] table.
type AccountCalendar struct {
	// Store is google, caldav or none. Unset means google for a Google
	// preset logging in with oauth2, none otherwise.
	Store string `toml:"store"`
	// URL is the CalDAV server, discovered through /.well-known/caldav.
	// Only with store caldav.
	URL string `toml:"url"`
	// Include lists the calendar IDs or names to sync; empty means every
	// visible calendar.
	Include []string `toml:"include"`
	// Exclude lists calendar IDs or names never synced.
	Exclude []string `toml:"exclude"`
	// Default is the calendar ID or name new events go to. Empty means the
	// primary calendar.
	Default string `toml:"default"`
}

// CalendarSettings returns the [calendar] values resolved. Parse has
// validated them. A Config that never went through Parse gets the default
// for each unset or unusable value: the host's zone, 5m, 30d and 365d.
func (c *Config) CalendarSettings() CalendarSettings {
	loc := time.Local
	if c.Calendar.Timezone != "" {
		if l, err := time.LoadLocation(c.Calendar.Timezone); err == nil {
			loc = l
		}
	}
	poll, err := time.ParseDuration(c.Calendar.Poll)
	if err != nil || poll < minCalendarPoll || poll > maxCalendarPoll {
		poll, _ = time.ParseDuration(defaultCalendarPoll) // the default always parses
	}
	return CalendarSettings{
		Location:     loc,
		Poll:         poll,
		WindowPast:   calendarWindowOr(c.Calendar.WindowPast, defaultCalendarWindowPast),
		WindowFuture: calendarWindowOr(c.Calendar.WindowFuture, defaultCalendarWindowFuture),
	}
}

// calendarWindowOr returns s as a window, or def when s does not parse or
// lies out of range.
func calendarWindowOr(s, def string) time.Duration {
	if d, err := ParseCacheDuration(s); err == nil && d >= minCalendarWindow && d <= maxCalendarWindow {
		return d
	}
	d, _ := ParseCacheDuration(def) // every default is a valid window
	return d
}

// applyDefaults fills the unset [calendar] keys. Timezone stays empty,
// which means the host's zone.
func (cc *CalendarConfig) applyDefaults() {
	if cc.Poll == "" {
		cc.Poll = defaultCalendarPoll
	}
	if cc.WindowPast == "" {
		cc.WindowPast = defaultCalendarWindowPast
	}
	if cc.WindowFuture == "" {
		cc.WindowFuture = defaultCalendarWindowFuture
	}
}

// validate reports each [calendar] key that does not parse or lies out of
// range. Parse has filled every unset key but timezone.
func (cc CalendarConfig) validate(add func(string, ...any)) {
	if cc.Timezone != "" {
		if _, err := time.LoadLocation(cc.Timezone); err != nil {
			add("calendar.timezone: unknown time zone %q; want an IANA name such as America/Los_Angeles or UTC", cc.Timezone)
		}
	}
	validateDuration("calendar.poll", cc.Poll, minCalendarPoll, maxCalendarPoll, add)
	validateCacheDuration("calendar.window_past", cc.WindowPast, minCalendarWindow, maxCalendarWindow, "1d and 3650d", add)
	validateCacheDuration("calendar.window_future", cc.WindowFuture, minCalendarWindow, maxCalendarWindow, "1d and 3650d", add)
}

// resolveCalendar returns a's calendar table with the store resolved: the
// configured store, else google for a Google preset logging in with oauth2,
// else none. The lists are copies.
func resolveCalendar(a Account, p provider.Preset) AccountCalendar {
	var out AccountCalendar
	if a.Calendar != nil {
		out = *a.Calendar
		out.Include = slices.Clone(out.Include)
		out.Exclude = slices.Clone(out.Exclude)
	}
	if out.Store == "" {
		out.Store = CalendarStoreNone
		if p.Gmail && provider.AuthMethod(a.Auth) == provider.AuthOAuth2 {
			out.Store = CalendarStoreGoogle
		}
	}
	return out
}

// calendarStores lists the store values, sorted, for problems.
var calendarStores = []string{CalendarStoreCalDAV, CalendarStoreGoogle, CalendarStoreNone}

// validateAccountCalendar reports problems in an account's calendar table.
// where is "accounts.<name>".
func validateAccountCalendar(where string, a Account, p provider.Preset, add func(string, ...any)) {
	cal := a.Calendar
	if cal == nil {
		return
	}
	where += ".calendar"
	switch cal.Store {
	case "", CalendarStoreNone, CalendarStoreCalDAV:
	case CalendarStoreGoogle:
		if !p.Gmail {
			add("%s.store: google needs provider google-workspace or gmail, not %s", where, p.Name)
		} else if provider.AuthMethod(a.Auth) != provider.AuthOAuth2 {
			add("%s.store: google needs auth oauth2", where)
		}
	default:
		add("%s.store: unknown store %q (known: %s)", where, cal.Store, strings.Join(calendarStores, ", "))
	}
	switch {
	case cal.Store == CalendarStoreCalDAV && cal.URL == "":
		add("%s.url: required when store is caldav", where)
	case cal.Store == CalendarStoreCalDAV:
		if !webURLOK(cal.URL) {
			add("%s.url: %s", where, webURLProblem)
		}
	case cal.URL != "":
		add("%s.url: only valid when store is caldav", where)
	}
	if slices.Contains(cal.Include, "") {
		add("%s.include: entries must not be empty", where)
	}
	if slices.Contains(cal.Exclude, "") {
		add("%s.exclude: entries must not be empty", where)
	}
}

// webURLProblem is how problems word a URL webURLOK refuses. The URL itself
// stays out, since it could carry a password.
const webURLProblem = "must be an https URL without user info, query or fragment (http only for 127.0.0.1 and localhost)"

// webURLOK reports whether s is an absolute https URL with a host and no
// user info, query or fragment. http is allowed for 127.0.0.1 and
// localhost, which only tests and local servers use.
func webURLOK(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return IsLoopbackHost(u.Hostname())
	default:
		return false
	}
}

// IsLoopbackHost reports whether host is 127.0.0.1 or localhost, the hosts
// a plain http URL may name.
func IsLoopbackHost(host string) bool {
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost")
}

// hostLabel is one DNS label: letters, digits and hyphens, 1 to 63 long,
// not starting or ending with a hyphen.
var hostLabelRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// validHostName reports whether s is a DNS host name of at most 253
// characters, without a trailing dot.
func validHostName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for label := range strings.SplitSeq(s, ".") {
		if !hostLabelRE.MatchString(label) {
			return false
		}
	}
	return true
}

// validateOAuthClient reports problems in the issuer and scopes of the
// OAuth client at where, "oauth_clients.<name>".
func validateOAuthClient(where string, oc OAuthClient, add func(string, ...any)) {
	if oc.Issuer != "" && !webURLOK(oc.Issuer) {
		add("%s.issuer: %s", where, webURLProblem)
	}
	if slices.ContainsFunc(oc.Scopes, func(s string) bool { return s == "" || strings.ContainsFunc(s, unicode.IsSpace) }) {
		add("%s.scopes: entries must be non-empty and hold no spaces", where)
	}
}

// oauthScopes returns the scopes an oauth2 account asks for: the preset's
// mail scopes, the Google calendar scopes when the calendar store is
// google, then the client's own, each once, in that order.
func oauthScopes(p provider.Preset, store string, client OAuthClient) []string {
	var scopes []string
	if p.OAuth2 != nil {
		scopes = append(scopes, p.OAuth2.Scopes...)
	}
	if store == CalendarStoreGoogle {
		scopes = append(scopes, provider.GoogleCalendarScopes...)
	}
	scopes = append(scopes, client.Scopes...)
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}
