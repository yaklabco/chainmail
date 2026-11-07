package daemon

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
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
