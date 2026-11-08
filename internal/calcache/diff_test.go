package calcache_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore/fake"
)

// describe renders a change as "kind key", with the key from After or,
// for a delete, Before.
func describe(c calcache.Change) string {
	row := c.After
	if row == nil {
		row = c.Before
	}
	return fmt.Sprintf("%s %s", c.Kind, keys([]calcache.EventRow{*row})[0])
}

func describeAll(cs []calcache.Change) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, describe(c))
	}
	return out
}

func TestDiffKinds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	master := weekly("series", t0.AddDate(0, 0, -21))
	rid1 := master.Start.AddDate(0, 0, 7)
	rid2 := master.Start.AddDate(0, 0, 28)
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{
		master, override(master, rid1, time.Hour),
		invited("inv", t0, calendar.PartStatNeedsAction),
		single("same", t0, t0.Add(time.Hour)),
		single("gone", t0, t0.Add(time.Hour)),
	}})
	rid1Key := "series@" + rid1.Format("20060102T150405Z")
	rid2Key := "series@" + rid2.Format("20060102T150405Z")

	moved := invited("inv", t0.Add(time.Hour), calendar.PartStatAccepted)
	tests := []struct {
		name string
		ch   calendar.Changes
		want []string
	}{
		{
			name: "nothing changed",
			ch:   calendar.Changes{Upserts: []calendar.Event{single("same", t0, t0.Add(time.Hour))}},
			want: []string{},
		},
		{
			name: "row replace: created, updated, deleted",
			ch: calendar.Changes{
				Upserts: []calendar.Event{moved, single("new", t0, t0.Add(time.Hour)), override(master, rid2, 0)},
				Deletes: []string{"gone", "unknown"},
			},
			want: []string{"deleted gone", "updated inv", "created new", "created " + rid2Key},
		},
		{
			name: "object replace drops the override not sent",
			ch:   calendar.Changes{ObjectReplace: true, Upserts: []calendar.Event{master}},
			want: []string{"deleted " + rid1Key},
		},
		{
			name: "deleting a series deletes every row",
			ch:   calendar.Changes{Deletes: []string{"series"}},
			want: []string{"deleted series", "deleted " + rid1Key},
		},
		{
			name: "full sync deletes what it does not list",
			ch:   calendar.Changes{Full: true, Upserts: []calendar.Event{single("same", t0, t0.Add(time.Hour))}},
			want: []string{"deleted gone", "deleted inv", "deleted series", "deleted " + rid1Key},
		},
		{
			name: "an etag alone is an update",
			ch: func() calendar.Changes {
				ev := single("same", t0, t0.Add(time.Hour))
				ev.ETag = "e2"
				return calendar.Changes{Upserts: []calendar.Event{ev}}
			}(),
			want: []string{"updated same"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := s.Diff(ctx, "acme", "work", tt.ch, self)
			if err != nil {
				t.Fatal(err)
			}
			same(t, "Diff", tt.want, describeAll(got))
		})
	}

	// Diff writes nothing.
	if _, err := s.Event(ctx, calendar.EventRef("acme", "work", "gone")); err != nil {
		t.Errorf("Diff removed a row: %v", err)
	}
}

func TestDiffBeforeAndAfter(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{
		invited("inv", t0, calendar.PartStatNeedsAction),
		single("gone", t0, t0.Add(time.Hour)),
	}})
	stored, err := s.Event(ctx, calendar.EventRef("acme", "work", "inv"))
	if err != nil {
		t.Fatal(err)
	}
	accepted := invited("inv", t0, calendar.PartStatAccepted)
	accepted.Sequence = 1
	changes, err := s.Diff(ctx, "acme", "work", calendar.Changes{
		Upserts: []calendar.Event{accepted, invited("fresh", t0, calendar.PartStatNeedsAction)},
		Deletes: []string{"gone"},
	}, self)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("Diff = %v, want three changes", describeAll(changes))
	}
	// Changes come sorted by ProviderID, then recurrence ID.
	add, del, upd := changes[0], changes[1], changes[2]

	if del.Kind != calcache.ChangeDeleted || del.After != nil || del.Before == nil || del.Before.Event.ProviderID != "gone" {
		t.Errorf("delete = %+v", del)
	}
	if upd.Kind != calcache.ChangeUpdated || upd.Before == nil || upd.After == nil {
		t.Fatalf("update = %+v", upd)
	}
	same(t, "update Before", stored, *upd.Before)
	if upd.Before.MyPartStat != calendar.PartStatNeedsAction || upd.After.MyPartStat != calendar.PartStatAccepted {
		t.Errorf("MyPartStat before %q after %q, want NEEDS-ACTION then ACCEPTED", upd.Before.MyPartStat, upd.After.MyPartStat)
	}
	if upd.After.Event.Sequence != 1 || upd.After.Ref != stored.Ref || upd.After.Account != "acme" || upd.After.CalendarID != "work" {
		t.Errorf("update After = %+v", upd.After)
	}
	if add.Kind != calcache.ChangeCreated || add.Before != nil || add.After == nil {
		t.Fatalf("create = %+v", add)
	}
	if add.After.Ref != calendar.EventRef("acme", "work", "fresh") || add.After.MyPartStat != calendar.PartStatNeedsAction {
		t.Errorf("create After = %+v", add.After)
	}

	// Applying the same changes stores exactly what After described.
	if err := s.Apply(ctx, "acme", "work", calendar.Changes{
		Upserts: []calendar.Event{accepted, invited("fresh", t0, calendar.PartStatNeedsAction)},
		Deletes: []string{"gone"},
	}, self, t0); err != nil {
		t.Fatal(err)
	}
	got, err := s.Event(ctx, stored.Ref)
	if err != nil {
		t.Fatal(err)
	}
	same(t, "stored after Apply", *upd.After, got)
	again, err := s.Diff(ctx, "acme", "work", calendar.Changes{Upserts: []calendar.Event{accepted}}, self)
	if err != nil || len(again) != 0 {
		t.Errorf("Diff after Apply = %v, %v; want no changes", describeAll(again), err)
	}
}

