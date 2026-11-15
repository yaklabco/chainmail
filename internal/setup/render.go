package setup

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

// Marker is the comment every file bootstrap generates starts with. A unit
// or plist without it belongs to someone else, and bootstrap will not
// replace it.
const Marker = "Written by chainmail bootstrap. Edits are kept for config.toml only."

// hashMarker is the marker as the first line of a TOML file or a unit.
const hashMarker = "# " + Marker + "\n"

// plistHeader is the start of every plist bootstrap writes: the XML
// declaration, the DOCTYPE and the marker as an XML comment.
const plistHeader = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- ` + Marker + ` -->
`

// HasMarker reports whether data starts with the marker comment in TOML,
// unit or plist form.
func HasMarker(data []byte) bool {
	return bytes.HasPrefix(data, []byte(hashMarker)) || bytes.HasPrefix(data, []byte(plistHeader))
}

// AccountSpec is the account bootstrap writes into config.toml.
type AccountSpec struct {
	Name        string
	Provider    string
	Address     string
	Auth        provider.AuthMethod
	OAuthClient string                   // "" for password accounts
	IMAP, SMTP  *config.EndpointOverride // non-nil only under the e2e build tag
}

// OAuthClientSpec is the OAuth client table an oauth2 account needs.
type OAuthClientSpec struct {
	Name     string
	ClientID string
}

// RenderService returns the unit (Linux) or plist (macOS) for l. It panics
// for any other platform, as LayoutFor does.
func RenderService(l Layout) []byte {
	switch l.Platform {
	case Linux:
		return renderUnit(l)
	case Darwin:
		return renderPlist(l)
	default:
		panic(fmt.Sprintf("setup: no service definition for platform %q", l.Platform))
	}
}

// daemonGroup is the daemon user's primary group: the user's own group on
// Linux (useradd --user-group), the client group on macOS.
func daemonGroup(l Layout) string {
	if l.Platform == Darwin {
		return l.ClientGroup
	}
	return l.DaemonUser
}

// unitTemplate is the deploy page's unit with the marker first, ExecStart
// from the layout and RestartPreventExitStatus=2, so a bad key or config
// does not restart forever. The verbs are, in order: ExecStart, User,
// Group, SupplementaryGroups, StateDirectory and RuntimeDirectory.
const unitTemplate = hashMarker + `[Unit]
Description=chainmail policy daemon
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=%s
User=%s
Group=%s
SupplementaryGroups=%s
StateDirectory=%s
StateDirectoryMode=0700
RuntimeDirectory=%s
RuntimeDirectoryMode=0755
UMask=0077
Restart=on-failure
RestartSec=5s
RestartPreventExitStatus=2
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
NoNewPrivileges=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
`

// renderUnit returns the systemd unit for l.
func renderUnit(l Layout) []byte {
	words := make([]string, 0, 1+len(l.ServiceArgs()))
	words = append(words, systemdArg(l.Binary))
	for _, a := range l.ServiceArgs() {
		words = append(words, systemdArg(a))
	}
	// StateDirectory= and RuntimeDirectory= name directories relative to
	// /var/lib and /run, the roots the layout's Linux paths sit under.
	return fmt.Appendf(nil, unitTemplate,
		strings.Join(words, " "),
		l.DaemonUser,
		daemonGroup(l),
		l.ClientGroup,
		strings.TrimPrefix(l.StateDir, "/var/lib/"),
		strings.TrimPrefix(l.SocketDir, "/run/"),
	)
}

// systemdQuoted lists the characters that make an ExecStart word need
// double quotes.
const systemdQuoted = " \t\"'\\;"

// systemdArg writes s as one ExecStart word. Specifier and variable
// characters are doubled so systemd passes them through, and a word with
// spaces, quotes, backslashes or semicolons is double-quoted.
func systemdArg(s string) string {
	s = strings.NewReplacer("%", "%%", "$", "$$").Replace(s)
	if s != "" && !strings.ContainsAny(s, systemdQuoted) {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// renderPlist returns the launchd plist for l: the deploy page's plist with
// the marker after the DOCTYPE and every value from the layout.
func renderPlist(l Layout) []byte {
	var b bytes.Buffer
	b.WriteString(plistHeader)
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	plistString(&b, "  ", "Label", l.ServiceLabel)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	plistArrayString(&b, l.Binary)
	for _, a := range l.ServiceArgs() {
		plistArrayString(&b, a)
	}
	b.WriteString("  </array>\n")
	plistString(&b, "  ", "UserName", l.DaemonUser)
	plistString(&b, "  ", "GroupName", daemonGroup(l))
	b.WriteString(`  <key>Umask</key>
  <integer>63</integer>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>5</integer>
