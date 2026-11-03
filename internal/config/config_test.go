package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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
			if !errors.Is(err, ErrUnknownKey) {
				t.Errorf("err = %v, want ErrUnknownKey", err)
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

func TestMaxMessageSizeUnparsedFallsBackToPreset(t *testing.T) {
	t.Parallel()
	// A Config built without Parse, as tests sometimes do, still gets the
	// preset's limit rather than zero.
	cfg := &Config{Accounts: map[string]Account{
		"host": {Provider: "hostinger", Address: "me@example.com", MaxMessageSize: "lots"},
	}}
	got, err := cfg.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxMessageSize != 35<<20 {
		t.Errorf("MaxMessageSize = %d, want the preset's %d", got.MaxMessageSize, 35<<20)
	}
}

const policyAccount = "[accounts.x]\nprovider = \"hostinger\"\naddress = \"a@b.c\"\n"

func TestPolicyTables(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(`
[policy]
send_allow = ["*@ainslies.us", "someone@example.com"]
max_recipients = 25
allow_expunge = true
allow_folder_delete = true

[accounts.gmail]
provider = "hostinger"
address = "me@example.com"

[accounts.gmail.policy]
send_allow = ["friend@gmail.com"]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Policy{
		SendAllow:         []string{"*@ainslies.us", "someone@example.com"},
		MaxRecipients:     25,
		AllowExpunge:      true,
		AllowFolderDelete: true,
	}
	if !slices.Equal(cfg.Policy.SendAllow, want.SendAllow) || cfg.Policy.MaxRecipients != want.MaxRecipients ||
		cfg.Policy.AllowExpunge != want.AllowExpunge || cfg.Policy.AllowFolderDelete != want.AllowFolderDelete {
		t.Errorf("policy = %+v, want %+v", cfg.Policy, want)
	}
	if got := cfg.Accounts["gmail"].Policy.SendAllow; !slices.Equal(got, []string{"friend@gmail.com"}) {
		t.Errorf("gmail send_allow = %q", got)
	}
}

func TestPolicyDefaults(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"no policy table":  policyAccount,
		"max_recipients 0": policyAccount + "[policy]\nmax_recipients = 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(config))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if cfg.Policy.MaxRecipients != DefaultMaxRecipients {
				t.Errorf("max_recipients = %d, want %d", cfg.Policy.MaxRecipients, DefaultMaxRecipients)
			}
			if cfg.Policy.AllowExpunge || cfg.Policy.AllowFolderDelete || len(cfg.Policy.SendAllow) != 0 {
				t.Errorf("policy = %+v, want defaults", cfg.Policy)
			}
		})
	}
}

func TestPolicyMaxRecipientsBounds(t *testing.T) {
	t.Parallel()
	for n, ok := range map[int]bool{1: true, 500: true, 501: false, -1: false} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(policyAccount + "[policy]\nmax_recipients = " + strconv.Itoa(n) + "\n"))
			if ok {
				if err != nil {
					t.Errorf("Parse: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || !slices.Contains(ve.Problems, "policy.max_recipients must be 1 to 500") {
				t.Errorf("err = %v, want policy.max_recipients must be 1 to 500", err)
			}
		})
	}
}

func TestPolicyInvalidEntries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  string
		problem string
	}{
		{
			name:    "global",
			config:  policyAccount + "[policy]\nsend_allow = [\"ok@x.com\", \"*@*\"]\n",
			problem: `policy.send_allow: invalid entry "*@*"`,
		},
		{
			name:    "per account",
			config:  policyAccount + "[accounts.x.policy]\nsend_allow = [\"bob@*\"]\n",
			problem: `accounts.x.policy.send_allow: invalid entry "bob@*"`,
		},
		{
			name:    "per account with unknown provider",
			config:  "[accounts.y]\nprovider = \"aol\"\naddress = \"a@b.c\"\n[accounts.y.policy]\nsend_allow = [\"*.example.com\"]\n",
			problem: `accounts.y.policy.send_allow: invalid entry "*.example.com"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.config))
			var ve *ValidationError
			if !errors.As(err, &ve) || !slices.Contains(ve.Problems, tt.problem) {
				t.Errorf("err = %v, want a problem %q", err, tt.problem)
			}
		})
	}
}

func TestPolicyRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"global":      policyAccount + "[policy]\nfoo = 1\n",
		"per account": policyAccount + "[accounts.x.policy]\nmax_recipients = 3\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(config))
			if !errors.Is(err, ErrUnknownKey) {
				t.Errorf("err = %v, want ErrUnknownKey", err)
			}
		})
	}
}

func TestPolicyWithDefaults(t *testing.T) {
	t.Parallel()
	if got := (Policy{}).WithDefaults().MaxRecipients; got != DefaultMaxRecipients {
		t.Errorf("unset max_recipients = %d, want %d", got, DefaultMaxRecipients)
	}
	if got := (Policy{MaxRecipients: 3}).WithDefaults().MaxRecipients; got != 3 {
		t.Errorf("set max_recipients = %d, want 3", got)
	}
}

func TestSendAllowProblems(t *testing.T) {
	t.Parallel()
	got := SendAllowProblems("policy.send_allow", []string{"a@b.test", "*@*", "*@x.test", "bob@*"})
	want := []string{`policy.send_allow: invalid entry "*@*"`, `policy.send_allow: invalid entry "bob@*"`}
	if !slices.Equal(got, want) {
		t.Errorf("SendAllowProblems = %q, want %q", got, want)
	}
	if got := SendAllowProblems("x", nil); got != nil {
		t.Errorf("no entries: %q, want nil", got)
	}
}

const watchAccount = "[accounts.host]\nprovider = \"hostinger\"\naddress = \"me@example.com\"\n"

func TestWatchDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validConfig))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if want := (WatchConfig{Retention: "168h", MaxEvents: 200000, ExpungeSweep: "1h", IdleFallbackPoll: "30s", GoogleCheck: "5s"}); cfg.Watch != want {
		t.Errorf("watch = %+v, want %+v", cfg.Watch, want)
	}
	want := WatchSettings{
		Retention:        168 * time.Hour,
		MaxEvents:        200000,
		ExpungeSweep:     time.Hour,
		IdleFallbackPoll: 30 * time.Second,
		GoogleCheck:      5 * time.Second,
	}
	if got := cfg.WatchSettings(); got != want {
		t.Errorf("WatchSettings() = %+v, want %+v", got, want)
	}
	for _, name := range []string{"hostinger", "icloud", "local"} {
		a := cfg.Accounts[name]
		if a.Watch == nil || !slices.Equal(*a.Watch, []string{"INBOX"}) {
			t.Errorf("%s: watch = %v, want [INBOX]", name, a.Watch)
		}
		if a.Sync.Poll != "5m" {
			t.Errorf("%s: sync.poll = %q, want 5m", name, a.Sync.Poll)
		}
		if a.Sync.Enabled == nil || !*a.Sync.Enabled {
			t.Errorf("%s: sync.enabled = %v, want true", name, a.Sync.Enabled)
		}
		if a.Sync.Folders != nil {
			t.Errorf("%s: sync.folders = %q, want nil", name, a.Sync.Folders)
		}
	}
	if w := cfg.Accounts["work"].Watch; w != nil {
		t.Errorf("work (Gmail preset): watch = %v, want nil", *w)
	}
}

