//go:build integration

package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// itDueConfig puts the due stages at midnight UTC with one day of
// warning, and configures no account.
const itDueConfig = `
[rules]
timezone = "UTC"
due_time = "00:00"
due_soon_days = 1
`

// itDueLead is how long before midnight the due test's clock starts: time
// to start the daemon and create the items, with room for a slow machine.
// The test waits it out once, for the clock to cross midnight.
const itDueLead = 8 * time.Second

// itDueBeats is how many heartbeats a quiet check reads: two seconds.
const itDueBeats = 8

// itDueKinds are the item due event kinds.
var itDueKinds = []string{"item_due_soon", "item_due", "item_overdue"}

// itDueTask creates a task in the inbox due on due ("" for none) in state
// ("" for backlog) and returns its ID.
func itDueTask(t *testing.T, h *daemontest.Harness, title, due, state string) string {
	t.Helper()
	return itemCreate(t, h, &chainmailv1.CreateItemRequest{Type: "task", Title: title, Area: "work", DueDate: due, State: state}).GetItem().GetId()
}

// itDueLine is a due event as the test compares it: kind, item and due date.
func itDueLine(ev *chainmailv1.WatchEvent) string {
	return ev.GetKind() + " " + ev.GetDue().GetItemId() + " " + ev.GetDue().GetDueDate()
}

// itDueEvents reads the due events after cursor, in log order.
func itDueEvents(t *testing.T, h *daemontest.Harness, cursor int64) []*chainmailv1.WatchEvent {
	t.Helper()
	var out []*chainmailv1.WatchEvent
	for {
		resp := readEvents(t, h, &chainmailv1.ReadEventsRequest{
			Cursor: &cursor, Filter: &chainmailv1.WatchFilter{Kinds: itDueKinds}, Limit: 200,
		})
		out = append(out, resp.GetEvents()...)
		if len(resp.GetEvents()) == 0 || resp.GetNextCursor() <= cursor {
			return out
		}
		cursor = resp.GetNextCursor()
	}
}

// itDueLines are the lines of evs, in their order.
func itDueLines(evs []*chainmailv1.WatchEvent) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = itDueLine(ev)
	}
	return out
}

// itCollectDue reads w until n due events have come, within, and returns
// their lines sorted. A scan writes its events in one transaction, in no
// set order.
func itCollectDue(t *testing.T, w *doveStream, n int, within time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(within)
	var evs []*chainmailv1.WatchEvent
	for len(evs) < n {
		if ev := w.item(time.Until(deadline), fmt.Sprintf("due event %d of %d", len(evs)+1, n)).GetEvent(); ev != nil {
			evs = append(evs, ev)
		}
	}
	lines := itDueLines(evs)
	slices.Sort(lines)
	return lines
}

// itQuietDue reads itDueBeats heartbeats from w, a stream of due events
// only, and fails the test for every event among them.
func itQuietDue(t *testing.T, w *doveStream) {
	t.Helper()
	for seen := 0; seen < itDueBeats; {
		it := w.item(doveReadyWithin, "heartbeat")
		if it.GetHeartbeat() != nil {
			seen++
			continue
		}
		t.Errorf("extra due event: %s", itDueLine(it.GetEvent()))
	}
}

// itCheckDue fails the test unless ev is a due event of a task in the
// inbox in state, with no account, no ref and no item text (amendment 9).
func itCheckDue(t *testing.T, ev *chainmailv1.WatchEvent, state string, titles []string) {
	t.Helper()
	d := ev.GetDue()
	if ev.GetAccount() != "" || ev.GetRef() != "" || d.GetType() != "task" || d.GetState() != state || d.GetParentId() != "" {
		t.Errorf("due event %v, want a task in the inbox in state %s with no account or ref", ev, state)
	}
	line := fmt.Sprintf("%v", ev)
	for _, title := range titles {
		if strings.Contains(line, title) {
			t.Errorf("due event %s holds the item title %q", itDueLine(ev), title)
		}
	}
}

// itWaitDueCheck polls RulesStatus until next_due_check_at is want.
func itWaitDueCheck(t *testing.T, h *daemontest.Harness, want time.Time) {
	t.Helper()
	for deadline := time.Now().Add(itRunWithin); ; time.Sleep(itPoll) {
		got := itRulesStatus(t, h).GetNextDueCheckAt()
		if got != nil && got.AsTime().Equal(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("next_due_check_at = %v after %v, want %v", got.AsTime(), itRunWithin, want)
		}
	}
}

