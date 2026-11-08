package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/render"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/watch"
)

// TestEventToProtoRuleAndDueKinds checks that a rule event carries its
// change and a due event its item, both with no account and no message.
func TestEventToProtoRuleAndDueKinds(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   watch.Event
		want *chainmailv1.WatchEvent
	}{
		{
			name: "rule",
			in: watch.Event{ID: 7, At: at, Kind: watch.KindRule, Rule: &watch.RuleChange{
				RuleID: "ru_a", Name: "tidy", Change: "disabled", Revision: 4, Actor: "1000/rule", Reason: "all_failed",
			}},
			want: &chainmailv1.WatchEvent{Id: 7, At: timestamppb.New(at), Kind: "rule", Rule: &chainmailv1.RuleChange{
				RuleId: "ru_a", Name: "tidy", Change: "disabled", Revision: 4, Actor: "1000/rule", Reason: "all_failed",
			}},
		},
		{
			name: "item_overdue",
			in: watch.Event{ID: 8, At: at, Kind: watch.KindItemOverdue, Due: &watch.ItemDue{
				ItemID: "it_a", Type: "task", DueDate: "2026-11-06", State: "in-progress", ParentID: "it_p",
			}},
			want: &chainmailv1.WatchEvent{Id: 8, At: timestamppb.New(at), Kind: "item_overdue", Due: &chainmailv1.WorkItemDue{
				ItemId: "it_a", Type: "task", DueDate: "2026-11-06", State: "in-progress", ParentId: "it_p",
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := eventToProto(tt.in); !proto.Equal(got, tt.want) {
				t.Errorf("eventToProto = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestKindNamesCoverEveryKind checks that every kind a reader may name is
// in kindNames under its own name, and that unreadable, which only the
// daemon's own readers see, is not.
func TestKindNamesCoverEveryKind(t *testing.T) {
	t.Parallel()
	kinds := []watch.Kind{
		watch.KindNew, watch.KindFlags, watch.KindExpunged, watch.KindStatus, watch.KindGap,
		watch.KindItem, watch.KindItemMail, watch.KindRule, watch.KindItemDueSoon, watch.KindItemDue, watch.KindItemOverdue,
		watch.KindInvite, watch.KindEventChanged, watch.KindEventCancelled, watch.KindInviteReply,
	}
	for _, k := range kinds {
		if got, ok := kindNames[string(k)]; !ok || got != k {
			t.Errorf("kindNames[%q] = %q, %v; want %q", k, got, ok, k)
		}
	}
	if _, ok := kindNames[string(watch.KindUnreadable)]; ok {
		t.Error("kindNames names unreadable, which is internal")
	}
	if len(kindNames) != len(kinds) {
		t.Errorf("kindNames has %d kinds, want %d", len(kindNames), len(kinds))
	}
}

// TestDefinitionOf checks the conversion of a client's definition: every
// field, the defaults for unset priority, max_per_run and enabled, a nil
// definition, and the two refusals it makes itself.
func TestDefinitionOf(t *testing.T) {
	t.Parallel()
	named := rules.Defaults()
	named.Name, named.Query = "plain", "in:inbox"
	zeroPriority := named
	zeroPriority.Priority = 0
	tests := []struct {
		name    string
		in      *chainmailv1.RuleDefinition
		want    rules.Definition
		wantErr bool
	}{
		{name: "nil is the defaults", in: nil, want: rules.Defaults()},
		{name: "unset priority, max_per_run and enabled", in: &chainmailv1.RuleDefinition{Name: "plain", Query: "in:inbox"}, want: named},
		{name: "explicit priority 0 is kept", in: &chainmailv1.RuleDefinition{Name: "plain", Query: "in:inbox", Priority: proto.Int32(0)}, want: zeroPriority},
		{
			name: "every field",
			in: &chainmailv1.RuleDefinition{
				Name: "full", Description: "d", Accounts: []string{"work", "host", "work"}, Query: "in:inbox",
				OnNew: true, Schedule: "at:07:00", Priority: proto.Int32(-5), Stop: true, MaxPerRun: 20, Enabled: proto.Bool(false),
				Actions: []*chainmailv1.RuleAction{
					{Kind: "copy", Folder: "Kept"},
					{Kind: "link_item", ItemId: "it_a", Anchor: true},
					{Kind: "forward", To: []string{"a@example.com"}, Note: "fyi"},
				},
			},
			want: rules.Definition{
				Name: "full", Description: "d", Accounts: []string{"host", "work"}, Query: "in:inbox",
				OnNew: true, Schedule: rules.Schedule{Kind: rules.ScheduleAt, At: rules.ClockTime{Hour: 7}},
				Priority: -5, Stop: true, MaxPerRun: 20, Enabled: false,
				Actions: []rules.Action{
					{Kind: rules.ActCopy, Folder: "Kept"},
					{Kind: rules.ActLinkItem, ItemID: "it_a", Anchor: true},
					{Kind: rules.ActForward, To: []string{"a@example.com"}, Note: "fyi"},
				},
			},
		},
		{name: "unknown action kind", in: &chainmailv1.RuleDefinition{Actions: []*chainmailv1.RuleAction{{Kind: "explode"}}}, wantErr: true},
		{name: "bad schedule", in: &chainmailv1.RuleDefinition{Schedule: "hourly"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := definitionOf(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("definitionOf = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if want := tt.want.Normalize(); !reflect.DeepEqual(got, want) {
				t.Errorf("definitionOf = %+v, want %+v", got, want)
			}
		})
	}
	if _, err := definitionOf(&chainmailv1.RuleDefinition{Actions: []*chainmailv1.RuleAction{{Kind: "explode"}}}); !errors.Is(err, errActionKind) {
		t.Errorf("unknown kind: %v, want errActionKind", err)
	}
}

// TestPatchOf checks that a patch sets exactly the fields the mask names,
// with an unset priority meaning the default, and keeps the expected
// revision.
func TestPatchOf(t *testing.T) {
	t.Parallel()
	m := &chainmailv1.RuleDefinition{Name: "n", Stop: true, MaxPerRun: 7, Actions: []*chainmailv1.RuleAction{{Kind: "trash"}}}
	expected := int64(3)
	p, err := patchOf(m, []string{"name", "priority", "stop", "actions", "max_per_run", "enabled"}, &expected)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name == nil || *p.Name != "n" || p.Priority == nil || *p.Priority != rules.DefaultPriority ||
		p.Stop == nil || !*p.Stop || p.MaxPerRun == nil || *p.MaxPerRun != 7 || p.Enabled == nil || *p.Enabled ||
		p.Actions == nil || len(*p.Actions) != 1 || (*p.Actions)[0].Kind != rules.ActTrash || p.ExpectedRevision != &expected {
		t.Errorf("patch = %+v", p)
	}
	if p.Description != nil || p.Query != nil || p.Accounts != nil || p.OnNew != nil || p.Schedule != nil {
		t.Errorf("patch = %+v, want the unnamed fields unset", p)
	}
	if _, err := patchOf(&chainmailv1.RuleDefinition{Schedule: "hourly"}, []string{"schedule"}, nil); err == nil {
		t.Error("a bad schedule in a named field passed")
	}
	if p, err := patchOf(nil, []string{"schedule"}, nil); err != nil || p.Schedule == nil || p.Schedule.Kind != rules.ScheduleNone {
		t.Errorf("nil patch naming schedule = %+v, %v; want the schedule cleared", p, err)
	}
}

// TestRuleMaskFields checks the mask: empty and unknown paths refused,
// repeats dropped in mask order.
func TestRuleMaskFields(t *testing.T) {
	t.Parallel()
	if _, target, err := ruleMaskFields(nil); !errors.Is(err, errRuleMask) || target != "fields=" {
		t.Errorf("empty mask: %q, %v", target, err)
	}
	if _, target, err := ruleMaskFields([]string{"name", "rule.query"}); !errors.Is(err, errRulePath) || target != "fields="+invalidTarget {
		t.Errorf("unknown path: %q, %v", target, err)
	}
	fields, target, err := ruleMaskFields([]string{"query", "name", "query"})
	if err != nil || !reflect.DeepEqual(fields, []string{"query", "name"}) || target != "fields=query,name" {
		t.Errorf("mask = %v, %q, %v", fields, target, err)
	}
}

// TestRuleProto checks that the description is wrapped as a block, the
// query on one line, zero times stay unset and actions round trip.
func TestRuleProto(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	desc := "Line one\n\tLine two\x07"
	r := rules.Rule{
		ID: "ru_a",
		Definition: rules.Definition{
			Name: "tidy", Description: desc, Accounts: []string{"host"}, Query: "subject:\x07old",
			Schedule: rules.Schedule{Kind: rules.ScheduleEvery, Every: 7 * 24 * time.Hour}, Priority: 3, MaxPerRun: 9, Enabled: true,
			Actions: []rules.Action{{Kind: rules.ActLabel, Label: "L"}, {Kind: rules.ActForward, To: []string{"a@example.com"}, Note: "n"}},
		},
		DisabledReason: "", Revision: 2, CreatedAt: at, UpdatedAt: at.Add(time.Hour),
		CreatedBy: "1000/cli", UpdatedBy: "1000/mcp", NextRunAt: at.Add(2 * time.Hour), FailedRuns: 1,
	}
	want := &chainmailv1.Rule{
		Id: "ru_a", Name: "tidy", Description: &chainmailv1.Untrusted{Untrusted: render.CleanText(desc)},
		Accounts: []string{"host"}, Query: untrusted("subject:\x07old"), Schedule: "every:7d", Priority: 3, MaxPerRun: 9, Enabled: true,
		Actions:  []*chainmailv1.RuleAction{{Kind: "label", Label: "L"}, {Kind: "forward", To: []string{"a@example.com"}, Note: "n"}},
		Revision: 2, CreatedAt: timestamppb.New(at), UpdatedAt: timestamppb.New(at.Add(time.Hour)),
		CreatedBy: "1000/cli", UpdatedBy: "1000/mcp", NextRunAt: timestamppb.New(at.Add(2 * time.Hour)), FailedRuns: 1,
	}
	if got := ruleProto(r); !proto.Equal(got, want) {
		t.Errorf("ruleProto = %v, want %v", got, want)
	}
	run := rules.Run{
		ID: 5, RuleID: "ru_a", Account: "host", Trigger: rules.TriggerManual, StartedAt: at, FinishedAt: at.Add(time.Second),
		Matched: 4, Applied: 2, Held: 1, Skipped: 1, Failed: 0, Outcome: rules.OutcomeCapped, Reason: "", Hint: "",
	}
	wantRun := &chainmailv1.RuleRun{
		Id: 5, RuleId: "ru_a", Account: "host", Trigger: "manual", StartedAt: timestamppb.New(at), FinishedAt: timestamppb.New(at.Add(time.Second)),
		Matched: 4, Applied: 2, Held: 1, Skipped: 1, Outcome: "capped",
	}
	if got := ruleRunProto(run); !proto.Equal(got, wantRun) {
		t.Errorf("ruleRunProto = %v, want %v", got, wantRun)
	}
}

// TestRuleErrorMapping checks that the store's refusals pass with their
// reason and hint, and anything else reads as fixed text.
func TestRuleErrorMapping(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	tests := []struct {
		name   string
		err    error
		code   connect.Code
		reason string
		msg    string
		hint   string
	}{
		{name: "invalid", err: &rules.InvalidError{Msg: "m", Hint: "h"}, code: connect.CodeInvalidArgument, reason: api.ReasonInvalidArgument, hint: "h"},
		{name: "not found", err: fmt.Errorf("%w: ru_x", rules.ErrNotFound), code: connect.CodeNotFound, reason: api.ReasonNotFound, hint: ruleListHint},
		{name: "store failure", err: errors.New("disk I/O error quoting ZQX"), code: connect.CodeInternal, reason: api.ReasonInternal, msg: "the rule store failed"},
		{name: "deadline", err: context.DeadlineExceeded, code: connect.CodeDeadlineExceeded, reason: api.ReasonTimeout, msg: "the rule store failed"},
	}
	for _, tt := range tests {
		cerr := toConnectError(s.rulesErr(tt.err))
		if cerr.Code() != tt.code || api.Detail(cerr).GetReason() != tt.reason || api.Detail(cerr).GetHint() != tt.hint {
			t.Errorf("%s: %v (%s, hint %q), want %v %s hint %q", tt.name, cerr, api.Detail(cerr).GetReason(), api.Detail(cerr).GetHint(), tt.code, tt.reason, tt.hint)
		}
		if tt.msg != "" && cerr.Message() != tt.msg {
			t.Errorf("%s: message %q, want %q", tt.name, cerr.Message(), tt.msg)
		}
	}
	if s.rulesErr(nil) != nil {
		t.Error("rulesErr(nil) is not nil")
	}
	for _, ref := range []string{"", "Bad Ref!", "ru_", strings.Repeat("a", 64)} {
		if target, err := ruleTarget(ref); !errors.Is(err, errRuleRef) || target != "rule="+invalidTarget {
			t.Errorf("ruleTarget(%q) = %q, %v", ref, target, err)
		}
	}
	for _, ref := range []string{"ru_01jc0abc", "stripe-receipts"} {
		if target, err := ruleTarget(ref); err != nil || target != "rule="+ref {
			t.Errorf("ruleTarget(%q) = %q, %v", ref, target, err)
		}
	}
}
