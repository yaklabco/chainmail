package fake_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/calstore/fake"
)

var t0 = time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)

const me = "james@example.com"

func newStore(t *testing.T, objectReplace bool) *fake.Store {
	t.Helper()
	s := fake.New(fake.Options{ObjectReplace: objectReplace, Now: func() time.Time { return t0 }})
	s.AddCalendar(calendar.Calendar{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true})
	s.AddCalendar(calendar.Calendar{ID: "shared", Name: "Shared", Access: calendar.AccessRead})
	return s
}

func meeting(uid string) calendar.Event {
	return calendar.Event{
		UID:       uid,
		Summary:   "meeting " + uid,
		Start:     t0,
		End:       t0.Add(time.Hour),
		Status:    calendar.StatusConfirmed,
		Organizer: &calendar.Person{Email: "boss@example.com"},
		Attendees: []calendar.Attendee{
			{Person: calendar.Person{Email: "boss@example.com"}, PartStat: calendar.PartStatAccepted},
			{Person: calendar.Person{Email: me}, PartStat: calendar.PartStatNeedsAction, RSVP: true},
			{Person: calendar.Person{Email: "ann@example.com"}, PartStat: calendar.PartStatNeedsAction},
		},
	}
}

func providers(evs []calendar.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		k := ev.ProviderID
		if !ev.RecurrenceID.IsZero() {
			k += "@" + ev.RecurrenceID.Format("20060102T150405Z")
		}
		out = append(out, k)
	}
	return out
}

func same(t *testing.T, what string, want, got any) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Errorf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}

func partStat(t *testing.T, ev calendar.Event, email string) calendar.PartStat {
	t.Helper()
	for _, a := range ev.Attendees {
		if a.Email == email {
			return a.PartStat
		}
	}
	t.Fatalf("%s is not an attendee", email)
	return ""
}

var _ calstore.Store = (*fake.Store)(nil)

func TestListCalendars(t *testing.T) {
	t.Parallel()
	s := newStore(t, false)
	cals, err := s.ListCalendars(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	same(t, "ListCalendars", []calendar.Calendar{
		{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true},
		{ID: "shared", Name: "Shared", Access: calendar.AccessRead},
	}, cals)
	s.AddCalendar(calendar.Calendar{ID: "work", Name: "Renamed", Access: calendar.AccessRead})
	s.RemoveCalendar("shared")
	cals, err = s.ListCalendars(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	same(t, "ListCalendars after changes", []calendar.Calendar{{ID: "work", Name: "Renamed", Access: calendar.AccessRead}}, cals)
}

func TestSyncTokens(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, false)
	a := s.Add("work", meeting("a@x"))
	b := s.Add("work", meeting("b@x"))

	full, err := s.Sync(ctx, "work", "")
	if err != nil {
		t.Fatal(err)
	}
	if !full.Full || full.Token == "" || full.ObjectReplace || len(full.Deletes) != 0 {
		t.Errorf("full sync = %+v", full)
	}
	same(t, "full upserts", []string{a, b}, sortedProviders(full.Upserts))

	none, err := s.Sync(ctx, "work", full.Token)
	if err != nil {
		t.Fatal(err)
	}
	if none.Full || len(none.Upserts) != 0 || len(none.Deletes) != 0 || none.Token != full.Token {
		t.Errorf("sync with nothing new = %+v", none)
	}

	s.Cancel("work", a)
	c := s.Add("work", meeting("c@x"))
	s.Remove("work", b)
	inc, err := s.Sync(ctx, "work", full.Token)
	if err != nil {
		t.Fatal(err)
	}
	if inc.Full || inc.Token == full.Token {
		t.Errorf("incremental sync = %+v", inc)
	}
	same(t, "incremental upserts", []string{a, c}, sortedProviders(inc.Upserts))
	same(t, "incremental deletes", []string{b}, inc.Deletes)
	for _, ev := range inc.Upserts {
		if ev.ProviderID == a && (ev.Status != calendar.StatusCancelled || ev.Sequence != 1) {
			t.Errorf("cancelled event = %+v, want CANCELLED with sequence 1", ev)
		}
	}

	// An object added and removed between syncs is a delete of something
	// the cache never saw, which a cache must tolerate.
	d := s.Add("work", meeting("d@x"))
	s.Remove("work", d)
	inc2, err := s.Sync(ctx, "work", inc.Token)
	if err != nil {
		t.Fatal(err)
	}
	same(t, "add then remove", []string{d}, inc2.Deletes)

	s.InvalidateTokens()
	if _, err := s.Sync(ctx, "work", inc2.Token); !errors.Is(err, calstore.ErrSyncTokenInvalid) {
		t.Errorf("sync with an invalidated token = %v, want ErrSyncTokenInvalid", err)
	}
	if _, err := s.Sync(ctx, "work", "garbage"); !errors.Is(err, calstore.ErrSyncTokenInvalid) {
		t.Errorf("sync with a garbage token = %v, want ErrSyncTokenInvalid", err)
	}
	if _, err := s.Sync(ctx, "nope", ""); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("sync of an unknown calendar = %v, want ErrNotFound", err)
	}
	// Another calendar's token is not valid here.
	shared, err := s.Sync(ctx, "shared", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, "work", shared.Token); !errors.Is(err, calstore.ErrSyncTokenInvalid) {
		t.Errorf("sync with another calendar's token = %v, want ErrSyncTokenInvalid", err)
	}
}

