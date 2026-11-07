package mcpserver

import (
	"errors"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/rules"
)

// ruleKindsText is the end of every unknown-kind refusal.
const ruleKindsText = "kinds are label, unlabel, copy, flag, unflag, read, unread, link_item, forward, move, archive and trash"

// ruleArgMessage returns the message of an INVALID_ARGUMENT tool error, or
// fails the test.
func ruleArgMessage(t *testing.T, err error) string {
	t.Helper()
	var te *toolError
	if !errors.As(err, &te) || te.body.Reason != api.ReasonInvalidArgument {
		t.Fatalf("error = %v, want an INVALID_ARGUMENT tool error", err)
	}
	return te.body.Message
}

// TestRuleToolValuesMatchTheModel ties the tools' lists and bounds to
// internal/rules and the proto, so neither can drift from the tools.
func TestRuleToolValuesMatchTheModel(t *testing.T) {
	t.Parallel()
	kinds := []rules.ActionKind{
		rules.ActLabel, rules.ActUnlabel, rules.ActCopy, rules.ActFlag, rules.ActUnflag, rules.ActRead,
		rules.ActUnread, rules.ActLinkItem, rules.ActForward, rules.ActMove, rules.ActArchive, rules.ActTrash,
	}
	if len(kinds) != len(ruleActionKinds) {
		t.Fatalf("ruleActionKinds = %v, want the %d kinds of internal/rules", ruleActionKinds, len(kinds))
	}
	for i, k := range kinds {
		if ruleActionKinds[i] != string(k) || !k.Valid() {
			t.Errorf("ruleActionKinds[%d] = %q, want %q", i, ruleActionKinds[i], k)
		}
	}
	for k := range ruleActionArgs {
		if !rules.ActionKind(k).Valid() {
			t.Errorf("ruleActionArgs names %q, which is not an action kind", k)
		}
	}
	for _, tr := range []rules.Trigger{rules.TriggerNew, rules.TriggerSchedule, rules.TriggerManual} {
		if !slices.Contains(ruleTriggers, string(tr)) {
			t.Errorf("ruleTriggers %v lacks %q", ruleTriggers, tr)
		}
	}
	if rules.MinPriority != -rulePriorityBound || rules.MaxPriority != rulePriorityBound || rules.MaxMaxPerRun != ruleMaxPerRun {
		t.Errorf("bounds %d, %d, %d differ from internal/rules", -rulePriorityBound, rulePriorityBound, ruleMaxPerRun)
	}
	fields := (&chainmailv1.RuleDefinition{}).ProtoReflect().Descriptor().Fields()
	var names []string
	for i := range fields.Len() {
		names = append(names, string(fields.Get(i).Name()))
	}
	if !slices.Equal(names, ruleUpdatePaths) {
		t.Errorf("ruleUpdatePaths = %v, want the RuleDefinition fields %v", ruleUpdatePaths, names)
	}
}

func TestRuleActionsBuildTheDaemonsActions(t *testing.T) {
	t.Parallel()
	// One of every shape. The daemon, not the tool, checks how actions
	// combine (one relocating action, last; one copy; one forward).
	in := []ruleActionIn{
		{Kind: "label", Label: "Receipts"},
		{Kind: "unlabel", Label: "Inbox"},
		{Kind: "copy", Folder: "Archive/2026"},
		{Kind: "flag"},
		{Kind: "unflag"},
		{Kind: "read"},
		{Kind: "unread"},
		{Kind: "link_item", ItemID: "it_01jc0story"},
		{Kind: "link_item", ItemID: "it_01jc0story", Anchor: true},
		{Kind: "forward", To: []string{"a@x.test", "b@x.test"}},
		{Kind: "forward", To: []string{"a@x.test"}, Note: "FYI"},
		{Kind: "move", Folder: "Receipts"},
		{Kind: "archive"},
		{Kind: "trash"},
	}
	want := []*chainmailv1.RuleAction{
		{Kind: "label", Label: "Receipts"},
		{Kind: "unlabel", Label: "Inbox"},
		{Kind: "copy", Folder: "Archive/2026"},
		{Kind: "flag"},
		{Kind: "unflag"},
		{Kind: "read"},
		{Kind: "unread"},
		{Kind: "link_item", ItemId: "it_01jc0story"},
		{Kind: "link_item", ItemId: "it_01jc0story", Anchor: true},
		{Kind: "forward", To: []string{"a@x.test", "b@x.test"}},
		{Kind: "forward", To: []string{"a@x.test"}, Note: "FYI"},
		{Kind: "move", Folder: "Receipts"},
		{Kind: "archive"},
		{Kind: "trash"},
	}
	got, err := ruleActions(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("%d actions, want %d", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("action %d = %v, want %v", i+1, got[i], want[i])
		}
	}
	if none, err := ruleActions(nil); err != nil || none == nil || len(none) != 0 {
		t.Errorf("ruleActions(nil) = %v, %v; want an empty list", none, err)
	}
}

func TestRuleActionsRejectBadShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []ruleActionIn
		msg  string
	}{
		{name: "unknown kind", in: []ruleActionIn{{Kind: "delete"}}, msg: `action 1: unknown kind "delete"; ` + ruleKindsText},
		{name: "kinds are lowercase", in: []ruleActionIn{{Kind: "Archive"}}, msg: `action 1: unknown kind "Archive"; ` + ruleKindsText},
		{name: "an empty kind", in: []ruleActionIn{{}}, msg: `action 1: unknown kind ""; ` + ruleKindsText},
		{name: "label needs a label", in: []ruleActionIn{{Kind: "label"}}, msg: "action 1 (label) needs label"},
		{name: "unlabel needs a label", in: []ruleActionIn{{Kind: "unlabel"}}, msg: "action 1 (unlabel) needs label"},
		{name: "copy needs a folder", in: []ruleActionIn{{Kind: "copy"}}, msg: "action 1 (copy) needs folder"},
		{name: "the second action is counted", in: []ruleActionIn{{Kind: "flag"}, {Kind: "move"}}, msg: "action 2 (move) needs folder"},
		{name: "link_item needs an item", in: []ruleActionIn{{Kind: "link_item", Anchor: true}}, msg: "action 1 (link_item) needs item_id"},
		{name: "forward needs addresses", in: []ruleActionIn{{Kind: "forward", Note: "FYI"}}, msg: "action 1 (forward) needs to"},
		{name: "archive takes no folder", in: []ruleActionIn{{Kind: "archive", Folder: "Old"}}, msg: "action 1 (archive) takes no folder"},
		{name: "label takes no note", in: []ruleActionIn{{Kind: "label", Label: "L", Note: "n"}}, msg: "action 1 (label) takes no note"},
		{name: "move takes no anchor", in: []ruleActionIn{{Kind: "move", Folder: "F", Anchor: true}}, msg: "action 1 (move) takes no anchor"},
		{name: "flag takes no addresses", in: []ruleActionIn{{Kind: "flag", To: []string{"a@x.test"}}}, msg: "action 1 (flag) takes no to"},
		{name: "copy takes no label", in: []ruleActionIn{{Kind: "copy", Folder: "F", Label: "L"}}, msg: "action 1 (copy) takes no label"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ruleActions(tt.in)
			if got := ruleArgMessage(t, err); got != tt.msg {
				t.Errorf("message = %q, want %q", got, tt.msg)
			}
		})
	}
}

func TestRuleDefinitionChecksBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		priority  *int
		maxPerRun int
		want      *int32 // the priority sent
		msg       string // empty when the definition is accepted
	}{
		{name: "no priority"},
		{name: "lowest priority", priority: new(-1000000), want: new(int32(-1000000))},
		{name: "highest priority", priority: new(1000000), want: new(int32(1000000))},
		{name: "priority 0 is a value", priority: new(0), want: new(int32(0))},
		{name: "priority below", priority: new(-1000001), msg: "priority must be -1000000 to 1000000, got -1000001"},
		{name: "priority above", priority: new(1000001), msg: "priority must be -1000000 to 1000000, got 1000001"},
		{name: "max_per_run 5000", maxPerRun: 5000},
		{name: "max_per_run below", maxPerRun: -1, msg: "max_per_run must be 1 to 5000, or 0 for 500, got -1"},
		{name: "max_per_run above", maxPerRun: 5001, msg: "max_per_run must be 1 to 5000, or 0 for 500, got 5001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, err := ruleDefinition(ruleDefinitionIn{
				Name: "tidy", Query: "in:inbox", Priority: tt.priority, MaxPerRun: tt.maxPerRun, Actions: []ruleActionIn{{Kind: "archive"}},
			})
			if tt.msg != "" {
				if got := ruleArgMessage(t, err); got != tt.msg {
					t.Errorf("message = %q, want %q", got, tt.msg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := &chainmailv1.RuleDefinition{
				Name: "tidy", Query: "in:inbox", Priority: tt.want, MaxPerRun: int32(tt.maxPerRun),
				Actions: []*chainmailv1.RuleAction{{Kind: "archive"}},
			}
			if !proto.Equal(d, want) {
				t.Errorf("definition = %v, want %v", d, want)
			}
		})
	}
}