func TestWatchSettings(t *testing.T) {
	t.Parallel()
	defaults := WatchSettings{
		Retention:        168 * time.Hour,
		MaxEvents:        200000,
		ExpungeSweep:     time.Hour,
		IdleFallbackPoll: 30 * time.Second,
		GoogleCheck:      5 * time.Second,
	}
	cfg, err := Parse([]byte(watchAccount + `
[watch]
retention = "24h"
max_events = 5000
expunge_sweep = "30m"
idle_fallback_poll = "1m30s"
google_check = "20s"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := WatchSettings{
		Retention:        24 * time.Hour,
		MaxEvents:        5000,
		ExpungeSweep:     30 * time.Minute,
		IdleFallbackPoll: 90 * time.Second,
		GoogleCheck:      20 * time.Second,
	}
	if got := cfg.WatchSettings(); got != want {
		t.Errorf("WatchSettings() = %+v, want %+v", got, want)
	}
	// A Config built without Parse, as tests sometimes do, gets the
	// defaults for unset or unparsable values rather than zero.
	for name, c := range map[string]*Config{
		"empty":      {},
		"unparsable": {Watch: WatchConfig{Retention: "a week", MaxEvents: -5, ExpungeSweep: "0s", IdleFallbackPoll: "-1m", GoogleCheck: "1x"}},
	} {
		if got := c.WatchSettings(); got != defaults {
			t.Errorf("%s: WatchSettings() = %+v, want the defaults %+v", name, got, defaults)
		}
	}
}

const gmailAccount = "[accounts.g]\nprovider = \"gmail\"\naddress = \"me@gmail.com\"\nauth = \"password\"\n"

func TestAccountWatchResolution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		extra       string
		watch       []string
		syncFolders []string
		poll        time.Duration
		enabled     bool
	}{
		{
			name:  "defaults",
			watch: []string{"INBOX"}, syncFolders: []string{"INBOX"}, poll: 5 * time.Minute, enabled: true,
		},
		{
			name:  "two watched folders",
			extra: `watch = ["INBOX", "Lists"]`,
			watch: []string{"INBOX", "Lists"}, syncFolders: []string{"INBOX", "Lists"}, poll: 5 * time.Minute, enabled: true,
		},
		{
			name:  "sync folders after watch in order without duplicates",
			extra: "watch = [\"INBOX\", \"Lists\"]\nsync = { folders = [\"Sent\", \"INBOX\", \"Archive\", \"Sent\", \"Lists\"], poll = \"10m\" }",
			watch: []string{"INBOX", "Lists"}, syncFolders: []string{"INBOX", "Lists", "Sent", "Archive"}, poll: 10 * time.Minute, enabled: true,
		},
		{
			name:  "empty watch polls sync folders",
			extra: "watch = []\nsync = { folders = [\"Sent\"] }",
			watch: []string{}, syncFolders: []string{"Sent"}, poll: 5 * time.Minute, enabled: true,
		},
		{
			name:  "empty watch and no sync folders",
			extra: "watch = []",
			watch: []string{}, syncFolders: []string{}, poll: 5 * time.Minute, enabled: true,
		},
		{
			name:  "disabled",
			extra: "sync = { enabled = false }",
			watch: []string{"INBOX"}, syncFolders: []string{"INBOX"}, poll: 5 * time.Minute, enabled: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Parse([]byte(watchAccount + tt.extra + "\n"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got, err := cfg.Account("host")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Watch, tt.watch) {
				t.Errorf("Watch = %q, want %q", got.Watch, tt.watch)
			}
			if !slices.Equal(got.SyncFolders, tt.syncFolders) {
				t.Errorf("SyncFolders = %q, want %q", got.SyncFolders, tt.syncFolders)
			}
			if got.SyncPoll != tt.poll {
				t.Errorf("SyncPoll = %v, want %v", got.SyncPoll, tt.poll)
			}
			if got.SyncEnabled != tt.enabled {
				t.Errorf("SyncEnabled = %v, want %v", got.SyncEnabled, tt.enabled)
			}
		})
	}
}

func TestAccountWatchResolutionGmail(t *testing.T) {
	t.Parallel()
	for extra, enabled := range map[string]bool{
		"":                           true,
		"sync = { enabled = false }": false,
		"sync = { poll = \"1h\" }":   true,
	} {
		cfg, err := Parse([]byte(gmailAccount + extra + "\n"))
		if err != nil {
			t.Fatalf("%q: Parse: %v", extra, err)
		}
		got, err := cfg.Account("g")
		if err != nil {
			t.Fatal(err)
		}
		if got.Watch != nil || got.SyncFolders != nil {
			t.Errorf("%q: Watch = %q, SyncFolders = %q, want both nil on a Gmail preset", extra, got.Watch, got.SyncFolders)
		}
		if got.SyncEnabled != enabled {
			t.Errorf("%q: SyncEnabled = %v, want %v", extra, got.SyncEnabled, enabled)
		}
	}
}

func TestAccountWatchUnparsedFallsBackToDefaults(t *testing.T) {
	t.Parallel()
	// A Config built without Parse watches INBOX, polls every 5m and syncs.
	watch := []string{"INBOX", "Lists"}
	cfg := &Config{Accounts: map[string]Account{
		"host":  {Provider: "hostinger", Address: "me@example.com"},
		"lists": {Provider: "hostinger", Address: "me@example.com", Watch: &watch},
	}}
	got, err := cfg.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Watch, []string{"INBOX"}) || !slices.Equal(got.SyncFolders, []string{"INBOX"}) {
		t.Errorf("Watch = %q, SyncFolders = %q, want [INBOX] for both", got.Watch, got.SyncFolders)
	}
	if got.SyncPoll != 5*time.Minute || !got.SyncEnabled {
		t.Errorf("SyncPoll = %v, SyncEnabled = %v, want 5m and true", got.SyncPoll, got.SyncEnabled)
	}
	lists, err := cfg.Account("lists")
	if err != nil {
		t.Fatal(err)
	}
	lists.Watch[0] = "changed"
	if watch[0] != "INBOX" {
		t.Errorf("changing the resolved watch list changed the config's: %q", watch)
	}
}

func TestWatchCount(t *testing.T) {
	t.Parallel()
	gmail, _ := provider.Lookup("gmail")
	hostinger, _ := provider.Lookup("hostinger")
	tests := []struct {
		name string
		acct ResolvedAccount
		want int
	}{
		{name: "disabled", acct: ResolvedAccount{Provider: hostinger, Watch: []string{"INBOX"}}, want: 0},
		{name: "gmail", acct: ResolvedAccount{Provider: gmail, SyncEnabled: true}, want: 1},
		{name: "gmail disabled", acct: ResolvedAccount{Provider: gmail}, want: 0},
		{name: "two folders", acct: ResolvedAccount{Provider: hostinger, Watch: []string{"INBOX", "Lists"}, SyncEnabled: true}, want: 2},
		{name: "empty watch", acct: ResolvedAccount{Provider: hostinger, Watch: []string{}, SyncEnabled: true}, want: 0},
	}
	for _, tt := range tests {
		if got := tt.acct.WatchCount(); got != tt.want {
			t.Errorf("%s: WatchCount() = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestWatchValidBounds(t *testing.T) {
	t.Parallel()
	for _, config := range []string{
		"[watch]\nretention = \"1h\"\n",
		"[watch]\nretention = \"2160h\"\n",
		"[watch]\nretention = \"1h30m\"\n",
		"[watch]\nmax_events = 1000\n",
		"[watch]\nmax_events = 10000000\n",
		"[watch]\nmax_events = 0\n",
		"[watch]\nexpunge_sweep = \"5m\"\n",
		"[watch]\nexpunge_sweep = \"24h\"\n",
		"[watch]\nidle_fallback_poll = \"10s\"\n",
		"[watch]\nidle_fallback_poll = \"10m\"\n",
		"[watch]\ngoogle_check = \"2s\"\n",
		"[watch]\ngoogle_check = \"10m\"\n",
		watchAccount + "sync = { poll = \"30s\" }\n",
		watchAccount + "sync = { poll = \"24h\" }\n",
		watchAccount + "watch = []\n",
		watchAccount + "watch = []\nsync = { folders = [\"Sent\"] }\n",
		gmailAccount + "sync = { enabled = false }\n",
		gmailAccount + "sync = { poll = \"1h\" }\n",
	} {
		if _, err := Parse([]byte(config)); err != nil {
			t.Errorf("%q: err = %v, want valid", config, err)
		}
	}
}

func TestWatchInvalid(t *testing.T) {
	t.Parallel()
	const workspace = "[oauth_clients.ws]\nclient_id = \"id\"\n[accounts.w]\nprovider = \"google-workspace\"\naddress = \"me@ainslies.us\"\noauth_client = \"ws\"\n"
	const durationHint = "want a Go duration such as 30s, 5m or 168h"
	tests := []struct {
		name    string
		config  string
		problem string
	}{
		{name: "retention too short", config: "[watch]\nretention = \"59m\"\n", problem: "watch.retention: 59m must be between 1h and 2160h"},
		{name: "retention too long", config: "[watch]\nretention = \"2161h\"\n", problem: "watch.retention: 2161h must be between 1h and 2160h"},
		{name: "retention not a duration", config: "[watch]\nretention = \"a week\"\n", problem: `watch.retention: duration "a week": ` + durationHint},
		{name: "max_events too few", config: "[watch]\nmax_events = 999\n", problem: "watch.max_events must be 1000 to 10000000"},
		{name: "max_events too many", config: "[watch]\nmax_events = 10000001\n", problem: "watch.max_events must be 1000 to 10000000"},
		{name: "max_events negative", config: "[watch]\nmax_events = -1\n", problem: "watch.max_events must be 1000 to 10000000"},
		{name: "expunge_sweep too short", config: "[watch]\nexpunge_sweep = \"4m59s\"\n", problem: "watch.expunge_sweep: 4m59s must be between 5m and 24h"},
		{name: "expunge_sweep too long", config: "[watch]\nexpunge_sweep = \"24h1s\"\n", problem: "watch.expunge_sweep: 24h1s must be between 5m and 24h"},
		{name: "expunge_sweep in days", config: "[watch]\nexpunge_sweep = \"1d\"\n", problem: `watch.expunge_sweep: duration "1d": ` + durationHint},
		{name: "idle_fallback_poll too short", config: "[watch]\nidle_fallback_poll = \"9s\"\n", problem: "watch.idle_fallback_poll: 9s must be between 10s and 10m"},
		{name: "idle_fallback_poll too long", config: "[watch]\nidle_fallback_poll = \"10m1s\"\n", problem: "watch.idle_fallback_poll: 10m1s must be between 10s and 10m"},
		{name: "idle_fallback_poll negative", config: "[watch]\nidle_fallback_poll = \"-30s\"\n", problem: "watch.idle_fallback_poll: -30s must be between 10s and 10m"},
		{name: "google_check too short", config: "[watch]\ngoogle_check = \"1s\"\n", problem: "watch.google_check: 1s must be between 2s and 10m"},
		{name: "google_check too long", config: "[watch]\ngoogle_check = \"10m1s\"\n", problem: "watch.google_check: 10m1s must be between 2s and 10m"},
		{name: "google_check not a duration", config: "[watch]\ngoogle_check = \"fast\"\n", problem: `watch.google_check: duration "fast": ` + durationHint},
		{name: "sync.poll too short", config: watchAccount + "sync = { poll = \"29s\" }\n", problem: "accounts.host: sync.poll: 29s must be between 30s and 24h"},
		{name: "sync.poll too long", config: watchAccount + "sync = { poll = \"24h1s\" }\n", problem: "accounts.host: sync.poll: 24h1s must be between 30s and 24h"},
		{name: "sync.poll not a duration", config: watchAccount + "sync = { poll = \"5 minutes\" }\n", problem: `accounts.host: sync.poll: duration "5 minutes": ` + durationHint},
		{name: "empty watch folder", config: watchAccount + "watch = [\"INBOX\", \"\"]\n", problem: "accounts.host: watch has an empty folder name"},
		{name: "duplicate watch folder", config: watchAccount + "watch = [\"INBOX\", \"Lists\", \"INBOX\"]\n", problem: "accounts.host: watch lists INBOX twice"},
		{name: "empty sync folder", config: watchAccount + "sync = { folders = [\"Sent\", \"\"] }\n", problem: "accounts.host: sync.folders has an empty folder name"},
		{name: "gmail watch", config: gmailAccount + "watch = [\"INBOX\"]\n", problem: "accounts.g: watch and sync.folders are not allowed on Gmail presets; they always watch All Mail"},
		{name: "gmail empty watch", config: gmailAccount + "watch = []\n", problem: "accounts.g: watch and sync.folders are not allowed on Gmail presets; they always watch All Mail"},
		{name: "gmail sync folders", config: gmailAccount + "sync = { folders = [\"Sent\"] }\n", problem: "accounts.g: watch and sync.folders are not allowed on Gmail presets; they always watch All Mail"},
		{name: "workspace watch", config: workspace + "watch = [\"INBOX\"]\n", problem: "accounts.w: watch and sync.folders are not allowed on Gmail presets; they always watch All Mail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.config))
			var ve *ValidationError
			if !errors.As(err, &ve) || !slices.Contains(ve.Problems, tt.problem) {
				t.Errorf("err = %v, want a problem %q", err, tt.problem)
			}
		})
	}
}

func TestWatchDuplicateReportedOnce(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(watchAccount + "watch = [\"INBOX\", \"INBOX\", \"INBOX\", \"\", \"\"]\n"))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want *ValidationError", err)
	}
	want := []string{"accounts.host: watch has an empty folder name", "accounts.host: watch lists INBOX twice"}
	if !slices.Equal(ve.Problems, want) {
		t.Errorf("problems = %q, want %q", ve.Problems, want)
	}
}

func TestWatchRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	for name, config := range map[string]string{
		"watch table":      watchAccount + "[watch]\nretension = \"24h\"\n",
		"inline sync":      watchAccount + "sync = { folder = [\"Sent\"] }\n",
		"sync table":       watchAccount + "[accounts.host.sync]\npolls = \"5m\"\n",
		"prefetch for SP7": watchAccount + "sync = { prefetch_bodies = true }\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(config))
			if !errors.Is(err, ErrUnknownKey) {
				t.Errorf("err = %v, want ErrUnknownKey", err)
			}
		})
	}
}

func TestParseNamesTheUnknownKey(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("[accounts.x]\nprovider = \"hostinger\"\naddress = \"a@b.c\"\nprovder = \"x\""))
	// mapstructure writes the path as accounts[x].provder; only the key's
	// own name is pinned here.
	if !errors.Is(err, ErrUnknownKey) || !strings.Contains(err.Error(), "provder") {
		t.Errorf("err = %v, want ErrUnknownKey naming provder", err)
	}
}

func TestParseRefusesKeysViperWouldFold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, config, want string
	}{
		{name: "upper table", config: "[accounts.Work]\nprovider = \"hostinger\"\naddress = \"a@b.c\"", want: `"accounts.Work"`},
		{name: "upper key", config: "[accounts.work]\nProvider = \"hostinger\"\naddress = \"a@b.c\"", want: `"accounts.work.Provider"`},
		{name: "dotted key", config: "[accounts.\"a.b\"]\nprovider = \"hostinger\"\naddress = \"a@b.c\"", want: `"accounts.a.b"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tt.config))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one naming %s", err, tt.want)
			}
		})
	}
}

