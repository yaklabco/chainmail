package cli

import (
	"slices"
	"testing"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/watch"
)

// TestWatchKindsMatchTheLog keeps --kind in step with the event log: every
// kind a reader can name, in the log's order. unreadable is internal.
func TestWatchKindsMatchTheLog(t *testing.T) {
	t.Parallel()
	var want []string
	for _, k := range []watch.Kind{
		watch.KindNew, watch.KindFlags, watch.KindExpunged, watch.KindStatus, watch.KindGap, watch.KindItem,
		watch.KindItemMail, watch.KindRule, watch.KindItemDueSoon, watch.KindItemDue, watch.KindItemOverdue,
	} {
		want = append(want, string(k))
	}
	if !slices.Equal(watchKinds, want) {
		t.Errorf("watchKinds = %q, want %q", watchKinds, want)
	}
}

// Rule and due events read as one line each: the rule, the change with an
// automatic disable's reason, the revision and the actor; or the item, its
// type, due date and state.
func TestEventLineRuleAndDueKinds(t *testing.T) {
	t.Parallel()
	due := &chainmailv1.WorkItemDue{ItemId: "it_task1", Type: "task", DueDate: "2026-11-20", State: "in-progress", ParentId: "it_admin_work"}
	tests := []struct {
		name string
		e    *chainmailv1.WatchEvent
		want string
	}{
		{
			name: "rule updated",
			e:    &chainmailv1.WatchEvent{Id: 10, Kind: "rule", Rule: &chainmailv1.RuleChange{RuleId: ruleID, Name: "receipts", Change: "updated", Revision: 3, Actor: "1000/cli"}},
			want: "10:00:00 rule      - - " + ruleID + " receipts updated revision 3 by 1000/cli\n",
		},
		{
			name: "rule disabled by the runner",
			e: &chainmailv1.WatchEvent{Id: 11, Kind: "rule", Rule: &chainmailv1.RuleChange{
				RuleId: ruleID, Name: "receipts", Change: "disabled", Revision: 4, Actor: "1000/rule", Reason: "repeated_failures",
			}},
			want: "10:00:00 rule      - - " + ruleID + " receipts disabled (repeated_failures) revision 4 by 1000/rule\n",
		},
		{
			name: "due soon",
			e:    &chainmailv1.WatchEvent{Id: 12, Kind: "item_due_soon", Due: due},
			want: "10:00:00 item_due_soon - - it_task1 task due 2026-11-20 in-progress\n",
		},
		{
			name: "due",
			e:    &chainmailv1.WatchEvent{Id: 13, Kind: "item_due", Due: due},
			want: "10:00:00 item_due  - - it_task1 task due 2026-11-20 in-progress\n",
		},
		{
			name: "overdue",
			e: &chainmailv1.WatchEvent{Id: 14, Kind: "item_overdue", Due: &chainmailv1.WorkItemDue{
				ItemId: "it_task1", Type: "task", DueDate: "2026-11-20", State: "back\x1b[2Jlog",
			}},
			want: "10:00:00 item_overdue - - it_task1 task due 2026-11-20 back[2Jlog\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := eventLine("10:00:00", tt.e); got != tt.want {
				t.Errorf("eventLine = %q, want %q", got, tt.want)
			}
		})
	}
}
