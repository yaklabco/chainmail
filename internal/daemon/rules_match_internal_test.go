package daemon

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/rules"
)

// TestRuleEnv checks that a rule is validated against every configured
// account, with whether it is a Google account, and against the policy's
// max_recipients, defaulted when the config leaves it out.
func TestRuleEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		config string
		want   rules.Env
	}{
		{
			name:   "default max_recipients",
			config: routeConfig,
			want: rules.Env{
				Accounts:      map[string]rules.AccountInfo{"gmail": {Gmail: true}, "host": {Gmail: false}},
				MaxRecipients: 10,
			},
		},
		{
			name:   "configured max_recipients",
			config: passwordConfig + "\n[policy]\nmax_recipients = 3\n",
			want: rules.Env{
				Accounts:      map[string]rules.AccountInfo{"host": {Gmail: false}},
				MaxRecipients: 3,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServerConfig(t, tt.config)
			got := s.ruleEnv()
			if !maps.Equal(got.Accounts, tt.want.Accounts) || got.MaxRecipients != tt.want.MaxRecipients {
				t.Errorf("ruleEnv() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestRuleMessageKey checks the act-once key of a cached row: the Gmail
// message ID on a Google account, else UIDVALIDITY, UID and folder, which a
// Google row without a message ID falls back to.
func TestRuleMessageKey(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerConfig(t, routeConfig)
	host, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	gmail, err := s.opts.Config.Account("gmail")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		gmail bool
		row   cache.Row
		want  string
	}{
		{name: "standard", row: cache.Row{Folder: "INBOX", UIDValidity: 7, UID: 42}, want: "i:7:42:INBOX"},
		{name: "standard ignores a message ID", row: cache.Row{Folder: "Archive", UIDValidity: 3, UID: 9, GmailMsgID: 55}, want: "i:3:9:Archive"},
		{name: "google", gmail: true, row: cache.Row{Folder: "[Gmail]/All Mail", UIDValidity: 3, UID: 9, GmailMsgID: 1001}, want: "g:1001"},
		{name: "google without a message ID", gmail: true, row: cache.Row{Folder: "[Gmail]/All Mail", UIDValidity: 3, UID: 9}, want: "i:3:9:[Gmail]/All Mail"},
	}
	for _, tt := range tests {
		acct := host
		if tt.gmail {
			acct = gmail
		}
		if got := ruleMessageKey(acct, tt.row); got != tt.want {
			t.Errorf("%s: ruleMessageKey = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestRuleActionsProto checks that every argument of an action reaches its
// API form, in order, and that the recipient list is a copy.
func TestRuleActionsProto(t *testing.T) {
	t.Parallel()
	to := []string{"a@example.com", "b@example.com"}
	in := []rules.Action{
		{Kind: rules.ActLabel, Label: "Receipts"},
		{Kind: rules.ActCopy, Folder: "Keep"},
		{Kind: rules.ActLinkItem, ItemID: "it_x", Anchor: true},
		{Kind: rules.ActForward, To: to, Note: "fyi"},
		{Kind: rules.ActArchive},
	}
	want := []*chainmailv1.RuleAction{
		{Kind: "label", Label: "Receipts"},
		{Kind: "copy", Folder: "Keep"},
		{Kind: "link_item", ItemId: "it_x", Anchor: true},
		{Kind: "forward", To: []string{"a@example.com", "b@example.com"}, Note: "fyi"},
		{Kind: "archive"},
	}
	got := ruleActionsProto(in)
	if len(got) != len(want) {
		t.Fatalf("got %d actions, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("action %d = %v, want %v", i, got[i], want[i])
		}
	}
	to[0] = "changed@example.com"
	if got[3].GetTo()[0] != "a@example.com" {
		t.Error("the API form shares the rule's recipient list")
	}
	if got := ruleActionsProto(nil); len(got) != 0 {
		t.Errorf("no actions gave %v, want none", got)
	}
}

// ruleMarker is in every query, folder, item ID and account name the rule
// tests put in a failing rule. No error text or hint may repeat it.
const ruleMarker = "zqx"

// ruleWantErr fails unless err maps to reason with exactly msg as its text,
// and neither its text nor its hint holds ruleMarker.
func ruleWantErr(t *testing.T, err error, reason, msg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %s %q", reason, msg)
	}
	d := api.Detail(toConnectError(err))
	if d.GetReason() != reason || err.Error() != msg {
		t.Errorf("err = %s %q, want %s %q", d.GetReason(), err.Error(), reason, msg)
	}
	if strings.Contains(err.Error(), ruleMarker) || strings.Contains(d.GetHint(), ruleMarker) {
		t.Errorf("err %q (hint %q) repeats rule text", err.Error(), d.GetHint())
	}
}

// TestRuleAccounts checks that a rule covers its own accounts, or every
// configured one, narrowed by its query's account: terms, and that a name
// no config holds is refused without repeating it.
func TestRuleAccounts(t *testing.T) {
	t.Parallel()
	s, _ := newTestServerConfig(t, routeConfig)
	tests := []struct {
		name     string
		accounts []string
		query    string
		want     []string
	}{
		{name: "every account", want: []string{"gmail", "host"}},
		{name: "explicit", accounts: []string{"host"}, want: []string{"host"}},
		{name: "explicit, repeated and unsorted", accounts: []string{"host", "gmail", "host"}, want: []string{"gmail", "host"}},
		{name: "query narrows every account", query: "account:gmail from:acme", want: []string{"gmail"}},
		{name: "query narrows the explicit list", accounts: []string{"gmail", "host"}, query: "account:host", want: []string{"host"}},
		{name: "query names two accounts", query: "account:host account:gmail", want: []string{"gmail", "host"}},
		{name: "query outside the explicit list", accounts: []string{"host"}, query: "account:gmail", want: []string{}},
	}
	for _, tt := range tests {
		accts, err := s.ruleAccounts(rules.Definition{Accounts: tt.accounts, Query: tt.query})
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		names := make([]string, len(accts))
		for i, a := range accts {
			names[i] = a.Name
		}
		if !slices.Equal(names, tt.want) {
			t.Errorf("%s: accounts = %q, want %q", tt.name, names, tt.want)
		}
	}
	errs := []struct {
		name   string
		d      rules.Definition
		reason string
		msg    string
	}{
		{"unknown account", rules.Definition{Accounts: []string{"zqxnope"}}, api.ReasonNotFound, "the rule names an account that is not configured"},
		{"invalid account name", rules.Definition{Accounts: []string{"zqx bad"}}, api.ReasonInvalidArgument, "the rule names an account that is not configured"},
		{"unknown account: term", rules.Definition{Query: "account:zqxnope"}, api.ReasonNotFound, "the rule names an account that is not configured"},
		{"query does not parse", rules.Definition{Query: "is:zqxbogus"}, api.ReasonInvalidArgument, "the rule's query does not parse"},
	}
	for _, tt := range errs {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.ruleAccounts(tt.d)
			ruleWantErr(t, err, tt.reason, tt.msg)
		})
	}
}