func TestParseRefusesWrongTypes(t *testing.T) {
	t.Parallel()
	base := "[accounts.host]\nprovider = \"hostinger\"\naddress = \"me@example.com\"\n"
	for name, extra := range map[string]string{
		"float for int":   "pool = 1.5",
		"string for int":  `pool = "3"`,
		"int for string":  "display_name = 3",
		"string for list": `watch = "INBOX"`,
		"bool for string": "signature = true",
		"string for bool": "[accounts.host.sync]\nenabled = \"yes\"",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(base + extra)); err == nil {
				t.Errorf("Parse accepted %q", extra)
			}
		})
	}
}

func TestParseKeepsAnEmptyWatchList(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte("[accounts.host]\nprovider = \"hostinger\"\naddress = \"me@example.com\"\nwatch = []\n"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := cfg.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	if a.Watch == nil || len(a.Watch) != 0 {
		t.Errorf("Watch = %#v, want an empty, non-nil list", a.Watch)
	}
}

// Not parallel: it sets the process environment.
func TestParseIgnoresTheEnvironment(t *testing.T) {
	t.Setenv("CHAINMAIL_POLICY_ALLOW_EXPUNGE", "true")
	t.Setenv("POLICY_ALLOW_EXPUNGE", "true")
	t.Setenv("CHAINMAIL_DAEMON_SOCKET", "/evil.sock")
	cfg, err := Parse([]byte("[accounts.host]\nprovider = \"hostinger\"\naddress = \"me@example.com\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.AllowExpunge || cfg.Daemon.Socket == "/evil.sock" {
		t.Errorf("environment reached the config: %+v %+v", cfg.Policy, cfg.Daemon)
	}
}
