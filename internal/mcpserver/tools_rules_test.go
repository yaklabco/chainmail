package mcpserver

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
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

// ruleAt is the time every faked rule and run carries, and ruleAtText how
// the tools show it.
var ruleAt = time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)

const ruleAtText = "2026-11-07T08:00:00Z"

// ruleActionsAnswer is a rule's actions as the daemon sends them, with note
// as the forward's note.
func ruleActionsAnswer(note string) []*chainmailv1.RuleAction {
	return []*chainmailv1.RuleAction{
		{Kind: "label", Label: "Receipts"},
		{Kind: "link_item", ItemId: "it_01jc0story", Anchor: true},
		{Kind: "forward", To: []string{"books@x.test"}, Note: note},
		{Kind: "archive"},
	}
}

// ruleActionsWant is ruleActionsAnswer as the tools shape it.
func ruleActionsWant(note string) []ruleActionOut {
	return []ruleActionOut{
		{Kind: "label", Label: "Receipts"},
		{Kind: "link_item", ItemID: "it_01jc0story", Anchor: true},
		{Kind: "forward", To: []string{"books@x.test"}, Note: serverNamePtr(note)},
		{Kind: "archive"},
	}
}

// ruleAnswer is a faked rule that chainmail switched off, with text as its
// description, its query and its forward note, and every other field set.
func ruleAnswer(text string) *chainmailv1.Rule {
	ts := timestamppb.New(ruleAt)
	return &chainmailv1.Rule{
		Id: "ru_01jc0stripe", Name: "stripe-receipts", Description: readU(text), Accounts: []string{"work"},
		Query: readU(text), OnNew: true, Schedule: "every:6h", Priority: 50, Stop: true,
		Actions: ruleActionsAnswer(text), MaxPerRun: 200, DisabledReason: "repeated_failures",
		Revision: 3, CreatedAt: ts, UpdatedAt: ts, CreatedBy: "1000/mcp", UpdatedBy: "1000/cli",
		NextRunAt: ts, LastRunAt: ts, FailedRuns: 5,
	}
}

// ruleWant is ruleAnswer(text) as the tools shape it.
func ruleWant(text string) ruleOut {
	return ruleOut{
		ID: "ru_01jc0stripe", Name: "stripe-receipts", Description: serverNamePtr(text), Accounts: []string{"work"},
		Query: serverName(text), OnNew: true, Schedule: "every:6h", Priority: 50, Stop: true,
		Actions: ruleActionsWant(text), MaxPerRun: 200, DisabledReason: "repeated_failures",
		Revision: 3, CreatedAt: ruleAtText, UpdatedAt: ruleAtText, CreatedBy: "1000/mcp", UpdatedBy: "1000/cli",
		NextRunAt: ruleAtText, LastRunAt: ruleAtText, FailedRuns: 5,
	}
}

// ruleRunsAnswer is a capped run on work and a failed run on home.
func ruleRunsAnswer() []*chainmailv1.RuleRun {
	ts := timestamppb.New(ruleAt)
	return []*chainmailv1.RuleRun{
		{
			Id: 41, RuleId: "ru_01jc0stripe", Account: "work", Trigger: "manual", StartedAt: ts, FinishedAt: ts,
			Matched: 7, Applied: 5, Held: 1, Skipped: 1, Outcome: "capped",
		},
		{
			Id: 42, RuleId: "ru_01jc0stripe", Account: "home", Trigger: "manual", StartedAt: ts, FinishedAt: ts,
			Matched: 2, Failed: 2, Outcome: "failed", Reason: "NOT_FOUND", Hint: "check that the folder exists",
		},
	}
}

// ruleRunsWant is ruleRunsAnswer as the tools shape it.
func ruleRunsWant() []ruleRunOut {
	return []ruleRunOut{
		{
			ID: 41, RuleID: "ru_01jc0stripe", Account: "work", Trigger: "manual", StartedAt: ruleAtText, FinishedAt: ruleAtText,
			Matched: 7, Applied: 5, Held: 1, Skipped: 1, Outcome: "capped",
		},
		{
			ID: 42, RuleID: "ru_01jc0stripe", Account: "home", Trigger: "manual", StartedAt: ruleAtText, FinishedAt: ruleAtText,
			Matched: 2, Failed: 2, Outcome: "failed", Reason: "NOT_FOUND", Hint: "check that the folder exists",
		},
	}
}

// getRuleAnswer is the rule with its runs, counts and one revision.
func getRuleAnswer(text string) *chainmailv1.GetRuleResponse {
	return &chainmailv1.GetRuleResponse{
		Rule: ruleAnswer(text), Runs: ruleRunsAnswer(),
		Counts: &chainmailv1.RuleCounts{Runs: 12, Matched: 40, Applied: 30, Held: 2, Failed: 3},
		Revisions: []*chainmailv1.RuleRevision{
			{Revision: 3, Change: "disabled", Rule: ruleAnswer(text), Actor: "1000/rule", At: timestamppb.New(ruleAt)},
		},
	}
}

