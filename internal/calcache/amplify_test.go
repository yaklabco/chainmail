package calcache_test

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
)

// manyOverrides is one CalDAV object: a daily master and n overrides,
// padded to about pad octets with a property chainmail does not model.
func manyOverrides(n, pad int) []byte {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\n")
	b.WriteString("BEGIN:VEVENT\r\nUID:many@example.com\r\nDTSTAMP:20261101T000000Z\r\n" +
		"DTSTART:20261101T090000Z\r\nDTEND:20261101T093000Z\r\nRRULE:FREQ=DAILY\r\nSUMMARY:Standup\r\n")
	for pad > 0 {
		chunk := min(pad, 70)
		b.WriteString("X-PAD:" + strings.Repeat("p", chunk) + "\r\n")
		pad -= chunk + 8
	}
	b.WriteString("END:VEVENT\r\n")
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	for i := range n {
		rid := start.AddDate(0, 0, i).Format("20060102T150405Z")
		moved := start.AddDate(0, 0, i).Add(time.Hour).Format("20060102T150405Z")
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:many@example.com\r\nDTSTAMP:20261101T000000Z\r\n"+
			"RECURRENCE-ID:%s\r\nDTSTART:%s\r\nDURATION:PT30M\r\nSUMMARY:Standup\r\nEND:VEVENT\r\n", rid, moved)
	}
	b.WriteString("END:VCALENDAR\r\n")
	return []byte(b.String())
}

// storedRawBytes sums the raw object bytes calendar.db holds, wherever the
// schema keeps them.
func storedRawBytes(t *testing.T, path string) int64 {
	t.Helper()
	var total, n int64
	rawQuery(t, path, "SELECT count(*) FROM pragma_table_info('cal_events') WHERE name = 'raw'", &n)
	if n > 0 {
		rawQuery(t, path, "SELECT coalesce(sum(length(raw)), 0) FROM cal_events", &total)
	}
	rawQuery(t, path, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'cal_raws'", &n)
	if n > 0 {
		var more int64
		rawQuery(t, path, "SELECT coalesce(sum(length(raw)), 0) FROM cal_raws", &more)
		total += more
	}
	return total
}

// TestSharedRawStoredOnce checks that an object with many overrides, whose
// events share one Raw, is stored and diffed without a copy of Raw per row.
func TestSharedRawStoredOnce(t *testing.T) {
	const overrides = 500
	data := manyOverrides(overrides, 100<<10)
	_, evs, err := calendar.ParseICS(data)
	if err != nil {
		t.Fatalf("ParseICS: %v", err)
	}
	if len(evs) != overrides+1 {
		t.Fatalf("parsed %d events, want %d", len(evs), overrides+1)
	}
	for i := range evs {
		evs[i].ProviderID = "/cal/many.ics"
		evs[i].ETag = "e1"
	}
	s, path := openCache(t)
	ctx := t.Context()
	if err := s.SetCalendars(ctx, "acme", []calendar.Calendar{{ID: "work", Access: calendar.AccessWrite}}, "work"); err != nil {
		t.Fatal(err)
	}
	ch := calendar.Changes{Upserts: evs, Token: "t1", Full: true, ObjectReplace: true}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := s.Diff(ctx, "acme", "work", ch, self); err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if err := s.Apply(ctx, "acme", "work", ch, self, t0); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	runtime.ReadMemStats(&after)
	if got, limit := after.TotalAlloc-before.TotalAlloc, uint64(24<<20); got > limit {
		t.Errorf("Diff and Apply allocated %d bytes for a %d byte object, want at most %d", got, len(data), limit)
	}
	if got := storedRawBytes(t, path); got > 2*int64(len(data)) {
		t.Errorf("calendar.db holds %d raw bytes for a %d byte object", got, len(data))
	}

	rows, err := s.Series(ctx, "acme", "work", "/cal/many.ics")
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(rows) != overrides+1 {
		t.Fatalf("Series = %d rows, want %d", len(rows), overrides+1)
	}
	for _, r := range rows {
		if string(r.Event.Raw) != string(data) {
			t.Fatalf("row %s does not carry the object's raw", r.Ref)
		}
	}
	if &rows[0].Event.Raw[0] != &rows[len(rows)-1].Event.Raw[0] {
		t.Error("rows of one object read back separate copies of its raw")
	}
}

// bigSeries is a weekly master with ProviderID p and n overrides, every
// row sharing one raw of rawLen octets unless own is set.
func bigSeries(p string, n, rawLen int, own bool) []calendar.Event {
	master := weekly(p, t0.AddDate(0, 0, -7))
	master.Raw = make([]byte, rawLen)
	evs := []calendar.Event{master}
	for i := range n {
		ov := override(master, master.Start.AddDate(0, 0, 7*(i+1)), time.Hour)
		if own {
			ov.Raw = make([]byte, rawLen)
		}
		evs = append(evs, ov)
	}
	return evs
}

func TestAdmit(t *testing.T) {
	t.Parallel()
	shared := bigSeries("shared", 1500, calendar.MaxICSBytes, false)
	tooMany := bigSeries("many", calendar.MaxObjectEvents, 10, false) // master + MaxObjectEvents overrides
	tooBig := bigSeries("big", 9, calendar.MaxICSBytes, true)
	small := single("small", t0, t0.Add(time.Hour))

	var upserts []calendar.Event
	upserts = append(upserts, shared...)
	upserts = append(upserts, tooMany...)
	upserts = append(upserts, tooBig...)
	upserts = append(upserts, small)
	ch := calendar.Changes{Upserts: upserts, Skipped: []string{"earlier"}, Full: true}
	got := calcache.Admit(ch)
	same(t, "Skipped", []string{"big", "earlier", "many"}, got.Skipped)
	if want := len(shared) + 1; len(got.Upserts) != want {
		t.Errorf("Admit kept %d upserts, want %d", len(got.Upserts), want)
	}
	for _, ev := range got.Upserts {
		if ev.ProviderID == "many" || ev.ProviderID == "big" {
			t.Fatalf("Admit kept an upsert of %s", ev.ProviderID)
		}
	}
	if len(ch.Upserts) != len(upserts) || len(ch.Skipped) != 1 {
		t.Error("Admit changed its argument")
	}
	if again := calcache.Admit(calendar.Changes{Upserts: shared}); again.Skipped != nil {
		t.Errorf("Admit skipped %v from a series under the limits", again.Skipped)
	}
}

// TestSkippedObjectsKeepTheirRows covers an object over the limits in a
// full sync: its stored rows stay as they are and are not changes, while
// the other objects sync.
func TestSkippedObjectsKeepTheirRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := setup(t)
	kept := weekly("many", t0.AddDate(0, 0, -7))
	apply(t, s, "work", calendar.Changes{Full: true, Upserts: []calendar.Event{kept, single("old", t0, t0.Add(time.Hour))}})

	grown := bigSeries("many", calendar.MaxObjectEvents, 10, false)
	ch := calendar.Changes{Full: true, Token: "t2", Upserts: append(grown, single("new", t0, t0.Add(time.Hour)))}
	changes, err := s.Diff(ctx, "acme", "work", ch, self)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	same(t, "changes", []string{"created new", "deleted old"}, describeAll(changes))
	apply(t, s, "work", ch)
	rows, err := s.Series(ctx, "acme", "work", "many")
	if err != nil {
		t.Fatal(err)
	}
	same(t, "rows of the skipped object", []string{"many"}, keys(rows))

	// A delete of a skipped object is not applied either.
	apply(t, s, "work", calendar.Changes{Token: "t3", Deletes: []string{"many"}, Skipped: []string{"many"}})
	if rows, _ := s.Series(ctx, "acme", "work", "many"); len(rows) != 1 {
		t.Errorf("a skipped object's delete removed its rows: %v", keys(rows))
	}
}

