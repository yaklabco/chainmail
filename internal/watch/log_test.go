package watch

import (
	"database/sql"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
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
