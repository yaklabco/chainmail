package rules

import (
	"fmt"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/watch"
)

// hooked returns an items Store over r's database whose due hook is r's
// checker, as the daemon wires it.
func (r *dueRig) hooked() *items.Store {
	st := items.NewStore(r.db, r.log, r.clock.Now)
	st.SetDueHook(r.checker.Hook)
	return st
}

// dueHookStart is 09:00 UTC on 2026-11-10, after the day's due time.
var dueHookStart = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func TestHookFiresInsideItemCreate(t *testing.T) {
	t.Parallel()
	r := newDueRig(t, dueHookStart, dueSettings)
	st := r.hooked()
	tail := r.tail(t)
	soon := r.create(t, st, items.NewItem{Title: "SECRET-DUE soon", DueDate: "2026-11-11"})
	undated := r.create(t, st, items.NewItem{Title: "SECRET-DUE undated"})
	late := r.create(t, st, items.NewItem{Title: "SECRET-DUE late", DueDate: "2026-11-01"})
	closed := r.create(t, st, items.NewItem{Title: "SECRET-DUE closed", DueDate: "2026-11-01", State: items.StateDone})
	wantDue(t, "events", r.lines(t, tail),
		"item created "+soon.ID, dueLine(watch.KindItemDueSoon, soon),
		"item created "+undated.ID,
		"item created "+late.ID, dueLine(watch.KindItemOverdue, late),
		"item created "+closed.ID)
	r.scan(t, 0) // the hook recorded what it fired
	r.noText(t, "SECRET-DUE")
}

func TestHookSkipsADuplicateCreate(t *testing.T) {
	t.Parallel()
	r := newDueRig(t, dueHookStart, dueSettings)
	st := r.hooked()
	n := items.NewItem{
		Type: items.TypeTask, Title: "keyed", Area: items.AreaWork, DueDate: "2026-11-11", IdempotencyKey: "due-key-1",
	}
	first := r.create(t, st, n)
	tail := r.tail(t)
	again, _, duplicate, err := st.Create(t.Context(), dueActor, n)
	if err != nil || !duplicate || again.ID != first.ID {
		t.Fatalf("replay = %s, duplicate %v, %v; want %s, duplicate true", again.ID, duplicate, err, first.ID)
	}
	if got := r.tail(t); got != tail {
		t.Errorf("log tail = %d after a replay, want %d", got, tail)
	}
	wantDue(t, "sent", r.sent(t, first.ID), "2026-11-11 item_due_soon")
}

func TestHookFiresOnItemUpdate(t *testing.T) {
	t.Parallel()
	r := newDueRig(t, dueHookStart, dueSettings)
	st := r.hooked()
	it := r.create(t, st, items.NewItem{Title: "SECRET-DUE renew"})
	steps := []struct {
		name   string
		fields []string
		values items.NewItem
		stage  watch.Kind // the due event after the item event; "" for none
		write  bool       // false: the update changes nothing
	}{
		{"due date set", []string{"due_date"}, items.NewItem{DueDate: "2026-11-11"}, watch.KindItemDueSoon, true},
		{"same due date", []string{"due_date"}, items.NewItem{DueDate: "2026-11-11"}, "", false},
		{"title", []string{"title"}, items.NewItem{Title: "SECRET-DUE renew the lease"}, "", true},
		{"due today", []string{"due_date"}, items.NewItem{DueDate: "2026-11-10"}, watch.KindItemDue, true},
		{"done", []string{"state"}, items.NewItem{State: items.StateDone}, "", true},
		{"reopened", []string{"state"}, items.NewItem{State: items.StateBacklog}, "", true},
		{"overdue date", []string{"due_date"}, items.NewItem{DueDate: "2026-11-05"}, watch.KindItemOverdue, true},
	}
	for _, s := range steps {
		tail := r.tail(t)
		got, err := st.Update(t.Context(), dueActor, it.ID, items.Patch{Fields: s.fields, Values: s.values})
		if err != nil {
			t.Fatalf("%s: Update: %v", s.name, err)
		}
		var want []string
		if s.write {
			want = append(want, "item updated "+it.ID)
		}
		if s.stage != "" {
			want = append(want, dueLine(s.stage, got))
		}
		wantDue(t, s.name, r.lines(t, tail), want...)
	}
	wantDue(t, "sent", r.sent(t, it.ID),
		"2026-11-05 item_due", "2026-11-05 item_due_soon", "2026-11-05 item_overdue",
		"2026-11-10 item_due", "2026-11-10 item_due_soon",
		"2026-11-11 item_due_soon")
	r.noText(t, "SECRET-DUE")
}

