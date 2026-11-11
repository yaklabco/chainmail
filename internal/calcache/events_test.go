package calcache_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
)

func TestSetCalendars(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	cals, err := s.Calendars(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	want := []calcache.CalendarRow{
		{Account: "acme", Calendar: calendar.Calendar{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true, TimeZone: "Europe/London"}, IsDefault: true},
		{Account: "acme", Calendar: calendar.Calendar{ID: "home", Name: "Home", Access: calendar.AccessRead}},
	}
	same(t, "Calendars", want, cals)
	other, err := s.Calendars(ctx, "other")
	if err != nil || len(other) != 0 {
		t.Errorf("Calendars(other) = %v, %v; want none", other, err)
	}
	if _, err := s.Calendar(ctx, "acme", "nope"); !errors.Is(err, calcache.ErrNotFound) {
		t.Errorf("Calendar(nope) = %v, want ErrNotFound", err)
	}

	// A sync keeps its token across a later SetCalendars that renames the
	// calendar and moves the default.
	apply(t, s, "work", calendar.Changes{Upserts: []calendar.Event{single("p1", t0, t0.Add(time.Hour))}, Token: "tok", Full: true})
	if err := s.SetError(ctx, "acme", "work", "server unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCalendars(ctx, "acme", []calendar.Calendar{
		{ID: "work", Name: "Work 2", Access: calendar.AccessWrite},
		{ID: "home", Name: "Home", Access: calendar.AccessRead},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	work, err := s.Calendar(ctx, "acme", "work")
	if err != nil {
		t.Fatal(err)
	}
	if work.Name != "Work 2" || work.IsDefault || work.SyncToken != "tok" || !work.LastSync.Equal(t0) || work.LastError != "server unavailable" {
		t.Errorf("work after SetCalendars = %+v", work)
	}
	home, err := s.Calendar(ctx, "acme", "home")
	if err != nil || !home.IsDefault {
		t.Errorf("home = %+v, %v; want the default", home, err)
	}

	// An unknown access value is stored as read: chainmail never writes to a
	// calendar it cannot tell is writable.
	if err := s.SetCalendars(ctx, "beta", []calendar.Calendar{{ID: "x", Access: "owner?"}}, ""); err != nil {
		t.Fatal(err)
	}
	if x, err := s.Calendar(ctx, "beta", "x"); err != nil || x.Access != calendar.AccessRead {
		t.Errorf("beta x = %+v, %v; want read access", x, err)
	}
}

func TestSetCalendarsRemovalCascades(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	if err := s.SetCalendars(ctx, "beta", []calendar.Calendar{{ID: "home", Access: calendar.AccessWrite}}, "home"); err != nil {
		t.Fatal(err)
	}
	apply(t, s, "work", calendar.Changes{Upserts: []calendar.Event{single("w1", t0, t0.Add(time.Hour))}, Full: true})
	apply(t, s, "home", calendar.Changes{Upserts: []calendar.Event{single("h1", t0, t0.Add(time.Hour))}, Full: true})
	if err := s.Apply(ctx, "beta", "home", calendar.Changes{Upserts: []calendar.Event{single("b1", t0, t0.Add(time.Hour))}, Full: true}, self, t0); err != nil {
		t.Fatal(err)
	}

	if err := s.SetCalendars(ctx, "acme", []calendar.Calendar{{ID: "work", Access: calendar.AccessWrite}}, "work"); err != nil {
		t.Fatal(err)
	}
	cals, err := s.Calendars(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 || cals[0].ID != "work" {
		t.Errorf("acme calendars = %+v, want only work", cals)
	}
	if _, err := s.Event(ctx, calendar.EventRef("acme", "home", "h1")); !errors.Is(err, calcache.ErrNotFound) {
		t.Errorf("event of the removed calendar: %v, want ErrNotFound", err)
	}
	if _, err := s.Event(ctx, calendar.EventRef("acme", "work", "w1")); err != nil {
		t.Errorf("event of the kept calendar: %v", err)
	}
	if _, err := s.Event(ctx, calendar.EventRef("beta", "home", "b1")); err != nil {
		t.Errorf("another account's calendar of the same ID lost its event: %v", err)
	}
	if n, ev, _ := s.Stats(ctx, "beta"); n != 1 || ev != 1 {
		t.Errorf("beta Stats = %d, %d; want 1, 1", n, ev)
	}

	// No calendars at all removes every calendar of the account.
	if err := s.SetCalendars(ctx, "acme", nil, ""); err != nil {
		t.Fatal(err)
	}
	if n, ev, _ := s.Stats(ctx, "acme"); n != 0 || ev != 0 {
		t.Errorf("acme Stats = %d, %d; want 0, 0", n, ev)
	}
}

func TestApplyRoundTripsEveryField(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	ev := calendar.Event{
		ProviderID:  "full",
		ETag:        `"etag-1"`,
		UID:         "full@example.com",
		Summary:     "Planning",
		Location:    "Room 1",
		Description: "line one\nline two",
		Start:       time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC),
		End:         time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC),
		TZID:        "America/Los_Angeles",
		Status:      calendar.StatusTentative,
		Sequence:    3,
		Transparent: true,
		Organizer:   &calendar.Person{Email: "j@alias.example", Name: "James"},
		Attendees: []calendar.Attendee{
			{Person: calendar.Person{Email: "a@example.com", Name: "A"}, PartStat: calendar.PartStatDeclined, Role: "OPT-PARTICIPANT"},
			{Person: calendar.Person{Email: "b@example.com"}, PartStat: calendar.PartStatNeedsAction, RSVP: true},
		},
		AttendeesTruncated: true,
		RRule:              "FREQ=DAILY;COUNT=5",
		RDates:             []time.Time{time.Date(2026, 12, 1, 17, 0, 0, 0, time.UTC)},
		ExDates:            []time.Time{time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC)},
		Updated:            time.Date(2026, 11, 1, 1, 2, 3, 4000, time.UTC),
		Raw:                []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"),
	}
	allDay := single("allday", time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC))
	allDay.AllDay = true
	allDay.Status = ""
	allDay.Raw = nil
	apply(t, s, "work", calendar.Changes{Upserts: []calendar.Event{ev, allDay}, Token: "t1", Full: true})

	ref := calendar.EventRef("acme", "work", "full")
	got, err := s.Event(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	want := calcache.EventRow{Ref: ref, Account: "acme", CalendarID: "work", Event: ev, MyPartStat: "", IsOrganizer: true}
	same(t, "Event", want, got)
	gotDay, err := s.Event(ctx, calendar.EventRef("acme", "work", "allday"))
	if err != nil {
		t.Fatal(err)
	}
	same(t, "all-day event", allDay, gotDay.Event)

	cal, err := s.Calendar(ctx, "acme", "work")
	if err != nil {
		t.Fatal(err)
	}
	if cal.SyncToken != "t1" || !cal.LastSync.Equal(t0) || cal.LastError != "" {
		t.Errorf("calendar after Apply = %+v", cal)
	}
}