func TestUpdateRuleRequest(t *testing.T) {
	t.Parallel()
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
	tests := []struct {
		name string
		in   updateRuleIn
		want *chainmailv1.UpdateRuleRequest
	}{
		{
			name: "enabled alone switches a rule off",
			in:   updateRuleIn{Rule: "stripe-receipts", Enabled: new(false)},
			want: &chainmailv1.UpdateRuleRequest{Rule: "stripe-receipts", Patch: &chainmailv1.RuleDefinition{Enabled: proto.Bool(false)}, UpdateMask: mask("enabled")},
		},
		{
			name: "every field, in RuleDefinition order, with a revision",
			in: updateRuleIn{
				Rule: "ru_01jc0stripe", Enabled: new(true), MaxPerRun: new(200),
				Actions: []ruleActionIn{{Kind: "label", Label: "Receipts"}, {Kind: "archive"}},
				Stop:    new(true), Priority: new(-5), Schedule: new("at:07:00"), OnNew: new(true), Query: new("from:stripe.com"),
				Accounts: []string{"work"}, Description: new("Stripe receipts"), Name: new("stripe"), ExpectedRevision: new(int64(3)),
			},
			want: &chainmailv1.UpdateRuleRequest{
				Rule: "ru_01jc0stripe",
				Patch: &chainmailv1.RuleDefinition{
					Name: "stripe", Description: "Stripe receipts", Accounts: []string{"work"}, Query: "from:stripe.com", OnNew: true,
					Schedule: "at:07:00", Priority: proto.Int32(-5), Stop: true,
					Actions:   []*chainmailv1.RuleAction{{Kind: "label", Label: "Receipts"}, {Kind: "archive"}},
					MaxPerRun: 200, Enabled: proto.Bool(true),
				},
				UpdateMask:       mask("name", "description", "accounts", "query", "on_new", "schedule", "priority", "stop", "actions", "max_per_run", "enabled"),
				ExpectedRevision: proto.Int64(3),
			},
		},
		{
			name: "clear sends the path with no value",
			in:   updateRuleIn{Rule: "tidy", Clear: []string{"schedule", "accounts", "description"}},
			want: &chainmailv1.UpdateRuleRequest{Rule: "tidy", Patch: &chainmailv1.RuleDefinition{}, UpdateMask: mask("description", "accounts", "schedule")},
		},
		{
			name: "false and 0 are values",
			in:   updateRuleIn{Rule: "tidy", OnNew: new(false), Stop: new(false), Priority: new(0)},
			want: &chainmailv1.UpdateRuleRequest{Rule: "tidy", Patch: &chainmailv1.RuleDefinition{Priority: proto.Int32(0)}, UpdateMask: mask("on_new", "priority", "stop")},
		},
		{
			name: "a repeated clear is named once",
			in:   updateRuleIn{Rule: "tidy", Clear: []string{"schedule", "schedule"}},
			want: &chainmailv1.UpdateRuleRequest{Rule: "tidy", Patch: &chainmailv1.RuleDefinition{}, UpdateMask: mask("schedule")},
		},
		{
			name: "given and cleared fields together",
			in:   updateRuleIn{Rule: "tidy", Query: new("in:inbox"), Clear: []string{"description"}},
			want: &chainmailv1.UpdateRuleRequest{Rule: "tidy", Patch: &chainmailv1.RuleDefinition{Query: "in:inbox"}, UpdateMask: mask("description", "query")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := updateRuleRequest(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, tt.want) {
				t.Errorf("request = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUpdateRuleRequestRefusals(t *testing.T) {
	t.Parallel()
	const nothing = "give at least one field to change, or name one in clear"
	tests := []struct {
		name string
		in   updateRuleIn
		msg  string
	}{
		{name: "nothing to change", in: updateRuleIn{Rule: "tidy"}, msg: nothing},
		{name: "a revision alone changes nothing", in: updateRuleIn{Rule: "tidy", ExpectedRevision: new(int64(3))}, msg: nothing},
		{name: "an empty action list is not a change", in: updateRuleIn{Rule: "tidy", Actions: []ruleActionIn{}}, msg: nothing},
		{name: "an empty clear", in: updateRuleIn{Rule: "tidy", Clear: []string{}}, msg: nothing},
		{name: "name is never cleared", in: updateRuleIn{Rule: "tidy", Clear: []string{"name"}}, msg: `clear cannot name "name"; it takes description, accounts and schedule`},
		{name: "query is never cleared", in: updateRuleIn{Rule: "tidy", Clear: []string{"query"}}, msg: `clear cannot name "query"; it takes description, accounts and schedule`},
		{name: "schedule given and cleared", in: updateRuleIn{Rule: "tidy", Schedule: new("every:6h"), Clear: []string{"schedule"}}, msg: "schedule is both given and cleared; give a value or clear it"},
		{name: "accounts given and cleared", in: updateRuleIn{Rule: "tidy", Accounts: []string{"work"}, Clear: []string{"accounts"}}, msg: "accounts is both given and cleared; give a value or clear it"},
		{name: "a bad action", in: updateRuleIn{Rule: "tidy", Actions: []ruleActionIn{{Kind: "move"}}}, msg: "action 1 (move) needs folder"},
		{name: "priority out of range", in: updateRuleIn{Rule: "tidy", Priority: new(2000000)}, msg: "priority must be -1000000 to 1000000, got 2000000"},
		{name: "max_per_run out of range", in: updateRuleIn{Rule: "tidy", MaxPerRun: new(6000)}, msg: "max_per_run must be 1 to 5000, or 0 for 500, got 6000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := updateRuleRequest(tt.in)
			if got := ruleArgMessage(t, err); got != tt.msg {
				t.Errorf("message = %q, want %q", got, tt.msg)
			}
		})
	}
}

func TestDryRunRuleRequest(t *testing.T) {
	t.Parallel()
	tidy := &ruleDefinitionIn{Name: "tidy", Query: "in:inbox", Actions: []ruleActionIn{{Kind: "archive"}}}
	tests := []struct {
		name string
		in   dryRunRuleIn
		want *chainmailv1.DryRunRuleRequest
		msg  string
	}{
		{name: "a saved rule", in: dryRunRuleIn{Rule: "stripe-receipts"}, want: &chainmailv1.DryRunRuleRequest{Rule: "stripe-receipts"}},
		{
			name: "trigger new on one account", in: dryRunRuleIn{Rule: "ru_01jc0stripe", Trigger: "new", Account: "work"},
			want: &chainmailv1.DryRunRuleRequest{Rule: "ru_01jc0stripe", Trigger: "new", Account: "work"},
		},
		{name: "trigger schedule", in: dryRunRuleIn{Rule: "tidy", Trigger: "schedule"}, want: &chainmailv1.DryRunRuleRequest{Rule: "tidy", Trigger: "schedule"}},
		{name: "trigger manual", in: dryRunRuleIn{Rule: "tidy", Trigger: "manual"}, want: &chainmailv1.DryRunRuleRequest{Rule: "tidy", Trigger: "manual"}},
		{
			name: "an unsaved rule", in: dryRunRuleIn{Definition: tidy},
			want: &chainmailv1.DryRunRuleRequest{Definition: &chainmailv1.RuleDefinition{
				Name: "tidy", Query: "in:inbox", Actions: []*chainmailv1.RuleAction{{Kind: "archive"}},
			}},
		},
		{name: "neither", in: dryRunRuleIn{}, msg: "give rule, a saved rule's ID or name, or definition, an unsaved rule"},
		{name: "both", in: dryRunRuleIn{Rule: "tidy", Definition: tidy}, msg: "give rule or definition, not both"},
		{name: "unknown trigger", in: dryRunRuleIn{Rule: "tidy", Trigger: "hourly"}, msg: `trigger must be new, schedule or manual, got "hourly"`},
		{
			name: "a bad definition",
			in:   dryRunRuleIn{Definition: &ruleDefinitionIn{Name: "t", Query: "q", Actions: []ruleActionIn{{Kind: "copy"}}}},
			msg:  "action 1 (copy) needs folder",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := dryRunRuleRequest(tt.in)
			if tt.msg != "" {
				if m := ruleArgMessage(t, err); m != tt.msg {
					t.Errorf("message = %q, want %q", m, tt.msg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, tt.want) {
				t.Errorf("request = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunRuleRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   runRuleIn
		want *chainmailv1.RunRuleRequest
		msg  string
	}{
		{name: "defaults", in: runRuleIn{Rule: "tidy"}, want: &chainmailv1.RunRuleRequest{Rule: "tidy"}},
		{name: "max 1 on one account", in: runRuleIn{Rule: "tidy", Max: 1, Account: "work"}, want: &chainmailv1.RunRuleRequest{Rule: "tidy", Max: 1, Account: "work"}},
		{name: "max 5000", in: runRuleIn{Rule: "tidy", Max: 5000}, want: &chainmailv1.RunRuleRequest{Rule: "tidy", Max: 5000}},
		{name: "max below", in: runRuleIn{Rule: "tidy", Max: -1}, msg: "max must be 1 to 5000, or 0 for the rule's max_per_run, got -1"},
		{name: "max above", in: runRuleIn{Rule: "tidy", Max: 5001}, msg: "max must be 1 to 5000, or 0 for the rule's max_per_run, got 5001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := runRuleRequest(tt.in)
			if tt.msg != "" {
				if m := ruleArgMessage(t, err); m != tt.msg {
					t.Errorf("message = %q, want %q", m, tt.msg)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(got, tt.want) {
				t.Errorf("request = %v, want %v", got, tt.want)
			}
		})
	}
}
