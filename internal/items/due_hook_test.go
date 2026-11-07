package items

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// dueCall is one call of a dueRecorder: the IDs and time it was given, and
// each item's due date and state as the write's transaction shows them.
type dueCall struct {
	ids  []string
	now  time.Time
	seen []string // "<id> <due_date> <state>"
}

// dueRecorder is a DueHook that records its calls and returns one item_due
// event per ID, or err when err is set.
type dueRecorder struct {
	mu    sync.Mutex
	calls []dueCall
	err   error
}

func (h *dueRecorder) hook(ctx context.Context, tx *sql.Tx, ids []string, now time.Time) ([]watch.Event, error) {
	call := dueCall{ids: slices.Clone(ids), now: now}
	for _, id := range ids {
		var (
			due   sql.NullString
			state string
		)
		if err := tx.QueryRowContext(ctx, "SELECT due_date, state FROM items WHERE id = ?", id).Scan(&due, &state); err != nil {
			return nil, err
		}
		call.seen = append(call.seen, id+" "+due.String+" "+state)
	}
	h.mu.Lock()
	h.calls = append(h.calls, call)
	err := h.err
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	events := make([]watch.Event, 0, len(ids))
	for _, id := range ids {
		events = append(events, watch.Event{
			At:   now,
			Kind: watch.KindItemDue,
			Due:  &watch.ItemDue{ItemID: id, Type: string(TypeTask), DueDate: "2026-11-10", State: string(StateBacklog)},
		})
	}
	return events, nil
}

// take returns the calls so far and forgets them.
func (h *dueRecorder) take() []dueCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	calls := h.calls
	h.calls = nil
	return calls
}

// withDueRecorder makes a dueRecorder r's due hook and returns it.
func (r *storeRig) withDueRecorder() *dueRecorder {
	h := &dueRecorder{}
	r.store.SetDueHook(h.hook)
	return h
}

