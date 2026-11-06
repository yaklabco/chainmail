package watch

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/db"
	"github.com/yaklabco/chainmail/internal/mailstore"
)

// t0 is the start time of every test clock in this package.
var t0 = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// testClock is a settable time source safe for concurrent use.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// openDB opens a fresh state.db in the test's temp dir.
func openDB(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := db.OpenState(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() }) // a close error at test end changes nothing
	return conn
}

// newLog returns a Log over a fresh state.db with a test clock at t0.
func newLog(t *testing.T) (*Log, *sql.DB, *testClock) {
	t.Helper()
	conn := openDB(t)
	c := &testClock{now: t0}
	return NewLog(conn, c.Now, nil), conn, c
}

// newEvent is a new-mail event for UID uid in account/folder.
func newEvent(account, folder string, uid uint32) Event {
	return Event{
		Kind:    KindNew,
		Account: account,
		Folder:  folder,
		Ref:     fmt.Sprintf("cm:%s:%s:1:%d", account, folder, uid),
		Message: &Message{
			From:    []mailstore.Address{{Name: "Ann", Email: "ann@example.com"}},
			Subject: "hello",
			Flags:   []string{`\Seen`},
		},
	}
}

// publish publishes events and returns them with their IDs set.
func publish(t *testing.T, l *Log, events ...Event) []Event {
	t.Helper()
	if err := l.Publish(t.Context(), events...); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return events
}

// ids lists the IDs of events.
func ids(events []Event) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.ID
	}
	return out
}
