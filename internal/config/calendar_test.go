package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/provider"
)

// describeCalendar writes resolved [calendar] settings as one string, the
// host's zone written "Local" whatever TZ names it.
func describeCalendar(s CalendarSettings) string {
	zone := s.Location.String()
	if s.Location == time.Local {
		zone = "Local"
	}
	return fmt.Sprintf("%s poll %s past %s future %s", zone, s.Poll, s.WindowPast, s.WindowFuture)
}

// problemsOf parses config and returns its validation problems, failing the
// test when the config parses or fails some other way.
func problemsOf(t *testing.T, config string) []string {
	t.Helper()
	_, err := Parse([]byte(config))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	return ve.Problems
}

func TestCalendarDefaults(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"no calendar table":    validConfig,
		"empty calendar table": validConfig + "\n[calendar]\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(config))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			want := CalendarConfig{Poll: "5m", WindowPast: "30d", WindowFuture: "365d"}
			if cfg.Calendar != want {
				t.Errorf("Calendar = %+v, want %+v", cfg.Calendar, want)
			}
			if got, want := describeCalendar(cfg.CalendarSettings()), "Local poll 5m0s past 720h0m0s future 8760h0m0s"; got != want {
				t.Errorf("CalendarSettings() = %s, want %s", got, want)
			}
		})
	}
}

