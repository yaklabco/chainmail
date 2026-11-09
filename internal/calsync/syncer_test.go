package calsync_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/calstore/fake"
	"github.com/yaklabco/chainmail/internal/calsync"
	"github.com/yaklabco/chainmail/internal/watch"
)

func TestNewStatusIsSyncing(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if got := r.syncer.Status(); got.State != calsync.StateSyncing || !got.LastSync.IsZero() || got.LastError != "" {
		t.Errorf("Status = %+v, want syncing with nothing else", got)
	}
}

// TestFirstSyncStoresAndEmitsNothing checks the first cycle: calendars and
// events are stored, the account is marked ready, the status is ready, and
// no event is published even for a pending invite.
func TestFirstSyncStoresAndEmitsNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store.Add("work", invited("a@x", t0.Add(24*time.Hour), calendar.PartStatNeedsAction))
	r.store.Add("home", single("b@x", t0.Add(48*time.Hour)))
	r.sync(t)

	if evs := r.events(t); len(evs) != 0 {
		t.Errorf("first sync published %d events, want 0", len(evs))
	}
	ready, err := r.cache.Ready(t.Context(), account)
	if err != nil || !ready {
		t.Errorf("Ready = %v, %v; want true", ready, err)
	}
	cals, events, err := r.cache.Stats(t.Context(), account)
	if err != nil || cals != 2 || events != 2 {
		t.Errorf("Stats = %d, %d, %v; want 2 calendars and 2 events", cals, events, err)
	}
	st := r.syncer.Status()
	if st.State != calsync.StateReady || !st.LastSync.Equal(t0) || st.LastError != "" {
		t.Errorf("Status = %+v, want ready at t0", st)
	}
	row, err := r.cache.Calendar(t.Context(), account, "work")
	if err != nil || !row.IsDefault || row.SyncToken == "" || !row.LastSync.Equal(t0) {
		t.Errorf("work calendar = %+v, %v; want default with a token synced at t0", row, err)
	}
}

func TestInviteEvent(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.sync(t)
	start := t0.Add(24 * time.Hour)
	p := r.store.Add("work", invited("a@x", start, calendar.PartStatNeedsAction))
	r.sync(t)

	evs := r.events(t)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	e := evs[0]
	ref := calendar.EventRef(account, "work", p)
	if e.Kind != "invite" || e.Account != account || e.Ref != ref || e.Folder != "" {
		t.Errorf("event = %+v, want an invite for %s", e, ref)
	}
	c := e.Calendar
	if c == nil || c.Ref != ref || c.CalendarID != "work" || c.Start != rfc(start) || c.End != rfc(start.Add(time.Hour)) || c.Organizer != boss {
		t.Errorf("payload = %+v", c)
	}
}

// TestNoInvite lists new rows that are not invites James must answer.
func TestNoInvite(t *testing.T) {
	t.Parallel()
	future := t0.Add(24 * time.Hour)
	cancelled := invited("c@x", future, calendar.PartStatNeedsAction)
	cancelled.Status = calendar.StatusCancelled
	tests := map[string]calendar.Event{
		"accepted":  invited("a@x", future, calendar.PartStatAccepted),
		"past":      invited("p@x", t0.Add(-2*time.Hour), calendar.PartStatNeedsAction),
		"cancelled": cancelled,
		"organizer": organized("o@x", future, calendar.PartStatNeedsAction),
		"no james":  single("n@x", future),
	}
	for name, ev := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.sync(t)
			r.store.Add("work", ev)
			r.sync(t)
			if evs := r.events(t); len(evs) != 0 {
				t.Errorf("got %d events, want 0: %+v", len(evs), evs)
			}
		})
	}
}

// TestRecurringInviteOnce checks that a new series with overrides gives one
// invite, and that a series that began in the past but goes on counts.
func TestRecurringInviteOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.sync(t)
	master := invited("s@x", t0.AddDate(0, 0, -21), calendar.PartStatNeedsAction)
	master.RRule = "FREQ=WEEKLY"
	moved := master
	moved.RRule = ""
	moved.RecurrenceID = t0.AddDate(0, 0, 7)
	moved.Start = moved.RecurrenceID.Add(time.Hour)
	moved.End = moved.Start.Add(time.Hour)
	p := r.store.Add("work", master, moved)
	r.sync(t)
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Kind != "invite" || evs[0].Ref != calendar.EventRef(account, "work", p) {
		t.Errorf("events = %+v, want one invite for the series", evs)
	}
}

