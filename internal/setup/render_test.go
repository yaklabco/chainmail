package setup

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

var update = flag.Bool("update", false, "rewrite golden files")

// deployPageUnit is the unit on the deploy page, which bootstrap did not
// write and so carries no marker.
const deployPageUnit = `[Unit]
Description=chainmail policy daemon
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/bin/chainmail daemon --config /etc/chainmail/config.toml --vault-key-file /etc/chainmail/vault-key
User=chainmail
Group=chainmail
`

// golden compares got with testdata/name, rewriting the file under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file; run go test -update to rewrite it\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestRenderServiceGolden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		platform Platform
		golden   string
	}{
		{platform: Linux, golden: "chainmail.service.golden"},
		{platform: Darwin, golden: "sh.chainmail.daemon.plist.golden"},
	}
	for _, tt := range tests {
		t.Run(string(tt.platform), func(t *testing.T) {
			t.Parallel()
			golden(t, tt.golden, RenderService(LayoutFor(tt.platform)))
		})
	}
}

// serviceView is what the invariants test reads from a rendered unit or
// plist.
type serviceView struct {
	program     string
	args        []string
	user        string
	clientGroup string
	umask       string
	environment bool
}

// parseUnit reads the keys the invariants test needs from a systemd unit.
// It splits ExecStart with systemdSplit, so a quoted argument is one word.
func parseUnit(t *testing.T, data []byte) serviceView {
	t.Helper()
	var v serviceView
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case key == "ExecStart":
			words := systemdSplit(t, value)
			v.program, v.args = words[0], words[1:]
		case key == "User":
			v.user = value
		case key == "SupplementaryGroups":
			v.clientGroup = value
		case key == "UMask":
			v.umask = value
		case strings.HasPrefix(key, "Environment"):
			v.environment = true
		}
	}
	return v
}

// systemdSplit splits an ExecStart value into words, honouring double
// quotes and backslash escapes inside them.
func systemdSplit(t *testing.T, s string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted && c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
		case c == '"':
			quoted = !quoted
			inWord = true
		case !quoted && c == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if quoted {
		t.Fatalf("unterminated quote in ExecStart %q", s)
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) == 0 {
		t.Fatalf("empty ExecStart")
	}
	return words
}

// plistValue is one value in a plist dict: a string, an integer, a
// boolean, an array of strings or a nested dict (kept as its raw kind).
type plistValue struct {
	kind  string
	text  string
	array []string
}

// parsePlist decodes the top-level dict of a plist into key and value.
func parsePlist(t *testing.T, data []byte) map[string]plistValue {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	out := map[string]plistValue{}
	depth := 0 // dict depth
	var key string
	var stack []string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("plist does not parse as XML: %v", err)
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			name := tk.Name.Local
			stack = append(stack, name)
			if name == "dict" {
				depth++
				if depth == 2 {
					out[key] = plistValue{kind: "dict"}
				}
				continue
			}
			if depth != 1 {
				continue
			}
			switch name {
			case "true", "false":
				out[key] = plistValue{kind: "bool", text: name}
			case "array":
				out[key] = plistValue{kind: "array"}
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
			if tk.Name.Local == "dict" {
				depth--
			}
		case xml.CharData:
			if depth != 1 || len(stack) == 0 {
				continue
			}
			text := string(tk)
			switch top := stack[len(stack)-1]; top {
			case "key":
				key = text
			case "string", "integer":
				if len(stack) >= 2 && stack[len(stack)-2] == "array" {
					v := out[key]
					v.array = append(v.array, text)
					out[key] = v
				} else {
					out[key] = plistValue{kind: top, text: text}
				}
			}
		}
	}
	return out
}

// parsePlistView reads the keys the invariants test needs from a plist.
func parsePlistView(t *testing.T, data []byte) serviceView {
	t.Helper()
	m := parsePlist(t, data)
	args := m["ProgramArguments"].array
	if len(args) == 0 {
		t.Fatalf("plist has no ProgramArguments")
	}
	_, env := m["EnvironmentVariables"]
	return serviceView{
		program:     args[0],
		args:        args[1:],
		user:        m["UserName"].text,
		clientGroup: m["GroupName"].text,
		umask:       m["Umask"].kind + ":" + m["Umask"].text,
		environment: env,
	}
}

func TestRenderServiceInvariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		platform  Platform
		parse     func(*testing.T, []byte) serviceView
		wantUmask string
	}{
		{platform: Linux, parse: parseUnit, wantUmask: "0077"},
		{platform: Darwin, parse: parsePlistView, wantUmask: "integer:63"},
	}
	for _, tt := range tests {
		t.Run(string(tt.platform), func(t *testing.T) {
			t.Parallel()
			l := LayoutFor(tt.platform)
			data := RenderService(l)
			if bytes.Contains(data, []byte("--dev")) {
				t.Errorf("service definition mentions --dev")
			}
			v := tt.parse(t, data)
			if v.program != l.Binary {
				t.Errorf("program = %q, want %q", v.program, l.Binary)
			}
			if !slices.Equal(v.args, l.ServiceArgs()) {
				t.Errorf("arguments = %q, want %q", v.args, l.ServiceArgs())
			}
			if v.user != l.DaemonUser {
				t.Errorf("user = %q, want %q", v.user, l.DaemonUser)
			}
			if v.clientGroup != l.ClientGroup {
				t.Errorf("client group = %q, want %q", v.clientGroup, l.ClientGroup)
			}
			if v.umask != tt.wantUmask {
				t.Errorf("umask = %q, want %q", v.umask, tt.wantUmask)
			}
			if v.environment {
				t.Errorf("service definition sets an environment")
			}
			if !HasMarker(data) {
				t.Errorf("HasMarker = false for the rendered service definition")
			}
		})
	}
}

func TestRenderServicePlistKeys(t *testing.T) {
	t.Parallel()
	l := LayoutFor(Darwin)
	m := parsePlist(t, RenderService(l))
	checks := []struct {
		key  string
		want plistValue
	}{
		{key: "Label", want: plistValue{kind: "string", text: l.ServiceLabel}},
		{key: "StandardErrorPath", want: plistValue{kind: "string", text: l.LogFile}},
		{key: "RunAtLoad", want: plistValue{kind: "bool", text: "true"}},
		{key: "ThrottleInterval", want: plistValue{kind: "integer", text: "5"}},
		{key: "KeepAlive", want: plistValue{kind: "dict"}},
	}
	for _, c := range checks {
		got := m[c.key]
		if got.kind != c.want.kind || got.text != c.want.text {
			t.Errorf("%s = %+v, want %+v", c.key, got, c.want)
		}
	}
}

func TestRenderServiceUnitKeys(t *testing.T) {
	t.Parallel()
	unit := string(RenderService(LayoutFor(Linux)))
	for _, want := range []string{
		"\nGroup=chainmail\n",
		"\nStateDirectory=chainmail\n",
		"\nRuntimeDirectory=chainmail\n",
		"\nRestartSec=5s\nRestartPreventExitStatus=2\n",
		"\nProtectSystem=strict\n",
		"\nWantedBy=multi-user.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q", want)
		}
	}
}