// TestSyncTokensDoNotSurviveCalendarRecreation removes a calendar and adds
// one with the same ID. The new calendar's change log starts again, so a
// token of the removed one would skip the new calendar's first changes: it
// must be invalid, which makes the cache resync from scratch.
func TestSyncTokensDoNotSurviveCalendarRecreation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, false)
	s.Add("work", meeting("a@x"))
	s.Add("work", meeting("b@x"))
	old, err := s.Sync(ctx, "work", "")
	if err != nil {
		t.Fatal(err)
	}

	info := calendar.Calendar{ID: "work", Name: "Work", Access: calendar.AccessWrite}
	s.RemoveCalendar("work")
	s.AddCalendar(info)
	s.Add("work", meeting("c@x"))
	s.Add("work", meeting("d@x"))
	s.Add("work", meeting("e@x"))
	if _, err := s.Sync(ctx, "work", old.Token); !errors.Is(err, calstore.ErrSyncTokenInvalid) {
		t.Errorf("sync of the recreated calendar with the removed one's token = %v, want ErrSyncTokenInvalid", err)
	}

	// The new calendar's own tokens work, and AddCalendar on a calendar
	// that still exists keeps them.
	fresh, err := s.Sync(ctx, "work", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Upserts) != 3 {
		t.Errorf("full sync of the recreated calendar = %d upserts, want 3", len(fresh.Upserts))
	}
	s.AddCalendar(info)
	if _, err := s.Sync(ctx, "work", fresh.Token); err != nil {
		t.Errorf("sync after AddCalendar of an existing calendar = %v, want the token kept", err)
	}
}

func sortedProviders(evs []calendar.Event) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range providers(evs) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func TestSyncObjectReplace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, true)
	master := meeting("series@x")
	master.RRule = "FREQ=WEEKLY"
	p := s.Add("work", master)
	first, err := s.Sync(ctx, "work", "")
	if err != nil {
		t.Fatal(err)
	}
	if !first.ObjectReplace {
		t.Error("ObjectReplace unset")
	}
	rid := t0.AddDate(0, 0, 7)
	s.CancelInstance("work", p, rid)
	inc, err := s.Sync(ctx, "work", first.Token)
	if err != nil {
		t.Fatal(err)
	}
	same(t, "object rows", []string{p, p + "@" + rid.Format("20060102T150405Z")}, providers(inc.Upserts))
	ov := inc.Upserts[1]
	if ov.Status != calendar.StatusCancelled || ov.RRule != "" || !ov.Start.Equal(rid) || !ov.End.Equal(rid.Add(time.Hour)) || ov.UID != master.UID {
		t.Errorf("cancelled instance = %+v", ov)
	}
	if inc.Upserts[0].ETag != ov.ETag {
		t.Errorf("rows of one object have etags %q and %q, want one", inc.Upserts[0].ETag, ov.ETag)
	}
}