func TestEventChanged(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	start := t0.Add(24 * time.Hour)
	p := r.store.Add("work", invited("a@x", start, calendar.PartStatAccepted))
	r.sync(t)
	newStart := start.Add(2 * time.Hour)
	r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event {
		rows[0].Start, rows[0].End = newStart, newStart.Add(90*time.Minute)
		rows[0].Location = "Room 2"
		rows[0].Summary = secret + " v2"
		return rows
	})
	r.sync(t)
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Kind != "event_changed" {
		t.Fatalf("events = %+v, want one event_changed", evs)
	}
	c := evs[0].Calendar
	if want := []string{"start", "end", "location"}; !slices.Equal(c.Fields, want) {
		t.Errorf("fields = %v, want %v", c.Fields, want)
	}
	if c.Start != rfc(newStart) || c.End != rfc(newStart.Add(90*time.Minute)) || c.CalendarID != "work" {
		t.Errorf("payload = %+v", c)
	}
}

func TestEventChangedRecurrenceAndAttendees(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ev := organized("a@x", t0.Add(24*time.Hour), calendar.PartStatAccepted)
	p := r.store.Add("work", ev)
	r.sync(t)
	r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event {
		rows[0].RRule = "FREQ=DAILY;COUNT=3"
		rows[0].Attendees = append(rows[0].Attendees, calendar.Attendee{Person: calendar.Person{Email: "new@example.com"}, PartStat: calendar.PartStatNeedsAction})
		return rows
	})
	r.sync(t)
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Kind != "event_changed" {
		t.Fatalf("events = %+v, want one event_changed", evs)
	}
	if want := []string{"recurrence", "attendees"}; !slices.Equal(evs[0].Calendar.Fields, want) {
		t.Errorf("fields = %v, want %v", evs[0].Calendar.Fields, want)
	}
}

// TestNoEventChanged lists updates that change nothing a watcher hears.
func TestNoEventChanged(t *testing.T) {
	t.Parallel()
	start := t0.Add(24 * time.Hour)
	tests := map[string]struct {
		ev     calendar.Event
		change func(*calendar.Event)
	}{
		"summary only": {invited("a@x", start, calendar.PartStatAccepted), func(e *calendar.Event) { e.Summary = "other" }},
		"not james":    {single("b@x", start), func(e *calendar.Event) { e.Start = e.Start.Add(time.Hour) }},
		"long past": {
			invited("c@x", t0.Add(-windowPast-48*time.Hour), calendar.PartStatAccepted),
			func(e *calendar.Event) { e.Location = "elsewhere" },
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, func(o *calsync.Options) { o.WindowPast = 365 * 24 * time.Hour })
			p := r.store.Add("work", tc.ev)
			r.sync(t)
			// The window shrinks back, so the long past row is before it,
			// though Prune has not had a reason to delete it yet.
			r2 := calsync.New(calsync.Options{Account: account, Store: r.store, Cache: r.cache, Log: r.log, Self: self, Poll: poll, WindowPast: windowPast, Clock: r.clock})
			r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event {
				tc.change(&rows[0])
				return rows
			})
			if err := r2.SyncNow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if evs := r.events(t); len(evs) != 0 {
				t.Errorf("got %d events, want 0: %+v", len(evs), evs)
			}
		})
	}
}

func TestEventCancelled(t *testing.T) {
	t.Parallel()
	start := t0.Add(24 * time.Hour)
	for _, how := range []string{"status", "deleted"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			p := r.store.Add("work", invited("a@x", start, calendar.PartStatAccepted))
			r.sync(t)
			if how == "status" {
				r.store.Cancel("work", p)
			} else {
				r.store.Remove("work", p)
			}
			r.sync(t)
			evs := r.events(t)
			if len(evs) != 1 || evs[0].Kind != "event_cancelled" {
				t.Fatalf("events = %+v, want one event_cancelled", evs)
			}
			if c := evs[0].Calendar; c.Ref != calendar.EventRef(account, "work", p) || c.Start != rfc(start) || c.CalendarID != "work" {
				t.Errorf("payload = %+v", c)
			}
		})
	}
}

// TestOrganizerCancelIsQuiet checks that James cancelling his own event
// emits no event_cancelled.
func TestOrganizerCancelIsQuiet(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	p := r.store.Add("work", organized("a@x", t0.Add(24*time.Hour), calendar.PartStatAccepted))
	r.sync(t)
	r.store.Cancel("work", p)
	r.sync(t)
	if evs := r.events(t); len(evs) != 0 {
		t.Errorf("events = %+v, want none", evs)
	}
}

func TestInstanceCancelled(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	master := invited("s@x", t0.Add(24*time.Hour), calendar.PartStatAccepted)
	master.RRule = "FREQ=WEEKLY"
	p := r.store.Add("work", master)
	r.sync(t)
	rid := master.Start.AddDate(0, 0, 7)
	r.store.CancelInstance("work", p, rid)
	r.sync(t)
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Kind != "event_cancelled" {
		t.Fatalf("events = %+v, want one event_cancelled", evs)
	}
	want := calendar.InstanceRef(calendar.EventRef(account, "work", p), rid)
	if evs[0].Ref != want || evs[0].Calendar.Start != rfc(rid) {
		t.Errorf("event = %+v, want ref %s starting %s", evs[0], want, rfc(rid))
	}
}