func TestApplyClearsLastError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	if err := s.SetError(ctx, "acme", "work", "calendar server unavailable"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Calendar(ctx, "acme", "work"); c.LastError != "calendar server unavailable" {
		t.Errorf("LastError = %q after SetError", c.LastError)
	}
	apply(t, s, "work", calendar.Changes{Token: "t2"})
	if c, _ := s.Calendar(ctx, "acme", "work"); c.LastError != "" || c.SyncToken != "t2" {
		t.Errorf("calendar after Apply = %+v, want the error cleared and the token kept", c)
	}
	if err := s.SetError(ctx, "acme", "nope", "x"); !errors.Is(err, calcache.ErrNotFound) {
		t.Errorf("SetError on an unknown calendar = %v, want ErrNotFound", err)
	}
	err := s.Apply(ctx, "acme", "nope", calendar.Changes{Token: "t"}, self, t0)
	if !errors.Is(err, calcache.ErrNotFound) {
		t.Errorf("Apply on an unknown calendar = %v, want ErrNotFound", err)
	}
}

// TestApplyRollsBackAnInvalidSync covers an Apply that fails part way: the
// token, LastSync, LastError and stored events stay as they were, so the
// next sync repeats the same changes from the old token.
func TestApplyRollsBackAnInvalidSync(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	apply(t, s, "work", calendar.Changes{Full: true, Token: "t1", Upserts: []calendar.Event{
		single("a", t0, t0.Add(time.Hour)),
		single("b", t0, t0.Add(time.Hour)),
	}})
	if err := s.SetError(ctx, "acme", "work", "calendar server unavailable"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Calendar(ctx, "acme", "work")
	if err != nil {
		t.Fatal(err)
	}
	beforeRows, err := s.Range(ctx, nil, nil, t0.AddDate(-1, 0, 0), t0.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}

	changed := single("b", t0.Add(time.Hour), t0.Add(2*time.Hour))
	changed.ETag = "e-b2"
	err = s.Apply(ctx, "acme", "work", calendar.Changes{
		Token:   "t2",
		Upserts: []calendar.Event{single("c", t0, t0.Add(time.Hour)), changed, {UID: "no-provider@example.com", Start: t0, End: t0}},
		Deletes: []string{"a"},
	}, self, t0.Add(time.Hour))
	if err == nil {
		t.Fatal("Apply with an upsert without a ProviderID succeeded, want an error")
	}

	after, err := s.Calendar(ctx, "acme", "work")
	if err != nil {
		t.Fatal(err)
	}
	same(t, "calendar after a failed Apply", before, after)
	if after.SyncToken != "t1" || !after.LastSync.Equal(t0) || after.LastError != "calendar server unavailable" {
		t.Errorf("calendar after a failed Apply = token %q, last sync %v, error %q; want t1, %v and the old error",
			after.SyncToken, after.LastSync, after.LastError, t0)
	}
	afterRows, err := s.Range(ctx, nil, nil, t0.AddDate(-1, 0, 0), t0.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	same(t, "events after a failed Apply", beforeRows, afterRows)
}

func TestMyPartStatAndIsOrganizer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	mine := single("mine", t0, t0.Add(time.Hour))
	mine.Organizer = &calendar.Person{Email: "James@Example.com"}
	mine.Attendees = []calendar.Attendee{{Person: calendar.Person{Email: "x@example.com"}, PartStat: calendar.PartStatAccepted}}
	noPartStat := invited("blank", t0, "")
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{
		invited("asked", t0, calendar.PartStatNeedsAction),
		invited("yes", t0, calendar.PartStatAccepted),
		mine,
		single("plain", t0, t0.Add(time.Hour)),
		noPartStat,
	}})
	tests := []struct {
		provider string
		ps       calendar.PartStat
		org      bool
	}{
		{"asked", calendar.PartStatNeedsAction, false},
		{"yes", calendar.PartStatAccepted, false},
		{"mine", "", true},
		{"plain", "", false},
		{"blank", calendar.PartStatNeedsAction, false},
	}
	for _, tt := range tests {
		row, err := s.Event(ctx, calendar.EventRef("acme", "work", tt.provider))
		if err != nil {
			t.Fatalf("%s: %v", tt.provider, err)
		}
		if row.MyPartStat != tt.ps || row.IsOrganizer != tt.org {
			t.Errorf("%s: MyPartStat %q IsOrganizer %v; want %q %v", tt.provider, row.MyPartStat, row.IsOrganizer, tt.ps, tt.org)
		}
	}

	// A nil self knows no address of James.
	if err := s.Apply(ctx, "acme", "home", calendar.Changes{Full: true, Upserts: []calendar.Event{invited("asked", t0, calendar.PartStatNeedsAction)}}, nil, t0); err != nil {
		t.Fatal(err)
	}
	row, err := s.Event(ctx, calendar.EventRef("acme", "home", "asked"))
	if err != nil || row.MyPartStat != "" || row.IsOrganizer {
		t.Errorf("row with nil self = %+v, %v; want no part stat", row, err)
	}
}