func TestCalendarSettings(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validConfig + `
[calendar]
timezone = "America/Los_Angeles"
poll = "90s"
window_past = "7d"
window_future = "48h"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := describeCalendar(cfg.CalendarSettings()), "America/Los_Angeles poll 1m30s past 168h0m0s future 48h0m0s"; got != want {
		t.Errorf("CalendarSettings() = %s, want %s", got, want)
	}
}

func TestCalendarSettingsUnparsedFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	for name, cc := range map[string]CalendarConfig{
		"zero":         {},
		"unusable":     {Timezone: "Mars/Olympus", Poll: "soon", WindowPast: "-1d", WindowFuture: "x"},
		"out of range": {Poll: "1s", WindowPast: "0d", WindowFuture: "9999d"},
	} {
		cfg := &Config{Calendar: cc}
		if got, want := describeCalendar(cfg.CalendarSettings()), "Local poll 5m0s past 720h0m0s future 8760h0m0s"; got != want {
			t.Errorf("%s: CalendarSettings() = %s, want %s", name, got, want)
		}
	}
}

func TestCalendarValidBounds(t *testing.T) {
	t.Parallel()
	for _, table := range []string{
		"poll = \"1m\"\nwindow_past = \"1d\"\nwindow_future = \"1d\"",
		"poll = \"24h\"\nwindow_past = \"3650d\"\nwindow_future = \"3650d\"",
		"timezone = \"UTC\"",
	} {
		if _, err := Parse([]byte(validConfig + "\n[calendar]\n" + table)); err != nil {
			t.Errorf("[calendar] %q: %v", table, err)
		}
	}
}

func TestCalendarInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		table   string
		problem string
	}{
		{
			name:    "unknown zone",
			table:   `timezone = "Mars/Olympus"`,
			problem: `calendar.timezone: unknown time zone "Mars/Olympus"; want an IANA name such as America/Los_Angeles or UTC`,
		},
		{
			name:    "poll too short",
			table:   `poll = "30s"`,
			problem: "calendar.poll: 30s must be between 1m and 24h",
		},
		{
			name:    "poll too long",
			table:   `poll = "25h"`,
			problem: "calendar.poll: 25h must be between 1m and 24h",
		},
		{
			name:    "poll not a duration",
			table:   `poll = "1d"`,
			problem: `calendar.poll: duration "1d": want a Go duration such as 30s, 5m or 168h`,
		},
		{
			name:    "window_past zero",
			table:   `window_past = "0d"`,
			problem: "calendar.window_past: 0d must be between 1d and 3650d",
		},
		{
			name:    "window_future too long",
			table:   `window_future = "3651d"`,
			problem: "calendar.window_future: 3651d must be between 1d and 3650d",
		},
		{
			name:    "window_future not a duration",
			table:   `window_future = "a year"`,
			problem: `calendar.window_future: duration "a year": want a number of days such as 90d, or a Go duration such as 30s, 5m or 168h`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			problems := problemsOf(t, validConfig+"\n[calendar]\n"+tt.table)
			if !slices.Contains(problems, tt.problem) {
				t.Errorf("problems = %q, want %q", problems, tt.problem)
			}
		})
	}
}

func TestCalendarRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"calendar":         validConfig + "\n[calendar]\npol = \"5m\"",
		"account calendar": validConfig + "\n[accounts.hostinger.calendar]\nstor = \"none\"",
	} {
		if _, err := Parse([]byte(config)); !errors.Is(err, ErrUnknownKey) {
			t.Errorf("%s: err = %v, want ErrUnknownKey", name, err)
		}
	}
}

// accountsWith is a config of one oauth client and one account per entry
// of accounts, each a TOML account table body keyed by its name.
func accountsWith(accounts map[string]string) string {
	var b strings.Builder
	b.WriteString("[oauth_clients.g]\nclient_id = \"g-id\"\n")
	b.WriteString("[oauth_clients.kc]\nclient_id = \"chainmail\"\nissuer = \"https://auth.example.org/realms/r\"\nscopes = [\"openid\", \"offline_access\"]\n")
	for _, name := range slices.Sorted(maps.Keys(accounts)) {
		fmt.Fprintf(&b, "[accounts.%s]\n%s\n", name, accounts[name])
	}
	return b.String()
}

const (
	workspaceBody = "provider = \"google-workspace\"\naddress = \"w@example.org\"\noauth_client = \"g\""
	gmailBody     = "provider = \"gmail\"\naddress = \"me@gmail.com\"\nauth = \"oauth2\"\noauth_client = \"g\""
	gmailPWBody   = "provider = \"gmail\"\naddress = \"pw@gmail.com\"\nauth = \"password\""
	hostingerBody = "provider = \"hostinger\"\naddress = \"me@example.com\""
	mailwright    = "provider = \"custom\"\naddress = \"james@example.org\"\nauth = \"oauth2\"\noauth_client = \"kc\"\n" +
		"imap = { host = \"nlb.internal\", port = 993, tls_server_name = \"mail.example.org\" }\n" +
		"smtp = { host = \"nlb.internal\", port = 465, tls_server_name = \"mail.example.org\" }"
)

func TestAccountCalendarStoreDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(accountsWith(map[string]string{
		"work":       workspaceBody,
		"gm":         gmailBody,
		"gmpw":       gmailPWBody,
		"host":       hostingerBody,
		"mailwright": mailwright,
	})))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for name, want := range map[string]string{
		"work":       CalendarStoreGoogle,
		"gm":         CalendarStoreGoogle,
		"gmpw":       CalendarStoreNone, // Google's Calendar API takes no password
		"host":       CalendarStoreNone,
		"mailwright": CalendarStoreNone,
	} {
		acct, err := cfg.Account(name)
		if err != nil {
			t.Fatalf("Account(%s): %v", name, err)
		}
		if acct.Calendar.Store != want {
			t.Errorf("%s: Calendar.Store = %q, want %q", name, acct.Calendar.Store, want)
		}
	}
}

func TestAccountCalendarResolved(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(accountsWith(map[string]string{
		"work":       workspaceBody + "\n[accounts.work.calendar]\ninclude = [\"primary\", \"Team\"]\nexclude = [\"Birthdays\"]\ndefault = \"primary\"",
		"mailwright": mailwright + "\n[accounts.mailwright.calendar]\nstore = \"caldav\"\nurl = \"https://mail.example.org/\"",
		"host":       hostingerBody + "\n[accounts.host.calendar]\nstore = \"none\"",
	})))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	work, _ := cfg.Account("work")
	wantWork := AccountCalendar{Store: CalendarStoreGoogle, Include: []string{"primary", "Team"}, Exclude: []string{"Birthdays"}, Default: "primary"}
	if fmt.Sprint(work.Calendar) != fmt.Sprint(wantWork) {
		t.Errorf("work Calendar = %+v, want %+v", work.Calendar, wantWork)
	}
	mw, _ := cfg.Account("mailwright")
	if mw.Calendar.Store != CalendarStoreCalDAV || mw.Calendar.URL != "https://mail.example.org/" {
		t.Errorf("mailwright Calendar = %+v, want caldav at https://mail.example.org/", mw.Calendar)
	}
	host, _ := cfg.Account("host")
	if host.Calendar.Store != CalendarStoreNone {
		t.Errorf("host Calendar.Store = %q, want none", host.Calendar.Store)
	}
}

func TestAccountCalendarLoopbackURL(t *testing.T) {
	t.Parallel()
	for _, url := range []string{"http://127.0.0.1:8008/dav/", "http://localhost:5232/", "https://127.0.0.1/"} {
		config := accountsWith(map[string]string{
			"host": hostingerBody + "\n[accounts.host.calendar]\nstore = \"caldav\"\nurl = \"" + url + "\"",
		})
		if _, err := Parse([]byte(config)); err != nil {
			t.Errorf("url %s: %v", url, err)
		}
	}
}

func TestOAuthScopesResolved(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(accountsWith(map[string]string{
		"work":       workspaceBody,
		"none":       strings.Replace(workspaceBody, "w@", "n@", 1) + "\n[accounts.none.calendar]\nstore = \"none\"",
		"mailwright": mailwright,
		"host":       hostingerBody,
	}) + "\n[oauth_clients.extra]\nclient_id = \"x\"\nscopes = [\"https://mail.google.com/\", \"openid\"]\n" +
		"[accounts.extra]\n" + strings.Replace(gmailBody, "\"g\"", "\"extra\"", 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mail := "https://mail.google.com/"
	tests := map[string]struct {
		scopes []string
		issuer string
	}{
		"work":       {scopes: append([]string{mail}, provider.GoogleCalendarScopes...)},
		"none":       {scopes: []string{mail}},
		"mailwright": {scopes: []string{"openid", "offline_access"}, issuer: "https://auth.example.org/realms/r"},
		"host":       {},
		// The client's scopes follow, without repeating the mail scope.
		"extra": {scopes: append(append([]string{mail}, provider.GoogleCalendarScopes...), "openid")},
	}
	for name, want := range tests {
		acct, err := cfg.Account(name)
		if err != nil {
			t.Fatalf("Account(%s): %v", name, err)
		}
		if !slices.Equal(acct.OAuthScopes, want.scopes) {
			t.Errorf("%s: OAuthScopes = %q, want %q", name, acct.OAuthScopes, want.scopes)
		}
		if acct.OAuthIssuer != want.issuer {
			t.Errorf("%s: OAuthIssuer = %q, want %q", name, acct.OAuthIssuer, want.issuer)
		}
	}
	// The resolved scopes are the account's own: changing them leaves the
	// preset and the shared calendar scopes alone.
	work, _ := cfg.Account("work")
	work.OAuthScopes[1] = "changed"
	if provider.GoogleCalendarScopes[0] == "changed" {
		t.Error("OAuthScopes shares provider.GoogleCalendarScopes")
	}
}

func TestTLSServerNameResolved(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(accountsWith(map[string]string{"mailwright": mailwright, "host": hostingerBody})))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	mw, _ := cfg.Account("mailwright")
	for proto, ep := range map[string]provider.Endpoint{"imap": mw.IMAP, "smtp": mw.SMTP} {
		if ep.Host != "nlb.internal" || ep.ServerName != "mail.example.org" || ep.TLSServerName() != "mail.example.org" {
			t.Errorf("%s = %+v, want host nlb.internal with server name mail.example.org", proto, ep)
		}
	}
	host, _ := cfg.Account("host")
	if host.IMAP.ServerName != "" || host.IMAP.TLSServerName() != "imap.hostinger.com" {
		t.Errorf("hostinger imap = %+v, want no server name", host.IMAP)
	}
}

func TestAccountCalendarAndOAuthInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  string
		problem string
	}{
		{
			name:    "unknown store",
			config:  accountsWith(map[string]string{"host": hostingerBody + "\n[accounts.host.calendar]\nstore = \"ical\""}),
			problem: `accounts.host.calendar.store: unknown store "ical" (known: caldav, google, none)`,
		},
		{
			name:    "caldav without url",
			config:  accountsWith(map[string]string{"host": hostingerBody + "\n[accounts.host.calendar]\nstore = \"caldav\""}),
			problem: "accounts.host.calendar.url: required when store is caldav",
		},
		{
			name:    "caldav over http",
			config:  accountsWith(map[string]string{"host": hostingerBody + "\n[accounts.host.calendar]\nstore = \"caldav\"\nurl = \"http://dav.example.com/\""}),
			problem: "accounts.host.calendar.url: must be an https URL without user info, query or fragment (http only for 127.0.0.1 and localhost)",
		},
		{
			name:    "caldav url with user info",
			config:  accountsWith(map[string]string{"host": hostingerBody + "\n[accounts.host.calendar]\nstore = \"caldav\"\nurl = \"https://me:pw@dav.example.com/\""}),
			problem: "accounts.host.calendar.url: must be an https URL without user info, query or fragment (http only for 127.0.0.1 and localhost)",
		},
		{
			name:    "caldav url not absolute",
			config:  accountsWith(map[string]string{"host": hostingerBody + "\n[accounts.host.calendar]\nstore = \"caldav\"\nurl = \"dav.example.com\""}),
			problem: "accounts.host.calendar.url: must be an https URL without user info, query or fragment (http only for 127.0.0.1 and localhost)",
		},
		{
			name:    "url without caldav",
			config:  accountsWith(map[string]string{"work": workspaceBody + "\n[accounts.work.calendar]\nurl = \"https://dav.example.com/\""}),
			problem: "accounts.work.calendar.url: only valid when store is caldav",
		},
		{
			name:    "google on a non-Google preset",
			config:  accountsWith(map[string]string{"host": hostingerBody + "\n[accounts.host.calendar]\nstore = \"google\""}),
			problem: "accounts.host.calendar.store: google needs provider google-workspace or gmail, not hostinger",
		},
		{
			name:    "google with a password",
			config:  accountsWith(map[string]string{"gmpw": gmailPWBody + "\n[accounts.gmpw.calendar]\nstore = \"google\""}),
			problem: "accounts.gmpw.calendar.store: google needs auth oauth2",
		},
		{
			name:    "empty include entry",
			config:  accountsWith(map[string]string{"work": workspaceBody + "\n[accounts.work.calendar]\ninclude = [\"\"]"}),
			problem: "accounts.work.calendar.include: entries must not be empty",
		},
		{
			name:    "empty exclude entry",
			config:  accountsWith(map[string]string{"work": workspaceBody + "\n[accounts.work.calendar]\nexclude = [\"a\", \"\"]"}),
			problem: "accounts.work.calendar.exclude: entries must not be empty",
		},
		{
			name:    "bad tls_server_name",
			config:  accountsWith(map[string]string{"host": hostingerBody + "\nimap = { host = \"h\", port = 993, tls_server_name = \"mail..example\" }"}),
			problem: `accounts.host.imap.tls_server_name: "mail..example" is not a valid host name`,
		},
		{
			name:    "tls_server_name with a port",
			config:  accountsWith(map[string]string{"host": hostingerBody + "\nsmtp = { host = \"h\", port = 465, tls_server_name = \"mail.example:465\" }"}),
			problem: `accounts.host.smtp.tls_server_name: "mail.example:465" is not a valid host name`,
		},
		{
			name:    "issuer over http",
			config:  "[oauth_clients.kc]\nclient_id = \"c\"\nissuer = \"http://auth.example.org/realms/r\"",
			problem: "oauth_clients.kc.issuer: must be an https URL without user info, query or fragment (http only for 127.0.0.1 and localhost)",
		},
		{
			name:    "issuer with a query",
			config:  "[oauth_clients.kc]\nclient_id = \"c\"\nissuer = \"https://auth.example.org/?realm=r\"",
			problem: "oauth_clients.kc.issuer: must be an https URL without user info, query or fragment (http only for 127.0.0.1 and localhost)",
		},
		{
			name:    "bad scope",
			config:  "[oauth_clients.kc]\nclient_id = \"c\"\nscopes = [\"openid email\"]",
			problem: "oauth_clients.kc.scopes: entries must be non-empty and hold no spaces",
		},
		{
			name:    "empty scope",
			config:  "[oauth_clients.kc]\nclient_id = \"c\"\nscopes = [\"\"]",
			problem: "oauth_clients.kc.scopes: entries must be non-empty and hold no spaces",
		},
		{
			name: "custom oauth2 without an issuer",
			config: accountsWith(map[string]string{
				"mailwright": strings.Replace(mailwright, "\"kc\"", "\"g\"", 1),
			}),
			problem: "accounts.mailwright: provider custom accepts auth oauth2 only with an oauth_client that has an issuer",
		},
		{
			name: "google preset with an issuer client",
			config: accountsWith(map[string]string{
				"work": strings.Replace(workspaceBody, "\"g\"", "\"kc\"", 1),
			}),
			problem: `accounts.work: oauth_client "kc" has an issuer; provider google-workspace uses its own OAuth2 endpoints`,
		},
		{
			name: "hostinger oauth2 with an issuer client",
			config: accountsWith(map[string]string{
				"host": hostingerBody + "\nauth = \"oauth2\"\noauth_client = \"kc\"",
			}),
			problem: `accounts.host: provider hostinger does not accept auth "oauth2"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			problems := problemsOf(t, tt.config)
			if !slices.Contains(problems, tt.problem) {
				t.Errorf("problems = %q, want %q", problems, tt.problem)
			}
		})
	}
}