func TestSystemdArgQuoting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{in: "/usr/local/libexec/chainmail/chainmail", want: "/usr/local/libexec/chainmail/chainmail"},
		{in: "/Library/Application Support/x", want: `"/Library/Application Support/x"`},
		{in: `a"b\c`, want: `"a\"b\\c"`},
		{in: "50%", want: "50%%"},
		{in: "$HOME", want: "$$HOME"},
		{in: "", want: `""`},
	}
	for _, tt := range tests {
		if got := systemdArg(tt.in); got != tt.want {
			t.Errorf("systemdArg(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNewConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		platform Platform
		account  AccountSpec
		client   *OAuthClientSpec
		golden   string
	}{
		{
			name:     "linux password gmail",
			platform: Linux,
			account: AccountSpec{
				Name: "me", Provider: "gmail", Address: "me@gmail.com", Auth: provider.AuthPassword,
			},
			golden: "config-linux.golden",
		},
		{
			name:     "darwin oauth google-workspace",
			platform: Darwin,
			account: AccountSpec{
				Name: "jane", Provider: "google-workspace", Address: "jane@example.com",
				Auth: provider.AuthOAuth2, OAuthClient: "google",
			},
			client: &OAuthClientSpec{Name: "google", ClientID: "id.apps.googleusercontent.com"},
			golden: "config-darwin.golden",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l := LayoutFor(tt.platform)
			data := NewConfig(l, tt.account, tt.client)
			golden(t, tt.golden, data)
			if !HasMarker(data) {
				t.Errorf("HasMarker = false for the new config")
			}
			cfg, err := config.Parse(data)
			if err != nil {
				t.Fatalf("config.Parse: %v", err)
			}
			if cfg.Daemon.Socket != l.Socket {
				t.Errorf("daemon.socket = %q, want %q", cfg.Daemon.Socket, l.Socket)
			}
			a, ok := cfg.Accounts[tt.account.Name]
			if !ok {
				t.Fatalf("account %q missing", tt.account.Name)
			}
			if a.Provider != tt.account.Provider || a.Address != tt.account.Address ||
				a.Auth != string(tt.account.Auth) || a.OAuthClient != tt.account.OAuthClient {
				t.Errorf("account = %+v, want %+v", a, tt.account)
			}
			if tt.client != nil && cfg.OAuthClients[tt.client.Name].ClientID != tt.client.ClientID {
				t.Errorf("oauth client = %+v, want ID %q", cfg.OAuthClients[tt.client.Name], tt.client.ClientID)
			}
		})
	}
}

// handWritten is a production config someone wrote, with comments and no
// trailing newline.
const handWritten = `# My own config. Keep this comment.
[policy]
send_allow = ["*@example.com"] # trailing comment

[oauth_clients.google]
client_id = "id.apps.googleusercontent.com"

[accounts.home]
provider = "hostinger"
address = "me@example.org"   # odd spacing`

func TestAppendAccount(t *testing.T) {
	t.Parallel()
	gmail := AccountSpec{Name: "work", Provider: "gmail", Address: "me@gmail.com", Auth: provider.AuthPassword}
	workspace := AccountSpec{
		Name: "jane", Provider: "google-workspace", Address: "jane@example.com",
		Auth: provider.AuthOAuth2, OAuthClient: "google",
	}

	t.Run("keeps the original bytes", func(t *testing.T) {
		t.Parallel()
		out, err := AppendAccount([]byte(handWritten), gmail, nil)
		if err != nil {
			t.Fatalf("AppendAccount: %v", err)
		}
		if !bytes.HasPrefix(out, []byte(handWritten)) {
			t.Errorf("result does not start with the original config:\n%s", out)
		}
		if !bytes.HasSuffix(out, []byte("\n")) {
			t.Errorf("result does not end with a newline")
		}
		cfg, err := config.Parse(out)
		if err != nil {
			t.Fatalf("config.Parse: %v", err)
		}
		if _, ok := cfg.Accounts["home"]; !ok {
			t.Errorf("original account lost")
		}
		if got := cfg.Accounts["work"]; got.Address != gmail.Address || got.Auth != "password" {
			t.Errorf("appended account = %+v", got)
		}
	})

	t.Run("existing client with the same ID", func(t *testing.T) {
		t.Parallel()
		client := &OAuthClientSpec{Name: "google", ClientID: "id.apps.googleusercontent.com"}
		out, err := AppendAccount([]byte(handWritten), workspace, client)
		if err != nil {
			t.Fatalf("AppendAccount: %v", err)
		}
		if n := bytes.Count(out, []byte("[oauth_clients.google]")); n != 1 {
			t.Errorf("found %d [oauth_clients.google] tables, want 1", n)
		}
		if _, err := config.Parse(out); err != nil {
			t.Fatalf("config.Parse: %v", err)
		}
	})

	t.Run("new client", func(t *testing.T) {
		t.Parallel()
		existing := []byte("[accounts.home]\nprovider = \"hostinger\"\naddress = \"me@example.org\"\n")
		client := &OAuthClientSpec{Name: "google", ClientID: "id.apps.googleusercontent.com"}
		out, err := AppendAccount(existing, workspace, client)
		if err != nil {
			t.Fatalf("AppendAccount: %v", err)
		}
		cfg, err := config.Parse(out)
		if err != nil {
			t.Fatalf("config.Parse: %v", err)
		}
		if cfg.OAuthClients["google"].ClientID != client.ClientID {
			t.Errorf("client = %+v", cfg.OAuthClients["google"])
		}
	})

	t.Run("overrides", func(t *testing.T) {
		t.Parallel()
		a := AccountSpec{
			Name: "e2e", Provider: "hostinger", Address: "e2e@example.net", Auth: provider.AuthPassword,
			IMAP: &config.EndpointOverride{Host: "127.0.0.1", Port: 3993, Security: "tls"},
			SMTP: &config.EndpointOverride{Host: "127.0.0.1", Port: 3465},
		}
		out, err := AppendAccount(nil, a, nil)
		if err != nil {
			t.Fatalf("AppendAccount: %v", err)
		}
		for _, want := range []string{
			"[accounts.e2e.imap]\nhost = \"127.0.0.1\"\nport = 3993\nsecurity = \"tls\"\n",
			"[accounts.e2e.smtp]\nhost = \"127.0.0.1\"\nport = 3465\nsecurity = \"tls\"\n",
		} {
			if !bytes.Contains(out, []byte(want)) {
				t.Errorf("result lacks %q:\n%s", want, out)
			}
		}
		cfg, err := config.Parse(out)
		if err != nil {
			t.Fatalf("config.Parse: %v", err)
		}
		if o := cfg.Accounts["e2e"].IMAP; o == nil || o.Host != "127.0.0.1" || o.Port != 3993 {
			t.Errorf("imap override = %+v", o)
		}
	})

	refusals := []struct {
		name     string
		existing string
		account  AccountSpec
		client   *OAuthClientSpec
	}{
		{
			name:     "duplicate account table",
			existing: "[accounts.work]\nprovider = \"hostinger\"\naddress = \"me@example.org\"\n",
			account:  gmail,
		},
		{
			name:     "account the config rejects after appending",
			existing: handWritten,
			account:  AccountSpec{Name: "bad", Provider: "nope", Address: "me@example.org", Auth: provider.AuthPassword},
		},
		{
			name:     "existing config does not parse",
			existing: "[policy\n",
			account:  gmail,
		},
		{
			name:     "existing client with another ID",
			existing: handWritten,
			account:  workspace,
			client:   &OAuthClientSpec{Name: "google", ClientID: "other.apps.googleusercontent.com"},
		},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := AppendAccount([]byte(tt.existing), tt.account, tt.client)
			r := asRefusal(t, err)
			if r.Hint == "" {
				t.Errorf("refusal %q has no hint", r.Msg)
			}
		})
	}
}