// TestDueEventsAcrossMidnight is spec section 10's due test and done when
// item 4, with the stages at midnight UTC and one day of warning. The
// daemon's clock starts a few seconds before midnight and runs on in real
// time. Items made then get the most advanced stage they have reached, at
// once, inside their create. The clock crosses midnight, and the checker,
// asleep until then, fires the next stage of each open item. A moved due
// date fires again, for the new date. A restart past the next midnight
// fires the one stage reached while the daemon was down, and another
// restart fires nothing. Each (item, due date, stage) fires once.
func TestDueEventsAcrossMidnight(t *testing.T) {
	day := 24 * time.Hour
	d := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	date := func(n int) string { return d.Add(time.Duration(n) * day).Format(time.DateOnly) }
	clock := &itClock{}
	clock.Set(d.Add(day - itDueLead))
	opts := daemontest.Options{Now: clock.Now, WatchHeartbeat: doveHeartbeat}
	h := daemontest.Start(t, itDueConfig, opts)
	start := itLogPosition(t, h)

	titles := []string{"Renew the domain", "Pay the invoice", "Already done", "No date at all"}
	soon := itDueTask(t, h, titles[0], date(1), "")
	today := itDueTask(t, h, titles[1], date(0), "in-progress")
	itDueTask(t, h, titles[2], date(1), "done")
	itDueTask(t, h, titles[3], "", "")
	if !clock.Now().Before(d.Add(day)) {
		t.Fatalf("the items were made after midnight: the setup took longer than %v", itDueLead)
	}

	// 1. Before midnight: due tomorrow is due soon, due today is due. The
	// done item and the undated one get nothing.
	evs := itDueEvents(t, h, start)
	want := []string{"item_due_soon " + soon + " " + date(1), "item_due " + today + " " + date(0)}
	if got := itDueLines(evs); !slices.Equal(got, want) {
		t.Fatalf("due events before midnight = %q, want %q", got, want)
	}
	itCheckDue(t, evs[0], "backlog", titles)
	itCheckDue(t, evs[1], "in-progress", titles)
	mark := evs[len(evs)-1].GetId()

	// 2. Midnight passes: due soon becomes due, due becomes overdue.
	w := subscribeDove(t, h, &chainmailv1.SubscribeRequest{Since: &mark, Filter: &chainmailv1.WatchFilter{Kinds: itDueKinds}})
	got := itCollectDue(t, w, 2, itDueLead+itRunWithin)
	want = []string{"item_due " + soon + " " + date(1), "item_overdue " + today + " " + date(0)}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("due events at midnight = %q, want %q", got, want)
	}
	itQuietDue(t, w)
	w.close()
	itWaitDueCheck(t, h, d.Add(2*day))

	// 3. Moving the due date gives new keys: three days out has reached no
	// stage, two days out is due soon again.
	pos := itLogPosition(t, h)
	for _, due := range []string{date(3), date(2)} {
		if _, err := itemUpdate(t, h, &chainmailv1.UpdateItemRequest{Id: soon, DueDate: due}, "due_date"); err != nil {
			t.Fatalf("moving %s to %s: %v", soon, due, err)
		}
	}
	if got := itDueLines(itDueEvents(t, h, pos)); !slices.Equal(got, []string{"item_due_soon " + soon + " " + date(2)}) {
		t.Errorf("due events after moving the date = %q, want one item_due_soon for %s", got, date(2))
	}

	// 4. Down past the next midnight: the start-up scan fires the stage
	// reached meanwhile, once. The overdue item is not told again.
	pos = itLogPosition(t, h)
	clock.Set(d.Add(2*day + 5*time.Second))
	restarted := opts
	restarted.Reuse = h
	h2 := daemontest.Start(t, itDueConfig, restarted)
	itWaitDueCheck(t, h2, d.Add(3*day))
	w2 := subscribeDove(t, h2, &chainmailv1.SubscribeRequest{Since: &pos, Filter: &chainmailv1.WatchFilter{Kinds: itDueKinds}})
	if got := itCollectDue(t, w2, 1, itRunWithin); !slices.Equal(got, []string{"item_due " + soon + " " + date(2)}) {
		t.Errorf("due events after the restart = %q, want one item_due for %s", got, date(2))
	}
	itQuietDue(t, w2)
	w2.close()

	// 5. Another restart at the same time fires nothing.
	pos = itLogPosition(t, h2)
	again := opts
	again.Reuse = h2
	h3 := daemontest.Start(t, itDueConfig, again)
	itWaitDueCheck(t, h3, d.Add(3*day))
	w3 := subscribeDove(t, h3, &chainmailv1.SubscribeRequest{Since: &pos, Filter: &chainmailv1.WatchFilter{Kinds: itDueKinds}})
	itQuietDue(t, w3)
	w3.close()

	// 6. Over the whole run, each (item, due date, stage) fired once.
	all := itDueLines(itDueEvents(t, h3, start))
	seen := map[string]bool{}
	for _, l := range all {
		if seen[l] {
			t.Errorf("due event %s fired twice", l)
		}
		seen[l] = true
	}
	if len(all) != 6 {
		t.Errorf("%d due events in all, want 6: %q", len(all), all)
	}
}