// getRuleWant is getRuleAnswer(text) as get_rule shapes it.
func getRuleWant(text string) getRuleOut {
	return getRuleOut{
		Rule: ruleWant(text), Runs: ruleRunsWant(),
		Counts:    ruleCountsOut{Runs: 12, Matched: 40, Applied: 30, Held: 2, Failed: 3},
		Revisions: []ruleRevisionOut{{Revision: 3, Change: "disabled", Rule: ruleWant(text), Actor: "1000/rule", At: ruleAtText}},
	}
}

// listRulesAnswer is two faked rules, both with text in their rule text.
func listRulesAnswer(text string) []*chainmailv1.Rule {
	cleanup := ruleAnswer(text)
	cleanup.Id, cleanup.Name, cleanup.Priority = "ru_01jc0cleanup", "inbox-cleanup", 200
	return []*chainmailv1.Rule{ruleAnswer(text), cleanup}
}

// listRulesWant is listRulesAnswer(text) as list_rules shapes it.
func listRulesWant(text string) listRulesOut {
	cleanup := ruleWant(text)
	cleanup.ID, cleanup.Name, cleanup.Priority = "ru_01jc0cleanup", "inbox-cleanup", 200
	return listRulesOut{Rules: []ruleOut{ruleWant(text), cleanup}}
}

// dryRunAnswer is a dry run with one match on work, text as its subject
// and as the forward's note, and an account whose cache cannot answer.
func dryRunAnswer(text string) *chainmailv1.DryRunRuleResponse {
	return &chainmailv1.DryRunRuleResponse{Accounts: []*chainmailv1.DryRunAccount{
		{
			Account: "work", Matched: 120, Skipped: 3, Actions: ruleActionsAnswer(text),
			Matches: []*chainmailv1.MessageRow{{
				Ref: testRef, Account: "work", Folder: "INBOX", Date: timestamppb.New(ruleAt),
				From: []*chainmailv1.Address{{Email: readU("billing@stripe.test")}}, Subject: readU(text), ThreadId: "18c0ffee",
			}},
		},
		{Account: "home", Reason: "CACHE_NOT_READY"},
	}}
}

// dryRunWant is dryRunAnswer(text) as dry_run_rule shapes it.
func dryRunWant(text string) dryRunRuleOut {
	return dryRunRuleOut{Accounts: []dryRunAccountOut{
		{
			Account: "work", Matched: 120, Skipped: 3, Actions: ruleActionsWant(text),
			Matches: []rowOut{{
				Ref: testRef, Account: "work", Folder: serverName("INBOX"), Date: ruleAtText,
				From: []untrusted{serverName("billing@stripe.test")}, To: []untrusted{}, Subject: serverName(text),
				Flags: []string{}, ThreadID: "18c0ffee",
			}},
		},
		{Account: "home", Matches: []rowOut{}, Actions: []ruleActionOut{}, Reason: "CACHE_NOT_READY"},
	}}
}

func TestRuleFrom(t *testing.T) {
	t.Parallel()
	if got, want := ruleFrom(ruleAnswer("Stripe receipts")), ruleWant("Stripe receipts"); !reflect.DeepEqual(got, want) {
		t.Errorf("ruleFrom =\n%+v\nwant\n%+v", got, want)
	}
	// A bare rule still shows its lists, empty, and leaves out what it lacks.
	bare := ruleFrom(&chainmailv1.Rule{Id: "ru_01jc0bare", Name: "bare", Query: readU("in:inbox"), Enabled: true})
	want := ruleOut{ID: "ru_01jc0bare", Name: "bare", Accounts: []string{}, Query: serverName("in:inbox"), Actions: []ruleActionOut{}, Enabled: true}
	if !reflect.DeepEqual(bare, want) {
		t.Errorf("ruleFrom(bare) =\n%+v\nwant\n%+v", bare, want)
	}
}

func TestRulesFromAndRunsFrom(t *testing.T) {
	t.Parallel()
	if got, want := rulesFrom(listRulesAnswer("x")), listRulesWant("x").Rules; !reflect.DeepEqual(got, want) {
		t.Errorf("rulesFrom =\n%+v\nwant\n%+v", got, want)
	}
	if got := ruleRunsFrom(ruleRunsAnswer()); !reflect.DeepEqual(got, ruleRunsWant()) {
		t.Errorf("ruleRunsFrom =\n%+v\nwant\n%+v", got, ruleRunsWant())
	}
	if got := rulesFrom(nil); got == nil || len(got) != 0 {
		t.Errorf("rulesFrom(nil) = %#v, want an empty list", got)
	}
	if got := ruleRunsFrom(nil); got == nil || len(got) != 0 {
		t.Errorf("ruleRunsFrom(nil) = %#v, want an empty list", got)
	}
}

