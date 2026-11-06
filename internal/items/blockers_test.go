package items

import (
	"slices"
	"testing"
)

// blockers returns the IDs of the live items that block id and that id blocks.
func (r *storeRig) blockers(t *testing.T, id string) (blockedBy, blocks []string) {
	t.Helper()
	by, bl, err := r.store.Blockers(t.Context(), id)
	if err != nil {
		t.Fatalf("Blockers: %v", err)
	}
	return rowIDs(by), rowIDs(bl)
}

func TestSetBlocker(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	a := r.create(t, NewItem{Type: TypeTask, Title: "a", Area: AreaWork})
	b := r.create(t, NewItem{Type: TypeTask, Title: "b", Area: AreaWork})
	c := r.create(t, NewItem{Type: TypeTask, Title: "c", Area: AreaWork})
	revision, tail := r.revision(t), r.tail(t)
	set := func(item, by string, linked bool) (bool, int64) {
		t.Helper()
		changed, rev, err := r.store.SetBlocker(t.Context(), testActor, item, by, linked)
		if err != nil {
			t.Fatalf("SetBlocker: %v", err)
		}
		return changed, rev
	}

	if changed, rev := set(a.ID, b.ID, true); !changed || rev != revision+1 {
		t.Errorf("block = %v at %d, want a change at %d", changed, rev, revision+1)
	}
	set(c.ID, a.ID, true)
	if r.get(t, a.ID).Revision != revision+2 || r.get(t, b.ID).Revision != revision+1 {
		t.Error("blocked and blocking items were not both stamped")
	}
	if by, bl := r.blockers(t, a.ID); !slices.Equal(by, []string{b.ID}) || !slices.Equal(bl, []string{c.ID}) {
		t.Errorf("Blockers(a) = %v, %v; want [b], [c]", by, bl)
	}
	if by, bl := r.blockers(t, b.ID); len(by) != 0 || !slices.Equal(bl, []string{a.ID}) {
		t.Errorf("Blockers(b) = %v, %v; want [], [a]", by, bl)
	}
	if changed, rev := set(a.ID, b.ID, true); changed || rev != revision+2 {
		t.Errorf("block again = %v at %d, want no change at %d", changed, rev, revision+2)
	}
	if changed, rev := set(a.ID, b.ID, false); !changed || rev != revision+3 {
		t.Errorf("unblock = %v at %d, want a change at %d", changed, rev, revision+3)
	}
	if changed, rev := set(a.ID, b.ID, false); changed || rev != revision+3 {
		t.Errorf("unblock again = %v at %d, want no change", changed, rev)
	}
	wantChanges(t, r.changes(t, tail),
		change(a.ID, TypeTask, revision+1, OpBlockers), change(b.ID, TypeTask, revision+1, OpBlockers),
		change(c.ID, TypeTask, revision+2, OpBlockers), change(a.ID, TypeTask, revision+2, OpBlockers),
		change(a.ID, TypeTask, revision+3, OpBlockers), change(b.ID, TypeTask, revision+3, OpBlockers))

	// Deleted items drop out of Blockers, and can still be unlinked.
	d := r.create(t, NewItem{Type: TypeTask, Title: "d", Area: AreaWork})
	set(a.ID, d.ID, true)
	r.markDeleted(t, d.ID)
	if by, bl := r.blockers(t, a.ID); len(by) != 0 || !slices.Equal(bl, []string{c.ID}) {
		t.Errorf("Blockers(a) with d deleted = %v, %v; want [], [c]", by, bl)
	}
	if changed, _ := set(a.ID, d.ID, false); !changed {
		t.Error("unlinking a deleted blocker changed nothing")
	}
	_, _, err := r.store.Blockers(t.Context(), "it_nope")
	wantMissing(t, err, "item")
}

func TestSetBlockerRefuses(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	a := r.create(t, NewItem{Type: TypeTask, Title: "a", Area: AreaWork})
	b := r.create(t, NewItem{Type: TypeTask, Title: "b", Area: AreaWork})
	c := r.create(t, NewItem{Type: TypeTask, Title: "c", Area: AreaWork})
	gone := r.create(t, NewItem{Type: TypeTask, Title: "gone", Area: AreaWork})
	r.markDeleted(t, gone.ID)
	goneToo := r.create(t, NewItem{Type: TypeTask, Title: "gone too", Area: AreaWork})
	r.markDeleted(t, goneToo.ID)
	for _, link := range [][2]string{{a.ID, b.ID}, {b.ID, c.ID}} { // a waits on b, b waits on c
		if _, _, err := r.store.SetBlocker(t.Context(), testActor, link[0], link[1], true); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name, item, by string
		want           error
		text           string
	}{
		{"itself", a.ID, a.ID, errSelfBlock, ""},
		{"a direct cycle", b.ID, a.ID, errBlockCycle, ""},
		{"a longer cycle", c.ID, a.ID, errBlockCycle, ""},
		{"an unknown item", "it_nope", a.ID, ErrNotFound, "not found: item"},
		{"an unknown blocker", a.ID, "it_nope", ErrNotFound, "not found: blocker"},
		{"a deleted item", gone.ID, a.ID, errItemDeleted, ""},
		{"a deleted blocker", a.ID, gone.ID, errBlockerDeleted, "the blocker is deleted"},
		{"both deleted", gone.ID, goneToo.ID, errItemDeleted, "the item is deleted"},
	}
	revision, tail := r.revision(t), r.tail(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := r.store.SetBlocker(t.Context(), testActor, tt.item, tt.by, true)
			wantRefusal(t, err, tt.want, tt.text)
		})
	}
	r.unchanged(t, revision, tail)
}
