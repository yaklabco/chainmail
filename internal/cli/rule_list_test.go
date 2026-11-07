package cli

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/rules"
)

// ruleListFixture is ruleFixture and a rule the runner disabled, which
// covers every account, runs by hand only, and moves mail to a folder
// whose name holds an escape sequence.
func ruleListFixture() *chainmailv1.ListRulesResponse {
	return &chainmailv1.ListRulesResponse{Rules: []*chainmailv1.Rule{
		ruleFixture(),
		{
			Id: ruleID2, Name: "old-newsletters", Description: ut(""), Query: ut("from:news@example.com older_than:30d"),
			Priority: 100, Actions: []*chainmailv1.RuleAction{{Kind: "move", Folder: "Old\x1b[2J/News"}},
			MaxPerRun: 200, DisabledReason: "repeated_failures", Revision: 7, FailedRuns: 5,
			CreatedAt: timestamppb.New(ruleAt), UpdatedAt: timestamppb.New(ruleAt.Add(2 * time.Hour)),
			CreatedBy: "1000/cli", UpdatedBy: "1000/rule",
		},
	}}
}

func TestRuleListGolden(t *testing.T) {
	t.Parallel()
	f := &fakeRules{list: func(*chainmailv1.ListRulesRequest) (*chainmailv1.ListRulesResponse, error) {
		return ruleListFixture(), nil
	}}
	socket := startFakeRules(t, f)
	for _, tt := range []struct{ output, golden string }{
		{output: "json", golden: "rule_list_json"},
		{output: "table", golden: "rule_list_table"},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, "--socket", socket, "--output", tt.output, "rule", "list")
			if got.code != 0 {
				t.Fatalf("rule list: %+v", got)
			}
			golden(t, tt.golden, got.stdout)
		})
	}
}

func TestRuleListRequestsAndUsage(t *testing.T) {
	t.Parallel()
	testRuleRequests(t, []ruleCase{
		{name: "list", args: []string{"list"}, want: &chainmailv1.ListRulesRequest{}},
		{
			name: "list every flag",
			args: []string{"list", "--account", "gmail", "--disabled", "--deleted"},
			want: &chainmailv1.ListRulesRequest{Account: "gmail", Enabled: proto.Bool(false), Deleted: true},
		},
		{name: "list enabled", args: []string{"list", "--enabled"}, want: &chainmailv1.ListRulesRequest{Enabled: proto.Bool(true)}},
	})
	testRuleUsage(t, []ruleCase{
		{name: "list both ways", args: []string{"list", "--enabled", "--disabled"}, usage: "--enabled and --disabled cannot be used together"},
		{name: "list argument", args: []string{"list", "extra"}, usage: "accepts 0 arg(s)"},
	})
}

// TestRuleValuesMatchTheModel keeps the triggers and outcomes help and
// completion offer in step with the rules model, in its order.
func TestRuleValuesMatchTheModel(t *testing.T) {
	t.Parallel()
	triggers := []string{string(rules.TriggerNew), string(rules.TriggerSchedule), string(rules.TriggerManual)}
	if !slices.Equal(ruleTriggers, triggers) {
		t.Errorf("ruleTriggers = %q, want %q", ruleTriggers, triggers)
	}
	outcomes := []string{
		string(rules.OutcomeOK), string(rules.OutcomeCapped), string(rules.OutcomeFailed),
		string(rules.OutcomeCacheNotReady), string(rules.OutcomeInterrupted),
	}
	if !slices.Equal(ruleOutcomes, outcomes) {
		t.Errorf("ruleOutcomes = %q, want %q", ruleOutcomes, outcomes)
	}
}