func TestGetRuleFrom(t *testing.T) {
	t.Parallel()
	if got, want := getRuleFrom(getRuleAnswer("Stripe receipts")), getRuleWant("Stripe receipts"); !reflect.DeepEqual(got, want) {
		t.Errorf("getRuleFrom =\n%+v\nwant\n%+v", got, want)
	}
	bare := getRuleFrom(&chainmailv1.GetRuleResponse{Rule: ruleAnswer("x")})
	if bare.Runs == nil || len(bare.Runs) != 0 || bare.Revisions == nil || len(bare.Revisions) != 0 || bare.Counts != (ruleCountsOut{}) {
		t.Errorf("getRuleFrom(rule only) = %+v, want empty runs and revisions and zero counts", bare)
	}
}

func TestDryRunFrom(t *testing.T) {
	t.Parallel()
	if got, want := dryRunFrom(dryRunAnswer("Your receipt")), dryRunWant("Your receipt"); !reflect.DeepEqual(got, want) {
		t.Errorf("dryRunFrom =\n%+v\nwant\n%+v", got, want)
	}
	if got := dryRunFrom(&chainmailv1.DryRunRuleResponse{}); got.Accounts == nil || len(got.Accounts) != 0 {
		t.Errorf("dryRunFrom(empty) = %+v, want an empty account list", got)
	}
}