// dueLines renders the item and item_due events after position after as
// "item <op> <id>" and "item_due <id>", in log order.
func (r *storeRig) dueLines(t *testing.T, after int64) []string {
	t.Helper()
	page, err := r.log.Read(t.Context(), watch.Query{
		After:  after,
		Filter: watch.Filter{Kinds: []watch.Kind{watch.KindItem, watch.KindItemDue}},
		Limit:  1000,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var out []string
	for _, e := range page.Events {
		switch {
		case e.Kind == watch.KindItem && e.Item != nil:
			out = append(out, "item "+e.Item.Op+" "+e.Item.ItemID)
		case e.Kind == watch.KindItemDue && e.Due != nil:
			out = append(out, "item_due "+e.Due.ItemID)
		default:
			t.Fatalf("event %d = %+v, want an item or item_due event", e.ID, e)
		}
	}
	return out
}

// wantDueLines fails the test unless got is exactly want, in order.
func wantDueLines(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s: events =\n%q\nwant\n%q", what, got, want)
	}
}

func TestDueHookRunsInsideCreate(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	h := r.withDueRecorder()
	tail := r.tail(t)
	it := r.create(t, NewItem{Type: TypeTask, Title: "pay the invoice", Area: AreaWork, DueDate: "2026-11-12"})
	calls := h.take()
	if len(calls) != 1 {
		t.Fatalf("hook calls = %+v, want 1", calls)
	}
	c := calls[0]
	if !reflect.DeepEqual(c.ids, []string{it.ID}) || !c.now.Equal(it.CreatedAt) {
		t.Errorf("hook call = %+v, want ids [%s] at %v", c, it.ID, it.CreatedAt)
	}
	if want := []string{it.ID + " 2026-11-12 backlog"}; !reflect.DeepEqual(c.seen, want) {
		t.Errorf("hook saw %q, want %q: it runs after the row is written", c.seen, want)
	}
	wantDueLines(t, "create", r.dueLines(t, tail), "item created "+it.ID, "item_due "+it.ID)
}

func TestDueHookSkipsADuplicateCreate(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	h := r.withDueRecorder()
	n := NewItem{Type: TypeTask, Title: "call the bank", Area: AreaPersonal, DueDate: "2026-11-12", IdempotencyKey: "due-k-1"}
	first := r.create(t, n)
	if calls := h.take(); len(calls) != 1 {
		t.Fatalf("hook calls on the first create = %+v, want 1", calls)
	}
	tail := r.tail(t)
	again, _, duplicate, err := r.store.Create(t.Context(), testActor, n)
	if err != nil || !duplicate || again.ID != first.ID {
		t.Fatalf("replay = %s, duplicate %v, %v; want %s, duplicate true", again.ID, duplicate, err, first.ID)
	}
	if calls := h.take(); len(calls) != 0 {
		t.Errorf("hook calls on a replay = %+v, want none", calls)
	}
	if got := r.tail(t); got != tail {
		t.Errorf("log tail = %d after a replay, want %d", got, tail)
	}
}

// Only a change to the due date or the state runs the hook, and only when
// the update changes something.
func TestDueHookRunsWhenTheDueDateOrStateChanges(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	h := r.withDueRecorder()
	it := r.create(t, NewItem{Type: TypeTask, Title: "renew", Area: AreaWork})
	h.take()
	steps := []struct {
		name  string
		patch Patch
		seen  string // the due date and state the hook sees; "" when it must not run
		write bool   // the update changes the item
	}{
		{"title only", Patch{Fields: []string{fieldTitle}, Values: NewItem{Title: "renew the lease"}}, "", true},
		{"due date set", Patch{Fields: []string{fieldDueDate}, Values: NewItem{DueDate: "2026-11-20"}}, "2026-11-20 backlog", true},
		{"same due date", Patch{Fields: []string{fieldDueDate}, Values: NewItem{DueDate: "2026-11-20"}}, "", false},
		{"state", Patch{Fields: []string{fieldState}, Values: NewItem{State: StateInProgress}}, "2026-11-20 in-progress", true},
		{"same state", Patch{Fields: []string{fieldState}, Values: NewItem{State: StateInProgress}}, "", false},
		{
			"priority and due date",
			Patch{Fields: []string{fieldPriority, fieldDueDate}, Values: NewItem{Priority: PriorityP1, DueDate: "2026-11-21"}},
			"2026-11-21 in-progress", true,
		},
		{"due date cleared", Patch{Fields: []string{fieldDueDate}, Values: NewItem{}}, " in-progress", true},
	}
	for _, st := range steps {
		tail := r.tail(t)
		got, err := r.store.Update(t.Context(), testActor, it.ID, st.patch)
		if err != nil {
			t.Fatalf("%s: Update: %v", st.name, err)
		}
		calls := h.take()
		var want []string
		if st.write {
			want = append(want, "item updated "+it.ID)
		}
		if st.seen == "" {
			if len(calls) != 0 {
				t.Errorf("%s: hook calls = %+v, want none", st.name, calls)
			}
		} else {
			want = append(want, "item_due "+it.ID)
			if len(calls) != 1 || !reflect.DeepEqual(calls[0].ids, []string{it.ID}) ||
				!calls[0].now.Equal(got.UpdatedAt) || !reflect.DeepEqual(calls[0].seen, []string{it.ID + " " + st.seen}) {
				t.Errorf("%s: hook calls = %+v, want one for %s seeing %q at %v", st.name, calls, it.ID, st.seen, got.UpdatedAt)
			}
		}
		wantDueLines(t, st.name, r.dueLines(t, tail), want...)
	}
}

func TestDueHookRunsOnRestoreAndNotOnDelete(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	h := r.withDueRecorder()
	story := r.create(t, NewItem{Type: TypeStory, Title: "move house", Area: AreaPersonal})
	task := r.create(t, NewItem{Type: TypeTask, Title: "book the van", ParentID: story.ID, DueDate: "2026-11-01"})
	h.take()

	tail := r.tail(t)
	if _, _, err := r.store.Delete(t.Context(), testActor, story.ID, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls := h.take(); len(calls) != 0 {
		t.Errorf("Delete called the hook: %+v", calls)
	}
	wantDueLines(t, "delete", r.dueLines(t, tail), "item deleted "+story.ID, "item deleted "+task.ID)

	tail = r.tail(t)
	ids, _, err := r.store.Restore(t.Context(), testActor, story.ID, true)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if want := []string{story.ID, task.ID}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("restored %q, want %q", ids, want)
	}
	calls := h.take()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].ids, ids) {
		t.Errorf("hook calls = %+v, want one with the restored ids %q", calls, ids)
	}
	wantDueLines(t, "restore", r.dueLines(t, tail),
		"item restored "+story.ID, "item restored "+task.ID, "item_due "+story.ID, "item_due "+task.ID)

	for range 2 { // the second delete is a no-op
		if _, _, err := r.store.Delete(t.Context(), testActor, task.ID, false); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if calls := h.take(); len(calls) != 0 {
		t.Errorf("Delete called the hook: %+v", calls)
	}
}

// The hook runs in the write's transaction, so its failure undoes the write.
func TestDueHookErrorRollsTheWriteBack(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	h := r.withDueRecorder()
	h.err = errors.New("due check failed")
	revision, tail := r.revision(t), r.tail(t)
	_, _, _, err := r.store.Create(t.Context(), testActor, NewItem{Type: TypeTask, Title: "pay", Area: AreaWork, DueDate: "2026-11-12"})
	if !errors.Is(err, h.err) {
		t.Fatalf("Create err = %v, want the hook's error", err)
	}
	r.unchanged(t, revision, tail)
	var n int
	if err := r.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM items").Scan(&n); err != nil || n != 0 {
		t.Errorf("items rows = %d, %v; want 0 after the rolled-back create", n, err)
	}
}