func TestApplyObjectReplace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	master := weekly("series", t0.AddDate(0, 0, -21))
	rid1 := master.Start.AddDate(0, 0, 7)
	rid2 := master.Start.AddDate(0, 0, 28)
	apply(t, s, "work", calendar.Changes{Full: true, ObjectReplace: true, Upserts: []calendar.Event{
		master, override(master, rid1, time.Hour), override(master, rid2, time.Hour), single("other", t0, t0.Add(time.Hour)),
	}})
	// The object now has the master and the second override only.
	apply(t, s, "work", calendar.Changes{ObjectReplace: true, Upserts: []calendar.Event{master, override(master, rid2, 2*time.Hour)}})
	rows, err := s.Series(ctx, "acme", "work", "series")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"series", "series@" + rid2.Format("20060102T150405Z")}
	same(t, "Series", want, keys(rows))
	if !rows[1].Event.Start.Equal(rid2.Add(2 * time.Hour)) {
		t.Errorf("override start = %v, want it moved by two hours", rows[1].Event.Start)
	}
	if rows[1].Ref != calendar.InstanceRef(calendar.EventRef("acme", "work", "series"), rid2) {
		t.Errorf("override ref = %q, want the instance ref", rows[1].Ref)
	}
	if _, err := s.Event(ctx, calendar.EventRef("acme", "work", "other")); err != nil {
		t.Errorf("an object not in the changes was touched: %v", err)
	}
}