// A custom account keeps password as its default auth: oauth2 is never
// picked for it, so configs written before issuer clients keep working.
func TestCustomAuthStillDefaultsToPassword(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	local, _ := cfg.Account("local")
	if local.Auth != provider.AuthPassword || local.OAuthScopes != nil || local.OAuthIssuer != "" {
		t.Errorf("local = auth %q scopes %q issuer %q, want password and no OAuth", local.Auth, local.OAuthScopes, local.OAuthIssuer)
	}
}

func TestIssuerClientResolved(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(accountsWith(map[string]string{"mailwright": mailwright})))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	client := cfg.OAuthClients["kc"]
	if client.Issuer != "https://auth.example.org/realms/r" || !slices.Equal(client.Scopes, []string{"openid", "offline_access"}) {
		t.Errorf("client = %+v", client)
	}
	mw, _ := cfg.Account("mailwright")
	if mw.Auth != provider.AuthOAuth2 || mw.ClientID != "chainmail" || mw.Provider.OAuth2 != nil {
		t.Errorf("mailwright = auth %q client %q preset oauth2 %v, want oauth2 through chainmail with no preset endpoints",
			mw.Auth, mw.ClientID, mw.Provider.OAuth2)
	}
}

func TestIssuerLoopbackAllowed(t *testing.T) {
	t.Parallel()
	for _, issuer := range []string{"http://127.0.0.1:41234", "http://localhost:8080/realms/r", "https://auth.example.org/realms/r/"} {
		if _, err := Parse([]byte("[oauth_clients.kc]\nclient_id = \"c\"\nissuer = \"" + issuer + "\"")); err != nil {
			t.Errorf("issuer %s: %v", issuer, err)
		}
	}
}