// TestRawsFollowTheirRows checks that a raw object no row names any more
// is dropped, by Apply and by Prune.
func TestRawsFollowTheirRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, path := openCache(t)
	if err := s.SetCalendars(ctx, "acme", []calendar.Calendar{{ID: "work", Access: calendar.AccessWrite}}, "work"); err != nil {
		t.Fatal(err)
	}
	count := func() int64 {
		var n int64
		rawQuery(t, path, "SELECT count(*) FROM cal_raws", &n)
		return n
	}
	old := single("p", t0.AddDate(0, 0, -60), t0.AddDate(0, 0, -60).Add(time.Hour))
	if err := s.Apply(ctx, "acme", "work", calendar.Changes{Full: true, Upserts: []calendar.Event{old, single("q", t0, t0.Add(time.Hour))}}, self, t0); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("cal_raws rows = %d, want 2", n)
	}
	changed := single("q", t0, t0.Add(time.Hour))
	changed.Raw = []byte("raw q, changed")
	if err := s.Apply(ctx, "acme", "work", calendar.Changes{Upserts: []calendar.Event{changed}}, self, t0); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Errorf("after a replaced raw, cal_raws rows = %d, want 2", n)
	}
	if _, err := s.Prune(ctx, t0.AddDate(0, 0, -30)); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Errorf("after Prune, cal_raws rows = %d, want 1", n)
	}
	row, err := s.Event(ctx, calendar.EventRef("acme", "work", "q"))
	if err != nil {
		t.Fatal(err)
	}
	if string(row.Event.Raw) != "raw q, changed" {
		t.Errorf("Raw = %q", row.Event.Raw)
	}
}