// TestDiffSelfChangesPartStat checks that MyPartStat follows the self
// function: an address that becomes James's own is an update.
func TestDiffSelfChangesPartStat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	ev := invited("inv", t0, calendar.PartStatNeedsAction)
	if err := s.Apply(ctx, "acme", "work", calendar.Changes{Full: true, Upserts: []calendar.Event{ev}}, func(string) bool { return false }, t0); err != nil {
		t.Fatal(err)
	}
	changes, err := s.Diff(ctx, "acme", "work", calendar.Changes{Upserts: []calendar.Event{ev}}, self)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Kind != calcache.ChangeUpdated || changes[0].After.MyPartStat != calendar.PartStatNeedsAction {
		t.Errorf("Diff = %+v, want one update with MyPartStat set", describeAll(changes))
	}
}

// TestSyncLoopWithFake runs the loop a syncer runs, Sync then Diff then
// Apply, against calstore/fake in both replace modes, and checks the
// changes each step reports.
func TestSyncLoopWithFake(t *testing.T) {
	t.Parallel()
	for _, objectReplace := range []bool{false, true} {
		t.Run(fmt.Sprintf("ObjectReplace=%v", objectReplace), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			s := setup(t)
			store := fake.New(fake.Options{ObjectReplace: objectReplace, Now: func() time.Time { return t0 }})
			store.AddCalendar(calendar.Calendar{ID: "work", Access: calendar.AccessWrite})
			token := ""
			step := func(what string, want ...string) {
				t.Helper()
				ch, err := store.Sync(ctx, "work", token)
				if err != nil {
					t.Fatal(err)
				}
				changes, err := s.Diff(ctx, "acme", "work", ch, self)
				if err != nil {
					t.Fatal(err)
				}
				if want == nil {
					want = []string{}
				}
				same(t, what, want, describeAll(changes))
				if err := s.Apply(ctx, "acme", "work", ch, self, t0); err != nil {
					t.Fatal(err)
				}
				token = ch.Token
			}

			series := invited("series", t0, calendar.PartStatNeedsAction)
			series.ProviderID = ""
			series.RRule = "FREQ=DAILY"
			p := store.Add("work", series)
			step("first sync", "created "+p)
			step("no change")

			rid := t0.AddDate(0, 0, 1)
			ridKey := p + "@" + rid.Format("20060102T150405Z")
			store.SetPartStat("work", p, rid, me, calendar.PartStatDeclined)
			// The etag of the whole object changes, so the master is updated
			// too.
			step("instance declined", "updated "+p, "created "+ridKey)
			row, err := s.Event(ctx, calendar.InstanceRef(calendar.EventRef("acme", "work", p), rid))
			if err != nil || row.MyPartStat != calendar.PartStatDeclined {
				t.Errorf("override = %+v, %v; want DECLINED", row, err)
			}

			store.Change("work", p, func(evs []calendar.Event) []calendar.Event { return evs[:1] })
			if objectReplace {
				step("override dropped", "updated "+p, "deleted "+ridKey)
			} else {
				// Row replace cannot tell that an override went away.
				step("override dropped", "updated "+p)
			}

			store.Cancel("work", p)
			step("cancelled", "updated "+p)
			store.Remove("work", p)
			if objectReplace {
				step("removed", "deleted "+p)
			} else {
				step("removed", "deleted "+p, "deleted "+ridKey)
			}

			store.InvalidateTokens()
			if _, err := store.Sync(ctx, "work", token); err == nil {
				t.Error("Sync with an invalidated token succeeded")
			}
			token = ""
			store.Add("work", single("again", t0, t0.Add(time.Hour)))
			step("full sync after invalidation", "created again")
		})
	}
}
