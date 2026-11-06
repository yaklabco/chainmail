package watch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// hookCall is one OnNew call as recordingHook saw it.
type hookCall struct {
	account string
	news    []NewMail
	// newInTx is how many new events the hook's transaction held when it
	// ran.
	newInTx int
}

// uids lists the UIDs of the call's new messages, in the order given.
func (c hookCall) uids() []uint32 {
	var uids []uint32
	for _, n := range c.news {
		uids = append(uids, n.Summary.Ref.UID)
	}
	return uids
}

// recordingHook is an OnNew hook that records each call. Through tx it
// stores a group named after the batch's first new UID, so a test can see
// the hook's writes share the batch's fate. Then it returns one flags event
// per new message, flagged with hookFlag of the new event's ID, unless told
// to fail, to return an event the log refuses, or to return nothing.
type recordingHook struct {
	calls []hookCall
	fail  error
	bad   bool
	quiet bool
}

func (h *recordingHook) onNew(ctx context.Context, tx *sql.Tx, account string, news []NewMail) ([]Event, error) {
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM events WHERE kind = 'new'").Scan(&n); err != nil {
		return nil, fmt.Errorf("counting new events: %w", err)
	}
	h.calls = append(h.calls, hookCall{account: account, news: slices.Clone(news), newInTx: n})
	at := formatTime(engineAt)
	if _, err := tx.ExecContext(ctx, "INSERT INTO event_groups (name, created_at, last_seen_at) VALUES (?, ?, ?)",
		fmt.Sprintf("hook-uid%d", news[0].Summary.Ref.UID), at, at); err != nil {
		return nil, fmt.Errorf("storing the hook's group: %w", err)
	}
	switch {
	case h.fail != nil:
		return nil, h.fail
	case h.bad:
		return []Event{{Kind: "moved", Account: account}}, nil
	case h.quiet:
		return nil, nil
	}
	out := make([]Event, 0, len(news))
	for _, m := range news {
		out = append(out, Event{Kind: KindFlags, Account: account, Folder: m.Event.Folder, Ref: m.Event.Ref, Flags: []string{hookFlag(m.Event.ID)}})
	}
	return out, nil
}

// hookFlag is the flag recordingHook sets on the flags event it returns
// for the new event with ID id.
func hookFlag(id int64) string { return fmt.Sprintf("$hook%d", id) }

// withMessageID is s with its Message-ID set to id.
func withMessageID(s mailstore.Summary, id string) mailstore.Summary {
	s.MessageID = id
	return s
}

// groupNames lists the log's groups by name.
func groupNames(t *testing.T, l *Log) []string {
	t.Helper()
	groups, err := l.Groups(t.Context())
	if err != nil {
		t.Fatalf("Groups: %v", err)
	}
	var names []string
	for _, g := range groups {
		names = append(names, g.Name)
	}
	return names
}