func TestInviteReply(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	p := r.store.Add("work", organized("a@x", t0.Add(24*time.Hour), calendar.PartStatNeedsAction))
	r.sync(t)
	r.store.SetPartStat("work", p, time.Time{}, boss, calendar.PartStatDeclined)
	r.sync(t)
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Kind != "invite_reply" {
		t.Fatalf("events = %+v, want one invite_reply", evs)
	}
	if c := evs[0].Calendar; c.Attendee != boss || c.PartStat != "DECLINED" || c.Ref != calendar.EventRef(account, "work", p) {
		t.Errorf("payload = %+v", c)
	}
}

// TestInstanceReply checks a reply to one instance of James's series.
func TestInstanceReply(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	master := organized("s@x", t0.Add(24*time.Hour), calendar.PartStatAccepted)
	master.RRule = "FREQ=WEEKLY"
	p := r.store.Add("work", master)
	r.sync(t)
	rid := master.Start.AddDate(0, 0, 7)
	r.store.SetPartStat("work", p, rid, boss, calendar.PartStatTentative)
	r.sync(t)
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Kind != "invite_reply" {
		t.Fatalf("events = %+v, want one invite_reply", evs)
	}
	if c := evs[0].Calendar; c.Attendee != boss || c.PartStat != "TENTATIVE" || c.Ref != calendar.InstanceRef(calendar.EventRef(account, "work", p), rid) {
		t.Errorf("payload = %+v", c)
	}
}

// TestEventsHoldNoEventText checks every published payload for event text.
func TestEventsHoldNoEventText(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	a := r.store.Add("work", invited("a@x", t0.Add(24*time.Hour), calendar.PartStatAccepted))
	o := r.store.Add("work", organized("o@x", t0.Add(24*time.Hour), calendar.PartStatNeedsAction))
	r.sync(t)
	r.store.Add("work", invited("n@x", t0.Add(24*time.Hour), calendar.PartStatNeedsAction))
	r.store.Change("work", a, func(rows []calendar.Event) []calendar.Event {
		rows[0].Location = secret + " room"
		return rows
	})
	r.store.SetPartStat("work", o, time.Time{}, boss, calendar.PartStatAccepted)
	r.sync(t)
	evs := r.events(t)
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(evs), evs)
	}
	for _, e := range evs {
		if s := fmt.Sprintf("%+v %+v", e, *e.Calendar); strings.Contains(s, secret) {
			t.Errorf("event holds event text: %s", s)
		}
	}
}

func TestIncludeExcludeAndDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		include, exclude []string
		def              string
		want             []string
		wantDefault      string
	}{
		{name: "all, primary default", want: []string{"home", "work"}, wantDefault: "work"},
		{name: "include by name, first writable default", include: []string{"home"}, want: []string{"home"}, wantDefault: "home"},
		{name: "include by id", include: []string{"work"}, want: []string{"work"}, wantDefault: "work"},
		{name: "exclude by name", exclude: []string{"WORK"}, want: []string{"home"}, wantDefault: "home"},
		{name: "named default", def: "Home", want: []string{"home", "work"}, wantDefault: "home"},
		{name: "default by id", def: "home", want: []string{"home", "work"}, wantDefault: "home"},
		{name: "primary keyword", def: "primary", want: []string{"home", "work"}, wantDefault: "work"},
		{name: "unknown default", def: "nope", want: []string{"home", "work"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, func(o *calsync.Options) {
				o.Include, o.Exclude, o.Default = tc.include, tc.exclude, tc.def
			})
			r.store.AddCalendar(calendar.Calendar{ID: "home", Name: "home", Access: calendar.AccessWrite})
			r.sync(t)
			rows, err := r.cache.Calendars(t.Context(), account)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			def := ""
			for _, c := range rows {
				got = append(got, c.ID)
				if c.IsDefault {
					def = c.ID
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, tc.want) || def != tc.wantDefault {
				t.Errorf("calendars %v default %q, want %v default %q", got, def, tc.want, tc.wantDefault)
			}
			for _, call := range r.store.CallsTo(fake.MethodSync) {
				if !slices.Contains(tc.want, call.CalendarID) {
					t.Errorf("synced excluded calendar %s", call.CalendarID)
				}
			}
		})
	}
}