func TestApplyRowReplace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	master := weekly("series", t0.AddDate(0, 0, -21))
	rid1 := master.Start.AddDate(0, 0, 7)
	rid2 := master.Start.AddDate(0, 0, 28)
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{master, override(master, rid1, time.Hour)}})
	// Google sends only the row that changed: a new override. The master and
	// the first override stay.
	cancelled := override(master, rid2, 0)
	cancelled.Status = calendar.StatusCancelled
	apply(t, s, "work", calendar.Changes{Upserts: []calendar.Event{cancelled}})
	rows, err := s.Series(ctx, "acme", "work", "series")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"series", "series@" + rid1.Format("20060102T150405Z"), "series@" + rid2.Format("20060102T150405Z")}
	same(t, "Series", want, keys(rows))
	if rows[2].Event.Status != calendar.StatusCancelled {
		t.Errorf("new override status = %q, want CANCELLED", rows[2].Event.Status)
	}
}

func TestApplyDeletesAndFullReplace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	master := weekly("series", t0.AddDate(0, 0, -21))
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{
		master, override(master, master.Start.AddDate(0, 0, 7), time.Hour),
		single("a", t0, t0.Add(time.Hour)), single("b", t0, t0.Add(time.Hour)), single("c", t0, t0.Add(time.Hour)),
	}})
	apply(t, s, "home", calendar.Changes{Full: true, Upserts: []calendar.Event{single("a", t0, t0.Add(time.Hour))}})

	// Deleting a ProviderID removes every row of it.
	apply(t, s, "work", calendar.Changes{Deletes: []string{"series", "a", "missing"}})
	if rows, err := s.Series(ctx, "acme", "work", "series"); err != nil || len(rows) != 0 {
		t.Errorf("Series after delete = %v, %v; want none", keys(rows), err)
	}
	if _, err := s.Event(ctx, calendar.EventRef("acme", "work", "a")); !errors.Is(err, calcache.ErrNotFound) {
		t.Errorf("deleted single: %v, want ErrNotFound", err)
	}
	if _, err := s.Event(ctx, calendar.EventRef("acme", "home", "a")); err != nil {
		t.Errorf("the same ProviderID in another calendar went too: %v", err)
	}

	// A full sync replaces the whole calendar.
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{single("c", t0, t0.Add(2*time.Hour)), single("d", t0, t0.Add(time.Hour))}})
	rows, err := s.Range(ctx, []string{"acme"}, []string{"work"}, t0.AddDate(-1, 0, 0), t0.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	same(t, "rows after a full sync", []string{"c", "d"}, keys(rows))
	if !rows[0].Event.End.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("c end = %v, want the new end", rows[0].Event.End)
	}
}