// OnNew runs once per batch with new mail, inside the batch's transaction
// after its events are appended: it sees the batch's new messages in UID
// order with their appended events, and its own events and writes commit
// with the batch.
func TestPassOnNewRunsInTheBatchTransaction(t *testing.T) {
	t.Parallel()
	hook := &recordingHook{}
	h := newEngineHarness(t, EngineOptions{Batch: 2, OnNew: hook.onNew})
	src := &scriptSource{results: []scriptResult{
		{delta: baselineDelta(1, 2)},
		{delta: mailstore.Delta{
			State: diffState(6, 14, true),
			New: []mailstore.Summary{
				withMessageID(diffSummary(4, 0), "four@example.com"),
				withMessageID(diffSummary(3, 0), "three@example.com"),
			},
			More: true,
		}},
		{delta: mailstore.Delta{State: diffState(6, 14, true), New: []mailstore.Summary{diffSummary(5, 0)}}},
	}}
	h.pass(t, src, PassOptions{Mode: "idle"})
	if len(hook.calls) != 0 {
		t.Fatalf("the baseline called OnNew %d times, want none", len(hook.calls))
	}
	if res, want := h.pass(t, src, PassOptions{Mode: "idle"}), (PassResult{Events: 6, CondStore: true, Batches: 2}); res != want {
		t.Errorf("Pass = %+v, want %+v: three new events and three hook events", res, want)
	}
	if len(hook.calls) != 2 {
		t.Fatalf("OnNew ran %d times, want once per batch: 2", len(hook.calls))
	}
	first, second := hook.calls[0], hook.calls[1]
	if first.account != "host" || second.account != "host" {
		t.Errorf("accounts = %q, %q; want host", first.account, second.account)
	}
	if !slices.Equal(first.uids(), []uint32{3, 4}) || !slices.Equal(second.uids(), []uint32{5}) {
		t.Errorf("UIDs = %v then %v, want [3 4] then [5]", first.uids(), second.uids())
	}
	// The batch's new events were already in the transaction: two, then
	// those two plus the second batch's one.
	if first.newInTx != 2 || second.newInTx != 3 {
		t.Errorf("new events visible to the hook = %d then %d, want 2 then 3", first.newInTx, second.newInTx)
	}
	if got := []string{first.news[0].Summary.MessageID, first.news[1].Summary.MessageID, second.news[0].Summary.MessageID}; !slices.Equal(got, []string{"three@example.com", "four@example.com", ""}) {
		t.Errorf("Message-IDs = %q, want the summaries' own", got)
	}
	events := h.events(t)
	want := []string{
		"status baselined",
		newLine(3, 0),
		newLine(4, 0),
		flagsLine(3, 0, []string{hookFlag(first.news[0].Event.ID)}, nil),
		flagsLine(4, 0, []string{hookFlag(first.news[1].Event.ID)}, nil),
		newLine(5, 0),
		flagsLine(5, 0, []string{hookFlag(second.news[0].Event.ID)}, nil),
	}
	if lines := diffLines(events); !slices.Equal(lines, want) {
		t.Fatalf("events = %q, want %q", lines, want)
	}
	// Each NewMail holds its new event as appended, ID included.
	for i, pair := range []struct {
		got  Event
		want Event
	}{
		{first.news[0].Event, events[1]},
		{first.news[1].Event, events[2]},
		{second.news[0].Event, events[5]},
	} {
		if !reflect.DeepEqual(pair.got, pair.want) {
			t.Errorf("NewMail %d event = %+v\nwant the logged %+v", i, pair.got, pair.want)
		}
	}
	for _, c := range hook.calls {
		for _, n := range c.news {
			if n.Event.Kind != KindNew || n.Event.ID <= 0 || n.Event.Ref != n.Summary.Ref.String() || !n.Event.At.Equal(engineAt) {
				t.Errorf("NewMail = %+v, want the summary's new event with an ID, stamped %v", n, engineAt)
			}
		}
	}
	if got := groupNames(t, h.log); !slices.Equal(got, []string{"hook-uid3", "hook-uid5"}) {
		t.Errorf("groups = %q, want the hook's writes from both batches", got)
	}
	if row := h.row(t); row.UIDNext != 6 {
		t.Errorf("UIDNEXT = %d, want 6", row.UIDNext)
	}
}

// A hook that returns no events adds nothing to the log or the count.
func TestPassOnNewWithoutEvents(t *testing.T) {
	t.Parallel()
	hook := &recordingHook{quiet: true}
	h := newEngineHarness(t, EngineOptions{OnNew: hook.onNew})
	src := &scriptSource{results: []scriptResult{
		{delta: baselineDelta(1, 2)},
		{delta: mailstore.Delta{State: diffState(4, 12, true), New: []mailstore.Summary{diffSummary(3, 0)}}},
	}}
	h.pass(t, src, PassOptions{Mode: "idle"})
	if res, want := h.pass(t, src, PassOptions{Mode: "idle"}), (PassResult{Events: 1, CondStore: true, Batches: 1}); res != want {
		t.Errorf("Pass = %+v, want %+v", res, want)
	}
	if lines, want := diffLines(h.events(t)), []string{"status baselined", newLine(3, 0)}; !slices.Equal(lines, want) {
		t.Errorf("events = %q, want %q", lines, want)
	}
	if len(hook.calls) != 1 {
		t.Errorf("OnNew ran %d times, want 1", len(hook.calls))
	}
}

