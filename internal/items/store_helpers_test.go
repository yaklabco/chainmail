package items

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// testActor makes every test write unless a test names another actor.
var testActor = Actor{UID: 1000, Client: "cli"}

// stepClock moves forward one millisecond on every Now, so items created one
// after another sort by creation time. Last reports the latest value handed out.
type stepClock struct {
	mu   sync.Mutex
	last time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = c.last.Add(time.Millisecond)
	return c.last
}

func (c *stepClock) Last() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// storeRig is a Store over a fresh state.db, with the watch log it appends to.
type storeRig struct {
	store *Store
	db    *sql.DB
	log   *watch.Log
	clock *stepClock
}

func newStoreRig(t *testing.T) *storeRig {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	clock := &stepClock{last: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)}
	log := watch.NewLog(conn, clock.Now, nil)
	return &storeRig{store: NewStore(conn, log, clock.Now), db: conn, log: log, clock: clock}
}

// get reads id and fails the test on error.
func (r *storeRig) get(t *testing.T, id string) Item {
	t.Helper()
	it, err := r.store.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return it
}

// revision is the global item revision.
func (r *storeRig) revision(t *testing.T) int64 {
	t.Helper()
	v, err := r.store.Revision(t.Context())
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	return v
}

// tail is the event log's tail. changes(t, tail) later reads what came after.
func (r *storeRig) tail(t *testing.T) int64 {
	t.Helper()
	v, err := r.log.Tail(t.Context())
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	return v
}

// changes returns the payloads of the item events after position after.
// Every one must be an item event with no account, folder or ref.
func (r *storeRig) changes(t *testing.T, after int64) []watch.ItemChange {
	t.Helper()
	page, err := r.log.Read(t.Context(), watch.Query{
		After:  after,
		Filter: watch.Filter{Kinds: []watch.Kind{watch.KindItem}},
		Limit:  1000,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var out []watch.ItemChange
	for _, e := range page.Events {
		if e.Kind != watch.KindItem || e.Item == nil || e.Account != "" || e.Folder != "" || e.Ref != "" {
			t.Fatalf("event %d = %+v, want an item event with no account, folder or ref", e.ID, e)
		}
		out = append(out, *e.Item)
	}
	return out
}

// unchanged fails the test when the revision or the log tail moved.
func (r *storeRig) unchanged(t *testing.T, revision, tail int64) {
	t.Helper()
	if got := r.revision(t); got != revision {
		t.Errorf("revision = %d, want %d unchanged", got, revision)
	}
	if got := r.tail(t); got != tail {
		t.Errorf("log tail = %d, want %d unchanged", got, tail)
	}
}

// noText fails the test when any stored event payload contains one of texts.
func (r *storeRig) noText(t *testing.T, texts ...string) {
	t.Helper()
	rows, err := r.db.QueryContext(t.Context(), "SELECT payload FROM events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // read-only; rows.Err reports any read failure
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		for _, text := range texts {
			if strings.Contains(payload, text) {
				t.Errorf("event payload %s holds item text %q", payload, text)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// change is the expected payload of one item event.
func change(id string, typ Type, revision int64, op string, fields ...string) watch.ItemChange {
	return watch.ItemChange{ItemID: id, Type: string(typ), Revision: revision, Op: op, Fields: fields}
}

// wantChanges fails the test unless got is exactly want, in order.
func wantChanges(t *testing.T, got []watch.ItemChange, want ...watch.ItemChange) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("item events =\n%+v\nwant\n%+v", got, want)
	}
}

// wantMissing fails the test unless err is ErrNotFound naming what.
func wantMissing(t *testing.T, err error, what string) {
	t.Helper()
	if !errors.Is(err, ErrNotFound) || err.Error() != "not found: "+what {
		t.Fatalf("err = %v, want not found: %s", err, what)
	}
}

// sameItem compares items, with times compared by instant.
func sameItem(t *testing.T, got, want Item) {
	t.Helper()
	times := []struct {
		name      string
		got, want time.Time
	}{
		{"DeletedAt", got.DeletedAt, want.DeletedAt},
		{"CreatedAt", got.CreatedAt, want.CreatedAt},
		{"UpdatedAt", got.UpdatedAt, want.UpdatedAt},
	}
	for _, tm := range times {
		if !tm.got.Equal(tm.want) {
			t.Errorf("item %s %s = %v, want %v", want.ID, tm.name, tm.got, tm.want)
		}
	}
	got.DeletedAt, got.CreatedAt, got.UpdatedAt = time.Time{}, time.Time{}, time.Time{}
	want.DeletedAt, want.CreatedAt, want.UpdatedAt = time.Time{}, time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("item %s =\n%+v\nwant\n%+v", want.ID, got, want)
	}
}