func TestEventLookups(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	master := weekly("series", t0.AddDate(0, 0, -21))
	rid := master.Start.AddDate(0, 0, 7)
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{master, override(master, rid, time.Hour)}})
	copied := master
	copied.ProviderID = "copy"
	apply(t, s, "home", calendar.Changes{Full: true, Upserts: []calendar.Event{copied}})

	base := calendar.EventRef("acme", "work", "series")
	row, err := s.Event(ctx, calendar.InstanceRef(base, rid))
	if err != nil {
		t.Fatal(err)
	}
	if !row.Event.RecurrenceID.Equal(rid) || row.Event.Summary != "moved" {
		t.Errorf("override by instance ref = %+v", row.Event)
	}
	if row, err := s.Event(ctx, base); err != nil || !row.Event.RecurrenceID.IsZero() {
		t.Errorf("master by base ref = %+v, %v", row.Event, err)
	}
	if _, err := s.Event(ctx, calendar.InstanceRef(base, rid.AddDate(0, 0, 7))); !errors.Is(err, calcache.ErrNotFound) {
		t.Errorf("instance ref with no stored override = %v, want ErrNotFound", err)
	}
	if _, err := s.Event(ctx, "not a ref"); !errors.Is(err, calcache.ErrNotFound) {
		t.Errorf("garbage ref = %v, want ErrNotFound", err)
	}

	byUID, err := s.ByUID(ctx, "acme", master.UID)
	if err != nil {
		t.Fatal(err)
	}
	same(t, "ByUID", []string{"copy", "series", "series@" + rid.Format("20060102T150405Z")}, keys(byUID))
	if rows, err := s.ByUID(ctx, "other", master.UID); err != nil || len(rows) != 0 {
		t.Errorf("ByUID for another account = %v, %v; want none", keys(rows), err)
	}
}