func TestOrganizerHelpers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, false)
	p := s.Add("work", meeting("a@x"))
	before, ok := s.Object("work", p)
	if !ok {
		t.Fatal("Object: missing")
	}
	s.SetPartStat("work", p, time.Time{}, "ann@example.com", calendar.PartStatDeclined)
	after, _ := s.Object("work", p)
	if partStat(t, after[0], "ann@example.com") != calendar.PartStatDeclined {
		t.Errorf("ann = %q, want DECLINED", partStat(t, after[0], "ann@example.com"))
	}
	if after[0].ETag == before[0].ETag {
		t.Error("SetPartStat kept the etag")
	}
	s.Change("work", p, func(evs []calendar.Event) []calendar.Event {
		evs[0].Location = "Room 2"
		evs[0].Sequence++
		return evs
	})
	got, err := s.GetEvent(ctx, "work", p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Location != "Room 2" || got[0].Sequence != 1 || got[0].ProviderID != p || !got[0].Updated.Equal(t0) {
		t.Errorf("GetEvent after Change = %+v", got)
	}
	// What GetEvent returns is a copy.
	got[0].Attendees[0].PartStat = calendar.PartStatTentative
	again, _ := s.GetEvent(ctx, "work", p)
	if again[0].Attendees[0].PartStat == calendar.PartStatTentative {
		t.Error("GetEvent shares its attendees with the store")
	}
	if _, err := s.GetEvent(ctx, "work", "nope"); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("GetEvent(nope) = %v, want ErrNotFound", err)
	}
}

func TestPut(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, false)
	ev := meeting("new@x")
	created, err := s.Put(ctx, "work", ev, "", calendar.NotifyNone)
	if err != nil {
		t.Fatal(err)
	}
	if created.ProviderID == "" || created.ETag == "" || created.UID != "new@x" {
		t.Errorf("created = %+v", created)
	}
	// The object name comes from the UID, so a second create finds it.
	if _, err := s.Put(ctx, "work", ev, "", calendar.NotifySend); !errors.Is(err, calstore.ErrExists) {
		t.Errorf("second create = %v, want ErrExists", err)
	}
	if _, err := s.Put(ctx, "shared", ev, "", calendar.NotifySend); !errors.Is(err, calstore.ErrReadOnly) {
		t.Errorf("create on a read-only calendar = %v, want ErrReadOnly", err)
	}
	if _, err := s.Put(ctx, "nope", ev, "", calendar.NotifySend); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("create on an unknown calendar = %v, want ErrNotFound", err)
	}

	update := created
	update.Summary = "renamed"
	if _, err := s.Put(ctx, "work", update, `"stale"`, calendar.NotifySend); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("update with a stale etag = %v, want ErrConflict", err)
	}
	updated, err := s.Put(ctx, "work", update, created.ETag, calendar.NotifySend)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Summary != "renamed" || updated.ETag == created.ETag || updated.ProviderID != created.ProviderID {
		t.Errorf("updated = %+v", updated)
	}
	missing := update
	missing.ProviderID = "nope"
	if _, err := s.Put(ctx, "work", missing, "x", calendar.NotifySend); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("update of a missing object = %v, want ErrNotFound", err)
	}

	puts := s.CallsTo(fake.MethodPut)
	if len(puts) != 7 {
		t.Fatalf("Put calls = %d, want 7", len(puts))
	}
	if puts[0].Notify != calendar.NotifyNone || puts[0].ETag != "" || puts[0].CalendarID != "work" || puts[0].Event.UID != "new@x" {
		t.Errorf("first Put call = %+v", puts[0])
	}
	if puts[5].Notify != calendar.NotifySend || puts[5].ETag != created.ETag || puts[5].ProviderID != created.ProviderID {
		t.Errorf("update Put call = %+v", puts[5])
	}
}