// TestRuleSummaries pins each summary line. A summary sits outside the
// untrusted markers, so it holds only IDs, counts and fixed words.
func TestRuleSummaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, got, want string
	}{
		{name: "one rule", got: ruleSummary("created", ruleWant("x")), want: "created rule ru_01jc0stripe at revision 3"},
		{
			name: "get, switched off", got: getRuleSummary(getRuleWant("x")),
			want: "rule ru_01jc0stripe at revision 3, disabled; recent runs 2, revisions 1",
		},
		{
			name: "get, on", got: getRuleSummary(getRuleOut{Rule: ruleOut{ID: "ru_01jc0bare", Revision: 1, Enabled: true}}),
			want: "rule ru_01jc0bare at revision 1, enabled; recent runs 0, revisions 0",
		},
		{
			name: "get, deleted", got: getRuleSummary(getRuleOut{Rule: ruleOut{ID: "ru_01jc0gone", Revision: 4, Enabled: true, DeletedAt: ruleAtText}}),
			want: "rule ru_01jc0gone at revision 4, deleted; recent runs 0, revisions 0",
		},
		{
			name: "dry run with an account that cannot answer", got: dryRunSummary(dryRunWant("x")),
			want: "dry run, nothing changed: matched 120, already acted on 3, accounts 2; accounts the cache cannot answer: 1, see reason",
		},
		{
			name: "dry run, every account answered", got: dryRunSummary(dryRunRuleOut{Accounts: []dryRunAccountOut{{Account: "work", Matched: 4}}}),
			want: "dry run, nothing changed: matched 4, already acted on 0, accounts 1",
		},
		{
			name: "run with a capped account", got: runRuleSummary(runRuleOut{Runs: ruleRunsWant()}),
			want: "runs 2: matched 9, applied 5, held 1, skipped 1, failed 2; a capped run stopped at its max: run the rule again to continue",
		},
		{
			name: "run, nothing capped", got: runRuleSummary(runRuleOut{Runs: ruleRunsWant()[1:]}),
			want: "runs 1: matched 2, applied 0, held 0, skipped 0, failed 2",
		},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s: summary = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// ruleNoCall fails the test if the fake daemon received any RulesService call.
func ruleNoCall(t *testing.T, f *fakeDaemon) {
	t.Helper()
	for _, c := range f.Calls() {
		if strings.HasPrefix(c.Procedure, "/chainmail.v1.RulesService/") {
			t.Errorf("the request reached the daemon: %s %v", c.Procedure, c.Msg)
		}
	}
}

// TestRuleToolRequests checks each tool sends the request its input means,
// over the wire, with every field of the shared definition mapped.
func TestRuleToolRequests(t *testing.T) {
	t.Parallel()
	every := map[string]any{
		"name": "stripe-receipts", "description": "Stripe receipts", "accounts": []string{"work", "home"},
		"query": "from:stripe.com subject:receipt", "on_new": true, "schedule": "at:07:00", "priority": 50, "stop": true,
		"actions": []map[string]any{
			{"kind": "label", "label": "Receipts"},
			{"kind": "link_item", "item_id": "it_01jc0story", "anchor": true},
			{"kind": "forward", "to": []string{"books@x.test"}, "note": "FYI"},
			{"kind": "archive"},
		},
		"max_per_run": 200, "enabled": false,
	}
	everyDef := &chainmailv1.RuleDefinition{
		Name: "stripe-receipts", Description: "Stripe receipts", Accounts: []string{"work", "home"},
		Query: "from:stripe.com subject:receipt", OnNew: true, Schedule: "at:07:00", Priority: proto.Int32(50), Stop: true,
		Actions: []*chainmailv1.RuleAction{
			{Kind: "label", Label: "Receipts"},
			{Kind: "link_item", ItemId: "it_01jc0story", Anchor: true},
			{Kind: "forward", To: []string{"books@x.test"}, Note: "FYI"},
			{Kind: "archive"},
		},
		MaxPerRun: 200, Enabled: proto.Bool(false),
	}
	tests := []struct {
		name string
		tool string
		args map[string]any
		proc string
		want proto.Message
	}{
		{
			name: "create with every field", tool: "create_rule", args: every,
			proc: chainmailv1connect.RulesServiceCreateRuleProcedure, want: &chainmailv1.CreateRuleRequest{Rule: everyDef},
		},
		{
			name: "create with defaults", tool: "create_rule",
			args: map[string]any{"name": "tidy", "query": "in:inbox is:read older_than:30d", "schedule": "every:1d", "actions": []map[string]any{{"kind": "archive"}}},
			proc: chainmailv1connect.RulesServiceCreateRuleProcedure,
			want: &chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
				Name: "tidy", Query: "in:inbox is:read older_than:30d", Schedule: "every:1d", Actions: []*chainmailv1.RuleAction{{Kind: "archive"}},
			}},
		},
		{
			name: "update switches a rule off at a revision", tool: "update_rule",
			args: map[string]any{"rule": "stripe-receipts", "enabled": false, "expected_revision": 3},
			proc: chainmailv1connect.RulesServiceUpdateRuleProcedure,
			want: &chainmailv1.UpdateRuleRequest{
				Rule: "stripe-receipts", Patch: &chainmailv1.RuleDefinition{Enabled: proto.Bool(false)},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enabled"}}, ExpectedRevision: proto.Int64(3),
			},
		},
		{
			name: "get", tool: "get_rule", args: map[string]any{"rule": "stripe-receipts"},
			proc: chainmailv1connect.RulesServiceGetRuleProcedure, want: &chainmailv1.GetRuleRequest{Rule: "stripe-receipts"},
		},
		{
			name: "get with history", tool: "get_rule", args: map[string]any{"rule": "ru_01jc0stripe", "history": true},
			proc: chainmailv1connect.RulesServiceGetRuleProcedure, want: &chainmailv1.GetRuleRequest{Rule: "ru_01jc0stripe", History: true},
		},
		{
			name: "list everything", tool: "list_rules", args: map[string]any{},
			proc: chainmailv1connect.RulesServiceListRulesProcedure, want: &chainmailv1.ListRulesRequest{},
		},
		{
			name: "list with every filter", tool: "list_rules", args: map[string]any{"account": "work", "enabled": false, "deleted": true},
			proc: chainmailv1connect.RulesServiceListRulesProcedure,
			want: &chainmailv1.ListRulesRequest{Account: "work", Enabled: proto.Bool(false), Deleted: true},
		},
		{
			name: "delete", tool: "delete_rule", args: map[string]any{"rule": "stripe-receipts"},
			proc: chainmailv1connect.RulesServiceDeleteRuleProcedure, want: &chainmailv1.DeleteRuleRequest{Rule: "stripe-receipts"},
		},
		{
			name: "restore", tool: "restore_rule", args: map[string]any{"rule": "ru_01jc0stripe"},
			proc: chainmailv1connect.RulesServiceRestoreRuleProcedure, want: &chainmailv1.RestoreRuleRequest{Rule: "ru_01jc0stripe"},
		},
		{
			name: "dry-run a saved rule as new mail", tool: "dry_run_rule", args: map[string]any{"rule": "stripe-receipts", "trigger": "new", "account": "work"},
			proc: chainmailv1connect.RulesServiceDryRunRuleProcedure,
			want: &chainmailv1.DryRunRuleRequest{Rule: "stripe-receipts", Trigger: "new", Account: "work"},
		},
		{
			name: "dry-run an unsaved rule", tool: "dry_run_rule", args: map[string]any{"definition": every},
			proc: chainmailv1connect.RulesServiceDryRunRuleProcedure, want: &chainmailv1.DryRunRuleRequest{Definition: everyDef},
		},
		{
			name: "run with defaults", tool: "run_rule", args: map[string]any{"rule": "tidy"},
			proc: chainmailv1connect.RulesServiceRunRuleProcedure, want: &chainmailv1.RunRuleRequest{Rule: "tidy"},
		},
		{
			name: "run with every field", tool: "run_rule", args: map[string]any{"rule": "tidy", "max": 25, "account": "work"},
			proc: chainmailv1connect.RulesServiceRunRuleProcedure, want: &chainmailv1.RunRuleRequest{Rule: "tidy", Max: 25, Account: "work"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest) // unfaked RPCs answer Unimplemented; the request is still recorded
			e.call(t, tt.tool, tt.args)
			if got := e.fake.lastCall(t, tt.proc).Msg; !proto.Equal(got, tt.want) {
				t.Errorf("request = %v, want %v", got, tt.want)
			}
			if c := e.fake.lastCall(t, tt.proc); c.Client != "mcp" {
				t.Errorf("client kind = %q, want mcp", c.Client)
			}
		})
	}
}

