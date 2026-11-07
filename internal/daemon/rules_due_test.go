package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// rpcDueKinds are the three due event kinds.
var rpcDueKinds = []string{"item_due_soon", "item_due", "item_overdue"}

// rpcDueLines reads every due event from the start of the log as
// "kind item type due_date state account".
func rpcDueLines(t *testing.T, h *daemontest.Harness) []string {
	t.Helper()
	var out []string
	for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Filter: &chainmailv1.WatchFilter{Kinds: rpcDueKinds}}).GetEvents() {
		d := e.GetDue()
		out = append(out, fmt.Sprintf("%s %s %s %s %s account=%q", e.GetKind(), d.GetItemId(), d.GetType(), d.GetDueDate(), d.GetState(), e.GetAccount()))
		if strings.Contains(e.String(), rpcMarker) {
			t.Errorf("due event %v holds item text", e)
		}
	}
	return out
}

// TestDueEventsReachWatchers checks that an item made due tomorrow gets
// item_due_soon in its create, that moving the due date into the past
// gets item_overdue in the update and nothing for the stages before it,
// that an item filter passes them, and that a reader naming no kinds gets
// none.
func TestDueEventsReachWatchers(t *testing.T) {
	t.Parallel()
	const config = hostingerConfig + `
[rules]
timezone = "UTC"
due_time = "08:00"
due_soon_days = 2
`
	h := daemontest.Start(t, config, daemontest.Options{Now: func() time.Time { return rpcNow }})
	id := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: "Pay " + rpcMarker, Area: "work", DueDate: "2026-11-08"}).GetItem().GetId()
	if _, err := itemUpdate(t, h, &chainmailv1.UpdateItemRequest{Id: id, DueDate: "2026-11-06"}, "due_date"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"item_due_soon " + id + ` task 2026-11-08 backlog account=""`,
		"item_overdue " + id + ` task 2026-11-06 backlog account=""`,
	}
	if got := rpcDueLines(t, h); !slices.Equal(got, want) {
		t.Errorf("due events = %q, want %q", got, want)
	}
	var kinds []string
	for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0), Filter: &chainmailv1.WatchFilter{Item: id}}).GetEvents() {
		kinds = append(kinds, e.GetKind())
	}
	if !slices.Contains(kinds, "item_due_soon") || !slices.Contains(kinds, "item_overdue") {
		t.Errorf("item filter kinds = %v, want both due events", kinds)
	}
	for _, e := range readEvents(t, h, &chainmailv1.ReadEventsRequest{Cursor: proto.Int64(0)}).GetEvents() {
		if slices.Contains(rpcDueKinds, e.GetKind()) {
			t.Errorf("a reader with no kinds got %v; due events are opt-in", e)
		}
	}
}

// TestDueSettingsComeFromConfig checks that due_time and due_soon_days
// reach the due checker: at 12:00 with due_time 13:00 and due_soon_days 0,
// an item due yesterday is due but not yet overdue, and items due today
// and tomorrow have no event yet.
func TestDueSettingsComeFromConfig(t *testing.T) {
	t.Parallel()
	const config = hostingerConfig + `
[rules]
timezone = "UTC"
due_time = "13:00"
due_soon_days = 0
`
	h := daemontest.Start(t, config, daemontest.Options{Now: func() time.Time { return rpcNow }})
	var yesterday string
	for _, due := range []string{"2026-11-06", "2026-11-07", "2026-11-08"} {
		id := itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: "Due " + due, Area: "work", DueDate: due}).GetItem().GetId()
		if due == "2026-11-06" {
			yesterday = id
		}
	}
	if got, want := rpcDueLines(t, h), []string{"item_due " + yesterday + ` task 2026-11-06 backlog account=""`}; !slices.Equal(got, want) {
		t.Errorf("due events = %q, want %q", got, want)
	}
}