// TestDefaultFirstWritable checks that with no primary calendar, the first
// writable one is the default.
func TestDefaultFirstWritable(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store.RemoveCalendar("work")
	r.store.AddCalendar(calendar.Calendar{ID: "/cal/a/", Name: "A", Access: calendar.AccessRead})
	r.store.AddCalendar(calendar.Calendar{ID: "/cal/b/", Name: "B", Access: calendar.AccessWrite})
	r.sync(t)
	row, err := r.cache.Calendar(t.Context(), account, "/cal/b/")
	if err != nil || !row.IsDefault {
		t.Errorf("/cal/b/ = %+v, %v; want the default", row, err)
	}
}

// TestSyncTokenInvalid checks that an expired token is dropped and the
// calendar synced in full in the same cycle, and that the full sync still
// reports changes against the stored rows.
func TestSyncTokenInvalid(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	p := r.store.Add("work", invited("a@x", t0.Add(24*time.Hour), calendar.PartStatAccepted))
	r.sync(t)
	r.store.InvalidateTokens()
	r.store.Cancel("work", p)
	r.store.ResetCalls()
	r.sync(t)
	var tokens []string
	for _, c := range r.store.CallsTo(fake.MethodSync) {
		if c.CalendarID == "work" {
			tokens = append(tokens, c.Token)
		}
	}
	if len(tokens) != 2 || tokens[0] == "" || tokens[1] != "" {
		t.Errorf("work Sync tokens = %q, want the stored one then empty", tokens)
	}
	if evs := r.events(t); len(evs) != 1 || evs[0].Kind != "event_cancelled" {
		t.Errorf("events = %+v, want one event_cancelled", evs)
	}
	if st := r.syncer.Status(); st.State != calsync.StateReady {
		t.Errorf("Status = %+v, want ready", st)
	}
}

func TestAuthRequired(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store.Fail(fake.MethodListCalendars, fmt.Errorf("%w: status 403", calstore.ErrAuth))
	err := r.syncer.SyncNow(t.Context())
	if !errors.Is(err, calstore.ErrAuth) {
		t.Fatalf("SyncNow = %v, want ErrAuth", err)
	}
	st := r.syncer.Status()
	if st.State != calsync.StateAuthRequired || st.LastError == "" || !errors.Is(st.LastErr, calstore.ErrAuth) {
		t.Errorf("Status = %+v, want auth_required with an error", st)
	}
	if ready, _ := r.cache.Ready(t.Context(), account); ready {
		t.Error("the account is ready after a failed first sync")
	}
	r.store.Fail(fake.MethodListCalendars, nil)
	r.sync(t)
	if st := r.syncer.Status(); st.State != calsync.StateReady || st.LastError != "" || st.LastErr != nil {
		t.Errorf("Status after recovery = %+v, want ready", st)
	}
}

// TestCalendarErrorRecorded checks that one calendar's failure is recorded
// on it as fixed text, the others still sync, and the account is not ready.
func TestCalendarErrorRecorded(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store.Add("home", single("b@x", t0.Add(time.Hour)))
	r.store.FailNext(fake.MethodSync, errors.New(secret+": status 500 from the server"))
	err := r.syncer.SyncNow(t.Context())
	if err == nil {
		t.Fatal("SyncNow succeeded with a failing calendar")
	}
	st := r.syncer.Status()
	if st.State != calsync.StateError || strings.Contains(st.LastError, secret) || st.LastError == "" {
		t.Errorf("Status = %+v, want error with fixed text", st)
	}
	rows, err := r.cache.Calendars(t.Context(), account)
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, c := range rows {
		if c.LastError != "" {
			failed++
			if strings.Contains(c.LastError, secret) {
				t.Errorf("calendar %s last error holds server text: %q", c.ID, c.LastError)
			}
		} else if c.LastSync.IsZero() {
			t.Errorf("calendar %s was not synced", c.ID)
		}
	}
	if failed != 1 {
		t.Errorf("%d calendars carry an error, want 1", failed)
	}
	if ready, _ := r.cache.Ready(t.Context(), account); ready {
		t.Error("the account is ready after an incomplete first cycle")
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store.Add("work", single("old@x", t0.Add(-windowPast-48*time.Hour)))
	r.store.Add("work", single("new@x", t0.Add(-time.Hour)))
	r.sync(t)
	_, events, err := r.cache.Stats(t.Context(), account)
	if err != nil || events != 1 {
		t.Errorf("Stats events = %d, %v; want 1 after the prune", events, err)
	}
}

// TestRemovedCalendarDropped checks SetCalendars runs every cycle.
func TestRemovedCalendarDropped(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.sync(t)
	r.store.RemoveCalendar("home")
	r.sync(t)
	if _, err := r.cache.Calendar(t.Context(), account, "home"); !errors.Is(err, calcache.ErrNotFound) {
		t.Errorf("Calendar(home) = %v, want ErrNotFound", err)
	}
}

// TestSyncNowSerialized runs cycles from several goroutines; -race and the
// fake's call log show they never overlap.
func TestSyncNowSerialized(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store.Add("work", single("a@x", t0.Add(time.Hour)))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := r.syncer.SyncNow(t.Context()); err != nil {
				t.Errorf("SyncNow: %v", err)
			}
		})
	}
	wg.Wait()
	calls := r.store.Calls()
	// Each cycle is ListCalendars then a Sync per calendar, never interleaved.
	for i, c := range calls {
		if c.Method == fake.MethodListCalendars && i+2 < len(calls) {
			if calls[i+1].Method != fake.MethodSync || calls[i+2].Method != fake.MethodSync {
				t.Fatalf("cycles overlap: %+v", calls)
			}
		}
	}
}