func TestRangeReturnsWhatExpandNeeds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	from, to := t0, t0.AddDate(0, 0, 7)

	master := weekly("weekly", t0.AddDate(-1, 0, 0))                   // started a year ago
	pastOverride := override(master, master.Start.AddDate(0, 0, 7), 0) // long ago, but part of the series
	inOverride := override(master, master.Start.AddDate(0, 0, 7*53), 2*time.Hour)
	futureMaster := weekly("future", to.Add(time.Hour)) // starts after the range
	rdateOnly := single("rdates", t0.AddDate(0, -2, 0), t0.AddDate(0, -2, 0).Add(time.Hour))
	rdateOnly.RDates = []time.Time{t0.Add(48 * time.Hour)}
	movedIn := override(futureMaster, futureMaster.Start.AddDate(0, 0, 7), -10*24*time.Hour) // moved into the range
	zero := single("zero", from, from)                                                       // zero length at from
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{
		master, pastOverride, inOverride,
		futureMaster, movedIn,
		rdateOnly,
		single("inside", t0.Add(24*time.Hour), t0.Add(25*time.Hour)),
		single("straddle", t0.Add(-time.Hour), t0.Add(time.Hour)),
		single("before", t0.Add(-2*time.Hour), t0),
		single("after", to, to.Add(time.Hour)),
		zero,
	}})
	apply(t, s, "home", calendar.Changes{Full: true, Upserts: []calendar.Event{single("homeone", t0.Add(time.Hour), t0.Add(2*time.Hour))}})
	if err := s.SetCalendars(ctx, "beta", []calendar.Calendar{{ID: "work", Access: calendar.AccessWrite}}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, "beta", "work", calendar.Changes{Full: true, Upserts: []calendar.Event{single("betaone", t0.Add(time.Hour), t0.Add(2*time.Hour))}}, self, t0); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Range(ctx, []string{"acme"}, []string{"work"}, from, to)
	if err != nil {
		t.Fatal(err)
	}
	got := keys(rows)
	slices.Sort(got)
	want := []string{
		"future@" + futureMaster.Start.AddDate(0, 0, 7).Format("20060102T150405Z"),
		"inside",
		"rdates",
		"straddle",
		"weekly",
		"weekly@" + pastOverride.RecurrenceID.Format("20060102T150405Z"),
		"weekly@" + inOverride.RecurrenceID.Format("20060102T150405Z"),
		"zero",
	}
	slices.Sort(want)
	same(t, "Range", want, got)

	// The rows expand to the instances in the range.
	var series []calendar.Event
	for _, r := range rows {
		if r.Event.ProviderID == "weekly" {
			series = append(series, r.Event)
		}
	}
	insts, _, err := calendar.Expand(series, from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 1 || insts[0].Event.Summary != "moved" {
		t.Errorf("weekly instances in the range = %+v, want the moved override only", insts)
	}

	all, err := s.Range(ctx, nil, nil, from, to)
	if err != nil {
		t.Fatal(err)
	}
	accounts := map[string]bool{}
	for _, r := range all {
		accounts[r.Account+"/"+r.CalendarID] = true
	}
	if !accounts["acme/home"] || !accounts["beta/work"] || !accounts["acme/work"] {
		t.Errorf("Range with no filters covered %v, want every calendar", accounts)
	}
	beta, err := s.Range(ctx, []string{"beta"}, nil, from, to)
	if err != nil {
		t.Fatal(err)
	}
	same(t, "Range(beta)", []string{"betaone"}, keys(beta))
	if empty, err := s.Range(ctx, nil, nil, to, from); err != nil || len(empty) != 0 {
		t.Errorf("Range with to before from = %v, %v; want none", keys(empty), err)
	}
}

func TestPruneKeepsMasters(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	master := weekly("weekly", t0.AddDate(-1, 0, 0))
	oldOverride := override(master, master.Start.AddDate(0, 0, 7), 0)
	newOverride := override(master, t0.AddDate(0, 0, 3), 0)
	rdateOnly := single("rdates", t0.AddDate(0, -2, 0), t0.AddDate(0, -2, 0).Add(time.Hour))
	rdateOnly.RDates = []time.Time{t0.Add(48 * time.Hour)}
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{
		master, oldOverride, newOverride, rdateOnly,
		single("old", t0.AddDate(0, -3, 0), t0.AddDate(0, -3, 0).Add(time.Hour)),
		single("edge", t0.Add(-time.Hour), t0),
		single("new", t0, t0.Add(time.Hour)),
	}})
	n, err := s.Prune(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Prune removed %d rows, want 2 (old single, old override)", n)
	}
	rows, err := s.Range(ctx, nil, nil, t0.AddDate(-2, 0, 0), t0.AddDate(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	got := keys(rows)
	slices.Sort(got)
	want := []string{"edge", "new", "rdates", "weekly", "weekly@" + newOverride.RecurrenceID.Format("20060102T150405Z")}
	same(t, "rows after Prune", want, got)
}

// TestPruneKeepsOccurrencesMovedEarlier covers an override whose own end is
// before the cutoff while the occurrence it replaces (its RECURRENCE-ID) is
// after it. Deleting it would let the master put the original occurrence
// back, so Prune keeps it (Amendment 18).
func TestPruneKeepsOccurrencesMovedEarlier(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	master := weekly("weekly", t0.AddDate(-1, 0, 0))
	rid := master.Start.AddDate(0, 0, 7*53) // the first occurrence after t0
	if !rid.After(t0) {
		t.Fatalf("recurrence ID %v is not after the cutoff %v", rid, t0)
	}
	moved := override(master, rid, -(rid.Sub(t0) + 2*time.Hour)) // ends 90 minutes before t0
	if !moved.End.Before(t0) {
		t.Fatalf("moved override ends %v, want before the cutoff %v", moved.End, t0)
	}
	// Ended and replaced before the cutoff: pruned.
	gone := override(master, master.Start.AddDate(0, 0, 7*50), -time.Hour)
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{master, moved, gone}})

	n, err := s.Prune(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Prune removed %d rows, want 1 (the override replacing a past occurrence)", n)
	}
	series, err := s.Series(ctx, "acme", "work", "weekly")
	if err != nil {
		t.Fatal(err)
	}
	same(t, "series after Prune", []string{"weekly", "weekly@" + rid.Format("20060102T150405Z")}, keys(series))

	events := make([]calendar.Event, 0, len(series))
	for _, r := range series {
		events = append(events, r.Event)
	}
	insts, _, err := calendar.Expand(events, rid.Add(-time.Hour), rid.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(insts) != 0 {
		t.Errorf("instances at the moved occurrence's original time = %+v, want none", insts)
	}
}

