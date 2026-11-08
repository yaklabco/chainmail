package watch

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
)

func TestEncodePayload(t *testing.T) {
	t.Parallel()
	snippet := "hi there"
	tests := []struct {
		name string
		e    Event
		want string
	}{
		{
			name: "new",
			e: Event{Kind: KindNew, Message: &Message{
				Date:          time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
				InternalDate:  time.Date(2026, 11, 1, 9, 31, 0, 0, time.UTC),
				From:          []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
				Subject:       "hello",
				Flags:         []string{`\Seen`},
				Labels:        []string{`\Inbox`},
				Size:          1234,
				HasAttachment: true,
				ThreadID:      "abc",
				Snippet:       &snippet,
			}},
			want: `{"message":{"date":"2026-11-01T09:30:00Z","internal_date":"2026-11-01T09:31:00Z",` +
				`"from":[{"Name":"Ann","Email":"ann@example.com"}],"to":null,"subject":"hello","flags":["\\Seen"],` +
				`"labels":["\\Inbox"],"size":1234,"has_attachment":true,"thread_id":"abc","snippet":"hi there"}}`,
		},
		{name: "flags", e: Event{Kind: KindFlags, Flags: []string{`\Seen`}, Labels: []string{"Work"}}, want: `{"flags":["\\Seen"],"labels":["Work"]}`},
		{name: "flags empty", e: Event{Kind: KindFlags}, want: `{"flags":[],"labels":[]}`},
		{name: "status", e: Event{Kind: KindStatus, State: StateWatching, Detail: "idle"}, want: `{"state":"watching","detail":"idle"}`},
		{name: "gap", e: Event{Kind: KindGap, Reason: GapUIDValidity}, want: `{"reason":"uidvalidity","next_id":0}`},
		{name: "expunged", e: Event{Kind: KindExpunged}, want: `{}`},
		{name: "unreadable", e: Event{Kind: KindUnreadable}, want: `{}`},
		{
			name: "item",
			e:    Event{Kind: KindItem, Item: &ItemChange{ItemID: "it_01", Type: "task", Revision: 7, Op: "updated", Fields: []string{"title", "state"}}},
			want: `{"item":{"item_id":"it_01","type":"task","revision":7,"op":"updated","fields":["title","state"]}}`,
		},
		{
			name: "item without fields",
			e:    Event{Kind: KindItem, Item: &ItemChange{ItemID: "it_admin_work", Type: "initiative", Revision: 1, Op: "created"}},
			want: `{"item":{"item_id":"it_admin_work","type":"initiative","revision":1,"op":"created"}}`,
		},
		{
			name: "item_mail",
			e: Event{Kind: KindItemMail, ItemID: "it_01", FromSelf: true, Message: &Message{
				Date:     time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
				From:     []mailstore.Address{{Name: "Me", Email: "me@example.com"}},
				Subject:  "re: plan",
				ThreadID: "18c2",
			}},
			want: `{"item_id":"it_01","from_self":true,"message":{"date":"2026-11-01T09:30:00Z","internal_date":"0001-01-01T00:00:00Z",` +
				`"from":[{"Name":"Me","Email":"me@example.com"}],"to":null,"subject":"re: plan","flags":null,` +
				`"size":0,"has_attachment":false,"thread_id":"18c2"}}`,
		},
		{
			name: "item_mail from someone else",
			e:    Event{Kind: KindItemMail, ItemID: "it_01", Message: &Message{Subject: "s"}},
			want: `{"item_id":"it_01","from_self":false,"message":{"date":"0001-01-01T00:00:00Z","internal_date":"0001-01-01T00:00:00Z",` +
				`"from":null,"to":null,"subject":"s","flags":null,"size":0,"has_attachment":false}}`,
		},
		{
			name: "rule",
			e:    Event{Kind: KindRule, Rule: &RuleChange{RuleID: "ru_01", Name: "receipts", Change: "created", Revision: 1, Actor: "1000/cli"}},
			want: `{"rule":{"rule_id":"ru_01","name":"receipts","change":"created","revision":1,"actor":"1000/cli"}}`,
		},
		{
			name: "rule disabled by the runner",
			e:    Event{Kind: KindRule, Rule: &RuleChange{RuleID: "ru_01", Name: "receipts", Change: "disabled", Revision: 4, Actor: "1000/rule", Reason: "all_failed"}},
			want: `{"rule":{"rule_id":"ru_01","name":"receipts","change":"disabled","revision":4,"actor":"1000/rule","reason":"all_failed"}}`,
		},
		{
			name: "item_due_soon",
			e:    Event{Kind: KindItemDueSoon, Due: &ItemDue{ItemID: "it_01", Type: "task", DueDate: "2026-11-09", State: "backlog", ParentID: "it_admin_work"}},
			want: `{"due":{"item_id":"it_01","type":"task","due_date":"2026-11-09","state":"backlog","parent_id":"it_admin_work"}}`,
		},
		{
			name: "item_due without a parent",
			e:    Event{Kind: KindItemDue, Due: &ItemDue{ItemID: "it_02", Type: "story", DueDate: "2026-11-07", State: "in-progress"}},
			want: `{"due":{"item_id":"it_02","type":"story","due_date":"2026-11-07","state":"in-progress"}}`,
		},
		{
			name: "item_overdue",
			e:    Event{Kind: KindItemOverdue, Due: &ItemDue{ItemID: "it_03", Type: "bug", DueDate: "2026-11-01", State: "in-review"}},
			want: `{"due":{"item_id":"it_03","type":"bug","due_date":"2026-11-01","state":"in-review"}}`,
		},
		{
			name: "invite from a calendar",
			e: Event{Kind: KindInvite, Account: "work", Ref: testCalRef, Calendar: &CalendarChange{
				Ref: testCalRef, CalendarID: "primary", Start: "2026-11-10T17:00:00Z", End: "2026-11-10T17:30:00Z", Organizer: "ann@example.com",
			}},
			want: `{"calendar":{"ref":"` + testCalRef + `","calendar_id":"primary","start":"2026-11-10T17:00:00Z",` +
				`"end":"2026-11-10T17:30:00Z","organizer":"ann@example.com"}}`,
		},
		{
			name: "invite from mail",
			e: Event{Kind: KindInvite, Account: "work", Ref: testCalRef, Calendar: &CalendarChange{
				Ref: testCalRef, MessageRef: "cm:work:INBOX:7:12", Start: "2026-11-10T17:00:00Z", End: "2026-11-10T17:30:00Z", Organizer: "ann@example.com",
			}},
			want: `{"calendar":{"ref":"` + testCalRef + `","message_ref":"cm:work:INBOX:7:12","start":"2026-11-10T17:00:00Z",` +
				`"end":"2026-11-10T17:30:00Z","organizer":"ann@example.com"}}`,
		},
		{
			name: "event_changed",
			e: Event{Kind: KindEventChanged, Account: "work", Ref: testCalRef, Calendar: &CalendarChange{
				Ref: testCalRef, CalendarID: "primary", Start: "2026-11-10T18:00:00Z", End: "2026-11-10T18:30:00Z", Fields: []string{"start", "end"},
			}},
			want: `{"calendar":{"ref":"` + testCalRef + `","calendar_id":"primary","start":"2026-11-10T18:00:00Z",` +
				`"end":"2026-11-10T18:30:00Z","fields":["start","end"]}}`,
		},
		{
			name: "event_cancelled",
			e:    Event{Kind: KindEventCancelled, Account: "work", Ref: testCalRef, Calendar: &CalendarChange{Ref: testCalRef, CalendarID: "primary"}},
			want: `{"calendar":{"ref":"` + testCalRef + `","calendar_id":"primary"}}`,
		},
		{
			name: "invite_reply",
			e: Event{Kind: KindInviteReply, Account: "work", Ref: testCalRef, Calendar: &CalendarChange{
				Ref: testCalRef, CalendarID: "primary", Attendee: "bob@example.com", PartStat: "ACCEPTED",
			}},
			want: `{"calendar":{"ref":"` + testCalRef + `","calendar_id":"primary","attendee":"bob@example.com","partstat":"ACCEPTED"}}`,
		},
		{
			name: "calendar change with nothing but a ref",
			e:    Event{Kind: KindEventChanged, Account: "work", Ref: testCalRef, Calendar: &CalendarChange{Ref: testCalRef}},
			want: `{"calendar":{"ref":"` + testCalRef + `"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := encodePayload(tt.e)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("payload =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestEncodePayloadRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, want string
		e          Event
	}{
		{name: "unknown kind", e: Event{Kind: "moved"}, want: `unknown event kind "moved"`},
		{name: "new without message", e: Event{Kind: KindNew, Ref: "r"}, want: "has no message"},
		{name: "item without change", e: Event{Kind: KindItem}, want: "item event has no change"},
		{name: "item_mail without message", e: Event{Kind: KindItemMail, Ref: "r", ItemID: "it_01"}, want: "item_mail event for r has no message"},
		{name: "item_mail without item", e: Event{Kind: KindItemMail, Ref: "r", Message: &Message{}}, want: "item_mail event for r has no item"},
		{name: "rule without change", e: Event{Kind: KindRule}, want: "rule event has no change"},
		{name: "item_due_soon without due", e: Event{Kind: KindItemDueSoon}, want: "item_due_soon event has no due item"},
		{name: "item_due without due", e: Event{Kind: KindItemDue}, want: "item_due event has no due item"},
		{name: "item_overdue without due", e: Event{Kind: KindItemOverdue}, want: "item_overdue event has no due item"},
		{name: "invite without change", e: Event{Kind: KindInvite, Ref: testCalRef}, want: "invite event for " + testCalRef + " has no calendar change"},
		{name: "event_changed without change", e: Event{Kind: KindEventChanged, Ref: testCalRef}, want: "event_changed event for " + testCalRef + " has no calendar change"},
		{name: "event_cancelled without change", e: Event{Kind: KindEventCancelled, Ref: testCalRef}, want: "event_cancelled event for " + testCalRef + " has no calendar change"},
		{name: "invite_reply without change", e: Event{Kind: KindInviteReply, Ref: testCalRef}, want: "invite_reply event for " + testCalRef + " has no calendar change"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := encodePayload(tt.e); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

// decodePayload restores exactly the fields encodePayload wrote, for every kind.
func TestDecodePayloadRoundTrip(t *testing.T) {
	t.Parallel()
	snippet := "preview"
	tests := []struct {
		name string
		e    Event
	}{
		{"new with every field", Event{Kind: KindNew, Message: &Message{
			Date:          time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
			InternalDate:  time.Date(2026, 11, 1, 9, 31, 0, 0, time.UTC),
			From:          []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
			To:            []mailstore.Address{{Email: "me@example.com"}},
			Subject:       "hello",
			Flags:         []string{`\Seen`, "$Label1"},
			Labels:        []string{`\Inbox`, "Work"},
			Size:          2048,
			HasAttachment: true,
			ThreadID:      "18c2",
			Snippet:       &snippet,
		}}},
		{"new with only a subject", Event{Kind: KindNew, Message: &Message{Subject: "bare"}}},
		{"flags with flags and labels", Event{Kind: KindFlags, Flags: []string{`\Seen`}, Labels: []string{"Work"}}},
		{"flags with none", Event{Kind: KindFlags}},
		{"expunged", Event{Kind: KindExpunged}},
		{"unreadable", Event{Kind: KindUnreadable}},
		{"status with detail", Event{Kind: KindStatus, State: StateAuthRequired, Detail: "run: chainmail auth login a"}},
		{"gap", Event{Kind: KindGap, Reason: GapUIDValidity}},
		{"item moved with fields", Event{Kind: KindItem, Item: &ItemChange{ItemID: "it_01", Type: "story", Revision: 12, Op: "moved", Fields: []string{"parent_id"}}}},
		{"item linked without fields", Event{Kind: KindItem, Item: &ItemChange{ItemID: "it_02", Type: "task", Revision: 13, Op: "linked"}}},
		{"item_mail from self with a snippet", Event{Kind: KindItemMail, ItemID: "it_01", FromSelf: true, Message: &Message{
			Date:     time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
			From:     []mailstore.Address{{Name: "Me", Email: "me@example.com"}},
			Subject:  "re: plan",
			Labels:   []string{`\Inbox`},
			ThreadID: "18c2",
			Snippet:  &snippet,
		}}},
		{"item_mail with only a subject", Event{Kind: KindItemMail, ItemID: "it_02", Message: &Message{Subject: "bare"}}},
		{"rule created", Event{Kind: KindRule, Rule: &RuleChange{RuleID: "ru_01", Name: "receipts", Change: "created", Revision: 1, Actor: "1000/cli"}}},
		{"rule disabled with a reason", Event{Kind: KindRule, Rule: &RuleChange{RuleID: "ru_01", Name: "receipts", Change: "disabled", Revision: 6, Actor: "1000/rule", Reason: "repeated_failures"}}},
		{"item_due_soon with a parent", Event{Kind: KindItemDueSoon, Due: &ItemDue{ItemID: "it_01", Type: "task", DueDate: "2026-11-09", State: "backlog", ParentID: "it_00"}}},
		{"item_due", Event{Kind: KindItemDue, Due: &ItemDue{ItemID: "it_02", Type: "feature", DueDate: "2026-11-07", State: "in-progress"}}},
		{"item_overdue", Event{Kind: KindItemOverdue, Due: &ItemDue{ItemID: "it_03", Type: "support-ticket", DueDate: "2026-10-30", State: "in-review"}}},
		{"invite with every field", Event{Kind: KindInvite, Calendar: &CalendarChange{
			Ref: testCalRef, MessageRef: "cm:work:INBOX:7:12", CalendarID: "primary",
			Start: "2026-11-10T17:00:00Z", End: "2026-11-10T17:30:00Z", Organizer: "ann@example.com",
			Fields: []string{"start"}, Attendee: "me@example.com", PartStat: "NEEDS-ACTION",
		}}},
		{"event_changed with fields", Event{Kind: KindEventChanged, Calendar: &CalendarChange{
			Ref: testCalRef + "@20261110T170000Z", CalendarID: "primary", Start: "2026-11-10T18:00:00Z", Fields: []string{"start", "location"},
		}}},
		{"event_cancelled with only a ref", Event{Kind: KindEventCancelled, Calendar: &CalendarChange{Ref: testCalRef}}},
		{"invite_reply", Event{Kind: KindInviteReply, Calendar: &CalendarChange{
			Ref: testCalRef, CalendarID: "primary", Attendee: "bob@example.com", PartStat: "DECLINED",
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := tt.e
			payload, err := encodePayload(want)
			if err != nil {
				t.Fatal(err)
			}
			got := Event{Kind: want.Kind}
			if err := decodePayload(&got, payload); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("decoded %s:\n got %+v\nwant %+v", payload, got, want)
			}
		})
	}
}

func TestDecodePayloadRejects(t *testing.T) {
	t.Parallel()
	e := Event{ID: 7, Kind: KindStatus}
	if err := decodePayload(&e, "{"); err == nil || !strings.Contains(err.Error(), "decoding event 7 payload") {
		t.Errorf("bad JSON: err = %v, want a decoding error", err)
	}
	e = Event{ID: 8, Kind: "moved"}
	if err := decodePayload(&e, "{}"); err == nil || !strings.Contains(err.Error(), `unknown kind "moved"`) {
		t.Errorf("unknown kind: err = %v, want a kind error", err)
	}
	for _, kind := range []Kind{
		KindItem, KindItemMail, KindRule, KindItemDueSoon, KindItemDue, KindItemOverdue,
		KindInvite, KindEventChanged, KindEventCancelled, KindInviteReply,
	} {
		e = Event{ID: 9, Kind: kind}
		if err := decodePayload(&e, "["); err == nil || !strings.Contains(err.Error(), "decoding event 9 payload") {
			t.Errorf("bad %s JSON: err = %v, want a decoding error", kind, err)
		}
	}
}

// An item event reaches every reader that opts in, outside any untrusted
// wrapping, so ItemChange must never carry item text. A new field here is a
// deliberate decision that updates this list.
func TestItemChangeHoldsNoText(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[ItemChange]()
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"ItemID", "Type", "Revision", "Op", "Fields"}
	if !slices.Equal(got, want) {
		t.Errorf("ItemChange fields = %v, want %v", got, want)
	}
}

// Rule and due events reach every reader that opts in, outside any
// untrusted wrapping, so RuleChange and ItemDue must never carry rule text
// or item text. A new field here is a deliberate decision that updates
// these lists.
func TestRuleAndDuePayloadsHoldNoText(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeFor[RuleChange](), []string{"RuleID", "Name", "Change", "Revision", "Actor", "Reason"}},
		{reflect.TypeFor[ItemDue](), []string{"ItemID", "Type", "DueDate", "State", "ParentID"}},
	} {
		var got []string
		for i := range tt.typ.NumField() {
			got = append(got, tt.typ.Field(i).Name)
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("%s fields = %v, want %v", tt.typ.Name(), got, tt.want)
		}
	}
}

// testCalRef is a calendar event ref of the ce_ form.
const testCalRef = "ce_0123456789abcdefghjkmnpqrs"

// The calendar kinds are stored under the spec's names, which the
// migration's kind check and every client name.
func TestCalendarKindNames(t *testing.T) {
	t.Parallel()
	got := []Kind{KindInvite, KindEventChanged, KindEventCancelled, KindInviteReply}
	want := []Kind{"invite", "event_changed", "event_cancelled", "invite_reply"}
	if !slices.Equal(got, want) {
		t.Errorf("calendar kinds = %q, want %q", got, want)
	}
}

// Calendar events reach every reader that opts in, outside any untrusted
// wrapping, so CalendarChange holds refs, IDs, times, addresses, field names
// and a PARTSTAT, never event text (summary, description, location,
// organizer or attendee names, comments, calendar names). A new field here
// is a deliberate decision that updates this list.
func TestCalendarChangeHoldsNoText(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeFor[CalendarChange]()
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"Ref", "MessageRef", "CalendarID", "Start", "End", "Organizer", "Fields", "Attendee", "PartStat"}
	if !slices.Equal(got, want) {
		t.Errorf("CalendarChange fields = %v, want %v", got, want)
	}
}