// TestRuleToolsRefuseLocally: input the schema accepts but the tools refuse
// never reaches the daemon.
func TestRuleToolsRefuseLocally(t *testing.T) {
	t.Parallel()
	archive := []map[string]any{{"kind": "archive"}}
	tests := []struct {
		name, tool string
		args       map[string]any
		inMessage  string
	}{
		{name: "unknown action kind", tool: "create_rule", args: map[string]any{"name": "r", "query": "q", "actions": []map[string]any{{"kind": "delete"}}}, inMessage: `action 1: unknown kind "delete"`},
		{name: "stray argument", tool: "create_rule", args: map[string]any{"name": "r", "query": "q", "actions": []map[string]any{{"kind": "archive", "folder": "Old"}}}, inMessage: "action 1 (archive) takes no folder"},
		{name: "priority", tool: "create_rule", args: map[string]any{"name": "r", "query": "q", "priority": 1000001, "actions": archive}, inMessage: "priority must be -1000000 to 1000000"},
		{name: "max_per_run", tool: "create_rule", args: map[string]any{"name": "r", "query": "q", "max_per_run": 5001, "actions": archive}, inMessage: "max_per_run must be 1 to 5000"},
		{name: "nothing to update", tool: "update_rule", args: map[string]any{"rule": "r"}, inMessage: "give at least one field to change"},
		{name: "an unclearable field", tool: "update_rule", args: map[string]any{"rule": "r", "clear": []string{"query"}}, inMessage: `clear cannot name "query"`},
		{name: "a forward with no addresses", tool: "update_rule", args: map[string]any{"rule": "r", "actions": []map[string]any{{"kind": "forward"}}}, inMessage: "action 1 (forward) needs to"},
		{name: "a dry run of nothing", tool: "dry_run_rule", args: map[string]any{}, inMessage: "give rule"},
		{
			name: "a dry run of two rules", tool: "dry_run_rule",
			args:      map[string]any{"rule": "r", "definition": map[string]any{"name": "r", "query": "q", "actions": archive}},
			inMessage: "not both",
		},
		{name: "an unknown trigger", tool: "dry_run_rule", args: map[string]any{"rule": "r", "trigger": "hourly"}, inMessage: `trigger must be new, schedule or manual, got "hourly"`},
		{name: "max above", tool: "run_rule", args: map[string]any{"rule": "r", "max": 5001}, inMessage: "max must be 1 to 5000"},
		{name: "max below", tool: "run_rule", args: map[string]any{"rule": "r", "max": -1}, inMessage: "max must be 1 to 5000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest)
			body := wantError(t, e.call(t, tt.tool, tt.args), api.ReasonInvalidArgument)
			if !strings.Contains(body.Message, tt.inMessage) {
				t.Errorf("message %q lacks %q", body.Message, tt.inMessage)
			}
			ruleNoCall(t, e.fake)
		})
	}
}

// rulesAnswering fakes the eight RulesService RPCs the tools call, with
// text in every piece of rule text and in the dry run's match subject.
func rulesAnswering(text string) *fakeDaemon {
	return &fakeDaemon{
		CreateRule: func(context.Context, *chainmailv1.CreateRuleRequest) (*chainmailv1.CreateRuleResponse, error) {
			return &chainmailv1.CreateRuleResponse{Rule: ruleAnswer(text)}, nil
		},
		UpdateRule: func(context.Context, *chainmailv1.UpdateRuleRequest) (*chainmailv1.UpdateRuleResponse, error) {
			return &chainmailv1.UpdateRuleResponse{Rule: ruleAnswer(text)}, nil
		},
		DeleteRule: func(context.Context, *chainmailv1.DeleteRuleRequest) (*chainmailv1.DeleteRuleResponse, error) {
			return &chainmailv1.DeleteRuleResponse{Rule: ruleAnswer(text)}, nil
		},
		RestoreRule: func(context.Context, *chainmailv1.RestoreRuleRequest) (*chainmailv1.RestoreRuleResponse, error) {
			return &chainmailv1.RestoreRuleResponse{Rule: ruleAnswer(text)}, nil
		},
		GetRule: func(context.Context, *chainmailv1.GetRuleRequest) (*chainmailv1.GetRuleResponse, error) {
			return getRuleAnswer(text), nil
		},
		ListRules: func(context.Context, *chainmailv1.ListRulesRequest) (*chainmailv1.ListRulesResponse, error) {
			return &chainmailv1.ListRulesResponse{Rules: listRulesAnswer(text)}, nil
		},
		DryRunRule: func(context.Context, *chainmailv1.DryRunRuleRequest) (*chainmailv1.DryRunRuleResponse, error) {
			return dryRunAnswer(text), nil
		},
		RunRule: func(context.Context, *chainmailv1.RunRuleRequest) (*chainmailv1.RunRuleResponse, error) {
			return &chainmailv1.RunRuleResponse{Runs: ruleRunsAnswer()}, nil
		},
	}
}

