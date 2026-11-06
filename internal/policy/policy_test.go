package policy

import (
	"slices"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/config"
)

// accounts configures two accounts and no policy.
const accounts = `
[accounts.gmail]
provider = "hostinger"
address = "James@Example.com"

[accounts.work]
provider = "hostinger"
address = "james@work.test"
`

func newPolicy(t *testing.T, toml string) *Policy {
	t.Helper()
	cfg, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestCheckSend(t *testing.T) {
	t.Parallel()
	const withPolicy = accounts + `
[policy]
send_allow = ["*@x.com", "friend@global.test"]
max_recipients = 2

[accounts.gmail.policy]
send_allow = ["pal@gmail.test", "*@club.test"]
`
	const mixedCase = accounts + `
[policy]
send_allow = ["*@MiXed.TEST"]

[accounts.gmail.policy]
send_allow = ["Pal@Upper.TEST"]
`
	tests := []struct {
		name       string
		config     string
		account    string
		recipients []string
		want       SendDecision
	}{
		{
			name:       "default holds a stranger",
			config:     accounts,
			account:    "gmail",
			recipients: []string{"stranger@x.com"},
			want:       SendDecision{Blocked: []string{"stranger@x.com"}},
		},
		{
			name:       "own address of another account",
			config:     accounts,
			account:    "gmail",
			recipients: []string{"james@work.test"},
			want:       SendDecision{Allowed: true},
		},
		{
			name:       "own address compared case-insensitively",
			config:     accounts,
			account:    "work",
			recipients: []string{"JAMES@example.COM"},
			want:       SendDecision{Allowed: true},
		},
		{
			name:       "wildcard matches mixed case",
			config:     withPolicy,
			account:    "work",
			recipients: []string{"Bob@X.com"},
			want:       SendDecision{Allowed: true},
		},
		{
			name:       "blocked mixed case reported as given",
			config:     withPolicy,
			account:    "work",
			recipients: []string{"Bob@Elsewhere.TEST"},
			want:       SendDecision{Blocked: []string{"Bob@Elsewhere.TEST"}},
		},
		{
			name:       "wildcard does not match a subdomain",
			config:     withPolicy,
			account:    "work",
			recipients: []string{"bob@sub.x.com"},
			want:       SendDecision{Blocked: []string{"bob@sub.x.com"}},
		},
		{
			name:       "per-account address applies to its account",
			config:     withPolicy,
			account:    "gmail",
			recipients: []string{"pal@gmail.test", "anyone@club.test"},
			want:       SendDecision{Allowed: true},
		},
		{
			name:       "per-account entries do not apply to another account",
			config:     withPolicy,
			account:    "work",
			recipients: []string{"pal@gmail.test", "anyone@club.test"},
			want:       SendDecision{Blocked: []string{"pal@gmail.test", "anyone@club.test"}},
		},
		{
			name:       "global entries apply to every account",
			config:     withPolicy,
			account:    "gmail",
			recipients: []string{"friend@global.test"},
			want:       SendDecision{Allowed: true},
		},
		{
			name:       "global entries apply to an unknown account",
			config:     withPolicy,
			account:    "nope",
			recipients: []string{"friend@global.test", "james@work.test"},
			want:       SendDecision{Allowed: true},
		},
		{
			name:       "unknown account gets no per-account entries",
			config:     withPolicy,
			account:    "nope",
			recipients: []string{"pal@gmail.test"},
			want:       SendDecision{Blocked: []string{"pal@gmail.test"}},
		},
		{
			name:       "blocked recipients keep input order",
			config:     accounts,
			account:    "gmail",
			recipients: []string{"c@z.test", "james@work.test", "a@z.test", "b@z.test"},
			want:       SendDecision{Blocked: []string{"c@z.test", "a@z.test", "b@z.test"}},
		},
		{
			name:       "too many recipients",
			config:     withPolicy,
			account:    "work",
			recipients: []string{"a@x.com", "b@x.com", "c@x.com"},
			want:       SendDecision{TooMany: true},
		},
		{
			name:       "exactly max recipients",
			config:     withPolicy,
			account:    "work",
			recipients: []string{"a@x.com", "b@x.com"},
			want:       SendDecision{Allowed: true},
		},
		{
			name:       "mixed-case configured entries match lowercase recipients",
			config:     mixedCase,
			account:    "gmail",
			recipients: []string{"bob@mixed.test", "pal@upper.test"},
			want:       SendDecision{Allowed: true},
		},
		{
			name:       "mixed-case wildcard does not match a lookalike domain",
			config:     mixedCase,
			account:    "gmail",
			recipients: []string{"bob@mixed.test.evil", "bob@xmixed.test"},
			want:       SendDecision{Blocked: []string{"bob@mixed.test.evil", "bob@xmixed.test"}},
		},
		{
			name:       "too many and blocked",
			config:     withPolicy,
			account:    "work",
			recipients: []string{"a@x.com", "b@y.test", "c@x.com"},
			want:       SendDecision{Blocked: []string{"b@y.test"}, TooMany: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := newPolicy(t, tt.config).CheckSend(tt.account, tt.recipients)
			if got.Allowed != tt.want.Allowed || got.TooMany != tt.want.TooMany || !slices.Equal(got.Blocked, tt.want.Blocked) {
				t.Errorf("CheckSend(%q, %q) = %+v, want %+v", tt.account, tt.recipients, got, tt.want)
			}
		})
	}
}

func TestCheckSendDefaultMaxRecipients(t *testing.T) {
	t.Parallel()
	p := newPolicy(t, accounts)
	own := slices.Repeat([]string{"james@work.test"}, config.DefaultMaxRecipients)
	if got := p.CheckSend("gmail", own); !got.Allowed {
		t.Errorf("%d recipients: %+v, want allowed", len(own), got)
	}
	if got := p.CheckSend("gmail", append(own, "james@work.test")); got.Allowed || !got.TooMany {
		t.Errorf("%d recipients: %+v, want too many", len(own)+1, got)
	}
}

func TestDestructiveGetters(t *testing.T) {
	t.Parallel()
	p := newPolicy(t, accounts)
	if p.AllowExpunge() || p.AllowFolderDelete() {
		t.Errorf("defaults: AllowExpunge=%v AllowFolderDelete=%v, want both false", p.AllowExpunge(), p.AllowFolderDelete())
	}
	p = newPolicy(t, accounts+"\n[policy]\nallow_expunge = true\n")
	if !p.AllowExpunge() || p.AllowFolderDelete() {
		t.Errorf("expunge only: AllowExpunge=%v AllowFolderDelete=%v", p.AllowExpunge(), p.AllowFolderDelete())
	}
	p = newPolicy(t, accounts+"\n[policy]\nallow_folder_delete = true\n")
	if p.AllowExpunge() || !p.AllowFolderDelete() {
		t.Errorf("folder delete only: AllowExpunge=%v AllowFolderDelete=%v", p.AllowExpunge(), p.AllowFolderDelete())
	}
}

func TestNewRejectsInvalidEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *config.Config
	}{
		{name: "global", cfg: &config.Config{Policy: config.Policy{SendAllow: []string{"*@*"}}}},
		{name: "account", cfg: &config.Config{Accounts: map[string]config.Account{
			"a": {Address: "a@b.test", Policy: config.AccountPolicy{SendAllow: []string{"bob@*"}}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tt.cfg); err == nil {
				t.Error("New: want an error for an invalid entry")
			}
		})
	}
}

