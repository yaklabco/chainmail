package daemon_test

import (
	"context"
	"os"
	"path/filepath"
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

const calendarDaemonConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.mw]
provider = "hostinger"
address = "james@mw.example"

[accounts.mw.calendar]
store = "caldav"
url = "https://mw.example/"
`

// waitCalendarReady polls GetCacheStatus until account's calendar line is
// ready, and returns the response.
func waitCalendarReady(t *testing.T, h *daemontest.Harness, account string) *chainmailv1.GetCacheStatusResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var last *chainmailv1.GetCacheStatusResponse
	for {
		resp, err := h.Client.Cache.GetCacheStatus(ctx, connect.NewRequest(&chainmailv1.GetCacheStatusRequest{}))
		if err != nil {
			t.Fatalf("GetCacheStatus: %v; last %v", err, last)
		}
		last = resp.Msg
		for _, c := range last.GetCalendars() {
			if c.GetAccount() == account && c.GetState() == "ready" {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCalendarSyncInDaemon runs the daemon with a fake calendar store: the
// syncer starts with the daemon, the cache status gains the calendar line,
// a change becomes a calendar watch event a reader can ask for by kind, and
// calendar.db sits next to cache.db.
func TestCalendarSyncInDaemon(t *testing.T) {
	t.Parallel()
	st := calfake.New(calfake.Options{ObjectReplace: true})
	st.AddCalendar(calendar.Calendar{ID: "/cal/work/", Name: "Work", Access: calendar.AccessWrite})
	start := time.Now().Add(48 * time.Hour).Truncate(time.Second).UTC()
	p := st.Add("/cal/work/", calendar.Event{
		UID: "a@x", Summary: "SECRET", Start: start, End: start.Add(time.Hour),
		Organizer: &calendar.Person{Email: "boss@example.com"},
		Attendees: []calendar.Attendee{{Person: calendar.Person{Email: "james@mw.example"}, PartStat: calendar.PartStatAccepted}},
	})
	h := daemontest.Start(t, calendarDaemonConfig, daemontest.Options{
		Calendar:       true,
		CalendarStores: map[string]calstore.Store{"mw": st},
		CalendarPoll:   20 * time.Millisecond,
	})
	resp := waitCalendarReady(t, h, "mw")
	if len(resp.GetCalendars()) != 1 {
		t.Fatalf("calendars = %v, want only mw, the account with a store", resp.GetCalendars())
	}
	mw := resp.GetCalendars()[0]
	if mw.GetCalendars() != 1 || mw.GetEvents() != 1 || mw.GetLastSyncAt() == nil {
		t.Errorf("mw calendar status = %v", mw)
	}
	if _, err := os.Stat(filepath.Join(h.StateDir(), "calendar.db")); err != nil {
		t.Errorf("calendar.db: %v", err)
	}

	st.Cancel("/cal/work/", p)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		r, err := h.Client.Watch.ReadEvents(ctx, connect.NewRequest(&chainmailv1.ReadEventsRequest{
			Cursor: proto.Int64(0), Filter: &chainmailv1.WatchFilter{Kinds: []string{"invite", "event_changed", "event_cancelled", "invite_reply"}},
		}))
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		if evs := r.Msg.GetEvents(); len(evs) > 0 {
			e := evs[0]
			if e.GetKind() != "event_cancelled" || e.GetAccount() != "mw" || e.GetCalendar().GetCalendarId() != "/cal/work/" ||
				e.GetCalendar().GetRef() != calendar.EventRef("mw", "/cal/work/", p) {
				t.Errorf("event = %v, want event_cancelled for the event", e)
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("no calendar event within 10s")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