// rulesFailing fakes the eight RulesService RPCs the tools call as failing with err.
func rulesFailing(err error) *fakeDaemon {
	return &fakeDaemon{
		CreateRule: func(context.Context, *chainmailv1.CreateRuleRequest) (*chainmailv1.CreateRuleResponse, error) {
			return nil, err
		},
		UpdateRule: func(context.Context, *chainmailv1.UpdateRuleRequest) (*chainmailv1.UpdateRuleResponse, error) {
			return nil, err
		},
		DeleteRule: func(context.Context, *chainmailv1.DeleteRuleRequest) (*chainmailv1.DeleteRuleResponse, error) {
			return nil, err
		},
		RestoreRule: func(context.Context, *chainmailv1.RestoreRuleRequest) (*chainmailv1.RestoreRuleResponse, error) {
			return nil, err
		},
		GetRule: func(context.Context, *chainmailv1.GetRuleRequest) (*chainmailv1.GetRuleResponse, error) {
			return nil, err
		},
		ListRules: func(context.Context, *chainmailv1.ListRulesRequest) (*chainmailv1.ListRulesResponse, error) {
			return nil, err
		},
		DryRunRule: func(context.Context, *chainmailv1.DryRunRuleRequest) (*chainmailv1.DryRunRuleResponse, error) {
			return nil, err
		},
		RunRule: func(context.Context, *chainmailv1.RunRuleRequest) (*chainmailv1.RunRuleResponse, error) {
			return nil, err
		},
	}
}

// ruleCalls is a valid call of every rule tool.
var ruleCalls = map[string]map[string]any{
	"create_rule":  {"name": "stripe-receipts", "query": "from:stripe.com", "actions": []map[string]any{{"kind": "archive"}}},
	"update_rule":  {"rule": "stripe-receipts", "enabled": true},
	"get_rule":     {"rule": "stripe-receipts", "history": true},
	"list_rules":   {},
	"delete_rule":  {"rule": "stripe-receipts"},
	"restore_rule": {"rule": "ru_01jc0stripe"},
	"dry_run_rule": {"rule": "stripe-receipts"},
	"run_rule":     {"rule": "stripe-receipts"},
}

func TestRuleToolOutputs(t *testing.T) {
	t.Parallel()
	const text = "Stripe receipts"
	tests := []struct {
		tool string
		want any
	}{
		{tool: "create_rule", want: oneRuleOut{Rule: ruleWant(text)}},
		{tool: "update_rule", want: oneRuleOut{Rule: ruleWant(text)}},
		{tool: "delete_rule", want: oneRuleOut{Rule: ruleWant(text)}},
		{tool: "restore_rule", want: oneRuleOut{Rule: ruleWant(text)}},
		{tool: "get_rule", want: getRuleWant(text)},
		{tool: "list_rules", want: listRulesWant(text)},
		{tool: "dry_run_rule", want: dryRunWant(text)},
		{tool: "run_rule", want: runRuleOut{Runs: ruleRunsWant()}},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, rulesAnswering(text), latest)
			res := e.call(t, tt.tool, ruleCalls[tt.tool])
			wantOK(t, res)
			got := reflect.New(reflect.TypeOf(tt.want))
			decode(t, res, got.Interface())
			if !reflect.DeepEqual(got.Elem().Interface(), tt.want) {
				t.Errorf("%s =\n%+v\nwant\n%+v", tt.tool, got.Elem().Interface(), tt.want)
			}
		})
	}
}

