package items

import (
	"context"
	"database/sql"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/watch"
)

// One transaction gets one revision, however often fn asks for it, and every
// event it returns is appended with that revision.
func TestWriteStampsOneRevision(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	at := r.clock.Now()
	err := r.store.write(t.Context(), func(_ *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		first, err := rev()
		if err != nil {
			return nil, err
		}
		second, err := rev()
		if err != nil {
			return nil, err
		}
		if first != 1 || second != 1 {
			t.Errorf("rev() = %d then %d, want 1 both times", first, second)
		}
		return []watch.Event{
			itemEvent(at, "it_a", TypeTask, first, OpUpdated, []string{"title"}),
			itemEvent(at, "it_b", TypeEpic, second, OpUpdated, []string{"state"}),
		}, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := r.revision(t); got != 1 {
		t.Errorf("revision = %d, want 1", got)
	}
	wantChanges(t, r.changes(t, 0),
		change("it_a", TypeTask, 1, OpUpdated, "title"),
		change("it_b", TypeEpic, 1, OpUpdated, "state"))
}

// A write that never asks for a revision, or fails, leaves no trace.
func TestWriteWithoutChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		fn      func(*sql.Tx, func() (int64, error)) ([]watch.Event, error)
		wantErr string
	}{
		{
			name: "nothing changed",
			fn:   func(*sql.Tx, func() (int64, error)) ([]watch.Event, error) { return nil, nil },
		},
		{
			name: "error after the revision",
			fn: func(_ *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
				if _, err := rev(); err != nil {
					return nil, err
				}
				return nil, errors.New("boom")
			},
			wantErr: "boom",
		},
		{
			name: "events without a revision",
			fn: func(*sql.Tx, func() (int64, error)) ([]watch.Event, error) {
				return []watch.Event{itemEvent(time.Now(), "it_a", TypeTask, 0, OpUpdated, nil)}, nil
			},
			wantErr: "changed nothing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newStoreRig(t)
			err := r.store.write(t.Context(), tt.fn)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("write: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			r.unchanged(t, 0, 0)
		})
	}
}

// A committed write wakes readers waiting on the log. The test parks Wait
// before the write without sleeping: it holds state.db's one connection,
// starts Wait, and lets go once the pool shows Wait queued for it. Wait has
// then taken the log's notify channel, and the pool hands the connection
// to Wait's Tail read before the write can begin, so Wait reads an empty
// log and parks. Only the write's Notify can wake it before ctx ends.
func TestWriteNotifiesTheLog(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hold, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("holding the connection: %v", err)
	}
	queued := r.db.Stats().WaitCount
	var (
		wg      sync.WaitGroup
		waitErr error
	)
	wg.Go(func() { waitErr = r.log.Wait(ctx, 0) })
	for r.db.Stats().WaitCount == queued {
		if ctx.Err() != nil {
			t.Fatal("Wait never asked for the connection")
		}
		runtime.Gosched()
	}
	if err := hold.Rollback(); err != nil {
		t.Fatalf("releasing the connection: %v", err)
	}
	err = r.store.write(t.Context(), func(_ *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		n, err := rev()
		if err != nil {
			return nil, err
		}
		return []watch.Event{itemEvent(r.clock.Now(), "it_notify", TypeTask, n, OpCreated, nil)}, nil
	})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	wg.Wait()
	if err := waitErr; err != nil {
		t.Fatalf("Wait: %v (the write did not notify the log)", err)
	}
}

func TestEnsureAdmin(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	if err := r.store.EnsureAdmin(t.Context(), testActor); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	at := r.clock.Last()
	for _, tt := range []struct {
		id   string
		area Area
	}{{AdminWorkID, AreaWork}, {AdminPersonalID, AreaPersonal}} {
		sameItem(t, r.get(t, tt.id), Item{
			ID: tt.id, Type: TypeInitiative, Title: "Admin", State: StateBacklog,
			Area: tt.area, EffectiveArea: tt.area, EffectivePriority: PriorityP2,
			Ongoing: true, Revision: 1, CreatedAt: at, UpdatedAt: at,
			CreatedBy: "1000/cli", UpdatedBy: "1000/cli",
		})
	}
	// The Admin items are bootstrap rows: they bump the revision, so a
	// changed_since reader sees them, but they put nothing in the event log.
	wantChanges(t, r.changes(t, 0))
	if tail := r.tail(t); tail != 0 {
		t.Fatalf("event log tail = %d after EnsureAdmin, want 0", tail)
	}

	// Running it again changes nothing.
	tail := r.tail(t)
	if err := r.store.EnsureAdmin(t.Context(), Actor{UID: 0, Client: "daemon"}); err != nil {
		t.Fatalf("EnsureAdmin again: %v", err)
	}
	r.unchanged(t, 1, tail)
}

func TestGetUnknown(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	_, err := r.store.Get(t.Context(), "it_nope")
	wantMissing(t, err, "item")
}
