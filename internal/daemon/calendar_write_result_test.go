package daemon_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	calfake "github.com/yaklabco/chainmail/internal/calstore/fake"
)

// A write that succeeded is reported as written even when the sync after
// it fails: the response carries the store's new version, and a keyed
// answer stores that version for its replays.
func TestWriteResultSurvivesAFailedSync(t *testing.T) {
	t.Parallel()
	var alone, invited string
	e := startCal(t, func(e *calEnv) {
		alone = e.mw.Add(mwCal, own("alone@x", "james@mw.example", e.start))
		invited = e.mw.Add(mwCal, invitation("inv@x", "james@mw.example", e.start))
	})
	ref := calendar.EventRef("mw", mwCal, alone)
	etag := e.etagOf(t, ref)
	e.mw.Fail(calfake.MethodSync, calstore.ErrUnavailable)

	up, err := e.update(t, &chainmailv1.UpdateEventRequest{Ref: ref, Etag: etag, UpdateMask: mask("title"), Title: "Renamed"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := e.mw.Object(mwCal, alone)
	if ev := up.GetEvent(); ev.GetSummary().GetUntrusted() != "Renamed" || ev.GetEtag() != rows[0].ETag || ev.GetEtag() == etag || ev.GetRef() != ref {
		t.Errorf("update response = %v, want the new title and etag %s", ev, rows[0].ETag)
	}

	iref := calendar.EventRef("mw", mwCal, invited)
	req := &chainmailv1.RespondRequest{Ref: iref, Response: "accept", IdempotencyKey: "rsvp-sync"}
	got, err := e.respond(t, req)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ = e.mw.Object(mwCal, invited)
	if ev := got.GetEvent(); ev.GetMyPartstat() != "ACCEPTED" || ev.GetEtag() != rows[0].ETag || ev.GetRef() != iref {
		t.Errorf("respond response = %v, want ACCEPTED with etag %s", ev, rows[0].ETag)
	}
	again, err := e.respond(t, req)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(again, got) || e.mw.Count(calfake.MethodRespond) != 1 {
		t.Errorf("replay = %v, want the stored answer %v", again, got)
	}
}

// The answer to a write that the sync after it missed describes the
// series the store wrote: a rule added or removed shows in recurring and
// recurrence_rule, and an instance keeps its series's rule.
func TestWriteResultKeepsTheWrittenRecurrence(t *testing.T) {
	t.Parallel()
	var alone, weekly, daily, invited string
	e := startCal(t, func(e *calEnv) {
		alone = e.mw.Add(mwCal, own("alone@x", "james@mw.example", e.start))
		master := own("weekly@x", "james@mw.example", e.start)
		master.RRule = "FREQ=WEEKLY;COUNT=3"
		weekly = e.mw.Add(mwCal, master)
		other := own("daily@x", "james@mw.example", e.start)
		other.RRule = "FREQ=DAILY"
		daily = e.mw.Add(mwCal, other)
		inv := invitation("inv@x", "james@mw.example", e.start)
		inv.RRule = "FREQ=DAILY;COUNT=5"
		invited = e.mw.Add(mwCal, inv)
	})
	ref := calendar.EventRef("mw", mwCal, alone)
	wref := calendar.EventRef("mw", mwCal, weekly)
	dref := calendar.EventRef("mw", mwCal, daily)
	second := e.start.Add(7 * 24 * time.Hour)
	iref := calendar.InstanceRef(wref, second)
	etags := map[string]string{ref: e.etagOf(t, ref), dref: e.etagOf(t, dref), iref: e.etagOf(t, iref)}
	e.mw.Fail(calfake.MethodSync, calstore.ErrUnavailable)

	for _, tt := range []struct {
		name      string
		req       *chainmailv1.UpdateEventRequest
		recurring bool
		rule      string
	}{
		{"rule added", &chainmailv1.UpdateEventRequest{Ref: ref, Etag: etags[ref], UpdateMask: mask("recurrence_rule"), RecurrenceRule: "FREQ=WEEKLY"}, true, "FREQ=WEEKLY"},
		{"instance renamed", &chainmailv1.UpdateEventRequest{Ref: iref, Etag: etags[iref], UpdateMask: mask("title"), Title: "Renamed"}, true, "FREQ=WEEKLY;COUNT=3"},
		{"rule removed", &chainmailv1.UpdateEventRequest{Ref: dref, Etag: etags[dref], UpdateMask: mask("recurrence_rule")}, false, ""},
	} {
		up, err := e.update(t, tt.req)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if ev := up.GetEvent(); ev.GetRecurring() != tt.recurring || ev.GetRecurrenceRule() != tt.rule || ev.GetEtag() == tt.req.GetEtag() || ev.GetRef() != tt.req.GetRef() {
			t.Errorf("%s: response = %v, want recurring %v with rule %q and a new etag", tt.name, ev, tt.recurring, tt.rule)
		}
	}

	day := calendar.InstanceRef(calendar.EventRef("mw", mwCal, invited), e.start.Add(24*time.Hour))
	got, err := e.respond(t, &chainmailv1.RespondRequest{Ref: day, Response: "accept"})
	if err != nil {
		t.Fatal(err)
	}
	if ev := got.GetEvent(); ev.GetMyPartstat() != "ACCEPTED" || !ev.GetRecurring() || ev.GetRecurrenceRule() != "FREQ=DAILY;COUNT=5" || ev.GetRef() != day {
		t.Errorf("instance answer = %v, want an accepted instance of the daily series", ev)
	}
}
