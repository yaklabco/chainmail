package daemon_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	calfake "github.com/yaklabco/chainmail/internal/calstore/fake"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// An attendee's PARTSTAT comes from the calendar server; a watch event
// carries only the documented values, and any other as OTHER.
func TestWatchInviteReplyPartStatIsAllowlisted(t *testing.T) {
	t.Parallel()
	st := calfake.New(calfake.Options{ObjectReplace: true})
	st.AddCalendar(calendar.Calendar{ID: "/cal/work/", Name: "Work", Access: calendar.AccessWrite})
	start := time.Now().Add(48 * time.Hour).Truncate(time.Second).UTC()
	p := st.Add("/cal/work/", calendar.Event{
		UID: "mine@x", Summary: "SECRET", Start: start, End: start.Add(time.Hour),
		Organizer: &calendar.Person{Email: "james@mw.example"},
		Attendees: []calendar.Attendee{
			{Person: calendar.Person{Email: "james@mw.example"}, PartStat: calendar.PartStatAccepted},
			{Person: calendar.Person{Email: "boss@example.com"}, PartStat: calendar.PartStatNeedsAction},
		},
	})
	h := daemontest.Start(t, calendarDaemonConfig, daemontest.Options{
		Calendar:       true,
		CalendarStores: map[string]calstore.Store{"mw": st},
		CalendarPoll:   20 * time.Millisecond,
	})
	waitCalendarReady(t, h, "mw")
	st.SetPartStat("/cal/work/", p, time.Time{}, "boss@example.com", calendar.PartStat("X-SECRET-STATE"))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		r, err := h.Client.Watch.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{
			Cursor: proto.Int64(0), Filter: &chainmailv1.WatchFilter{Kinds: []string{"invite_reply"}},
		}))
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		if evs := r.Msg.GetEvents(); len(evs) > 0 {
			if c := evs[0].GetCalendar(); c.GetPartstat() != "OTHER" || c.GetAttendee().GetEmail().GetUntrusted() != "boss@example.com" {
				t.Errorf("invite_reply payload = %v, want partstat OTHER", c)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("no invite_reply event within 10s")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
