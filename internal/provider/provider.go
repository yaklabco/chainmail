// Package provider holds connection presets for the mail providers chainmail supports.
package provider

import (
	"fmt"
	"maps"
	"net"
	"slices"
	"strconv"
)

// Security is how a connection to a mail server is protected.
type Security int

const (
	// SecurityTLS means TLS from the first byte, as on ports 993 and 465.
	SecurityTLS Security = iota + 1
	// SecurityStartTLS means a plain connection upgraded with STARTTLS, as on port 587.
	SecurityStartTLS
)

// String returns the config spelling of s.
func (s Security) String() string {
	switch s {
	case SecurityTLS:
		return "tls"
	case SecurityStartTLS:
		return "starttls"
	default:
		return fmt.Sprintf("Security(%d)", int(s))
	}
}

// ParseSecurity parses the config spelling of a Security value.
func ParseSecurity(s string) (Security, error) {
	switch s {
	case "tls":
		return SecurityTLS, nil
	case "starttls":
		return SecurityStartTLS, nil
	default:
		return 0, fmt.Errorf("unknown security %q: want tls or starttls", s)
	}
}

// Endpoint is a mail server address and how to secure the connection to it.
type Endpoint struct {
	Host     string
	Port     int
	Security Security
}

// Addr returns host:port, bracketing IPv6 hosts.
func (e Endpoint) Addr() string {
	return net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
}

// AuthMethod is how chainmail logs in to an account.
type AuthMethod string

// Supported auth methods.
const (
	AuthPassword AuthMethod = "password"
	AuthOAuth2   AuthMethod = "oauth2"
)

// OAuth2 holds a provider's OAuth2 endpoints and the scopes mail access needs.
type OAuth2 struct {
	AuthURL  string
	TokenURL string
	Scopes   []string
}

// Preset describes one provider.
type Preset struct {
	Name string
	// IMAP and SMTP are zero for the custom preset, which needs endpoints in config.
	IMAP Endpoint
	SMTP Endpoint
	// AuthMethods lists the methods the provider accepts.
	AuthMethods []AuthMethod
	// OAuth2 is set when the provider supports OAuth2.
	OAuth2 *OAuth2
	// MaxConnections caps simultaneous IMAP connections per account, IDLE included.
	MaxConnections int
	// DefaultPool is how many IMAP connections the daemon uses when config
	// sets no pool and no probe result exists.
	DefaultPool int
	// MaxMessageSize is the largest message the provider accepts, in bytes.
	// Config max_message_size overrides it.
	MaxMessageSize int64
	// Gmail is set for providers that speak Gmail's X-GM-EXT-1 extensions.
	Gmail bool
}

// SupportsAuth reports whether p accepts method m.
func (p Preset) SupportsAuth(m AuthMethod) bool {
	return slices.Contains(p.AuthMethods, m)
}

var googleOAuth2 = OAuth2{
	AuthURL:  "https://accounts.google.com/o/oauth2/auth",
	TokenURL: "https://oauth2.googleapis.com/token",
	Scopes:   []string{"https://mail.google.com/"},
}

var (
	gmailIMAP = Endpoint{Host: "imap.gmail.com", Port: 993, Security: SecurityTLS}
	gmailSMTP = Endpoint{Host: "smtp.gmail.com", Port: 587, Security: SecurityStartTLS}
)

// Gmail allows 15 IMAP connections per account. Both Google presets take 10
// so the user's other mail clients on the same account keep 5 for
// themselves. iCloud and Hostinger don't document a limit, so they start
// low until the live tests measure them. DefaultPool is the pool size before
// a probe has measured the account. Google documents a 25 MiB message limit;
// iCloud and Hostinger take 35 MiB until measured, and custom servers 25 MiB.
var presets = map[string]Preset{
	"google-workspace": {
		IMAP: gmailIMAP, SMTP: gmailSMTP,
		AuthMethods: []AuthMethod{AuthOAuth2}, OAuth2: &googleOAuth2,
		MaxConnections: 10, Gmail: true,
		DefaultPool: 6, MaxMessageSize: 25 << 20,
	},
	"gmail": {
		IMAP: gmailIMAP, SMTP: gmailSMTP,
		AuthMethods: []AuthMethod{AuthOAuth2, AuthPassword}, OAuth2: &googleOAuth2,
		MaxConnections: 10, Gmail: true,
		DefaultPool: 6, MaxMessageSize: 25 << 20,
	},
	"hostinger": {
		IMAP:           Endpoint{Host: "imap.hostinger.com", Port: 993, Security: SecurityTLS},
		SMTP:           Endpoint{Host: "smtp.hostinger.com", Port: 465, Security: SecurityTLS},
		AuthMethods:    []AuthMethod{AuthPassword},
		MaxConnections: 3,
		DefaultPool:    3,
		MaxMessageSize: 35 << 20,
	},
	"icloud": {
		IMAP:           Endpoint{Host: "imap.mail.me.com", Port: 993, Security: SecurityTLS},
		SMTP:           Endpoint{Host: "smtp.mail.me.com", Port: 587, Security: SecurityStartTLS},
		AuthMethods:    []AuthMethod{AuthPassword},
		MaxConnections: 3,
		DefaultPool:    3,
		MaxMessageSize: 35 << 20,
	},
	"custom": {
		AuthMethods:    []AuthMethod{AuthPassword},
		MaxConnections: 3,
		DefaultPool:    3,
		MaxMessageSize: 25 << 20,
	},
}

// Lookup returns a copy of the preset called name.
func Lookup(name string) (Preset, bool) {
	p, ok := presets[name]
	if !ok {
		return Preset{}, false
	}
	p.Name = name
	p.AuthMethods = slices.Clone(p.AuthMethods)
	if p.OAuth2 != nil {
		o := *p.OAuth2
		o.Scopes = slices.Clone(o.Scopes)
		p.OAuth2 = &o
	}
	return p, true
}

// Names returns every preset name in sorted order.
func Names() []string {
	return slices.Sorted(maps.Keys(presets))
}
