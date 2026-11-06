package items_test

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/watch"
)

// linkT0 is the start of every link test clock.
var linkT0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

// linkActor makes every change in these tests.
var linkActor = items.Actor{UID: 1000, Client: "test"}

// noteMarker is note text that must never reach an event or an error.
const noteMarker = "note-marker-5e1f"

// linkClock is a settable time source safe for concurrent use.
type linkClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *linkClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *linkClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// linkRig is a fresh state.db with an items store and the event log it
// appends to, on a clock the test moves.
type linkRig struct {
	db    *sql.DB
	log   *watch.Log
	store *items.Store
	clock *linkClock
}

func newLinkRig(t *testing.T) *linkRig {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	c := &linkClock{now: linkT0}
	log := watch.NewLog(conn, c.Now, nil)
	return &linkRig{db: conn, log: log, store: items.NewStore(conn, log, c.Now), clock: c}
}

// create makes an item and returns its ID, then moves the clock on 1ms so
// items made in a row have distinct times.
func (r *linkRig) create(t *testing.T, n items.NewItem) string {
	t.Helper()
	it, _, _, err := r.store.Create(t.Context(), linkActor, n)
	if err != nil {
		t.Fatalf("Create(%s): %v", n.Type, err)
	}
	r.clock.Advance(time.Millisecond)
	return it.ID
}

// task makes a root task in the work area: an inbox task.
func (r *linkRig) task(t *testing.T) string {
	t.Helper()
	return r.create(t, items.NewItem{Type: items.TypeTask, Title: "a task", Area: items.AreaWork})
}

// link links the thread key on account to id, with no anchor and no note.
func (r *linkRig) link(t *testing.T, id, account, key string) items.Link {
	t.Helper()
	l, _, _, err := r.store.LinkThread(t.Context(), linkActor, id, linkTarget(account, key), false, "")
	if err != nil {
		t.Fatalf("LinkThread(%s, %s): %v", account, key, err)
	}
	return l
}

// remove soft-deletes id, which must have no live children.
func (r *linkRig) remove(t *testing.T, id string) {
	t.Helper()
	if _, _, err := r.store.Delete(t.Context(), linkActor, id, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// revision is the global item revision.
func (r *linkRig) revision(t *testing.T) int64 {
	t.Helper()
	rev, err := r.store.Revision(t.Context())
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	return rev
}

// count runs a count query on the state.db. No transaction may be open.
func (r *linkRig) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := r.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// events returns the logged events of the given kinds, oldest first.
func (r *linkRig) events(t *testing.T, kinds ...watch.Kind) []watch.Event {
	t.Helper()
	page, err := r.log.Read(t.Context(), watch.Query{Filter: watch.Filter{Kinds: kinds}, Limit: 1000})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return slices.DeleteFunc(page.Events, func(e watch.Event) bool { return !slices.Contains(kinds, e.Kind) })
}

// linkTarget is the thread with key on account, as the daemon reads it from
// a message: the root and the message's own Message-ID derive from key.
func linkTarget(account, key string) items.LinkTarget {
	return items.LinkTarget{Account: account, ThreadKey: key, RootMsgID: "root-" + key + "@example.net", MsgID: "msg-" + key + "@example.net"}
}

// sameLink reports whether two links agree, comparing times by instant.
func sameLink(a, b items.Link) bool {
	at, bt := a.CreatedAt, b.CreatedAt
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	return a == b && at.Equal(bt)
}

// wantItemEvent checks that e is the item event of op on id at revision:
// no account, folder, ref or message, and only the ID, type, revision and
// op in its payload.
func wantItemEvent(t *testing.T, e watch.Event, id string, typ items.Type, revision int64, op string) {
	t.Helper()
	if e.Kind != watch.KindItem || e.Account != "" || e.Folder != "" || e.Ref != "" || e.Message != nil || e.Item == nil {
		t.Fatalf("event = %+v, want an item event", e)
	}
	want := watch.ItemChange{ItemID: id, Type: string(typ), Revision: revision, Op: op}
	if !reflect.DeepEqual(*e.Item, want) {
		t.Errorf("item change = %+v, want %+v", *e.Item, want)
	}
}
