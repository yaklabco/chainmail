package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/jamesainslie/chainmail/internal/provider"
)

const validConfig = `
[daemon]
socket = "/tmp/cm.sock"

[oauth_clients.workspace]
client_id = "id-123"

[accounts.work]
provider = "google-workspace"
address = "james@ainslies.us"
oauth_client = "workspace"

[accounts.hostinger]
provider = "hostinger"
address = "me@example.com"

[accounts.icloud]
provider = "icloud"
address = "me@icloud.com"
username = "me"

[accounts.local]
provider = "custom"
address = "dev@example.test"
imap = { host = "127.0.0.1", port = 1993 }
smtp = { host = "127.0.0.1", port = 1587, security = "starttls" }
`

func TestParseValid(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.Socket != "/tmp/cm.sock" {
		t.Errorf("socket = %q", cfg.Daemon.Socket)
	}
	if cfg.Daemon.ClientGroup != DefaultClientGroup {
		t.Errorf("client group = %q, want default", cfg.Daemon.ClientGroup)
	}
	if got, want := cfg.AccountNames(), []string{"hostinger", "icloud", "local", "work"}; !slices.Equal(got, want) {
		t.Errorf("AccountNames() = %v, want %v", got, want)
	}

	work, err := cfg.Account("work")
	if err != nil {
		t.Fatalf("Account(work): %v", err)
	}
	if work.Auth != provider.AuthOAuth2 {
		t.Errorf("work auth = %q, want oauth2 by default", work.Auth)
	}
	if work.ClientID != "id-123" || work.OAuthClient != "workspace" {
		t.Errorf("work oauth client = %q/%q", work.OAuthClient, work.ClientID)
	}
	if work.Username != "james@ainslies.us" {
		t.Errorf("work username = %q, want the address", work.Username)
	}
	if work.IMAP.Addr() != "imap.gmail.com:993" {
		t.Errorf("work IMAP = %s", work.IMAP.Addr())
	}

	hostinger, err := cfg.Account("hostinger")
	if err != nil {
		t.Fatalf("Account(hostinger): %v", err)
	}
	if hostinger.Auth != provider.AuthPassword {
		t.Errorf("hostinger auth = %q, want password by default", hostinger.Auth)
	}

	icloud, err := cfg.Account("icloud")
	if err != nil {
		t.Fatalf("Account(icloud): %v", err)
	}
	if icloud.Username != "me" {
		t.Errorf("icloud username = %q", icloud.Username)
	}

	local, err := cfg.Account("local")
	if err != nil {
		t.Fatalf("Account(local): %v", err)
	}
	if want := (provider.Endpoint{Host: "127.0.0.1", Port: 1993, Security: provider.SecurityTLS}); local.IMAP != want {
		t.Errorf("local IMAP = %+v, want %+v", local.IMAP, want)
	}
	if local.SMTP.Security != provider.SecurityStartTLS {
		t.Errorf("local SMTP security = %v", local.SMTP.Security)
	}
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(""))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Daemon.Socket != DefaultSocket || cfg.Daemon.ClientGroup != DefaultClientGroup {
		t.Errorf("defaults = %+v", cfg.Daemon)
	}
}

func TestAccountUnknown(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Account("nope"); !errors.Is(err, ErrUnknownAccount) {
		t.Errorf("err = %v, want ErrUnknownAccount", err)
	}
}

func TestValidName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"work": true, "gmail-2": true, "a": true, strings.Repeat("a", 63): true,
		"": false, "-x": false, "Work": false, "a/b": false, "../x": false, "a\x00": false, strings.Repeat("a", 64): false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%.20q) = %v, want %v", name, got, want)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  string
		problem string
	}{
		{
			name:    "unknown provider",
			config:  "[accounts.a]\nprovider = \"aol\"\naddress = \"a@b.c\"",
			problem: `unknown provider "aol"`,
		},
		{
			name:    "bad account name",
			config:  "[accounts.Work_1]\nprovider = \"hostinger\"\naddress = \"a@b.c\"",
			problem: "name must be lowercase",
		},
		{
			name:    "address with display name",
			config:  "[accounts.a]\nprovider = \"hostinger\"\naddress = \"Bob <a@b.c>\"",
			problem: "must be a bare email address",
		},
		{
			name:    "gmail needs explicit auth",
			config:  "[accounts.a]\nprovider = \"gmail\"\naddress = \"a@gmail.com\"",
			problem: "auth is required",
		},
		{
			name:    "auth not supported by provider",
			config:  "[accounts.a]\nprovider = \"hostinger\"\naddress = \"a@b.c\"\nauth = \"oauth2\"",
			problem: "does not accept auth",
		},
		{
			name:    "oauth2 without client",
			config:  "[accounts.a]\nprovider = \"google-workspace\"\naddress = \"a@b.c\"",
			problem: "oauth_client is required",
		},
		{
			name:    "oauth2 with undefined client",
			config:  "[accounts.a]\nprovider = \"google-workspace\"\naddress = \"a@b.c\"\noauth_client = \"x\"",
			problem: `oauth_client "x" is not defined`,
		},
		{
			name:    "password with oauth client",
			config:  "[oauth_clients.x]\nclient_id = \"i\"\n[accounts.a]\nprovider = \"hostinger\"\naddress = \"a@b.c\"\noauth_client = \"x\"",
			problem: "only valid with oauth2",
		},
		{
			name:    "custom without endpoints",
			config:  "[accounts.a]\nprovider = \"custom\"\naddress = \"a@b.c\"",
			problem: "needs both imap and smtp",
		},
		{
			name:    "override with bad port",
			config:  "[accounts.a]\nprovider = \"hostinger\"\naddress = \"a@b.c\"\nimap = { host = \"h\", port = 0 }",
			problem: "port must be between 1 and 65535",
		},
		{
			name:    "override with bad security",
			config:  "[accounts.a]\nprovider = \"hostinger\"\naddress = \"a@b.c\"\nimap = { host = \"h\", port = 993, security = \"ssl\" }",
			problem: `unknown security "ssl"`,
		},
		{
			name:    "override without host",
			config:  "[accounts.a]\nprovider = \"hostinger\"\naddress = \"a@b.c\"\nsmtp = { port = 465 }",
			problem: "host is required",
		},
		{
			name:    "oauth client without id",
			config:  "[oauth_clients.x]\nclient_id = \"\"",
			problem: "client_id is required",
		},
		{
			name:    "bad oauth client name",
			config:  "[oauth_clients.\"../x\"]\nclient_id = \"i\"",
			problem: "oauth_clients.../x: name must be lowercase",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.config))
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			if !slices.ContainsFunc(ve.Problems, func(p string) bool { return strings.Contains(p, tt.problem) }) {
				t.Errorf("problems = %q, want one containing %q", ve.Problems, tt.problem)
			}
		})
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
	}{
		{name: "daemon", config: "[daemon]\nsockett = \"/x\""},
		{name: "account", config: "[accounts.x]\nprovder = \"hostinger\"\naddress = \"a@b.c\""},
		{name: "inline imap", config: "[accounts.x]\nprovider = \"hostinger\"\naddress = \"a@b.c\"\nimap = { host = \"h\", prot = 993 }"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.config))
			var strict *toml.StrictMissingError
			if !errors.As(err, &strict) {
				t.Errorf("err = %v, want *toml.StrictMissingError", err)
			}
		})
	}
}

func TestParseReportsEveryProblem(t *testing.T) {
	t.Parallel()
	config := "[accounts.a]\nprovider = \"aol\"\naddress = \"a@b.c\"\n" +
		"[accounts.b]\nprovider = \"hostinger\"\naddress = \"Bob <b@b.c>\""
	_, err := Parse([]byte(config))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	if len(ve.Problems) < 2 {
		t.Errorf("problems = %q, want at least 2", ve.Problems)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Accounts) != 4 {
		t.Errorf("accounts = %d, want 4", len(cfg.Accounts))
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file err = %v, want os.ErrNotExist", err)
	}
}

func TestBodyLimit(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Daemon.BodyLimit != DefaultBodyLimit || cfg.Daemon.BodyLimitBytes() != 64<<10 {
		t.Errorf("default body limit = %q (%d bytes)", cfg.Daemon.BodyLimit, cfg.Daemon.BodyLimitBytes())
	}
	cfg, err = Parse([]byte("[daemon]\nbody_limit = \"1MiB\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Daemon.BodyLimitBytes(); got != 1<<20 {
		t.Errorf("1MiB = %d bytes", got)
	}
	if got := (Daemon{}).BodyLimitBytes(); got != 64<<10 {
		t.Errorf("unparsed Daemon{} body limit = %d bytes, want the 64KiB default", got)
	}
	for _, bad := range []string{"64kb", "0", "16MiB", "-1", "1.5KiB", "512"} {
		_, err := Parse([]byte("[daemon]\nbody_limit = \"" + bad + "\"\n"))
		var verr *ValidationError
		if !errors.As(err, &verr) || !strings.Contains(err.Error(), "daemon.body_limit") {
			t.Errorf("body_limit %q: err = %v, want a daemon.body_limit problem", bad, err)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	t.Parallel()
	tests := map[string]int64{"65536": 65536, "512B": 512, "64KiB": 64 << 10, "2MiB": 2 << 20}
	for in, want := range tests {
		if got, err := ParseByteSize(in); err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "KiB", "1 KiB", "1kib", "1GiB", "9999999999999999999"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) succeeded", bad)
		}
	}
}

func TestComposeFields(t *testing.T) {
	t.Parallel()
	config := `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
display_name = "James Ainslie"
signature = "James\n[ainslies.us](https://ainslies.us)"
archive_folder = "Archive"
pool = 4
max_message_size = "10MiB"
`
	cfg, err := Parse([]byte(config))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := cfg.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "James Ainslie" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	if got.Signature != "James\n[ainslies.us](https://ainslies.us)" {
		t.Errorf("Signature = %q", got.Signature)
	}
	if got.ArchiveFolder != "Archive" {
		t.Errorf("ArchiveFolder = %q", got.ArchiveFolder)
	}
	if got.Pool != 4 {
		t.Errorf("Pool = %d, want 4", got.Pool)
	}
	if got.MaxMessageSize != 10<<20 {
		t.Errorf("MaxMessageSize = %d, want %d", got.MaxMessageSize, 10<<20)
	}
}

func TestComposeFieldDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]int64{
		"work":      25 << 20,
		"hostinger": 35 << 20,
		"icloud":    35 << 20,
		"local":     25 << 20,
	}
	for name, size := range tests {
		got, err := cfg.Account(name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Pool != 0 {
			t.Errorf("%s: Pool = %d, want 0 (not set)", name, got.Pool)
		}
		if got.MaxMessageSize != size {
			t.Errorf("%s: MaxMessageSize = %d, want the preset's %d", name, got.MaxMessageSize, size)
		}
		if got.DisplayName != "" || got.Signature != "" || got.ArchiveFolder != "" {
			t.Errorf("%s: compose fields = %q %q %q, want empty", name, got.DisplayName, got.Signature, got.ArchiveFolder)
		}
	}
}

func TestComposeFieldsInvalid(t *testing.T) {
	t.Parallel()
	const base = "[accounts.host]\nprovider = \"hostinger\"\naddress = \"me@example.com\"\n"
	tests := []struct {
		name  string
		extra string
		want  string
	}{
		{name: "pool negative", extra: "pool = -1", want: "accounts.host: pool"},
		{name: "pool too large", extra: "pool = 13", want: "accounts.host: pool"},
		{name: "size unparsable", extra: `max_message_size = "10MB"`, want: "accounts.host: max_message_size"},
		{name: "size zero", extra: `max_message_size = "0"`, want: "accounts.host: max_message_size"},
		{name: "size too small", extra: `max_message_size = "512B"`, want: "accounts.host: max_message_size"},
		{name: "size too large", extra: `max_message_size = "2048MiB"`, want: "accounts.host: max_message_size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(base + tt.extra + "\n"))
			var verr *ValidationError
			if !errors.As(err, &verr) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want a %q problem", err, tt.want)
			}
		})
	}
	for _, ok := range []string{"pool = 0", "pool = 1", "pool = 12", `max_message_size = "1KiB"`, `max_message_size = "1024MiB"`} {
		if _, err := Parse([]byte(base + ok + "\n")); err != nil {
			t.Errorf("%s: err = %v, want valid", ok, err)
		}
	}
}
