package daemon_test

import (
	"testing"

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
