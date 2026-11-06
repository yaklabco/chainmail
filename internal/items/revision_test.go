package items

import "testing"

func TestRevisionsOnlyIncrease(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	var (
		epic, task, other Item
		crit              []Criterion
	)
	steps := []struct {
		name string
		run  func() error
	}{
		{"ensure admin", func() error { return r.store.EnsureAdmin(ctx, testActor) }},
		{"create epic", func() (err error) {
			epic, _, _, err = r.store.Create(ctx, testActor, NewItem{Type: TypeEpic, Title: "epic", Area: AreaWork})
			return err
		}},
		{"create task", func() (err error) {
			task, crit, _, err = r.store.Create(ctx, testActor, NewItem{Type: TypeTask, Title: "task", ParentID: epic.ID, Criteria: []string{"c"}})
			return err
		}},
		{"create chore", func() (err error) {
			other, _, _, err = r.store.Create(ctx, testActor, NewItem{Type: TypeTask, Title: "chore", ParentID: AdminWorkID})
			return err
		}},
		{"update", func() error {
			_, err := r.store.Update(ctx, testActor, task.ID, Patch{Fields: []string{fieldTitle}, Values: NewItem{Title: "task two"}})
			return err
		}},
		{"move", func() error {
			_, err := r.store.Update(ctx, testActor, task.ID, Patch{Fields: []string{fieldParent}, Values: NewItem{ParentID: AdminWorkID}})
			return err
		}},
		{"edit criteria", func() error {
			_, _, err := r.store.EditCriteria(ctx, testActor, task.ID, CriteriaEdit{Add: []string{"d"}})
			return err
		}},
		{"tick", func() error {
			_, _, err := r.store.TickCriterion(ctx, testActor, task.ID, crit[0].ID, true, "proof")
			return err
		}},
		{"block", func() error {
			_, _, err := r.store.SetBlocker(ctx, testActor, task.ID, other.ID, true)
			return err
		}},
		{"unblock", func() error {
			_, _, err := r.store.SetBlocker(ctx, testActor, task.ID, other.ID, false)
			return err
		}},
		{"delete", func() error {
			_, _, err := r.store.Delete(ctx, testActor, epic.ID, true)
			return err
		}},
		{"restore", func() error {
			_, _, err := r.store.Restore(ctx, testActor, epic.ID, false)
			return err
		}},
	}
	var last int64
	for _, s := range steps {
		if err := s.run(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		got := r.revision(t)
		if got != last+1 {
			t.Fatalf("after %s the revision is %d, want exactly %d", s.name, got, last+1)
		}
		last = got
	}
	changes := r.changes(t, 0)
	for i := 1; i < len(changes); i++ {
		if changes[i].Revision < changes[i-1].Revision {
			t.Errorf("event %d has revision %d after %d", i, changes[i].Revision, changes[i-1].Revision)
		}
	}
	if changes[len(changes)-1].Revision != last {
		t.Errorf("the last event has revision %d, want %d", changes[len(changes)-1].Revision, last)
	}
	// A write that changes nothing leaves the revision where it is.
	if _, err := r.store.Update(ctx, testActor, task.ID, Patch{Fields: []string{fieldTitle}, Values: NewItem{Title: "task two"}}); err != nil {
		t.Fatal(err)
	}
	if got := r.revision(t); got != last {
		t.Errorf("a no-op update moved the revision to %d", got)
	}
}