func TestRespond(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, false)
	p := s.Add("work", meeting("a@x"))
	got, err := s.Respond(ctx, "work", p, calendar.Response{Self: "James@Example.com", PartStat: calendar.PartStatAccepted, Comment: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if partStat(t, got, me) != calendar.PartStatAccepted {
		t.Errorf("Respond result = %+v", got)
	}

	series := meeting("series@x")
	series.RRule = "FREQ=DAILY"
	sp := s.Add("work", series)
	rid := t0.AddDate(0, 0, 2)
	inst, err := s.Respond(ctx, "work", sp, calendar.Response{Self: me, PartStat: calendar.PartStatDeclined, RecurrenceID: rid})
	if err != nil {
		t.Fatal(err)
	}
	if !inst.RecurrenceID.Equal(rid) || partStat(t, inst, me) != calendar.PartStatDeclined || inst.RRule != "" {
		t.Errorf("instance response = %+v", inst)
	}
	rows, _ := s.Object("work", sp)
	if len(rows) != 2 || partStat(t, rows[0], me) != calendar.PartStatNeedsAction {
		t.Errorf("series after an instance response = %+v, want the master untouched and one override", rows)
	}

	if _, err := s.Respond(ctx, "work", p, calendar.Response{Self: "stranger@example.com", PartStat: calendar.PartStatAccepted}); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("Respond by a non-attendee = %v, want ErrNotFound", err)
	}
	if _, err := s.Respond(ctx, "work", "nope", calendar.Response{Self: me, PartStat: calendar.PartStatAccepted}); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("Respond to a missing object = %v, want ErrNotFound", err)
	}
	calls := s.CallsTo(fake.MethodRespond)
	if len(calls) != 4 || calls[0].Response.Comment != "ok" || calls[1].Response.RecurrenceID != rid {
		t.Errorf("Respond calls = %+v", calls)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, false)
	p := s.Add("work", meeting("a@x"))
	rows, _ := s.Object("work", p)
	if err := s.Delete(ctx, "work", p, `"stale"`, calendar.NotifySend); !errors.Is(err, calstore.ErrConflict) {
		t.Errorf("Delete with a stale etag = %v, want ErrConflict", err)
	}
	if err := s.Delete(ctx, "work", p, rows[0].ETag, calendar.NotifyNone); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Object("work", p); ok {
		t.Error("object survived Delete")
	}
	if err := s.Delete(ctx, "work", p, "", calendar.NotifySend); !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("Delete of a missing object = %v, want ErrNotFound", err)
	}
	sp := s.Add("shared", meeting("b@x"))
	if err := s.Delete(ctx, "shared", sp, "", calendar.NotifySend); !errors.Is(err, calstore.ErrReadOnly) {
		t.Errorf("Delete on a read-only calendar = %v, want ErrReadOnly", err)
	}
	calls := s.CallsTo(fake.MethodDelete)
	if len(calls) != 4 || calls[1].Notify != calendar.NotifyNone || calls[1].ETag != rows[0].ETag {
		t.Errorf("Delete calls = %+v", calls)
	}
}

