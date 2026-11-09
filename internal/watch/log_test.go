package watch

import (
	"database/sql"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
)

// countEvents counts the rows in the events table.
func countEvents(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(t.Context(), "SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// readIDs runs q and returns the IDs of the events it read, and Next.
func readIDs(t *testing.T, l *Log, q Query) ([]int64, int64) {
	t.Helper()
	page, err := l.Read(t.Context(), q)
	if err != nil {
		t.Fatalf("Read(%+v): %v", q, err)
	}
	return ids(page.Events), page.Next
}

// Every kind survives Publish and Read with each field intact.
func TestPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	snippet := "preview"
	at := time.Date(2026, 11, 2, 10, 0, 0, 123456789, time.UTC)
	in := []Event{
		{
			At: at, Kind: KindNew, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:1",
			Message: &Message{
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
			},
		},
		{At: at, Kind: KindFlags, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:1", Flags: []string{`\Seen`}, Labels: []string{"Work"}},
		{At: at, Kind: KindFlags, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:2"},
		{At: at, Kind: KindExpunged, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:3"},
		{At: at, Kind: KindStatus, Account: "a", Folder: "INBOX", State: StateAuthRequired, Detail: "run: chainmail auth login a"},
		{At: at, Kind: KindGap, Account: "a", Folder: "INBOX", Reason: GapUIDValidity},
	}
	stored := publish(t, l, in...)
	page, err := l.Read(t.Context(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != len(stored) {
		t.Fatalf("read %d events, want %d", len(page.Events), len(stored))
	}
	for i, got := range page.Events {
		if !reflect.DeepEqual(got, stored[i]) {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, got, stored[i])
		}
	}
}

// An API read skips unreadable events and still moves past them, so a
// group never stalls on one. The cache's internal read gets them with
// their ref intact.
func TestReadHidesUnreadableEventsFromTheAPI(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	stored := publish(t, l,
		Event{At: at, Kind: KindNew, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:1", Message: &Message{Subject: "hi"}},
		Event{At: at, Kind: KindUnreadable, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:2"},
	)
	if got, next := readIDs(t, l, Query{After: stored[0].ID}); len(got) != 0 || next != stored[1].ID {
		t.Errorf("API read after the new event = %v, next %d; want nothing, next %d", got, next, stored[1].ID)
	}
	page, err := l.Read(t.Context(), Query{Filter: Filter{Internal: true, Accounts: []string{"a"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Events, stored) {
		t.Errorf("internal read =\n%+v\nwant\n%+v", page.Events, stored)
	}
}

func TestTailOfEmptyLog(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	tail, err := l.Tail(t.Context())
	if err != nil || tail != 0 {
		t.Errorf("Tail = %d, %v, want 0", tail, err)
	}
}

// Append writes in the caller's transaction: nothing is visible until the
// caller commits, and a rollback keeps nothing.
func TestAppendUsesCallerTransaction(t *testing.T) {
	t.Parallel()
	l, conn, clock := newLog(t)
	clock.Set(t0.Add(time.Minute))

	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{newEvent("a", "INBOX", 1)}
	if err := l.Append(t.Context(), tx, events); err != nil {
		t.Fatal(err)
	}
	if events[0].ID != 1 || !events[0].At.Equal(t0.Add(time.Minute)) {
		t.Errorf("appended event ID %d at %v, want ID 1 at the clock", events[0].ID, events[0].At)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, conn); n != 0 {
		t.Fatalf("%d events after rollback, want 0", n)
	}

	tx, err = conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	at := t0.Add(-time.Hour)
	events = []Event{{At: at, Kind: KindExpunged, Account: "a", Folder: "INBOX", Ref: "r"}}
	if err := l.Append(t.Context(), tx, events); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	page, err := l.Read(t.Context(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || !page.Events[0].At.Equal(at) {
		t.Errorf("read %+v, want one event at the At it was given", page.Events)
	}
}

func TestAppendRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, want string
		e          Event
	}{
		{name: "trimmed gap", e: Event{Kind: KindGap, Reason: GapTrimmed, NextID: 3}, want: "never stored"},
		{name: "unknown kind", e: Event{Kind: "moved", Account: "a"}, want: `unknown event kind "moved"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, conn, _ := newLog(t)
			if err := l.Publish(t.Context(), newEvent("a", "INBOX", 1), tt.e); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Publish err = %v, want %q", err, tt.want)
			}
			if n := countEvents(t, conn); n != 0 {
				t.Errorf("%d events stored after a failed Publish, want 0", n)
			}
		})
	}
}

func TestPublishNothing(t *testing.T) {
	t.Parallel()
	l, conn, _ := newLog(t)
	if err := l.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, conn); n != 0 {
		t.Errorf("%d events, want 0", n)
	}
}

func TestReadAfterLimitNext(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	for uid := range uint32(5) {
		publish(t, l, newEvent("a", "INBOX", uid+1))
	}
	tests := []struct {
		after    int64
		limit    int
		want     []int64
		wantNext int64
	}{
		{after: 0, limit: 2, want: []int64{1, 2}, wantNext: 2},
		{after: 2, limit: 2, want: []int64{3, 4}, wantNext: 4},
		{after: 4, limit: 2, want: []int64{5}, wantNext: 5},
		{after: 5, limit: 2, want: []int64{}, wantNext: 5},
		{after: 9, limit: 2, want: []int64{}, wantNext: 9},
		{after: 0, limit: 0, want: []int64{1, 2, 3, 4, 5}, wantNext: 5},
	}
	for _, tt := range tests {
		got, next := readIDs(t, l, Query{After: tt.after, Limit: tt.limit})
		if !slices.Equal(got, tt.want) || next != tt.wantNext {
			t.Errorf("Read(after %d, limit %d) = %v next %d, want %v next %d", tt.after, tt.limit, got, next, tt.want, tt.wantNext)
		}
	}
}

// Next moves past events the filter drops, so a reader never examines them twice.
func TestReadNextAdvancesPastFilteredEvents(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	publish(t, l,
		newEvent("a", "INBOX", 1), newEvent("b", "INBOX", 2), newEvent("a", "INBOX", 3),
		newEvent("b", "INBOX", 4), newEvent("b", "INBOX", 5))
	f := Filter{Accounts: []string{"a"}}
	tests := []struct {
		after    int64
		want     []int64
		wantNext int64
	}{
		{after: 0, want: []int64{1}, wantNext: 1},
		{after: 1, want: []int64{3}, wantNext: 3},
		{after: 3, want: []int64{}, wantNext: 5},
	}
	for _, tt := range tests {
		got, next := readIDs(t, l, Query{After: tt.after, Filter: f, Limit: 1})
		if !slices.Equal(got, tt.want) || next != tt.wantNext {
			t.Errorf("Read(after %d) = %v next %d, want %v next %d", tt.after, got, next, tt.want, tt.wantNext)
		}
	}
}

func TestReadLimits(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	events := make([]Event, 150)
	for i := range events {
		events[i] = newEvent("a", "INBOX", uint32(i+1))
	}
	publish(t, l, events...)
	got, next := readIDs(t, l, Query{})
	if len(got) != 100 || next != 100 {
		t.Errorf("default limit read %d events, next %d, want 100 and 100", len(got), next)
	}
	for _, limit := range []int{-1, 1001} {
		if _, err := l.Read(t.Context(), Query{Limit: limit}); err == nil || !strings.Contains(err.Error(), "must be 1 to 1000") {
			t.Errorf("Read(limit %d) err = %v, want a limit error", limit, err)
		}
	}
	if _, err := l.Read(t.Context(), Query{After: -1}); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("Read(after -1) err = %v, want a position error", err)
	}
}

// A filter that matches nothing examines at most maxScan rows per Read, and
// Next lets the caller carry on.
func TestReadScanCap(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	events := make([]Event, maxScan+100)
	for i := range events {
		events[i] = newEvent("a", "INBOX", uint32(i+1))
	}
	publish(t, l, events...)
	f := Filter{Accounts: []string{"nobody"}}
	got, next := readIDs(t, l, Query{Filter: f})
	if len(got) != 0 || next != maxScan {
		t.Errorf("first read = %d events next %d, want 0 and %d", len(got), next, maxScan)
	}
	got, next = readIDs(t, l, Query{After: next, Filter: f})
	if len(got) != 0 || next != maxScan+100 {
		t.Errorf("second read = %d events next %d, want 0 and %d", len(got), next, maxScan+100)
	}
}

// item and item_mail events survive Publish and Read with every field
// intact. An item event has no account, folder or ref.
func TestItemPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	snippet := "see the plan below"
	at := time.Date(2026, 11, 6, 10, 0, 0, 123456789, time.UTC)
	in := []Event{
		{At: at, Kind: KindItem, Item: &ItemChange{ItemID: "it_01", Type: "task", Revision: 3, Op: "updated", Fields: []string{"title", "due_date"}}},
		{At: at, Kind: KindItem, Item: &ItemChange{ItemID: "it_02", Type: "epic", Revision: 4, Op: "deleted"}},
		{
			At: at, Kind: KindItemMail, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:9",
			ItemID: "it_01", FromSelf: true,
			Message: &Message{
				Date:          time.Date(2026, 11, 6, 9, 30, 0, 0, time.UTC),
				InternalDate:  time.Date(2026, 11, 6, 9, 31, 0, 0, time.UTC),
				From:          []mailstore.Address{{Name: "Me", Email: "me@example.com"}},
				To:            []mailstore.Address{{Email: "ann@example.com"}},
				Subject:       "re: plan",
				Flags:         []string{`\Seen`},
				Labels:        []string{`\Inbox`, "Work"},
				Size:          900,
				HasAttachment: true,
				ThreadID:      "5e1f",
				Snippet:       &snippet,
			},
		},
		{At: at, Kind: KindItemMail, Account: "a", Folder: "Sent", Ref: "cm:a:Sent:3:2", ItemID: "it_02", Message: &Message{Subject: "bare"}},
	}
	stored := publish(t, l, in...)
	page, err := l.Read(t.Context(), Query{Filter: Filter{Kinds: []Kind{KindItem, KindItemMail}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != len(stored) {
		t.Fatalf("read %d events, want %d", len(page.Events), len(stored))
	}
	for i, got := range page.Events {
		if !reflect.DeepEqual(got, stored[i]) {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, got, stored[i])
		}
	}
}

// A reader that names no kinds, and the cache's internal reader, never sees
// item kinds and still moves past them. Named kinds see them; an item
// filter sees only its items' events, plus status and gap.
func TestReadItemKinds(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	stored := publish(t, l,
		Event{At: at, Kind: KindNew, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:1", Message: &Message{Subject: "hi"}},
		Event{At: at, Kind: KindItem, Item: &ItemChange{ItemID: "it_a", Type: "task", Revision: 1, Op: "linked"}},
		Event{At: at, Kind: KindItemMail, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:1", ItemID: "it_a", Message: &Message{Subject: "hi"}},
		Event{At: at, Kind: KindItem, Item: &ItemChange{ItemID: "it_b", Type: "epic", Revision: 2, Op: "created"}},
		Event{At: at, Kind: KindItemMail, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:1", ItemID: "it_b", Message: &Message{Subject: "hi"}},
		Event{At: at, Kind: KindStatus, Account: "a", Folder: "INBOX", State: StateWatching},
	)
	last := stored[len(stored)-1].ID
	tests := []struct {
		name string
		f    Filter
		want []int // indexes into stored
	}{
		{"api reader with no kinds", Filter{}, []int{0, 5}},
		{"api reader for the account", Filter{Accounts: []string{"a"}}, []int{0, 5}},
		{"cache reader", Filter{Accounts: []string{"a"}, Internal: true}, []int{0, 5}},
		{"both item kinds named", Filter{Kinds: []Kind{KindItem, KindItemMail}}, []int{1, 2, 3, 4, 5}},
		{"item kind named", Filter{Kinds: []Kind{KindItem}}, []int{1, 3, 5}},
		{"item filter", Filter{Items: map[string]bool{"it_a": true}}, []int{1, 2, 5}},
		{"item filter and item_mail", Filter{Kinds: []Kind{KindItemMail}, Items: map[string]bool{"it_b": true}}, []int{4, 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var want []int64
			for _, i := range tt.want {
				want = append(want, stored[i].ID)
			}
			got, next := readIDs(t, l, Query{Filter: tt.f})
			if !slices.Equal(got, want) || next != last {
				t.Errorf("Read(%+v) = %v next %d, want %v next %d", tt.f, got, next, want, last)
			}
		})
	}
}

// rule and due events survive Publish and Read with every field intact.
// None has an account, folder or ref.
func TestRuleAndDuePayloadRoundTrip(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	at := time.Date(2026, 11, 7, 8, 0, 0, 123456789, time.UTC)
	in := []Event{
		{At: at, Kind: KindRule, Rule: &RuleChange{RuleID: "ru_01", Name: "receipts", Change: "created", Revision: 1, Actor: "1000/cli"}},
		{At: at, Kind: KindRule, Rule: &RuleChange{RuleID: "ru_01", Name: "receipts", Change: "disabled", Revision: 4, Actor: "1000/rule", Reason: "all_failed"}},
		{At: at, Kind: KindItemDueSoon, Due: &ItemDue{ItemID: "it_01", Type: "task", DueDate: "2026-11-09", State: "backlog", ParentID: "it_admin_work"}},
		{At: at, Kind: KindItemDue, Due: &ItemDue{ItemID: "it_02", Type: "story", DueDate: "2026-11-07", State: "in-progress"}},
		{At: at, Kind: KindItemOverdue, Due: &ItemDue{ItemID: "it_03", Type: "bug", DueDate: "2026-11-01", State: "in-review", ParentID: "it_01"}},
	}
	stored := publish(t, l, in...)
	page, err := l.Read(t.Context(), Query{Filter: Filter{Kinds: []Kind{KindRule, KindItemDueSoon, KindItemDue, KindItemOverdue}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != len(stored) {
		t.Fatalf("read %d events, want %d", len(page.Events), len(stored))
	}
	for i, got := range page.Events {
		if !reflect.DeepEqual(got, stored[i]) {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, got, stored[i])
		}
		if got.Account != "" || got.Folder != "" || got.Ref != "" {
			t.Errorf("event %d has account %q, folder %q, ref %q, want none", i, got.Account, got.Folder, got.Ref)
		}
	}
}

// A reader that names no kinds, and the cache's internal reader, never sees
// the rule and due kinds and still moves past them. Named kinds see them; an
// item filter sees its items' due events and no rule events.
func TestReadOptInKinds(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	at := time.Date(2026, 11, 7, 8, 0, 0, 0, time.UTC)
	stored := publish(t, l,
		Event{At: at, Kind: KindNew, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:1", Message: &Message{Subject: "hi"}},
		Event{At: at, Kind: KindRule, Rule: &RuleChange{RuleID: "ru_a", Name: "receipts", Change: "created", Revision: 1, Actor: "1000/cli"}},
		Event{At: at, Kind: KindItemDueSoon, Due: &ItemDue{ItemID: "it_a", Type: "task", DueDate: "2026-11-09", State: "backlog"}},
		Event{At: at, Kind: KindItemDue, Due: &ItemDue{ItemID: "it_b", Type: "epic", DueDate: "2026-11-07", State: "backlog"}},
		Event{At: at, Kind: KindItemOverdue, Due: &ItemDue{ItemID: "it_a", Type: "task", DueDate: "2026-11-01", State: "in-review"}},
		Event{At: at, Kind: KindItem, Item: &ItemChange{ItemID: "it_a", Type: "task", Revision: 2, Op: "updated"}},
		Event{At: at, Kind: KindStatus, Account: "a", Folder: "INBOX", State: StateWatching},
	)
	last := stored[len(stored)-1].ID
	dueKinds := []Kind{KindItemDueSoon, KindItemDue, KindItemOverdue}
	tests := []struct {
		name string
		f    Filter
		want []int // indexes into stored
	}{
		{"api reader with no kinds", Filter{}, []int{0, 6}},
		{"api reader for the account", Filter{Accounts: []string{"a"}}, []int{0, 6}},
		{"cache reader", Filter{Accounts: []string{"a"}, Internal: true}, []int{0, 6}},
		{"internal reader", Filter{Internal: true}, []int{0, 6}},
		{"rule named", Filter{Kinds: []Kind{KindRule}}, []int{1, 6}},
		{"due kinds named", Filter{Kinds: dueKinds}, []int{2, 3, 4, 6}},
		// The due events have no folder and the status event is a control
		// event, so In drops none of them.
		{"due kinds named with in", Filter{Kinds: dueKinds, In: "Archive"}, []int{2, 3, 4, 6}},
		{"item filter", Filter{Items: map[string]bool{"it_a": true}}, []int{2, 4, 5, 6}},
		{"item filter and the rule kind", Filter{Kinds: []Kind{KindRule}, Items: map[string]bool{"it_a": true}}, []int{6}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var want []int64
			for _, i := range tt.want {
				want = append(want, stored[i].ID)
			}
			got, next := readIDs(t, l, Query{Filter: tt.f})
			if !slices.Equal(got, want) || next != last {
				t.Errorf("Read(%+v) = %v next %d, want %v next %d", tt.f, got, next, want, last)
			}
		})
	}
}

// The calendar kinds survive Publish and Read with every field intact.
// Each carries its account and its ce_ ref, and no folder.
func TestCalendarPayloadRoundTrip(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	at := time.Date(2026, 11, 8, 9, 0, 0, 123456789, time.UTC)
	ref := "ce_0123456789abcdefghjkmnpqrs"
	in := []Event{
		{At: at, Kind: KindInvite, Account: "work", Ref: ref, Calendar: &CalendarChange{
			Ref: ref, CalendarID: "primary", Start: "2026-11-10T17:00:00Z", End: "2026-11-10T17:30:00Z", Organizer: "ann@example.com",
		}},
		{At: at, Kind: KindInvite, Account: "home", Ref: ref, Calendar: &CalendarChange{
			Ref: ref, MessageRef: "cm:home:INBOX:7:12", Start: "2026-11-10T17:00:00Z", End: "2026-11-10T17:30:00Z", Organizer: "ann@example.com",
		}},
		{At: at, Kind: KindEventChanged, Account: "work", Ref: ref + "@20261110T170000Z", Calendar: &CalendarChange{
			Ref: ref + "@20261110T170000Z", CalendarID: "primary", Start: "2026-11-10T18:00:00Z", End: "2026-11-10T18:30:00Z", Fields: []string{"start", "end"},
		}},
		{At: at, Kind: KindEventCancelled, Account: "work", Ref: ref, Calendar: &CalendarChange{Ref: ref, CalendarID: "primary"}},
		{At: at, Kind: KindInviteReply, Account: "work", Ref: ref, Calendar: &CalendarChange{
			Ref: ref, CalendarID: "primary", Attendee: "bob@example.com", PartStat: "TENTATIVE",
		}},
	}
	stored := publish(t, l, in...)
	page, err := l.Read(t.Context(), Query{Filter: Filter{Kinds: []Kind{KindInvite, KindEventChanged, KindEventCancelled, KindInviteReply}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != len(stored) {
		t.Fatalf("read %d events, want %d", len(page.Events), len(stored))
	}
	for i, got := range page.Events {
		if !reflect.DeepEqual(got, stored[i]) {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, got, stored[i])
		}
		if got.Account == "" || got.Folder != "" || got.Ref == "" {
			t.Errorf("event %d has account %q, folder %q, ref %q, want an account, no folder and a ref", i, got.Account, got.Folder, got.Ref)
		}
	}
}

// Publish refuses a calendar event with no change and writes nothing.
func TestPublishRefusesCalendarEventWithoutChange(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	for _, kind := range []Kind{KindInvite, KindEventChanged, KindEventCancelled, KindInviteReply} {
		err := l.Publish(t.Context(), Event{Kind: kind, Account: "work", Ref: "ce_0123456789abcdefghjkmnpqrs"})
		if err == nil || !strings.Contains(err.Error(), "has no calendar change") {
			t.Errorf("Publish %s without a change: err = %v, want a refusal", kind, err)
		}
	}
	if tail, err := l.Tail(t.Context()); err != nil || tail != 0 {
		t.Errorf("tail = %d, %v, want 0", tail, err)
	}
}

// A reader that names no kinds, and the cache's internal reader, never sees
// the calendar kinds and still moves past them. Named kinds see them for
// the accounts asked for; an item filter drops them.
func TestReadCalendarKinds(t *testing.T) {
	t.Parallel()
	l, _, _ := newLog(t)
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	ref := "ce_0123456789abcdefghjkmnpqrs"
	stored := publish(t, l,
		Event{At: at, Kind: KindNew, Account: "a", Folder: "INBOX", Ref: "cm:a:INBOX:7:1", Message: &Message{Subject: "hi"}},
		Event{At: at, Kind: KindInvite, Account: "a", Ref: ref, Calendar: &CalendarChange{Ref: ref}},
		Event{At: at, Kind: KindEventChanged, Account: "b", Ref: ref, Calendar: &CalendarChange{Ref: ref, Fields: []string{"start"}}},
		Event{At: at, Kind: KindEventCancelled, Account: "a", Ref: ref, Calendar: &CalendarChange{Ref: ref}},
		Event{At: at, Kind: KindInviteReply, Account: "a", Ref: ref, Calendar: &CalendarChange{Ref: ref, Attendee: "bob@example.com", PartStat: "ACCEPTED"}},
		Event{At: at, Kind: KindStatus, Account: "a", Folder: "INBOX", State: StateWatching},
	)
	last := stored[len(stored)-1].ID
	calKinds := []Kind{KindInvite, KindEventChanged, KindEventCancelled, KindInviteReply}
	tests := []struct {
		name string
		f    Filter
		want []int // indexes into stored
	}{
		{"api reader with no kinds", Filter{}, []int{0, 5}},
		{"api reader for the account", Filter{Accounts: []string{"a"}}, []int{0, 5}},
		{"cache reader", Filter{Accounts: []string{"a"}, Internal: true}, []int{0, 5}},
		{"internal reader", Filter{Internal: true}, []int{0, 5}},
		{"calendar kinds named", Filter{Kinds: calKinds}, []int{1, 2, 3, 4, 5}},
		{"calendar kinds named for one account", Filter{Kinds: calKinds, Accounts: []string{"a"}}, []int{1, 3, 4, 5}},
		{"invite named", Filter{Kinds: []Kind{KindInvite}}, []int{1, 5}},
		{"calendar kinds named with in", Filter{Kinds: calKinds, In: "Archive"}, []int{1, 2, 3, 4, 5}},
		{"item filter", Filter{Items: map[string]bool{"it_a": true}}, []int{5}},
		{"item filter and the calendar kinds", Filter{Kinds: calKinds, Items: map[string]bool{"it_a": true}}, []int{5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var want []int64
			for _, i := range tt.want {
				want = append(want, stored[i].ID)
			}
			got, next := readIDs(t, l, Query{Filter: tt.f})
			if !slices.Equal(got, want) || next != last {
				t.Errorf("Read(%+v) = %v next %d, want %v next %d", tt.f, got, next, want, last)
			}
		})
	}
}
