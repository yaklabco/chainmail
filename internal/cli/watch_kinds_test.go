package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

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
		watch.KindInvite, watch.KindEventChanged, watch.KindEventCancelled, watch.KindInviteReply,
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

// Calendar events read as one line each with refs, field names, times in
// UTC and PARTSTATs, never event text or addresses.
func TestEventLineCalendarKinds(t *testing.T) {
	t.Parallel()
	start := timestamppb.New(time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC))
	end := timestamppb.New(time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC))
	tests := []struct {
		name string
		e    *chainmailv1.WatchEvent
		want string
	}{
		{
			name: "invite from a calendar",
			e: &chainmailv1.WatchEvent{Id: 20, Kind: "invite", Account: "work", Ref: calRef, Calendar: &chainmailv1.CalendarChange{
				Ref: calRef, CalendarId: "primary", Start: start, End: end, Organizer: addr("Alice", "alice@example.com"),
			}},
			want: "10:00:00 invite    work - " + calRef + " 2026-11-10T14:00:00Z to 2026-11-10T15:00:00Z\n",
		},
		{
			name: "invite by mail",
			e: &chainmailv1.WatchEvent{Id: 21, Kind: "invite", Account: "host", Ref: calRef3, Calendar: &chainmailv1.CalendarChange{
				Ref: calRef3, MessageRef: "cm1:host:INBOX:7:42", Start: start, End: end,
			}},
			want: "10:00:00 invite    host - " + calRef3 + " 2026-11-10T14:00:00Z to 2026-11-10T15:00:00Z mail cm1:host:INBOX:7:42\n",
		},
		{
			name: "changed",
			e: &chainmailv1.WatchEvent{Id: 22, Kind: "event_changed", Account: "work", Ref: calRef, Calendar: &chainmailv1.CalendarChange{
				Ref: calRef, Fields: []string{"start", "end", "location"}, Start: start, End: end,
			}},
			want: "10:00:00 event_changed work - " + calRef + " start,end,location now 2026-11-10T14:00:00Z to 2026-11-10T15:00:00Z\n",
		},
		{
			name: "cancelled",
			e: &chainmailv1.WatchEvent{Id: 23, Kind: "event_cancelled", Account: "work", Ref: calInstRef, Calendar: &chainmailv1.CalendarChange{
				Ref: calInstRef, Start: start,
			}},
			want: "10:00:00 event_cancelled work - " + calInstRef + " 2026-11-10T14:00:00Z\n",
		},
		{
			name: "a reply",
			e: &chainmailv1.WatchEvent{Id: 24, Kind: "invite_reply", Account: "work", Ref: calRef, Calendar: &chainmailv1.CalendarChange{
				Ref: calRef, Attendee: addr("Bob", "bob@example.com"), Partstat: "ACCEPTED",
			}},
			want: "10:00:00 invite_reply work - " + calRef + " ACCEPTED\n",
		},
		{
			name: "a field name from the daemon is cleaned",
			e: &chainmailv1.WatchEvent{Id: 25, Kind: "event_changed", Account: "work", Ref: calRef, Calendar: &chainmailv1.CalendarChange{
				Ref: calRef, Fields: []string{"sta\x1b[2Jrt"},
			}},
			want: "10:00:00 event_changed work - " + calRef + " sta[2Jrt\n",
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

// --kind takes the calendar kinds, and names them when it refuses one.
func TestWatchCalendarKindsParse(t *testing.T) {
	t.Parallel()
	got, err := parseKinds([]string{"invite", "event_changed", "event_cancelled", "invite_reply"})
	if err != nil || len(got) != 4 {
		t.Fatalf("parseKinds = %v, %v", got, err)
	}
	_, err = parseKinds([]string{"invites"})
	if err == nil || !strings.Contains(err.Error(), "invite, event_changed, event_cancelled or invite_reply") {
		t.Errorf("parseKinds(invites) = %v, want the list of kinds", err)
	}
}