// TestRuleToolsHandEmptyListsAsArrays: a list with nothing in it is still
// an array, so an agent never has to tell null from empty.
func TestRuleToolsHandEmptyListsAsArrays(t *testing.T) {
	t.Parallel()
	f := &fakeDaemon{
		GetRule: func(context.Context, *chainmailv1.GetRuleRequest) (*chainmailv1.GetRuleResponse, error) {
			return &chainmailv1.GetRuleResponse{Rule: &chainmailv1.Rule{Id: "ru_01jc0bare", Name: "bare", Query: readU("in:inbox")}}, nil
		},
		ListRules: func(context.Context, *chainmailv1.ListRulesRequest) (*chainmailv1.ListRulesResponse, error) {
			return &chainmailv1.ListRulesResponse{}, nil
		},
		DryRunRule: func(context.Context, *chainmailv1.DryRunRuleRequest) (*chainmailv1.DryRunRuleResponse, error) {
			return &chainmailv1.DryRunRuleResponse{Accounts: []*chainmailv1.DryRunAccount{{Account: "work"}}}, nil
		},
		RunRule: func(context.Context, *chainmailv1.RunRuleRequest) (*chainmailv1.RunRuleResponse, error) {
			return &chainmailv1.RunRuleResponse{}, nil
		},
	}
	e := newTestEnv(t, f, latest)
	empty := func(what string, v any) {
		t.Helper()
		if list, ok := v.([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %v, want an empty array", what, v)
		}
	}
	var got map[string]any
	res := e.call(t, "get_rule", ruleCalls["get_rule"])
	wantOK(t, res)
	decode(t, res, &got)
	empty("get_rule runs", got["runs"])
	empty("get_rule revisions", got["revisions"])
	rule, _ := got["rule"].(map[string]any)
	empty("get_rule rule.accounts", rule["accounts"])
	empty("get_rule rule.actions", rule["actions"])

	got = nil
	res = e.call(t, "list_rules", ruleCalls["list_rules"])
	wantOK(t, res)
	decode(t, res, &got)
	empty("list_rules rules", got["rules"])

	var dry struct {
		Accounts []map[string]any `json:"accounts"`
	}
	res = e.call(t, "dry_run_rule", ruleCalls["dry_run_rule"])
	wantOK(t, res)
	decode(t, res, &dry)
	if len(dry.Accounts) != 1 {
		t.Fatalf("dry_run_rule accounts = %v, want one", dry.Accounts)
	}
	empty("dry_run_rule matches", dry.Accounts[0]["matches"])
	empty("dry_run_rule actions", dry.Accounts[0]["actions"])

	got = nil
	res = e.call(t, "run_rule", ruleCalls["run_rule"])
	wantOK(t, res)
	decode(t, res, &got)
	empty("run_rule runs", got["runs"])
}

// TestRuleToolSummaries pins each tool's first line. run_rule's output
// holds no rule or message text, so it answers in plain text, without
// untrusted markers.
func TestRuleToolSummaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tool, summary string
		plain         bool
	}{
		{tool: "create_rule", summary: "created rule ru_01jc0stripe at revision 3"},
		{tool: "update_rule", summary: "updated rule ru_01jc0stripe at revision 3"},
		{tool: "delete_rule", summary: "deleted rule ru_01jc0stripe at revision 3"},
		{tool: "restore_rule", summary: "restored rule ru_01jc0stripe at revision 3"},
		{tool: "get_rule", summary: "rule ru_01jc0stripe at revision 3, disabled; recent runs 2, revisions 1"},
		{tool: "list_rules", summary: "rules: 2"},
		{tool: "dry_run_rule", summary: "dry run, nothing changed: matched 120, already acted on 3, accounts 2; accounts the cache cannot answer: 1, see reason"},
		{tool: "run_rule", summary: "runs 2: matched 9, applied 5, held 1, skipped 1, failed 2; a capped run stopped at its max: run the rule again to continue", plain: true},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, rulesAnswering("Stripe receipts"), latest)
			res := e.call(t, tt.tool, ruleCalls[tt.tool])
			wantOK(t, res)
			text := textOf(res)
			if !strings.HasPrefix(text, tt.summary+"\n") {
				t.Errorf("text = %q, want the summary %q first", text, tt.summary)
			}
			if marked := strings.Contains(text, "<<<chainmail-untrusted "); marked == tt.plain {
				t.Errorf("untrusted markers present = %t, want %t: %q", marked, !tt.plain, text)
			}
			if !tt.plain && strings.Count(text, "<<<end chainmail-untrusted ") != 1 {
				t.Errorf("text is not wrapped once: %q", text)
			}
		})
	}
}

// TestRuleToolsWrapRuleText: rule text and message rows reach the client
// only as untrusted data, in the structured content and between the
// markers.
func TestRuleToolsWrapRuleText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tool string
		want int
	}{
		{tool: "create_rule", want: 3},  // description, query, forward note
		{tool: "update_rule", want: 3},  // as create_rule
		{tool: "delete_rule", want: 3},  // as create_rule
		{tool: "restore_rule", want: 3}, // as create_rule
		{tool: "get_rule", want: 6},     // the rule, and the rule of its one revision
		{tool: "list_rules", want: 6},   // two rules
		{tool: "dry_run_rule", want: 2}, // the forward note of the actions, the match's subject
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, rulesAnswering(readHostile), latest)
			res := e.call(t, tt.tool, ruleCalls[tt.tool])
			wantOK(t, res)
			if n := readWrappedOnly(t, res.StructuredContent, readHostile); n != tt.want {
				t.Errorf("%d enveloped hostile strings, want %d", n, tt.want)
			}
			readInsideMarkers(t, textOf(res), readHostile)
		})
	}
}

func TestRuleToolsPassDaemonErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                      string
		err                       error
		reason, inMessage, inHint string
	}{
		{
			name:   "an unknown rule",
			err:    api.New(connect.CodeNotFound, api.ReasonNotFound, "run: chainmail rule list", errors.New("rule not found")),
			reason: api.ReasonNotFound, inMessage: "rule not found", inHint: "chainmail rule list",
		},
		{
			name:   "a stale revision",
			err:    api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "read the rule again with get_rule", errors.New("the rule changed since it was read")),
			reason: api.ReasonInvalidArgument, inMessage: "changed since it was read", inHint: "get_rule",
		},
		{
			name:   "a cache that cannot answer",
			err:    api.New(connect.CodeFailedPrecondition, api.ReasonCacheNotReady, "the cache has not caught up; run: chainmail cache status", errors.New("the cache cannot answer the rule completely yet")),
			reason: api.ReasonCacheNotReady, inMessage: "cannot answer the rule", inHint: "chainmail cache status",
		},
		{
			name:   "a bare error gets fixed text",
			err:    errors.New("disk I/O error at /var/lib/chainmail/state.db"),
			reason: api.ReasonInternal, inMessage: "internal error", inHint: "tell James",
		},
	}
	for tool, args := range ruleCalls {
		for _, tt := range tests {
			t.Run(tool+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				e := newTestEnv(t, rulesFailing(tt.err), latest)
				res := e.call(t, tool, args)
				body := wantError(t, res, tt.reason)
				if !strings.Contains(body.Message, tt.inMessage) || !strings.Contains(body.Hint, tt.inHint) {
					t.Errorf("error = %+v, want a message with %q and a hint with %q", body, tt.inMessage, tt.inHint)
				}
				if strings.Contains(textOf(res), "/var/lib/chainmail") {
					t.Errorf("the daemon's own error text leaked: %q", textOf(res))
				}
			})
		}
	}
}