func TestNewZeroConfig(t *testing.T) {
	t.Parallel()
	p, err := New(&config.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.CheckSend("any", []string{"a@b.test"}); got.Allowed || !slices.Equal(got.Blocked, []string{"a@b.test"}) {
		t.Errorf("CheckSend = %+v, want a@b.test blocked", got)
	}
	if got := p.CheckSend("any", nil); !got.Allowed {
		t.Errorf("CheckSend(nil) = %+v, want allowed", got)
	}
}

func TestOwnAddressIsNeverAPattern(t *testing.T) {
	t.Parallel()
	// net/mail accepts * in a local part, so *@x.com is a valid account
	// address. It must allow only itself, not the whole domain.
	p := newPolicy(t, "[accounts.odd]\nprovider = \"hostinger\"\naddress = \"*@x.com\"\n")
	if got := p.CheckSend("odd", []string{"*@x.com"}); !got.Allowed {
		t.Errorf("own address: %+v, want allowed", got)
	}
	if got := p.CheckSend("odd", []string{"bob@x.com"}); got.Allowed {
		t.Errorf("bob@x.com: %+v, want blocked", got)
	}
}

// A recipient that is not a plain address is blocked whatever the
// allowlist says: its decoded form could carry SMTP path delimiters and
// ESMTP parameters that reach a recipient the allowlist never saw.
func TestCheckSendBlocksUnsafeAddresses(t *testing.T) {
	t.Parallel()
	const injected = "victim@blocked.invalid> ORCPT=rfc822;x@example.net"
	// net/mail, and so config.Parse, accepts an address literal as an
	// account address and as a bare entry, but it is not a plain address.
	p := newPolicy(t, `
[accounts.odd]
provider = "hostinger"
address = "me@[192.0.2.1]"

[policy]
send_allow = ["*@example.net", "pal@[192.0.2.2]", "friend@x.test"]
`)
	tests := []struct {
		name       string
		recipients []string
		want       SendDecision
	}{
		{
			name:       "the review payload allowed by a wildcard",
			recipients: []string{"ok@example.net", injected},
			want:       SendDecision{Blocked: []string{injected}},
		},
		{
			name:       "a space allowed by a wildcard",
			recipients: []string{"John Smith@Example.NET"},
			want:       SendDecision{Blocked: []string{"John Smith@Example.NET"}},
		},
		{
			name:       "an unsafe own address",
			recipients: []string{"ME@[192.0.2.1]"},
			want:       SendDecision{Blocked: []string{"ME@[192.0.2.1]"}},
		},
		{
			name:       "an unsafe bare entry",
			recipients: []string{"pal@[192.0.2.2]"},
			want:       SendDecision{Blocked: []string{"pal@[192.0.2.2]"}},
		},
		{
			name:       "plain addresses are still allowed",
			recipients: []string{"ok@example.net", "Friend@X.test"},
			want:       SendDecision{Allowed: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := p.CheckSend("odd", tt.recipients)
			if got.Allowed != tt.want.Allowed || got.TooMany != tt.want.TooMany || !slices.Equal(got.Blocked, tt.want.Blocked) {
				t.Errorf("CheckSend(%q) = %+v, want %+v", tt.recipients, got, tt.want)
			}
		})
	}
}

func TestNewWordsInvalidEntriesAsParseDoes(t *testing.T) {
	t.Parallel()
	_, perr := config.Parse([]byte("[policy]\nsend_allow = [\"*@*\"]\n"))
	_, nerr := New(&config.Config{Policy: config.Policy{SendAllow: []string{"*@*"}}})
	if perr == nil || nerr == nil || nerr.Error() != `policy.send_allow: invalid entry "*@*"` || !strings.Contains(perr.Error(), nerr.Error()) {
		t.Errorf("Parse: %v; New: %v; want the same wording", perr, nerr)
	}
}
