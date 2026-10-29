// Package config loads and validates the chainmail daemon configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/mail"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/jamesainslie/chainmail/internal/provider"
)

// Production defaults.
const (
	DefaultPath        = "/etc/chainmail/config.toml"
	DefaultSocket      = "/run/chainmail/chainmail.sock"
	DefaultClientGroup = "chainmail-clients"
	// DefaultBodyLimit truncates message bodies longer than 64 KiB unless the request asks for the full body.
	DefaultBodyLimit = "64KiB"
)

// ErrUnknownAccount means no account has the requested name.
var ErrUnknownAccount = errors.New("unknown account")

// Config is the daemon configuration file. It holds no secrets.
type Config struct {
	Daemon       Daemon                 `toml:"daemon"`
	OAuthClients map[string]OAuthClient `toml:"oauth_clients"`
	Accounts     map[string]Account     `toml:"accounts"`
}

// Daemon configures the socket and how messages are rendered.
type Daemon struct {
	Socket      string `toml:"socket"`
	ClientGroup string `toml:"client_group"`
	// BodyLimit truncates message bodies longer than this, such as "64KiB".
	BodyLimit string `toml:"body_limit"`
}

// Body limits accepted in config. They bound one rendered body only. The Get
// and GetThread handlers enforce the 16 MiB unary response cap themselves.
const (
	minBodyLimit = 1 << 10
	maxBodyLimit = 8 << 20
)

var byteSizeRE = regexp.MustCompile(`^([0-9]{1,12})(B|KiB|MiB)?$`)

// ParseByteSize parses a size such as 65536, 512B, 64KiB or 2MiB.
func ParseByteSize(s string) (int64, error) {
	m := byteSizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("size %q: want a number of bytes, optionally followed by B, KiB or MiB", s)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64) // at most 12 digits always fits
	switch m[2] {
	case "KiB":
		n <<= 10
	case "MiB":
		n <<= 20
	}
	return n, nil
}

// BodyLimitBytes returns body_limit in bytes. Parse has validated it. A Daemon
// that never went through Parse gets the default.
func (d Daemon) BodyLimitBytes() int {
	n, err := ParseByteSize(d.BodyLimit)
	if err != nil || n == 0 {
		n, _ = ParseByteSize(DefaultBodyLimit)
	}
	return int(n)
}

// OAuthClient is an OAuth2 client registration. Its secret lives in the vault
// as oauth_clients/<name>.
type OAuthClient struct {
	ClientID string `toml:"client_id"`
}

// Account is one mail account as written in the config file.
type Account struct {
	Provider    string            `toml:"provider"`
	Address     string            `toml:"address"`
	Username    string            `toml:"username"`
	Auth        string            `toml:"auth"`
	OAuthClient string            `toml:"oauth_client"`
	IMAP        *EndpointOverride `toml:"imap"`
	SMTP        *EndpointOverride `toml:"smtp"`
}

// EndpointOverride replaces a preset's server address.
type EndpointOverride struct {
	Host     string `toml:"host"`
	Port     int    `toml:"port"`
	Security string `toml:"security"`
}

// ResolvedAccount is an account with its preset and defaults applied.
type ResolvedAccount struct {
	Name        string
	Address     string
	Username    string
	Provider    provider.Preset
	Auth        provider.AuthMethod
	OAuthClient string
	ClientID    string
	IMAP        provider.Endpoint
	SMTP        provider.Endpoint
}

// ValidationError lists every problem found in a config.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid config: " + strings.Join(e.Problems, "; ")
}

// nameRE matches account and OAuth client names. Both become vault secret
// names (accounts/<name>, oauth_clients/<name>), so no path characters.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidName reports whether name can name an account or OAuth client.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes config TOML, rejecting unknown fields, applies defaults and validates the result.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// AccountNames returns the configured account names in sorted order.
func (c *Config) AccountNames() []string {
	return slices.Sorted(maps.Keys(c.Accounts))
}

// Account returns the named account with its preset and overrides applied.
// It is only valid on a Config returned by Parse or Load, which validate the
// providers and endpoint security that Account relies on.
func (c *Config) Account(name string) (ResolvedAccount, error) {
	a, ok := c.Accounts[name]
	if !ok {
		return ResolvedAccount{}, fmt.Errorf("%w: %q", ErrUnknownAccount, name)
	}
	p, _ := provider.Lookup(a.Provider) // Parse rejected unknown providers
	r := ResolvedAccount{
		Name:        name,
		Address:     a.Address,
		Username:    a.Username,
		Provider:    p,
		Auth:        provider.AuthMethod(a.Auth),
		OAuthClient: a.OAuthClient,
		IMAP:        override(p.IMAP, a.IMAP),
		SMTP:        override(p.SMTP, a.SMTP),
	}
	if a.OAuthClient != "" {
		r.ClientID = c.OAuthClients[a.OAuthClient].ClientID
	}
	return r, nil
}