func TestFreeBusy(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, false)
	s.SetFreeBusy("ann@example.com", calendar.FreeBusy{Known: true, Busy: []calendar.BusyBlock{
		{Start: t0.Add(-2 * time.Hour), End: t0.Add(-time.Hour)},
		{Start: t0.Add(-30 * time.Minute), End: t0.Add(30 * time.Minute)},
		{Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour)},
	}})
	got, err := s.FreeBusy(ctx, []string{"ann@example.com", "who@example.com"}, t0, t0.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	same(t, "FreeBusy", map[string]calendar.FreeBusy{
		"ann@example.com": {Known: true, Busy: []calendar.BusyBlock{
			{Start: t0, End: t0.Add(30 * time.Minute)},
			{Start: t0.Add(2 * time.Hour), End: t0.Add(3 * time.Hour)},
		}},
		"who@example.com": {Known: false},
	}, got)
	calls := s.CallsTo(fake.MethodFreeBusy)
	if len(calls) != 1 || len(calls[0].People) != 2 || !calls[0].From.Equal(t0) {
		t.Errorf("FreeBusy calls = %+v", calls)
	}
}

func TestScriptedErrors(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, false)
	s.FailNext(fake.MethodSync, calstore.ErrUnavailable, calstore.ErrAuth)
	if _, err := s.Sync(ctx, "work", ""); !errors.Is(err, calstore.ErrUnavailable) {
		t.Errorf("first Sync = %v, want ErrUnavailable", err)
	}
	if _, err := s.Sync(ctx, "work", ""); !errors.Is(err, calstore.ErrAuth) {
		t.Errorf("second Sync = %v, want ErrAuth", err)
	}
	if _, err := s.Sync(ctx, "work", ""); err != nil {
		t.Errorf("third Sync = %v, want the queue drained", err)
	}

	s.Fail(fake.MethodListCalendars, calstore.ErrAuth)
	for range 2 {
		if _, err := s.ListCalendars(ctx); !errors.Is(err, calstore.ErrAuth) {
			t.Errorf("ListCalendars = %v, want ErrAuth until cleared", err)
		}
	}
	// A queued error goes first, then the standing one.
	s.FailNext(fake.MethodListCalendars, calstore.ErrUnavailable)
	if _, err := s.ListCalendars(ctx); !errors.Is(err, calstore.ErrUnavailable) {
		t.Errorf("ListCalendars = %v, want the queued error", err)
	}
	s.Fail(fake.MethodListCalendars, nil)
	if _, err := s.ListCalendars(ctx); err != nil {
		t.Errorf("ListCalendars after clearing = %v", err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetEvent(cancelled, "work", "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("GetEvent with a cancelled context = %v, want context.Canceled", err)
	}

	if n := s.Count(fake.MethodSync); n != 3 {
		t.Errorf("Count(Sync) = %d, want 3, failed calls included", n)
	}
	if n := s.Count(fake.MethodListCalendars); n != 4 {
		t.Errorf("Count(ListCalendars) = %d, want 4", n)
	}
	if len(s.Calls()) != 8 {
		t.Errorf("Calls = %d, want 8", len(s.Calls()))
	}
	s.ResetCalls()
	if len(s.Calls()) != 0 {
		t.Error("ResetCalls kept calls")
	}
}

func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, false)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for j := range 20 {
				p := s.Add("work", meeting(string(rune('a'+i))+string(rune('a'+j))+"@x"))
				if _, err := s.Sync(ctx, "work", ""); err != nil {
					t.Error(err)
				}
				s.SetPartStat("work", p, time.Time{}, me, calendar.PartStatAccepted)
			}
		})
	}
	wg.Wait()
	full, err := s.Sync(ctx, "work", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Upserts) != 80 {
		t.Errorf("events after concurrent adds = %d, want 80", len(full.Upserts))
	}
}

// TestStoreObjectID checks that ObjectID names the object a create of the
// UID makes.
func TestStoreObjectID(t *testing.T) {
	s := newStore(t, false)
	got, err := s.Put(context.Background(), "work", meeting("u1@x"), "", calendar.NotifySend)
	if err != nil {
		t.Fatal(err)
	}
	if id := s.ObjectID("work", "u1@x"); id != got.ProviderID || id != fake.ObjectName("u1@x") {
		t.Errorf("ObjectID = %q, want %q", id, got.ProviderID)
	}
}