// TestRuleToolAnnotations pins each rule tool's title and hints, that a
// tool whose output holds rule text says it is untrusted, and that the
// descriptions teach what C14 asks: dry-run first, and the rules a rule
// must follow.
func TestRuleToolAnnotations(t *testing.T) {
	t.Parallel()
	type want struct {
		title     string
		h         hints
		untrusted bool
		mentions  []string
	}
	read := hints{readOnly: true, idempotent: true}
	write := hints{idempotent: true}
	table := map[string]want{
		"create_rule": {"Create a rule", hints{}, true, []string{
			"dry_run_rule", "Google accounts only", "comes last", "send policy", "every:<duration>", "at:HH:MM", "at most once", "in: term",
		}},
		"update_rule":  {"Update a rule", write, true, []string{"dry_run_rule", "expected_revision", "enabled alone", "clear"}},
		"get_rule":     {"Show a rule", read, true, []string{"last 10 runs", "history", "by ID only"}},
		"list_rules":   {"List rules", read, true, []string{"priority order", "deleted"}},
		"delete_rule":  {"Delete a rule", write, true, []string{"restore_rule"}},
		"restore_rule": {"Restore a rule", write, true, []string{"by its ID"}},
		"dry_run_rule": {"Dry-run a rule", read, true, []string{"before create_rule", "before run_rule", "newest 200", "first 50", "CACHE_NOT_READY"}},
		"run_rule":     {"Run a rule now", hints{destructive: true, openWorld: true}, false, []string{"dry_run_rule", "for real", "10 minutes", "capped", "held"}},
	}
	e := newTestEnv(t, &fakeDaemon{}, latest)
	res, err := e.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tl := range res.Tools {
		w, ok := table[tl.Name]
		if !ok {
			continue
		}
		seen[tl.Name] = true
		a := tl.Annotations
		if a == nil || a.DestructiveHint == nil || a.OpenWorldHint == nil {
			t.Errorf("%s: annotations %+v leave a hint unset", tl.Name, a)
			continue
		}
		got := hints{a.ReadOnlyHint, *a.DestructiveHint, a.IdempotentHint, *a.OpenWorldHint}
		if got != w.h || tl.Title != w.title || a.Title != w.title {
			t.Errorf("%s: title %q, hints %+v; want %q, %+v", tl.Name, tl.Title, got, w.title, w.h)
		}
		if w.untrusted != strings.Contains(tl.Description, "untrusted") {
			t.Errorf("%s: description names untrusted text = %t, want %t: %q", tl.Name, !w.untrusted, w.untrusted, tl.Description)
		}
		for _, m := range w.mentions {
			if !strings.Contains(tl.Description, m) {
				t.Errorf("%s: description lacks %q: %q", tl.Name, m, tl.Description)
			}
		}
	}
	for name := range table {
		if !seen[name] {
			t.Errorf("%s is not listed", name)
		}
	}
}

// TestRuleRunToolsAllowTenMinutes checks dry_run_rule and run_rule send a
// deadline of ruleRunTimeout, which replaces the daemon's 30 second default.
func TestRuleRunToolsAllowTenMinutes(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"dry_run_rule", "run_rule"} {
		t.Run(tool, func(t *testing.T) {
			t.Parallel()
			type seen struct {
				left time.Duration
				ok   bool
			}
			got := make(chan seen, 1)
			note := func(ctx context.Context) {
				d, ok := ctx.Deadline()
				got <- seen{left: time.Until(d), ok: ok}
			}
			f := &fakeDaemon{
				DryRunRule: func(ctx context.Context, _ *chainmailv1.DryRunRuleRequest) (*chainmailv1.DryRunRuleResponse, error) {
					note(ctx)
					return &chainmailv1.DryRunRuleResponse{}, nil
				},
				RunRule: func(ctx context.Context, _ *chainmailv1.RunRuleRequest) (*chainmailv1.RunRuleResponse, error) {
					note(ctx)
					return &chainmailv1.RunRuleResponse{}, nil
				},
			}
			e := newTestEnv(t, f, latest)
			wantOK(t, e.call(t, tool, ruleCalls[tool]))
			s := <-got
			switch {
			case !s.ok:
				t.Error("the daemon call has no deadline")
			case s.left <= ruleRunTimeout-time.Minute || s.left > ruleRunTimeout:
				t.Errorf("deadline in %v, want just under %v", s.left, ruleRunTimeout)
			}
		})
	}
}
