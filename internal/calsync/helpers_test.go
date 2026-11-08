package calsync_test

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore/fake"
	"github.com/yaklabco/chainmail/internal/calsync"
	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/watch"
)

// t0 is the time every test clock starts at.
var t0 = time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)

const (
	account = "acme"
	me      = "james@example.com"
	boss    = "boss@example.com"
	// secret is event text no error, status or event may carry.
	secret = "SECRET-EVENT-TEXT"
)

func self(addr string) bool { return strings.EqualFold(addr, me) }

// wait is one After call the syncer made.
type wait struct {
	d  time.Duration
	ch chan time.Time
}

// testClock is a watch.Clock whose time moves only when a test sets it.
// Each After call is handed to the test through waits.
type testClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan wait
}

func newClock(now time.Time) *testClock {
	return &testClock{now: now, waits: make(chan wait, 8)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.waits <- wait{d: d, ch: ch}
	return ch
}

func (c *testClock) set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// next returns the syncer's next sleep, failing the test when none comes.
func (c *testClock) next(t *testing.T) wait {
	t.Helper()
	select {
	case w := <-c.waits:
		return w
	case <-time.After(5 * time.Second): // a guard; a working Run sleeps at once
		t.Fatal("Run did not sleep")
		return wait{}
	}
}

type rig struct {
	store  *fake.Store
	cache  *calcache.Store
	log    *watch.Log
	clock  *testClock
	syncer *calsync.Syncer
}

const (
	poll       = 5 * time.Minute
	windowPast = 30 * 24 * time.Hour
)

// newRig builds a syncer over a fake store with calendars work (writable,
// primary) and home (read-only), a real calendar.db and a real event log.
func newRig(t *testing.T, edit ...func(*calsync.Options)) *rig {
	t.Helper()
	dir := t.TempDir()
	cc, err := calcache.Open(t.Context(), filepath.Join(dir, calcache.FileName))
	if err != nil {
		t.Fatalf("calcache.Open: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() }) // a close error at test end changes nothing
	conn, err := db.OpenState(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	clock := newClock(t0)
	log := watch.NewLog(conn, clock.Now, nil)
	st := fake.New(fake.Options{Now: clock.Now})
	st.AddCalendar(calendar.Calendar{ID: "work", Name: "Work", Access: calendar.AccessWrite, Primary: true})
	st.AddCalendar(calendar.Calendar{ID: "home", Name: "Home", Access: calendar.AccessRead})
	o := calsync.Options{
		Account:    account,
		Store:      st,
		Cache:      cc,
		Log:        log,
		Self:       self,
		Poll:       poll,
		WindowPast: windowPast,
		Clock:      clock,
	}
	for _, e := range edit {
		e(&o)
	}
	return &rig{store: st, cache: cc, log: log, clock: clock, syncer: calsync.New(o)}
}

// sync runs one cycle and fails the test on an error.
func (r *rig) sync(t *testing.T) {
	t.Helper()
	if err := r.syncer.SyncNow(t.Context()); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
}

var calendarKinds = []watch.Kind{watch.KindInvite, watch.KindEventChanged, watch.KindEventCancelled, watch.KindInviteReply}

// events reads every calendar event in the log.
func (r *rig) events(t *testing.T) []watch.Event {
	t.Helper()
	page, err := r.log.Read(t.Context(), watch.Query{Filter: watch.Filter{Kinds: calendarKinds}, Limit: 1000})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return page.Events
}

// single is a confirmed single event owned by nobody in particular.
func single(uid string, start time.Time) calendar.Event {
	return calendar.Event{
		UID:         uid,
		Summary:     secret,
		Description: secret,
		Location:    "Room 1",
		Start:       start,
		End:         start.Add(time.Hour),
		Status:      calendar.StatusConfirmed,
	}
}

// invited is a single event the boss organizes with James invited.
func invited(uid string, start time.Time, ps calendar.PartStat) calendar.Event {
	ev := single(uid, start)
	ev.Organizer = &calendar.Person{Email: boss, Name: secret}
	ev.Attendees = []calendar.Attendee{
		{Person: calendar.Person{Email: boss, Name: secret}, PartStat: calendar.PartStatAccepted},
		{Person: calendar.Person{Email: me, Name: secret}, PartStat: ps},
	}
	return ev
}

// organized is a single event James organizes with the boss invited.
func organized(uid string, start time.Time, bossPS calendar.PartStat) calendar.Event {
	ev := single(uid, start)
	ev.Organizer = &calendar.Person{Email: me, Name: secret}
	ev.Attendees = []calendar.Attendee{
		{Person: calendar.Person{Email: me, Name: secret}, PartStat: calendar.PartStatAccepted},
		{Person: calendar.Person{Email: boss, Name: secret}, PartStat: bossPS},
	}
	return ev
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }
