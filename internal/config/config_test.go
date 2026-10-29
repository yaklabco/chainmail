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