func TestHookFiresOnItemRestore(t *testing.T) {
	t.Parallel()
	r := newDueRig(t, dueHookStart, dueSettings)
	st := r.hooked()
	story := r.create(t, r.plain, items.NewItem{Type: items.TypeStory, Title: "move house"})
	late := r.create(t, r.plain, items.NewItem{Title: "book the van", DueDate: "2026-11-01", ParentID: story.ID})
	if _, _, err := r.plain.Delete(t.Context(), dueActor, story.ID, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	tail := r.tail(t)
	if _, _, err := st.Restore(t.Context(), dueActor, story.ID, true); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	wantDue(t, "restore events", r.lines(t, tail),
		"item restored "+story.ID, "item restored "+late.ID, dueLine(watch.KindItemOverdue, late))

	tail = r.tail(t)
	if _, _, err := st.Delete(t.Context(), dueActor, story.ID, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	wantDue(t, "delete events", r.lines(t, tail), "item deleted "+story.ID, "item deleted "+late.ID)
	wantDue(t, "sent", r.sent(t, late.ID), "2026-11-01 item_due", "2026-11-01 item_due_soon", "2026-11-01 item_overdue")
}

// Hook checks only the open items among its IDs, binds them in chunks,
// appends nothing itself and writes only through the caller's transaction,
// so a rollback leaves no item_due_sent rows.
func TestHookUsesOnlyTheCallersTransaction(t *testing.T) {
	t.Parallel()
	r := newDueRig(t, dueHookStart, dueSettings)
	soon := r.create(t, r.plain, items.NewItem{Title: "soon", DueDate: "2026-11-11"})
	closed := r.create(t, r.plain, items.NewItem{Title: "closed", DueDate: "2026-11-11", State: items.StateDone})
	undated := r.create(t, r.plain, items.NewItem{Title: "undated"})
	gone := r.create(t, r.plain, items.NewItem{Title: "gone", DueDate: "2026-11-11"})
	if _, _, err := r.plain.Delete(t.Context(), dueActor, gone.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ids := []string{closed.ID, undated.ID, gone.ID}
	for i := range 600 {
		ids = append(ids, fmt.Sprintf("it_missing_%d", i))
	}
	ids = append(ids, soon.ID) // past the first chunk
	tail := r.tail(t)

	tx, err := r.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }() // the explicit rollback below is the one that counts
	events, err := r.checker.Hook(t.Context(), tx, ids, dueHookStart)
	if err != nil {
		t.Fatalf("Hook: %v", err)
	}
	none, err := r.checker.Hook(t.Context(), tx, nil, dueHookStart)
	if err != nil {
		t.Fatalf("Hook with no ids: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	if len(events) != 1 || events[0].Kind != watch.KindItemDueSoon || events[0].ID != 0 || !events[0].At.Equal(dueHookStart) ||
		events[0].Due == nil || *events[0].Due != (watch.ItemDue{ItemID: soon.ID, Type: "task", DueDate: "2026-11-11", State: "backlog"}) {
		t.Errorf("Hook events = %+v, want one unappended item_due_soon for %s", events, soon.ID)
	}
	if len(none) != 0 {
		t.Errorf("Hook with no ids = %+v, want none", none)
	}
	if got := r.tail(t); got != tail {
		t.Errorf("log tail = %d, want %d: Hook leaves appending to the caller", got, tail)
	}
	var n int
	if err := r.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM item_due_sent").Scan(&n); err != nil || n != 0 {
		t.Errorf("item_due_sent rows = %d, %v; want 0 after the rollback", n, err)
	}
}