func TestPokeAndNilSyncer(t *testing.T) {
	t.Parallel()
	var s *calsync.Syncer
	s.Poke() // a nil syncer, for accounts without a store, ignores a poke
	if st := s.Status(); st.State != "" {
		t.Errorf("nil Status = %+v", st)
	}
	r := newRig(t)
	r.syncer.Poke()
	r.syncer.Poke() // never blocks
}

func TestStatusErrorText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: status 503", calstore.ErrUnavailable), calsync.StateError},
		{fmt.Errorf("%w: status 401", calstore.ErrAuth), calsync.StateAuthRequired},
	}
	for _, tc := range tests {
		r := newRig(t)
		r.store.Fail(fake.MethodListCalendars, tc.err)
		_ = r.syncer.SyncNow(t.Context())
		st := r.syncer.Status()
		if st.State != tc.want || strings.Contains(st.LastError, "status") {
			t.Errorf("%v: Status = %+v, want %s with fixed text", tc.err, st, tc.want)
		}
	}
}

// TestObjectReplaceQuietForPrunedRows checks a CalDAV-style sync, where
// each changed object comes back whole: overrides the prune deleted come
// back as new rows in the past and emit nothing.
func TestObjectReplaceQuietForPrunedRows(t *testing.T) {
	t.Parallel()
	r, p, _ := objectReplaceSeries(t)
	r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event {
		return rows // sent again whole: the pruned override comes back
	})
	r.sync(t)
	if evs := r.events(t); len(evs) != 0 {
		t.Errorf("events = %+v, want none", evs)
	}
}

// objectReplaceSeries stores, on a CalDAV-style store, a weekly series
// James is invited to with an old moved override (pruned by the first
// sync) and an upcoming one moved by an hour, and runs that first sync. It
// returns the rig, the ProviderID and the upcoming override.
func objectReplaceSeries(t *testing.T) (*rig, string, calendar.Event) {
	t.Helper()
	r := newRig(t)
	r.store = fake.New(fake.Options{ObjectReplace: true, Now: r.clock.Now})
	r.store.AddCalendar(calendar.Calendar{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true})
	r.syncer = calsync.New(calsync.Options{Account: account, Store: r.store, Cache: r.cache, Log: r.log, Self: self, Poll: poll, WindowPast: windowPast, Clock: r.clock})
	master := invited("s@x", t0.AddDate(0, 0, -70), calendar.PartStatNeedsAction)
	master.RRule = "FREQ=WEEKLY"
	old := master
	old.RRule = ""
	old.RecurrenceID = master.Start.AddDate(0, 0, 7)
	old.Start = old.RecurrenceID.Add(time.Hour)
	old.End = old.Start.Add(time.Hour)
	soon := old
	soon.RecurrenceID = master.Start.AddDate(0, 0, 77)
	soon.Start = soon.RecurrenceID.Add(time.Hour)
	soon.End = soon.Start.Add(time.Hour)
	p := r.store.Add("work", master, old, soon)
	r.sync(t) // the first sync; the prune deletes the old override
	return r, p, soon
}

// TestUpcomingOverrideRemoved checks that taking an upcoming moved
// override out of a series that stays puts the instance back at the time
// its master gives, which is an event_changed for that instance.
func TestUpcomingOverrideRemoved(t *testing.T) {
	t.Parallel()
	r, p, soon := objectReplaceSeries(t)
	r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event {
		return rows[:2] // drop the upcoming override; the old one is sent again
	})
	r.sync(t)
	want := calendar.InstanceRef(calendar.EventRef(account, "work", p), soon.RecurrenceID)
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Kind != watch.KindEventChanged || evs[0].Ref != want {
		t.Fatalf("events = %+v, want one event_changed for %s", evs, want)
	}
	c := evs[0].Calendar
	if strings.Join(c.Fields, ",") != "start,end" || c.Start != rfc(soon.RecurrenceID) || c.End != rfc(soon.RecurrenceID.Add(time.Hour)) {
		t.Errorf("change = %+v, want fields start,end at the master's time", c)
	}
}

