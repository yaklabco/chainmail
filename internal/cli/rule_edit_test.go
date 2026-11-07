package cli

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

// ruleMask is an UpdateRule mask of paths.
func ruleMask(paths ...string) *fieldmaskpb.FieldMask {
	return &fieldmaskpb.FieldMask{Paths: paths}
}

func TestRuleEditRequestsAndUsage(t *testing.T) {
	t.Parallel()
	partial := ruleTOML(t, "query = 'from:stripe.com'\n\n[[actions]]\nkind = 'archive'\n")
	empty := ruleTOML(t, "")
	testRuleRequests(t, []ruleCase{
		{
			// The mask lists paths in RuleDefinition's order, not the flags'.
			name: "rename and priority",
			args: []string{"edit", "receipts", "--priority", "10", "--name", "stripe-receipts"},
			want: &chainmailv1.UpdateRuleRequest{
				Rule: "receipts", Patch: &chainmailv1.RuleDefinition{Name: "stripe-receipts", Priority: proto.Int32(10)},
				UpdateMask: ruleMask("name", "priority"),
			},
		},
		{
			name: "time of day",
			args: []string{"edit", "inbox-cleanup", "--at", "07:00"},
			want: &chainmailv1.UpdateRuleRequest{Rule: "inbox-cleanup", Patch: &chainmailv1.RuleDefinition{Schedule: "at:07:00"}, UpdateMask: ruleMask("schedule")},
		},
		{
			name: "off the schedule",
			args: []string{"edit", "inbox-cleanup", "--no-schedule"},
			want: &chainmailv1.UpdateRuleRequest{Rule: "inbox-cleanup", Patch: &chainmailv1.RuleDefinition{}, UpdateMask: ruleMask("schedule")},
		},
		{
			name: "every account",
			args: []string{"edit", "receipts", "--all-accounts"},
			want: &chainmailv1.UpdateRuleRequest{Rule: "receipts", Patch: &chainmailv1.RuleDefinition{}, UpdateMask: ruleMask("accounts")},
		},
		{
			name: "fields and switches",
			args: []string{
				"edit", "receipts", "--account", "gmail", "--account", "gwork", "--on-new=false", "--stop",
				"--description", "Stripe", "--query", "from:stripe.com", "--max-per-run", "100",
			},
			want: &chainmailv1.UpdateRuleRequest{
				Rule: "receipts",
				Patch: &chainmailv1.RuleDefinition{
					Description: "Stripe", Accounts: []string{"gmail", "gwork"}, Query: "from:stripe.com", Stop: true, MaxPerRun: 100,
				},
				UpdateMask: ruleMask("description", "accounts", "query", "on_new", "stop", "max_per_run"),
			},
		},
		{
			name: "actions at a revision",
			args: []string{"edit", "receipts", "--action", "forward=books@example.com", "--note", "for the books", "--action", "archive", "--expected-revision", "3"},
			want: &chainmailv1.UpdateRuleRequest{
				Rule: "receipts",
				Patch: &chainmailv1.RuleDefinition{Actions: []*chainmailv1.RuleAction{
					{Kind: "forward", To: []string{"books@example.com"}, Note: "for the books"}, {Kind: "archive"},
				}},
				UpdateMask: ruleMask("actions"), ExpectedRevision: proto.Int64(3),
			},
		},
		{
			name: "the keys a file holds",
			args: []string{"edit", "receipts", "-f", partial},
			want: &chainmailv1.UpdateRuleRequest{
				Rule:       "receipts",
				Patch:      &chainmailv1.RuleDefinition{Query: "from:stripe.com", Actions: []*chainmailv1.RuleAction{{Kind: "archive"}}},
				UpdateMask: ruleMask("query", "actions"),
			},
		},
		{
			name: "enable",
			args: []string{"enable", "receipts"},
			want: &chainmailv1.UpdateRuleRequest{Rule: "receipts", Patch: &chainmailv1.RuleDefinition{Enabled: proto.Bool(true)}, UpdateMask: ruleMask("enabled")},
		},
		{
			name: "disable",
			args: []string{"disable", ruleID},
			want: &chainmailv1.UpdateRuleRequest{Rule: ruleID, Patch: &chainmailv1.RuleDefinition{Enabled: proto.Bool(false)}, UpdateMask: ruleMask("enabled")},
		},
		{name: "delete", args: []string{"delete", "receipts"}, want: &chainmailv1.DeleteRuleRequest{Rule: "receipts"}},
		{name: "restore", args: []string{"restore", ruleID}, want: &chainmailv1.RestoreRuleRequest{Rule: ruleID}},
	})
	testRuleUsage(t, []ruleCase{
		{name: "edit nothing", args: []string{"edit", "receipts"}, usage: "nothing to change: give a field flag or --file"},
		{name: "edit with an empty file", args: []string{"edit", "receipts", "-f", empty}, usage: "nothing to change: the rule file holds no keys"},
		{name: "edit on and off a schedule", args: []string{"edit", "receipts", "--no-schedule", "--every", "1h"}, usage: "--no-schedule cannot be used with --every or --at"},
		{name: "edit both account forms", args: []string{"edit", "receipts", "--all-accounts", "--account", "gmail"}, usage: "--all-accounts and --account cannot be used together"},
		{name: "edit with a file and a flag", args: []string{"edit", "receipts", "-f", partial, "--stop"}, usage: "--file and --stop cannot be used together"},
		{name: "edit a note alone", args: []string{"edit", "receipts", "--note", "x"}, usage: "--note applies to a forward action"},
		{name: "edit with a bad action", args: []string{"edit", "receipts", "--action", "move"}, usage: "--action move needs a value"},
		{name: "edit without a rule", args: []string{"edit"}, usage: "accepts 1 arg(s)"},
		{name: "enable two", args: []string{"enable", "a", "b"}, usage: "accepts 1 arg(s)"},
		{name: "delete none", args: []string{"delete"}, usage: "accepts 1 arg(s)"},
		{name: "restore none", args: []string{"restore"}, usage: "accepts 1 arg(s)"},
		// Restore takes a rule ID only (contract amendment 3): a live rule may
		// hold a deleted rule's name by now.
		{name: "restore by name", args: []string{"restore", "receipts"}, usage: "restore takes a rule ID"},
		{name: "restore by name hints", args: []string{"restore", "receipts"}, usage: "find it with chainmail rule list --deleted"},
	})
}

func TestRuleEditTable(t *testing.T) {
	t.Parallel()
	tail := " rule receipts " + ruleID + ", revision 3\n"
	testRuleTable(t, ruleEchoFake, []ruleCase{
		{name: "edit", args: []string{"edit", "receipts", "--stop"}, out: "updated" + tail},
		{name: "enable", args: []string{"enable", "receipts"}, out: "enabled" + tail},
		{name: "disable", args: []string{"disable", "receipts"}, out: "disabled" + tail},
		{name: "delete", args: []string{"delete", "receipts"}, out: "deleted" + tail},
		{name: "restore", args: []string{"restore", ruleID}, out: "restored" + tail},
	})
}

func TestCompleteRuleEdit(t *testing.T) {
	t.Parallel()
	testRuleCompletion(t, []ruleCase{
		{name: "edit", args: []string{"rule", "edit", "rec"}, cands: []string{"receipts"}},
		{name: "enable", args: []string{"rule", "enable", ""}, cands: []string{"receipts", "inbox-zero"}},
		{name: "delete takes one", args: []string{"rule", "delete", "receipts", ""}},
		{name: "restore offers deleted rules by ID", args: []string{"rule", "restore", ""}, cands: []string{"ru_gone"}},
	})
}