`)
	plistString(&b, "  ", "StandardErrorPath", l.LogFile)
	b.WriteString("</dict>\n</plist>\n")
	return b.Bytes()
}

// plistString writes a key and its string value at indent.
func plistString(b *bytes.Buffer, indent, key, value string) {
	b.WriteString(indent + "<key>" + xmlText(key) + "</key>\n")
	b.WriteString(indent + "<string>" + xmlText(value) + "</string>\n")
}

// plistArrayString writes one string element of ProgramArguments.
func plistArrayString(b *bytes.Buffer, value string) {
	b.WriteString("    <string>" + xmlText(value) + "</string>\n")
}

// xmlText escapes s as XML character data.
func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s)) // a strings.Builder never fails
	return b.String()
}

// NewConfig returns a fresh config.toml holding the marker, on macOS the
// [daemon] socket, the account and, when non-nil, the OAuth client.
func NewConfig(l Layout, a AccountSpec, c *OAuthClientSpec) []byte {
	var b strings.Builder
	b.WriteString(hashMarker)
	// The daemon's default socket is the Linux path; any other layout
	// names its socket.
	if l.Socket != config.DefaultSocket {
		b.WriteString("\n[daemon]\n")
		b.WriteString("socket = " + tomlQuote(l.Socket) + "\n")
	}
	if c != nil {
		b.WriteString("\n" + clientTOML(*c))
	}
	b.WriteString("\n" + accountTOML(a))
	return []byte(b.String())
}

// AppendAccount appends the account table, and the OAuth client table
// when non-nil and absent, to existing as text. It parses the result with
// config.Parse and returns a *Refusal if that fails.
//
// existing is parsed first, so a config that does not parse, an account
// that already exists and a client already defined with another ID are
// refused before anything is appended. Every byte of existing is kept as
// a prefix of the result.
func AppendAccount(existing []byte, a AccountSpec, c *OAuthClientSpec) ([]byte, error) {
	cur, err := config.Parse(existing)
	if err != nil {
		return nil, refuse("fix the config by hand so it parses, then run bootstrap again",
			"the existing config does not parse: %v", err)
	}
	if _, ok := cur.Accounts[a.Name]; ok {
		return nil, refuse("choose another account name with --name",
			"account %q already exists in the config", a.Name)
	}
	out := bytes.Clone(existing)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	sep := func() {
		if len(out) > 0 {
			out = append(out, '\n')
		}
	}
	if c != nil {
		switch have, ok := cur.OAuthClients[c.Name]; {
		case !ok:
			sep()
			out = append(out, clientTOML(*c)...)
		case have.ClientID != c.ClientID:
			return nil, refuse("use the client ID already in the config, or name another client with --oauth-client",
				"OAuth client %q already exists in the config with another client ID", c.Name)
		}
	}
	sep()
	out = append(out, accountTOML(a)...)
	if _, err := config.Parse(out); err != nil {
		return nil, refuse("fix the config by hand, then run bootstrap again",
			"the config would not be valid with account %q added: %v", a.Name, err)
	}
	return out, nil
}

// clientTOML is the [oauth_clients.<name>] table for c.
func clientTOML(c OAuthClientSpec) string {
	return "[oauth_clients." + tomlKey(c.Name) + "]\n" +
		"client_id = " + tomlQuote(c.ClientID) + "\n"
}

// accountTOML is the [accounts.<name>] table for a, followed by its imap
// and smtp override tables when set.
func accountTOML(a AccountSpec) string {
	var b strings.Builder
	table := "accounts." + tomlKey(a.Name)
	b.WriteString("[" + table + "]\n")
	b.WriteString("provider = " + tomlQuote(a.Provider) + "\n")
	b.WriteString("address = " + tomlQuote(a.Address) + "\n")
	b.WriteString("auth = " + tomlQuote(string(a.Auth)) + "\n")
	if a.OAuthClient != "" {
		b.WriteString("oauth_client = " + tomlQuote(a.OAuthClient) + "\n")
	}
	for _, o := range []struct {
		key string
		ep  *config.EndpointOverride
	}{{"imap", a.IMAP}, {"smtp", a.SMTP}} {
		if o.ep == nil {
			continue
		}
		security := o.ep.Security
		if security == "" {
			security = provider.SecurityTLS.String()
		}
		fmt.Fprintf(&b, "\n[%s.%s]\n", table, o.key)
		b.WriteString("host = " + tomlQuote(o.ep.Host) + "\n")
		fmt.Fprintf(&b, "port = %d\n", o.ep.Port)
		b.WriteString("security = " + tomlQuote(security) + "\n")
		if o.ep.ServerName != "" {
			b.WriteString("tls_server_name = " + tomlQuote(o.ep.ServerName) + "\n")
		}
	}
	return b.String()
}

// bareKeyRE matches a TOML bare key.
var bareKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// tomlKey writes name as a TOML key: bare when it can be, else quoted.
func tomlKey(name string) string {
	if bareKeyRE.MatchString(name) {
		return name
	}
	return tomlQuote(name)
}

// tomlQuote writes s as a TOML basic string. Quotes and backslashes are
// escaped, and every control character is written as \uXXXX, so the
// result is valid TOML whatever s holds. strconv.Quote is not used: its
// \x and \a escapes are not TOML. Invalid UTF-8 becomes U+FFFD.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