func TestStatsAndReady(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	ready, err := s.Ready(ctx, "acme")
	if err != nil || ready {
		t.Errorf("Ready before MarkReady = %v, %v; want false", ready, err)
	}
	master := weekly("weekly", t0)
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{master, override(master, t0.AddDate(0, 0, 7), 0), single("a", t0, t0)}})
	apply(t, s, "home", calendar.Changes{Full: true, Upserts: []calendar.Event{single("b", t0, t0)}})
	cals, events, err := s.Stats(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if cals != 2 || events != 4 {
		t.Errorf("Stats = %d calendars, %d events; want 2, 4", cals, events)
	}
	if err := s.MarkReady(ctx, "acme", t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkReady(ctx, "acme", t0.Add(time.Hour)); err != nil {
		t.Fatalf("MarkReady again: %v", err)
	}
	if ready, err := s.Ready(ctx, "acme"); err != nil || !ready {
		t.Errorf("Ready after MarkReady = %v, %v; want true", ready, err)
	}
	if ready, err := s.Ready(ctx, "beta"); err != nil || ready {
		t.Errorf("Ready(beta) = %v, %v; want false", ready, err)
	}
}

// Rederive recomputes MyPartStat and IsOrganizer of an account's stored rows
// with a new self, so rows a wider self stored are put right without a
// full sync.
func TestRederive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	// The alias organizes it and invited James, listed first; the wide self
	// counts both as James.
	cross := single("cross", t0, t0.Add(time.Hour))
	cross.Organizer = &calendar.Person{Email: "j@alias.example"}
	cross.Attendees = []calendar.Attendee{
		{Person: calendar.Person{Email: me}, PartStat: calendar.PartStatNeedsAction},
		{Person: calendar.Person{Email: "j@alias.example"}, PartStat: calendar.PartStatAccepted},
	}
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{cross, invited("asked", t0, calendar.PartStatNeedsAction)}})
	row, err := s.Event(ctx, calendar.EventRef("acme", "work", "cross"))
	if err != nil || !row.IsOrganizer {
		t.Fatalf("before: %+v, %v; want the wide self's organizer", row, err)
	}

	alias := func(addr string) bool { return strings.EqualFold(addr, "j@alias.example") }
	n, err := s.Rederive(ctx, "acme", alias)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Rederive changed %d rows, want 2", n)
	}
	row, err = s.Event(ctx, calendar.EventRef("acme", "work", "cross"))
	if err != nil || !row.IsOrganizer || row.MyPartStat != calendar.PartStatAccepted {
		t.Errorf("cross for the alias = %+v, %v; want organizer, ACCEPTED", row, err)
	}
	row, err = s.Event(ctx, calendar.EventRef("acme", "work", "asked"))
	if err != nil || row.IsOrganizer || row.MyPartStat != "" {
		t.Errorf("asked for the alias = %+v, %v; want no part stat", row, err)
	}

	mine := func(addr string) bool { return strings.EqualFold(addr, me) }
	if _, err := s.Rederive(ctx, "acme", mine); err != nil {
		t.Fatal(err)
	}
	row, err = s.Event(ctx, calendar.EventRef("acme", "work", "cross"))
	if err != nil || row.IsOrganizer || row.MyPartStat != calendar.PartStatNeedsAction {
		t.Errorf("cross for James = %+v, %v; want an invite", row, err)
	}
	// Nothing to change is no write.
	if n, err := s.Rederive(ctx, "acme", mine); err != nil || n != 0 {
		t.Errorf("second Rederive = %d, %v; want 0", n, err)
	}
	// An account with no rows changes nothing.
	if n, err := s.Rederive(ctx, "nobody", alias); err != nil || n != 0 {
		t.Errorf("Rederive of an empty account = %d, %v", n, err)
	}
}