// TestRemovedOverrideRestoresLocation checks an override that changed only
// the place: removing it reports the location going back.
func TestRemovedOverrideRestoresLocation(t *testing.T) {
	t.Parallel()
	r, p, _ := objectReplaceSeries(t)
	r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event {
		rows[2].Start, rows[2].End = rows[2].RecurrenceID, rows[2].RecurrenceID.Add(time.Hour)
		rows[2].Location = "Room 9"
		return rows
	})
	r.sync(t)
	n := len(r.events(t))
	r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event { return rows[:2] })
	r.sync(t)
	evs := r.events(t)[n:]
	if len(evs) != 1 || evs[0].Kind != watch.KindEventChanged || strings.Join(evs[0].Calendar.Fields, ",") != "location" {
		t.Errorf("events = %+v, want one event_changed of location", evs)
	}
}

// TestRemovedCancelledOverrideRestoresTheInstance checks that removing a
// cancelled override brings the instance back: an event_changed of its
// status, or an invite when James has still to answer the series.
func TestRemovedCancelledOverrideRestoresTheInstance(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ps   calendar.PartStat
		kind watch.Kind
	}{
		{"accepted", calendar.PartStatAccepted, watch.KindEventChanged},
		{"unanswered", calendar.PartStatNeedsAction, watch.KindInvite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.store = fake.New(fake.Options{ObjectReplace: true, Now: r.clock.Now})
			r.store.AddCalendar(calendar.Calendar{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true})
			r.syncer = calsync.New(calsync.Options{Account: account, Store: r.store, Cache: r.cache, Log: r.log, Self: self, Poll: poll, WindowPast: windowPast, Clock: r.clock})
			master := invited("s@x", t0.Add(24*time.Hour), tc.ps)
			master.RRule = "FREQ=WEEKLY"
			rid := master.Start.AddDate(0, 0, 7)
			gone := calendar.Event{UID: master.UID, RecurrenceID: rid, Status: calendar.StatusCancelled}
			p := r.store.Add("work", master, gone)
			r.sync(t)
			r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event { return rows[:1] })
			r.sync(t)
			want := calendar.InstanceRef(calendar.EventRef(account, "work", p), rid)
			evs := r.events(t)
			if len(evs) != 1 || evs[0].Kind != tc.kind || evs[0].Ref != want {
				t.Fatalf("events = %+v, want one %s for %s", evs, tc.kind, want)
			}
			if tc.kind == watch.KindEventChanged && strings.Join(evs[0].Calendar.Fields, ",") != "status" {
				t.Errorf("fields = %v, want status", evs[0].Calendar.Fields)
			}
			if evs[0].Calendar.Start != rfc(rid) {
				t.Errorf("start = %s, want %s", evs[0].Calendar.Start, rfc(rid))
			}
		})
	}
}

// TestPastOverrideRemovedQuietly checks that removing an override of an
// instance that ended before the window stays quiet.
func TestPastOverrideRemovedQuietly(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store = fake.New(fake.Options{ObjectReplace: true, Now: r.clock.Now})
	r.store.AddCalendar(calendar.Calendar{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true})
	r.syncer = calsync.New(calsync.Options{Account: account, Store: r.store, Cache: r.cache, Log: r.log, Self: self, Poll: poll, WindowPast: windowPast, Clock: r.clock})
	master := invited("s@x", t0.AddDate(0, 0, -70), calendar.PartStatAccepted)
	master.RRule = "FREQ=WEEKLY"
	// Moved from before the window to after it, so the prune keeps it.
	kept := master
	kept.RRule = ""
	kept.RecurrenceID = master.Start.AddDate(0, 0, 7)
	kept.Start = t0.Add(time.Hour)
	kept.End = kept.Start.Add(time.Hour)
	p := r.store.Add("work", master, kept)
	r.sync(t)
	r.clock.set(t0.Add(windowPast + 72*time.Hour)) // the moved instance is now past the window too
	r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event { return rows[:1] })
	r.sync(t)
	if evs := r.events(t); len(evs) != 0 {
		t.Errorf("events = %+v, want none", evs)
	}
}

// TestRecurringInviteMovedIntoFuture checks that a new series whose own
// occurrences are all past, but whose override moves one into the future,
// gives one invite for the series.
func TestRecurringInviteMovedIntoFuture(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.sync(t)
	master := invited("s@x", t0.AddDate(0, 0, -21), calendar.PartStatNeedsAction)
	master.RRule = "FREQ=WEEKLY;COUNT=2"
	moved := master
	moved.RRule = ""
	moved.RecurrenceID = master.Start.AddDate(0, 0, 7)
	moved.Start = t0.AddDate(0, 0, 3)
	moved.End = moved.Start.Add(time.Hour)
	p := r.store.Add("work", master, moved)
	r.sync(t)
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Kind != "invite" || evs[0].Ref != calendar.EventRef(account, "work", p) {
		t.Errorf("events = %+v, want one invite for the series", evs)
	}
}

