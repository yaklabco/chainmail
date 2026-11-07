package daemon

import (
	"maps"
	"testing"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
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