func override(base provider.Endpoint, o *EndpointOverride) provider.Endpoint {
	if o == nil {
		return base
	}
	sec, _ := provider.ParseSecurity(o.Security) // Parse rejected invalid values
	return provider.Endpoint{Host: o.Host, Port: o.Port, Security: sec}
}

func (c *Config) applyDefaults() {
	if c.Daemon.Socket == "" {
		c.Daemon.Socket = DefaultSocket
	}
	if c.Daemon.ClientGroup == "" {
		c.Daemon.ClientGroup = DefaultClientGroup
	}
	if c.Daemon.BodyLimit == "" {
		c.Daemon.BodyLimit = DefaultBodyLimit
	}
	for name, a := range c.Accounts {
		if a.Username == "" {
			a.Username = a.Address
		}
		if p, ok := provider.Lookup(a.Provider); ok && a.Auth == "" && len(p.AuthMethods) == 1 {
			a.Auth = string(p.AuthMethods[0])
		}
		for _, o := range []*EndpointOverride{a.IMAP, a.SMTP} {
			if o != nil && o.Security == "" {
				o.Security = provider.SecurityTLS.String()
			}
		}
		c.Accounts[name] = a
	}
}

func (c *Config) validate() error {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if n, err := ParseByteSize(c.Daemon.BodyLimit); err != nil {
		add("daemon.body_limit: %v", err)
	} else if n < minBodyLimit || n > maxBodyLimit {
		add("daemon.body_limit: %s must be between 1KiB and 8MiB", c.Daemon.BodyLimit)
	}
	for _, name := range slices.Sorted(maps.Keys(c.OAuthClients)) {
		where := "oauth_clients." + name
		if !nameRE.MatchString(name) {
			add("%s: name must be lowercase letters, digits and hyphens", where)
		}
		if c.OAuthClients[name].ClientID == "" {
			add("%s: client_id is required", where)
		}
	}
	for _, name := range c.AccountNames() {
		c.validateAccount(name, add)
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func (c *Config) validateAccount(name string, add func(string, ...any)) {
	a := c.Accounts[name]
	where := "accounts." + name
	if !nameRE.MatchString(name) {
		add("%s: name must be lowercase letters, digits and hyphens", where)
	}
	p, ok := provider.Lookup(a.Provider)
	if !ok {
		add("%s: unknown provider %q (known: %s)", where, a.Provider, strings.Join(provider.Names(), ", "))
		return
	}
	if addr, err := mail.ParseAddress(a.Address); err != nil || addr.Name != "" || addr.Address != a.Address {
		add("%s: address %q must be a bare email address", where, a.Address)
	}
	method := provider.AuthMethod(a.Auth)
	switch {
	case a.Auth == "":
		add("%s: auth is required (provider %s accepts %s)", where, p.Name, joinMethods(p.AuthMethods))
	case !p.SupportsAuth(method):
		add("%s: provider %s does not accept auth %q", where, p.Name, a.Auth)
	case method == provider.AuthOAuth2 && a.OAuthClient == "":
		add("%s: oauth_client is required for oauth2", where)
	case method == provider.AuthOAuth2:
		if _, ok := c.OAuthClients[a.OAuthClient]; !ok {
			add("%s: oauth_client %q is not defined", where, a.OAuthClient)
		}
	case a.OAuthClient != "":
		add("%s: oauth_client is only valid with oauth2", where)
	}
	if p.Name == "custom" && (a.IMAP == nil || a.SMTP == nil) {
		add("%s: provider custom needs both imap and smtp", where)
	}
	validateOverride(where+".imap", a.IMAP, add)
	validateOverride(where+".smtp", a.SMTP, add)
}

func validateOverride(where string, o *EndpointOverride, add func(string, ...any)) {
	if o == nil {
		return
	}
	if o.Host == "" {
		add("%s: host is required", where)
	}
	if o.Port < 1 || o.Port > 65535 {
		add("%s: port must be between 1 and 65535", where)
	}
	if _, err := provider.ParseSecurity(o.Security); err != nil {
		add("%s: %v", where, err)
	}
}

func joinMethods(ms []provider.AuthMethod) string {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = string(m)
	}
	return strings.Join(parts, " or ")
}