func TestTOMLQuote(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"plain",
		`quote " and backslash \`,
		"newline\nand tab\tand bell\a and del\x7f",
		"unicode é and emoji \U0001F600",
	} {
		quoted := tomlQuote(s)
		var got struct{ V string }
		if err := toml.Unmarshal([]byte("V = "+quoted+"\n"), &got); err != nil {
			t.Errorf("tomlQuote(%q) = %s does not parse: %v", s, quoted, err)
			continue
		}
		if got.V != s {
			t.Errorf("tomlQuote(%q) round-trips to %q", s, got.V)
		}
		if strings.ContainsAny(quoted, "\n\t\a\x7f") {
			t.Errorf("tomlQuote(%q) = %s holds a raw control character", s, strconv.Quote(quoted))
		}
	}
}

func TestHasMarker(t *testing.T) {
	t.Parallel()
	gmail := AccountSpec{Name: "me", Provider: "gmail", Address: "me@gmail.com", Auth: provider.AuthPassword}
	tests := []struct {
		name string
		data []byte
		want bool
	}{
		{name: "unit", data: RenderService(LayoutFor(Linux)), want: true},
		{name: "plist", data: RenderService(LayoutFor(Darwin)), want: true},
		{name: "config", data: NewConfig(LayoutFor(Linux), gmail, nil), want: true},
		{name: "deploy page unit", data: []byte(deployPageUnit), want: false},
		{name: "empty", data: []byte(""), want: false},
		{name: "nil", data: nil, want: false},
		{name: "marker later in the file", data: []byte("[policy]\n# " + Marker + "\n"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := HasMarker(tt.data); got != tt.want {
				t.Errorf("HasMarker = %t, want %t", got, tt.want)
			}
		})
	}
}
