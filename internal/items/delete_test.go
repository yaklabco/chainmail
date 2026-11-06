package items

import (
	"fmt"
	"slices"
	"testing"

	"github.com/yaklabco/chainmail/internal/watch"
)

// chain makes epic (work) holding story holding task, and early, a story
// under epic that is deleted on its own first. early is returned as stored
// after its delete.
func chain(t *testing.T, r *storeRig) (epic, story, task, early Item) {
	t.Helper()
	epic = r.create(t, NewItem{Type: TypeEpic, Title: "epic", Area: AreaWork})
	story = r.create(t, NewItem{Type: TypeStory, Title: "story", ParentID: epic.ID})
	task = r.create(t, NewItem{Type: TypeTask, Title: "task", ParentID: story.ID})
	early = r.create(t, NewItem{Type: TypeStory, Title: "early", ParentID: epic.ID})
	ids, revision, err := r.store.Delete(t.Context(), testActor, early.ID, false)
	if err != nil || !slices.Equal(ids, []string{early.ID}) || revision != early.Revision+1 {
		t.Fatalf("Delete early = %v at %d, %v", ids, revision, err)
	}
	return epic, story, task, r.get(t, early.ID)
}

func TestDeleteCascade(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	epic, story, task, early := chain(t, r)
	revision, tail := r.revision(t), r.tail(t)
	_, _, err := r.store.Delete(t.Context(), testActor, epic.ID, false)
	wantErr(t, err, errHasChildren)
	r.unchanged(t, revision, tail)

	ids, got, err := r.store.Delete(t.Context(), testActor, epic.ID, true)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	at := r.clock.Last()
	if want := []string{epic.ID, story.ID, task.ID}; !slices.Equal(ids, want) || got != revision+1 {
		t.Errorf("Delete = %v at %d, want %v at %d", ids, got, want, revision+1)
	}
	for _, id := range ids {
		if it := r.get(t, id); !it.DeletedAt.Equal(at) || it.Revision != got || !it.UpdatedAt.Equal(at) {
			t.Errorf("%s after the delete = %+v, want deleted at %v, revision %d", id, it, at, got)
		}
	}
	if it := r.get(t, early.ID); !it.DeletedAt.Equal(early.DeletedAt) || it.Revision != early.Revision {
		t.Errorf("early, deleted before, = %+v; want it untouched", it)
	}
	wantChanges(t, r.changes(t, tail),
		change(epic.ID, TypeEpic, got, OpDeleted),
		change(story.ID, TypeStory, got, OpDeleted),
		change(task.ID, TypeTask, got, OpDeleted))

	// Deleting a deleted item changes nothing.
	tail = r.tail(t)
	ids, again, err := r.store.Delete(t.Context(), testActor, epic.ID, true)
	if err != nil || len(ids) != 0 || again != got {
		t.Errorf("second Delete = %v at %d, %v; want nothing at %d", ids, again, err, got)
	}
	r.unchanged(t, got, tail)
}

func TestDeleteRefuses(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	if err := r.store.EnsureAdmin(t.Context(), testActor); err != nil {
		t.Fatal(err)
	}
	chore := r.create(t, NewItem{Type: TypeTask, Title: "chore", ParentID: AdminWorkID})
	revision, tail := r.revision(t), r.tail(t)
	for _, cascade := range []bool{false, true} {
		_, _, err := r.store.Delete(t.Context(), testActor, AdminWorkID, cascade)
		wantErr(t, err, errOngoingFixed)
	}
	_, _, err := r.store.Delete(t.Context(), testActor, "it_nope", false)
	wantMissing(t, err, "item")
	r.unchanged(t, revision, tail)

	// A chore under Admin deletes like any item, and a parent whose children
	// are all deleted needs no cascade.
	epic := r.create(t, NewItem{Type: TypeEpic, Title: "epic", Area: AreaWork})
	story := r.create(t, NewItem{Type: TypeStory, Title: "story", ParentID: epic.ID})
	for _, id := range []string{chore.ID, story.ID, epic.ID} {
		if ids, _, err := r.store.Delete(t.Context(), testActor, id, false); err != nil || !slices.Equal(ids, []string{id}) {
			t.Errorf("Delete %s = %v, %v", id, ids, err)
		}
	}
}

func TestRestore(t *testing.T) {
	t.Parallel()
	for _, cascade := range []bool{false, true} {
		t.Run(fmt.Sprint("cascade=", cascade), func(t *testing.T) {
			t.Parallel()
			r := newStoreRig(t)
			epic, story, task, early := chain(t, r)
			if _, _, err := r.store.Delete(t.Context(), testActor, epic.ID, true); err != nil {
				t.Fatal(err)
			}
			revision, tail := r.revision(t), r.tail(t)
			ids, got, err := r.store.Restore(t.Context(), testActor, epic.ID, cascade)
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			want := []string{epic.ID}
			if cascade {
				want = append(want, story.ID, task.ID)
			}
			if !slices.Equal(ids, want) || got != revision+1 {
				t.Errorf("Restore = %v at %d, want %v at %d", ids, got, want, revision+1)
			}
			var events []watch.ItemChange
			for _, it := range []Item{epic, story, task, early} {
				live := slices.Contains(want, it.ID)
				if stored := r.get(t, it.ID); stored.DeletedAt.IsZero() != live {
					t.Errorf("%s live = %v, want %v", it.Title, stored.DeletedAt.IsZero(), live)
				}
				if live {
					events = append(events, change(it.ID, it.Type, got, OpRestored))
				}
			}
			wantChanges(t, r.changes(t, tail), events...)
		})
	}
}

func TestRestoreRefuses(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	epic, story, _, _ := chain(t, r)
	if _, _, err := r.store.Delete(t.Context(), testActor, epic.ID, true); err != nil {
		t.Fatal(err)
	}
	live := r.create(t, NewItem{Type: TypeTask, Title: "live", Area: AreaWork})
	holder := r.create(t, NewItem{Type: TypeEpic, Title: "holder", Area: AreaWork})
	held := r.create(t, NewItem{Type: TypeStory, Title: "held", ParentID: holder.ID})
	if _, _, err := r.store.Delete(t.Context(), testActor, held.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Update(t.Context(), testActor, holder.ID, Patch{Fields: []string{fieldType}, Values: NewItem{Type: TypeTask}}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, id string
		want     error
		text     string
	}{
		{"a live item", live.ID, errNotDeleted, ""},
		{"an unknown item", "it_nope", ErrNotFound, "not found: item"},
		{"under a deleted parent", story.ID, errParentDeleted, ""},
		{"under a parent that can no longer hold it", held.ID, errParentRule, ""},
	}
	revision, tail := r.revision(t), r.tail(t)
	for _, tt := range tests {
		for _, cascade := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s cascade=%v", tt.name, cascade), func(t *testing.T) {
				_, _, err := r.store.Restore(t.Context(), testActor, tt.id, cascade)
				wantRefusal(t, err, tt.want, tt.text)
			})
		}
	}
	r.unchanged(t, revision, tail)
}