// TestRecurringNoInviteWhenFutureCancelled checks that a new series whose
// only future occurrence is cancelled by an override gives no invite.
func TestRecurringNoInviteWhenFutureCancelled(t *testing.T) {
	t.Parallel()
	master := invited("s@x", t0.AddDate(0, 0, -8), calendar.PartStatNeedsAction)
	master.RRule = "FREQ=WEEKLY;COUNT=3"
	rid := master.Start.AddDate(0, 0, 14) // t0 + 6 days, the only future one
	full := master
	full.RRule = ""
	full.RecurrenceID = rid
	full.Start, full.End = rid, rid.Add(time.Hour)
	full.Status = calendar.StatusCancelled
	sparse := calendar.Event{UID: master.UID, RecurrenceID: rid, Status: calendar.StatusCancelled}
	for name, cancelled := range map[string]calendar.Event{"full": full, "sparse": sparse} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.sync(t)
			r.store.Add("work", master, cancelled)
			r.sync(t)
			if evs := r.events(t); len(evs) != 0 {
				t.Errorf("events = %+v, want none", evs)
			}
		})
	}
}

// TestRecurringInviteOnOverride checks that a new series James has already
// accepted, with one instance that asks him again, gives one invite for
// that instance.
func TestRecurringInviteOnOverride(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.sync(t)
	master := invited("s@x", t0.Add(24*time.Hour), calendar.PartStatAccepted)
	master.RRule = "FREQ=WEEKLY;COUNT=3"
	asks := invited("s@x", master.Start.AddDate(0, 0, 7), calendar.PartStatNeedsAction)
	asks.RecurrenceID = asks.Start
	p := r.store.Add("work", master, asks)
	r.sync(t)
	want := calendar.InstanceRef(calendar.EventRef(account, "work", p), asks.RecurrenceID)
	evs := r.events(t)
	if len(evs) != 1 || evs[0].Kind != "invite" || evs[0].Ref != want {
		t.Errorf("events = %+v, want one invite for %s", evs, want)
	}
}

// TestStoredMasterReadFails checks that when the stored master of a new
// sparse cancelled override cannot be read, the cycle fails before it
// publishes or applies anything, and the next cycle reports the cancelled
// instance.
func TestStoredMasterReadFails(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// Like Google, the store sends only the instance that changed, so the
	// override's series role comes from the stored master.
	instancesOnly := &changedInstances{Store: r.store}
	r.syncer = calsync.New(calsync.Options{Account: account, Store: instancesOnly, Cache: r.cache, Log: r.log, Self: self, Poll: poll, WindowPast: windowPast, Clock: r.clock})
	var failing atomic.Bool
	calsync.SetSeriesRead(r.syncer, func(ctx context.Context, account, calID, providerID string) ([]calcache.EventRow, error) {
		if failing.Load() {
			return nil, errors.New("disk I/O error")
		}
		return r.cache.Series(ctx, account, calID, providerID)
	})
	master := invited("s@x", t0.Add(24*time.Hour), calendar.PartStatAccepted)
	master.RRule = "FREQ=WEEKLY"
	p := r.store.Add("work", master)
	r.sync(t)
	before, err := r.cache.Calendar(t.Context(), account, "work")
	if err != nil {
		t.Fatal(err)
	}
	instancesOnly.overridesOnly.Store(true)
	rid := master.Start.AddDate(0, 0, 7)
	r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event {
		return append(rows, calendar.Event{UID: master.UID, RecurrenceID: rid, Status: calendar.StatusCancelled})
	})

	failing.Store(true)
	if err := r.syncer.SyncNow(t.Context()); err == nil {
		t.Fatal("SyncNow succeeded with the stored master unreadable")
	}
	if evs := r.events(t); len(evs) != 0 {
		t.Errorf("events = %+v, want none from a failed cycle", evs)
	}
	after, err := r.cache.Calendar(t.Context(), account, "work")
	if err != nil {
		t.Fatal(err)
	}
	if after.SyncToken != before.SyncToken || !after.LastSync.Equal(before.LastSync) {
		t.Errorf("token %q synced %v after the failed cycle, want %q synced %v", after.SyncToken, after.LastSync, before.SyncToken, before.LastSync)
	}
	if st := r.syncer.Status(); st.State != calsync.StateError || strings.Contains(st.LastError, "disk") {
		t.Errorf("Status = %+v, want error with fixed text", st)
	}

	failing.Store(false)
	r.sync(t)
	evs := r.events(t)
	want := calendar.InstanceRef(calendar.EventRef(account, "work", p), rid)
	if len(evs) != 1 || evs[0].Kind != "event_cancelled" || evs[0].Ref != want {
		t.Errorf("events = %+v, want one event_cancelled for %s", evs, want)
	}
}