// A hook that fails, or returns an event the log refuses, rolls the whole
// batch back: its events, the hook's writes, the checkpoint and the known
// UIDs. The next pass fetches the same range, the hook sees the same
// messages, and each is reported once.
func TestPassOnNewFailureRollsBackTheBatch(t *testing.T) {
	t.Parallel()
	errHook := errors.New("items table busy")
	tests := []struct {
		name    string
		hook    recordingHook
		wantErr error
		wantMsg string
	}{
		{name: "hook fails", hook: recordingHook{fail: errHook}, wantErr: errHook, wantMsg: "running the new mail hook"},
		{name: "hook returns a refused event", hook: recordingHook{bad: true}, wantMsg: "appending new mail hook events"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hook := &tt.hook
			h := newEngineHarness(t, EngineOptions{OnNew: hook.onNew})
			delta := mailstore.Delta{
				State:   diffState(5, 12, true),
				New:     []mailstore.Summary{diffSummary(3, 0), diffSummary(4, 0)},
				Changed: []mailstore.FlagState{{UID: 1, Flags: []string{`\Seen`}}},
			}
			src := &scriptSource{results: []scriptResult{{delta: baselineDelta(1, 2)}, {delta: delta}, {delta: delta}}}
			h.pass(t, src, PassOptions{Mode: "idle"})
			row, uids, lines, tail := h.row(t), h.uids(t), diffLines(h.events(t)), h.tail(t)

			_, err := h.engine.Pass(t.Context(), "host", "INBOX", src, PassOptions{Mode: "idle"})
			switch {
			case err == nil:
				t.Fatal("Pass succeeded, want the hook's failure")
			case tt.wantErr != nil && !errors.Is(err, tt.wantErr):
				t.Errorf("err = %v, want one matching %v", err, tt.wantErr)
			case !strings.Contains(err.Error(), tt.wantMsg):
				t.Errorf("err = %v, want it to mention %q", err, tt.wantMsg)
			}
			if got := h.row(t); !reflect.DeepEqual(got, row) {
				t.Errorf("row = %+v, want it unchanged: %+v", got, row)
			}
			if got := h.uids(t); !slices.Equal(got, uids) {
				t.Errorf("known UIDs = %v, want %v", got, uids)
			}
			if got := diffLines(h.events(t)); !slices.Equal(got, lines) {
				t.Errorf("events = %q, want %q", got, lines)
			}
			if got := h.tail(t); got != tail {
				t.Errorf("Tail = %d, want %d: the appended events must roll back", got, tail)
			}
			if got := groupNames(t, h.log); len(got) != 0 {
				t.Errorf("groups = %q, want the hook's write rolled back", got)
			}

			*hook = recordingHook{calls: hook.calls}
			res := h.pass(t, src, PassOptions{Mode: "idle"})
			if want := (PassResult{Events: 5, CondStore: true, Batches: 1}); res != want {
				t.Errorf("rerun = %+v, want %+v: flags, two new and two hook events", res, want)
			}
			if got := src.reqs[2].From.UIDNext; got != 3 {
				t.Errorf("rerun started at UIDNEXT %d, want 3", got)
			}
			if len(hook.calls) != 2 || !slices.Equal(hook.calls[1].uids(), []uint32{3, 4}) {
				t.Errorf("hook calls = %d, last with UIDs %v; want 2, the rerun with [3 4]", len(hook.calls), hook.calls[len(hook.calls)-1].uids())
			}
			if got := newUIDs(t, h.events(t)); !slices.Equal(got, []uint32{3, 4}) {
				t.Errorf("new UIDs = %v, want each of 3 and 4 once", got)
			}
			if got := groupNames(t, h.log); !slices.Equal(got, []string{"hook-uid3"}) {
				t.Errorf("groups = %q, want the rerun's write only", got)
			}
		})
	}
}

