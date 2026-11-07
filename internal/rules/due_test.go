package rules

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/watch"
)

// dueActor makes every item write in the due tests.
var dueActor = items.Actor{UID: 1000, Client: "cli"}

// dueSettings fire at 08:00 UTC, with item_due_soon two days ahead.
var dueSettings = DueSettings{Location: time.UTC, At: ClockTime{Hour: 8}, SoonDays: 2}

// dueWait is one After call: how long Run asked to sleep, and the channel
// the test sends on to wake it.
type dueWait struct {
	d  time.Duration
	ch chan time.Time
}

// dueClock is a watch.Clock whose time moves only when a test sets it. Each
// After call is handed to the test through waits instead of a timer.
type dueClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan dueWait
}

func newDueClock(now time.Time) *dueClock {
	return &dueClock{now: now, waits: make(chan dueWait, 4)}
}

func (c *dueClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *dueClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.waits <- dueWait{d: d, ch: ch}
	return ch
}

// set moves the clock to now.
func (c *dueClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// next returns Run's next sleep, failing the test when none comes.
func (c *dueClock) next(t *testing.T) dueWait {
	t.Helper()
	select {
	case w := <-c.waits:
		return w
	case <-time.After(5 * time.Second): // a guard; a working Run answers at once
		t.Fatal("Run did not sleep")
		return dueWait{}
	}
}

// dueRig is a DueChecker over a fresh state.db, with an items Store that has
// no due hook for setting items up.
type dueRig struct {
	db      *sql.DB
	log     *watch.Log
	clock   *dueClock
	checker *DueChecker
	plain   *items.Store
}

func newDueRig(t *testing.T, start time.Time, s DueSettings) *dueRig {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	clock := newDueClock(start)
	log := watch.NewLog(conn, clock.Now, nil)
	return &dueRig{
		db:      conn,
		log:     log,
		clock:   clock,
		checker: NewDueChecker(conn, log, clock.Now, s),
		plain:   items.NewStore(conn, log, clock.Now),
	}
}

// create creates n through st as dueActor: a task unless n names a type,
// in the work area when it has no parent.
func (r *dueRig) create(t *testing.T, st *items.Store, n items.NewItem) items.Item {
	t.Helper()
	if n.Type == "" {
		n.Type = items.TypeTask
	}
	if n.ParentID == "" {
		n.Area = items.AreaWork
	}
	it, _, _, err := st.Create(t.Context(), dueActor, n)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return it
}

// setDue moves id's due date through the hookless store.
func (r *dueRig) setDue(t *testing.T, id, due string) items.Item {
	t.Helper()
	it, err := r.plain.Update(t.Context(), dueActor, id, items.Patch{Fields: []string{"due_date"}, Values: items.NewItem{DueDate: due}})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return it
}

// tail is the event log's tail.
func (r *dueRig) tail(t *testing.T) int64 {
	t.Helper()
	v, err := r.log.Tail(t.Context())
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	return v
}

// exec runs a statement on the state database.
func (r *dueRig) exec(t *testing.T, stmt string) {
	t.Helper()
	if _, err := r.db.ExecContext(t.Context(), stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// scan runs Scan and fails the test unless it fires want events.
func (r *dueRig) scan(t *testing.T, want int) {
	t.Helper()
	n, err := r.checker.Scan(t.Context())
	if err != nil || n != want {
		t.Fatalf("Scan = %d, %v; want %d", n, err, want)
	}
}

// lines renders the item and due events after position after, in log order:
// "item <op> <id>" for item events and dueLine for due events. Every due
// event must carry an ItemDue and no account, folder, ref or item change.
func (r *dueRig) lines(t *testing.T, after int64) []string {
	t.Helper()
	page, err := r.log.Read(t.Context(), watch.Query{
		After: after,
		Filter: watch.Filter{Kinds: []watch.Kind{
			watch.KindItem, watch.KindItemDueSoon, watch.KindItemDue, watch.KindItemOverdue,
		}},
		Limit: 1000,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var out []string
	for _, e := range page.Events {
		switch e.Kind {
		case watch.KindItem:
			out = append(out, "item "+e.Item.Op+" "+e.Item.ItemID)
		case watch.KindItemDueSoon, watch.KindItemDue, watch.KindItemOverdue:
			if e.Due == nil || e.Item != nil || e.Account != "" || e.Folder != "" || e.Ref != "" {
				t.Fatalf("due event %d = %+v, want an ItemDue and no account, folder, ref or item change", e.ID, e)
			}
			out = append(out, strings.Join([]string{
				string(e.Kind), e.Due.ItemID, e.Due.Type, e.Due.DueDate, e.Due.State, e.Due.ParentID,
			}, " "))
		default:
			t.Fatalf("event %d has kind %s", e.ID, e.Kind)
		}
	}
	return out
}

// dueLine is the line lines renders for a due event of kind k about it.
func dueLine(k watch.Kind, it items.Item) string {
	return strings.Join([]string{
		string(k), it.ID, string(it.Type), it.DueDate, string(it.State), it.ParentID,
	}, " ")
}

// sent lists id's item_due_sent rows as "<due_date> <stage>", sorted.
func (r *dueRig) sent(t *testing.T, id string) []string {
	t.Helper()
	rows, err := r.db.QueryContext(t.Context(),
		"SELECT due_date, stage FROM item_due_sent WHERE item_id = ? ORDER BY due_date, stage", id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	var out []string
	for rows.Next() {
		var date, stage string
		if err := rows.Scan(&date, &stage); err != nil {
			t.Fatal(err)
		}
		out = append(out, date+" "+stage)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// noText fails the test when any stored event payload contains text.
func (r *dueRig) noText(t *testing.T, text string) {
	t.Helper()
	var n int
	if err := r.db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM events WHERE instr(payload, ?) > 0", text).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d event payloads hold item text %q", n, text)
	}
}

// wantDue fails the test unless got is exactly want, in order.
func wantDue(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s =\n%q\nwant\n%q", what, got, want)
	}
}

func TestNewDueCheckerDefaults(t *testing.T) {
	t.Parallel()
	d := NewDueChecker(nil, nil, nil, DueSettings{At: ClockTime{Hour: 8}})
	if d.settings.Location != time.Local {
		t.Errorf("Location = %v, want time.Local for nil", d.settings.Location)
	}
	if d.now == nil {
		t.Error("now is nil, want time.Now")
	}
}

// Only the most advanced stage reached fires, once; reaching it records the
// earlier stages; done, deleted and undated items never fire; a moved due
// date fires again and moving it back does not.
func TestScanFiresTheMostAdvancedStageOnce(t *testing.T) {
	t.Parallel()
	r := newDueRig(t, time.Date(2026, 11, 10, 7, 59, 0, 0, time.UTC), dueSettings)
	story := r.create(t, r.plain, items.NewItem{Type: items.TypeStory, Title: "SECRET-DUE story"})
	a := r.create(t, r.plain, items.NewItem{Title: "SECRET-DUE a", DueDate: "2026-11-01"})
	b := r.create(t, r.plain, items.NewItem{Title: "SECRET-DUE b", DueDate: "2026-11-09", State: items.StateInReview})
	c := r.create(t, r.plain, items.NewItem{
		Title: "SECRET-DUE c", DueDate: "2026-11-10", State: items.StateInProgress, ParentID: story.ID,
	})
	d := r.create(t, r.plain, items.NewItem{Title: "SECRET-DUE d", DueDate: "2026-11-12"})
	e := r.create(t, r.plain, items.NewItem{Title: "SECRET-DUE e", DueDate: "2026-11-13"})
	done := r.create(t, r.plain, items.NewItem{Title: "SECRET-DUE done", DueDate: "2026-11-10", State: items.StateDone})
	gone := r.create(t, r.plain, items.NewItem{Title: "SECRET-DUE gone", DueDate: "2026-11-10"})
	undated := r.create(t, r.plain, items.NewItem{Title: "SECRET-DUE undated"})
	if _, _, err := r.plain.Delete(t.Context(), dueActor, gone.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// 07:59: a is overdue, b due, c due soon; d is not yet due soon.
	tail := r.tail(t)
	r.scan(t, 3)
	wantDue(t, "events at 07:59", r.lines(t, tail),
		dueLine(watch.KindItemOverdue, a), dueLine(watch.KindItemDue, b), dueLine(watch.KindItemDueSoon, c))
	tail = r.tail(t)
	r.scan(t, 0)

	// 08:00: each of b, c and d moves one stage on; a has nothing new.
	r.clock.set(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC))
	r.scan(t, 3)
	wantDue(t, "events at 08:00", r.lines(t, tail),
		dueLine(watch.KindItemOverdue, b), dueLine(watch.KindItemDue, c), dueLine(watch.KindItemDueSoon, d))
	r.scan(t, 0)

	wantDue(t, "a sent", r.sent(t, a.ID), "2026-11-01 item_due", "2026-11-01 item_due_soon", "2026-11-01 item_overdue")
	wantDue(t, "b sent", r.sent(t, b.ID), "2026-11-09 item_due", "2026-11-09 item_due_soon", "2026-11-09 item_overdue")
	wantDue(t, "c sent", r.sent(t, c.ID), "2026-11-10 item_due", "2026-11-10 item_due_soon")
	wantDue(t, "d sent", r.sent(t, d.ID), "2026-11-12 item_due_soon")
	for _, it := range []items.Item{story, e, done, gone, undated} {
		wantDue(t, it.Title+" sent", r.sent(t, it.ID))
	}

	// A new due date is a new key; going back to a date already sent is not.
	tail = r.tail(t)
	r.setDue(t, d.ID, "2026-11-20")
	r.scan(t, 0)
	moved := r.setDue(t, d.ID, "2026-11-11")
	r.scan(t, 1)
	r.setDue(t, d.ID, "2026-11-12")
	r.scan(t, 0)
	wantDue(t, "events after moves", r.lines(t, tail),
		"item updated "+d.ID, "item updated "+d.ID, dueLine(watch.KindItemDueSoon, moved), "item updated "+d.ID)
	wantDue(t, "d sent after moves", r.sent(t, d.ID), "2026-11-11 item_due_soon", "2026-11-12 item_due_soon")
	r.noText(t, "SECRET-DUE")
}

// With due_soon_days 0 no item_due_soon fires, but it is still recorded as
// passed when a later stage fires.
func TestScanWithoutDueSoon(t *testing.T) {
	t.Parallel()
	s := dueSettings
	s.SoonDays = 0
	r := newDueRig(t, time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), s)
	tomorrow := r.create(t, r.plain, items.NewItem{Title: "tomorrow", DueDate: "2026-11-11"})
	today := r.create(t, r.plain, items.NewItem{Title: "today", DueDate: "2026-11-10"})
	tail := r.tail(t)
	r.scan(t, 1)
	wantDue(t, "events", r.lines(t, tail), dueLine(watch.KindItemDue, today))
	wantDue(t, "today sent", r.sent(t, today.ID), "2026-11-10 item_due", "2026-11-10 item_due_soon")
	wantDue(t, "tomorrow sent", r.sent(t, tomorrow.ID))

	tail = r.tail(t)
	r.clock.set(time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC))
	r.scan(t, 2)
	wantDue(t, "events next day", r.lines(t, tail),
		dueLine(watch.KindItemOverdue, today), dueLine(watch.KindItemDue, tomorrow))
}

// Stages fall at the due time on the settings' wall clock, not in UTC.
func TestScanUsesTheLocation(t *testing.T) {
	t.Parallel()
	s := dueSettings
	s.Location = time.FixedZone("UTC-8", -8*60*60)
	r := newDueRig(t, time.Date(2026, 11, 10, 15, 59, 0, 0, time.UTC), s) // 07:59 local
	it := r.create(t, r.plain, items.NewItem{Title: "local", DueDate: "2026-11-10"})
	tail := r.tail(t)
	r.scan(t, 1)
	r.clock.set(time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)) // 08:00 local
	r.scan(t, 1)
	wantDue(t, "events", r.lines(t, tail), dueLine(watch.KindItemDueSoon, it), dueLine(watch.KindItemDue, it))
}

// The due stages DueStage returns are the watch kinds the due checker
// writes, so a stage is also the event kind it fires.
func TestDueStagesAreWatchKinds(t *testing.T) {
	t.Parallel()
	pairs := []struct{ stage, kind watch.Kind }{
		{StageDueSoon, watch.KindItemDueSoon},
		{StageDue, watch.KindItemDue},
		{StageOverdue, watch.KindItemOverdue},
	}
	for _, p := range pairs {
		if p.stage != p.kind {
			t.Errorf("stage %q, want the watch kind %q", p.stage, p.kind)
		}
	}
}