// changedInstances is a calendar store whose incremental syncs, once
// overridesOnly is set, send only the overrides of each changed object, as
// Google sends only the instance that changed.
type changedInstances struct {
	calstore.Store
	overridesOnly atomic.Bool
}

func (s *changedInstances) Sync(ctx context.Context, calID, token string) (calendar.Changes, error) {
	ch, err := s.Store.Sync(ctx, calID, token)
	if err != nil || ch.Full || !s.overridesOnly.Load() {
		return ch, err
	}
	ch.Upserts = slices.DeleteFunc(ch.Upserts, func(ev calendar.Event) bool { return ev.RecurrenceID.IsZero() })
	return ch, nil
}

// TestRemovedOverrideRestoresAnAnswer checks a series James organizes
// whose override differed from its master only in the boss's answer:
// removing the override gives the instance the master's answer back, an
// invite_reply for the boss. An instance past the window stays quiet.
func TestRemovedOverrideRestoresAnAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		past bool
	}{{"upcoming", false}, {"past", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.store = fake.New(fake.Options{ObjectReplace: true, Now: r.clock.Now})
			r.store.AddCalendar(calendar.Calendar{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true})
			r.syncer = calsync.New(calsync.Options{Account: account, Store: r.store, Cache: r.cache, Log: r.log, Self: self, Poll: poll, WindowPast: windowPast, Clock: r.clock})
			master := organized("s@x", t0.Add(24*time.Hour), calendar.PartStatAccepted)
			master.RRule = "FREQ=WEEKLY"
			rid := master.Start.AddDate(0, 0, 7)
			answered := master
			answered.RRule = ""
			answered.RecurrenceID, answered.Start, answered.End = rid, rid, rid.Add(time.Hour)
			answered.Attendees = slices.Clone(master.Attendees)
			answered.Attendees[1].PartStat = calendar.PartStatDeclined
			p := r.store.Add("work", master, answered)
			r.sync(t)
			if tc.past {
				r.clock.set(rid.Add(windowPast + 72*time.Hour))
			}
			r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event { return rows[:1] })
			r.sync(t)
			evs := r.events(t)
			if tc.past {
				if len(evs) != 0 {
					t.Errorf("events = %+v, want none", evs)
				}
				return
			}
			if len(evs) != 1 || evs[0].Kind != watch.KindInviteReply {
				t.Fatalf("events = %+v, want one invite_reply", evs)
			}
			if c := evs[0].Calendar; c.Attendee != boss || c.PartStat != "ACCEPTED" || c.Ref != calendar.InstanceRef(calendar.EventRef(account, "work", p), rid) {
				t.Errorf("payload = %+v, want the boss's restored ACCEPTED on the instance", c)
			}
		})
	}
}

// TestRemovedCancelledOverrideRestoresAnAnswer checks a cancelled override
// that listed the boss with another answer: bringing the instance back
// reports its status and the boss's restored answer.
func TestRemovedCancelledOverrideRestoresAnAnswer(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.store = fake.New(fake.Options{ObjectReplace: true, Now: r.clock.Now})
	r.store.AddCalendar(calendar.Calendar{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true})
	r.syncer = calsync.New(calsync.Options{Account: account, Store: r.store, Cache: r.cache, Log: r.log, Self: self, Poll: poll, WindowPast: windowPast, Clock: r.clock})
	master := organized("s@x", t0.Add(24*time.Hour), calendar.PartStatAccepted)
	master.RRule = "FREQ=WEEKLY"
	rid := master.Start.AddDate(0, 0, 7)
	gone := master
	gone.RRule, gone.Status = "", calendar.StatusCancelled
	gone.RecurrenceID, gone.Start, gone.End = rid, rid, rid.Add(time.Hour)
	gone.Attendees = slices.Clone(master.Attendees)
	gone.Attendees[1].PartStat = calendar.PartStatDeclined
	p := r.store.Add("work", master, gone)
	r.sync(t)
	r.store.Change("work", p, func(rows []calendar.Event) []calendar.Event { return rows[:1] })
	r.sync(t)
	var got []string
	for _, e := range r.events(t) {
		got = append(got, string(e.Kind)+" "+strings.Join(e.Calendar.Fields, ",")+" "+e.Calendar.Attendee+" "+e.Calendar.PartStat)
	}
	want := []string{"event_changed status  ", "invite_reply  " + boss + " ACCEPTED"}
	if !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}