// Rederive keys each write by the whole row: an override of a series gets
// its own roles, a row with no organizer or attendees has none, and
// another account's rows keep theirs.
func TestRederiveOverridesAndOtherAccounts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	if err := s.SetCalendars(ctx, "beta", []calendar.Calendar{{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true}}, "work"); err != nil {
		t.Fatal(err)
	}
	master := weekly("series", t0)
	master.Organizer = &calendar.Person{Email: "boss@example.com"}
	rid := t0.AddDate(0, 0, 7)
	ov := override(master, rid, time.Hour)
	ov.Organizer = &calendar.Person{Email: "j@alias.example"}
	ov.Attendees = []calendar.Attendee{
		{Person: calendar.Person{Email: me}, PartStat: calendar.PartStatTentative},
		{Person: calendar.Person{Email: "j@alias.example"}, PartStat: calendar.PartStatAccepted},
	}
	ch := calendar.Changes{Full: true, Upserts: []calendar.Event{master, ov, single("bare", t0, t0.Add(time.Hour))}}
	apply(t, s, "work", ch)
	if err := s.Apply(ctx, "beta", "work", ch, self, t0); err != nil {
		t.Fatal(err)
	}

	mine := func(addr string) bool { return strings.EqualFold(addr, me) }
	n, err := s.Rederive(ctx, "acme", mine)
	if err != nil || n != 1 {
		t.Fatalf("Rederive = %d, %v; want only the override changed", n, err)
	}
	ovRef := calendar.InstanceRef(calendar.EventRef("acme", "work", "series"), rid)
	if row, err := s.Event(ctx, ovRef); err != nil || row.IsOrganizer || row.MyPartStat != calendar.PartStatTentative {
		t.Errorf("override = %+v, %v; want James's TENTATIVE, not organizer", row, err)
	}
	if row, err := s.Event(ctx, calendar.EventRef("acme", "work", "series")); err != nil || row.IsOrganizer || row.MyPartStat != "" {
		t.Errorf("master = %+v, %v; want no role", row, err)
	}
	if row, err := s.Event(ctx, calendar.EventRef("acme", "work", "bare")); err != nil || row.IsOrganizer || row.MyPartStat != "" {
		t.Errorf("bare = %+v, %v; want no role", row, err)
	}
	other := calendar.InstanceRef(calendar.EventRef("beta", "work", "series"), rid)
	if row, err := s.Event(ctx, other); err != nil || !row.IsOrganizer {
		t.Errorf("beta's override = %+v, %v; want the roles it was stored with", row, err)
	}
}
