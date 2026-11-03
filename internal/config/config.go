// Package config loads and validates the chainmail daemon configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/mail"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/jamesainslie/chainmail/internal/mailaddr/allow"
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
	Policy       Policy                 `toml:"policy"`
	Watch        WatchConfig            `toml:"watch"`
}

// DefaultMaxRecipients is the most recipients a send may have unless
// max_recipients says otherwise.
const DefaultMaxRecipients = 10

// Bounds of max_recipients.
const (
	minMaxRecipients = 1
	maxMaxRecipients = 500
)

// Policy is the global [policy] table: what agents may do without James.
type Policy struct {
	// SendAllow lists the recipients every account may send to, each a bare
	// address or *@domain.
	SendAllow []string `toml:"send_allow"`
	// MaxRecipients is the most recipients one send may have, 1 to 500.
	MaxRecipients int `toml:"max_recipients"`
	// AllowExpunge permits expunging messages.
	AllowExpunge bool `toml:"allow_expunge"`
	// AllowFolderDelete permits deleting folders.
	AllowFolderDelete bool `toml:"allow_folder_delete"`
}

// WithDefaults returns p with Parse's defaults for unset fields: a
// max_recipients of 0 becomes DefaultMaxRecipients.
func (p Policy) WithDefaults() Policy {
	if p.MaxRecipients == 0 {
		p.MaxRecipients = DefaultMaxRecipients
	}
	return p
}