// A batch without new messages never calls OnNew: a baseline, flag
// changes, expunges, unreadable mail, or nothing at all.
func TestPassOnNewSkipsBatchesWithoutNewMail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		delta mailstore.Delta
		sweep bool
	}{
		{name: "rebased", delta: mailstore.Delta{State: diffState(4, 12, true), Rebased: true, UIDs: []uint32{1, 2, 3}}},
		{name: "flags only", delta: mailstore.Delta{State: diffState(3, 12, true), Changed: []mailstore.FlagState{{UID: 1, Flags: []string{`\Seen`}}}}},
		{name: "expunge only", delta: mailstore.Delta{State: diffState(3, 12, true), UIDs: []uint32{2}}, sweep: true},
		{name: "unreadable only", delta: mailstore.Delta{State: diffState(4, 12, true), Unreadable: []uint32{3}}},
		{name: "nothing", delta: mailstore.Delta{State: diffState(3, 10, true)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hook := &recordingHook{}
			h := newEngineHarness(t, EngineOptions{OnNew: hook.onNew})
			src := &scriptSource{results: []scriptResult{{delta: baselineDelta(1, 2)}, {delta: tt.delta}}}
			h.pass(t, src, PassOptions{Mode: "idle"})
			h.pass(t, src, PassOptions{Mode: "idle", Sweep: tt.sweep})
			if len(hook.calls) != 0 {
				t.Errorf("OnNew ran %d times, want none", len(hook.calls))
			}
			for _, e := range h.events(t) {
				if e.Kind == KindFlags && len(e.Flags) > 0 && strings.HasPrefix(e.Flags[0], "$hook") {
					t.Errorf("hook event %s in the log", e.Ref)
				}
			}
		})
	}
}

// pairNewMail pairs each new summary with the new event of the same ref,
// skipping every other kind, and refuses a batch whose new events do not
// line up with its summaries, which only a bug in diff could build.
func TestPairNewMail(t *testing.T) {
	t.Parallel()
	newEvent := func(id int64, uid uint32) Event {
		return Event{ID: id, Kind: KindNew, Account: "host", Folder: "INBOX", Ref: diffRef(uid, 0)}
	}
	flags := Event{ID: 1, Kind: KindFlags, Account: "host", Folder: "INBOX", Ref: diffRef(2, 0)}
	tests := []struct {
		name    string
		dr      diffResult
		want    []int64 // the paired event IDs, in order
		wantErr string
	}{
		{
			name: "paired in order past other kinds",
			dr: diffResult{
				Events: []Event{flags, newEvent(2, 5), newEvent(3, 6)},
				New:    []mailstore.Summary{diffSummary(5, 0), diffSummary(6, 0)},
			},
			want: []int64{2, 3},
		},
		{
			name: "fewer new events than summaries",
			dr: diffResult{
				Events: []Event{flags, newEvent(2, 5)},
				New:    []mailstore.Summary{diffSummary(5, 0), diffSummary(6, 0)},
			},
			wantErr: "sync batch has 1 new events for 2 new messages",
		},
		{
			name: "more new events than summaries",
			dr: diffResult{
				Events: []Event{newEvent(2, 5), newEvent(3, 6)},
				New:    []mailstore.Summary{diffSummary(5, 0)},
			},
			wantErr: "sync batch has 2 new events for 1 new messages",
		},
		{
			name: "events out of order",
			dr: diffResult{
				Events: []Event{newEvent(2, 6), newEvent(3, 5)},
				New:    []mailstore.Summary{diffSummary(5, 0), diffSummary(6, 0)},
			},
			wantErr: fmt.Sprintf("new event 0 is for %s, not %s", diffRef(6, 0), diffRef(5, 0)),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			news, err := pairNewMail(tt.dr)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("pairNewMail = %v, %v; want error %q", news, err, tt.wantErr)
				}
				if news != nil {
					t.Errorf("pairNewMail returned %v beside its error, want nil", news)
				}
				return
			}
			if err != nil {
				t.Fatalf("pairNewMail: %v", err)
			}
			if len(news) != len(tt.dr.New) {
				t.Fatalf("pairNewMail = %d pairs, want %d", len(news), len(tt.dr.New))
			}
			for i, n := range news {
				if n.Event.ID != tt.want[i] || n.Event.Ref != n.Summary.Ref.String() || n.Summary.Ref != tt.dr.New[i].Ref {
					t.Errorf("pair %d = event %d %s, summary %s; want event %d with summary %s",
						i, n.Event.ID, n.Event.Ref, n.Summary.Ref, tt.want[i], tt.dr.New[i].Ref)
				}
			}
		})
	}
}