// AccountPolicy is an account's [accounts.<name>.policy] table.
type AccountPolicy struct {
	// SendAllow is added to the global send_allow for this account only.
	SendAllow []string `toml:"send_allow"`
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

// Account limits accepted in config.
const (
	maxPool           = 12
	minMaxMessageSize = 1 << 10
	maxMaxMessageSize = 1 << 30
)

var byteSizeRE = regexp.MustCompile(`^([0-9]{1,12})(B|KiB|MiB|GiB|TiB)?$`)

// ParseByteSize parses a size such as 65536, 512B, 64KiB, 2MiB, 2GiB or
// 1TiB. A size above math.MaxInt64 bytes is an error.
func ParseByteSize(s string) (int64, error) {
	m := byteSizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("size %q: want a number of bytes, optionally followed by B, KiB, MiB, GiB or TiB", s)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64) // at most 12 digits always fits
	var shift uint
	switch m[2] {
	case "KiB":
		shift = 10
	case "MiB":
		shift = 20
	case "GiB":
		shift = 30
	case "TiB":
		shift = 40
	}
	if n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("size %q: too large", s)
	}
	return n << shift, nil
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
	// DisplayName is the From display name. Empty means none.
	DisplayName string `toml:"display_name"`
	// Signature is Markdown appended to new messages. Empty means none.
	Signature string `toml:"signature"`
	// ArchiveFolder is where Archive moves mail on standard IMAP accounts.
	// Empty means the folder with the \Archive attribute.
	ArchiveFolder string `toml:"archive_folder"`
	// Pool overrides the probed IMAP pool size: 1 to 12, or 0 for not set.
	Pool int `toml:"pool"`
	// MaxMessageSize overrides the preset message size limit, such as "35MiB".
	MaxMessageSize string `toml:"max_message_size"`
	// Policy holds the account's own send allowlist.
	Policy AccountPolicy `toml:"policy"`
	// Watch lists the folders watched with IDLE. Nil means the default,
	// ["INBOX"]; an empty list watches nothing. Gmail presets must leave it
	// unset.
	Watch *[]string `toml:"watch"`
	// Sync is the account's sync table.
	Sync AccountSync `toml:"sync"`
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
	// DisplayName is the From display name. Empty means none.
	DisplayName string
	// Signature is Markdown. Empty means none.
	Signature string
	// ArchiveFolder is the configured archive folder. Empty means the
	// \Archive folder.
	ArchiveFolder string
	// Pool is the configured pool size. 0 means not set.
	Pool int
	// MaxMessageSize is the largest message to send, in bytes: max_message_size
	// when set, else the preset's.
	MaxMessageSize int64
	// Watch lists the folders watched with IDLE. Nil on Gmail presets,
	// which watch their \All folder instead.
	Watch []string
	// SyncFolders lists every synced folder, Watch first, then the rest of
	// sync.folders in config order, without duplicates. Nil on Gmail presets.
	SyncFolders []string
	// SyncPoll is how often synced folders that are not watched are checked.
	SyncPoll time.Duration
	// SyncEnabled is false when sync.enabled is false.
	SyncEnabled bool
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
		Name:           name,
		Address:        a.Address,
		Username:       a.Username,
		Provider:       p,
		Auth:           provider.AuthMethod(a.Auth),
		OAuthClient:    a.OAuthClient,
		IMAP:           override(p.IMAP, a.IMAP),
		SMTP:           override(p.SMTP, a.SMTP),
		DisplayName:    a.DisplayName,
		Signature:      a.Signature,
		ArchiveFolder:  a.ArchiveFolder,
		Pool:           a.Pool,
		MaxMessageSize: p.MaxMessageSize,
		SyncPoll:       durationOr(a.Sync.Poll, defaultSyncPoll),
		SyncEnabled:    a.Sync.Enabled == nil || *a.Sync.Enabled,
	}
	// Parse validated max_message_size. A Config that never went through
	// Parse keeps the preset's limit.
	if n, err := ParseByteSize(a.MaxMessageSize); err == nil && n > 0 {
		r.MaxMessageSize = n
	}
	if a.OAuthClient != "" {
		r.ClientID = c.OAuthClients[a.OAuthClient].ClientID
	}
	if !p.Gmail {
		r.Watch = watchList(a.Watch)
		r.SyncFolders = syncFolders(r.Watch, a.Sync.Folders)
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
	c.Policy = c.Policy.WithDefaults()
	c.Watch.applyDefaults()
	for name, a := range c.Accounts {
		if a.Username == "" {
			a.Username = a.Address
		}
		p, known := provider.Lookup(a.Provider)
		if known && a.Auth == "" && len(p.AuthMethods) == 1 {
			a.Auth = string(p.AuthMethods[0])
		}
		for _, o := range []*EndpointOverride{a.IMAP, a.SMTP} {
			if o != nil && o.Security == "" {
				o.Security = provider.SecurityTLS.String()
			}
		}
		a.applyWatchDefaults(p, known)
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
	if c.Policy.MaxRecipients < minMaxRecipients || c.Policy.MaxRecipients > maxMaxRecipients {
		add("policy.max_recipients must be %d to %d", minMaxRecipients, maxMaxRecipients)
	}
	validateSendAllow("policy.send_allow", c.Policy.SendAllow, add)
	c.Watch.validate(add)
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
	validateSendAllow(where+".policy.send_allow", a.Policy.SendAllow, add)
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
	if a.Pool < 0 || a.Pool > maxPool {
		add("%s: pool must be between 1 and %d, or 0 to use the probed size", where, maxPool)
	}
	if a.MaxMessageSize != "" {
		if n, err := ParseByteSize(a.MaxMessageSize); err != nil {
			add("%s: max_message_size: %v", where, err)
		} else if n < minMaxMessageSize || n > maxMaxMessageSize {
			add("%s: max_message_size: %s must be between 1KiB and 1024MiB", where, a.MaxMessageSize)
		}
	}
	validateAccountWatch(where, a, p, add)
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

// validateSendAllow reports each problem SendAllowProblems finds.
func validateSendAllow(where string, entries []string, add func(string, ...any)) {
	for _, p := range SendAllowProblems(where, entries) {
		add("%s", p)
	}
}

// SendAllowProblems lists the entries of a send_allow list at where that are
// not a bare address or *@domain, each worded as Parse reports it:
// `<where>: invalid entry "<entry>"`. It checks with the matcher's own rules,
// so the config check and the policy never disagree.
func SendAllowProblems(where string, entries []string) []string {
	var out []string
	for _, e := range entries {
		if !allow.Valid(e) {
			out = append(out, fmt.Sprintf("%s: invalid entry %q", where, e))
		}
	}
	return out
}
