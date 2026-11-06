package items

import (
	"slices"
	"testing"
)

// Effective area and priority come from the nearest item that sets them,
// with p2 when nothing on the way to the root sets a priority.
func TestEffectiveValues(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	obj := r.create(t, NewItem{Type: TypeObjective, Title: "objective", Area: AreaWork})
	kr := r.create(t, NewItem{Type: TypeKeyResult, Title: "key result", ParentID: obj.ID, Priority: PriorityP0})
	epic := r.create(t, NewItem{Type: TypeEpic, Title: "epic", ParentID: kr.ID, Area: AreaPersonal})
	task := r.create(t, NewItem{Type: TypeTask, Title: "task", ParentID: epic.ID})
	if task.EffectiveArea != AreaPersonal || task.EffectivePriority != PriorityP0 {
		t.Errorf("Create returned effective %s/%s, want personal/p0", task.EffectiveArea, task.EffectivePriority)
	}
	tests := []struct {
		id                 string
		area, effArea      Area
		priority, effPrior Priority
	}{
		{obj.ID, AreaWork, AreaWork, "", PriorityP2},
		{kr.ID, "", AreaWork, PriorityP0, PriorityP0},
		{epic.ID, AreaPersonal, AreaPersonal, "", PriorityP0},
		{task.ID, "", AreaPersonal, "", PriorityP0},
	}
	for _, tt := range tests {
		got := r.get(t, tt.id)
		if got.Area != tt.area || got.EffectiveArea != tt.effArea || got.Priority != tt.priority || got.EffectivePriority != tt.effPrior {
			t.Errorf("%s: area %q effective %q, priority %q effective %q; want %q %q, %q %q",
				got.Type, got.Area, got.EffectiveArea, got.Priority, got.EffectivePriority,
				tt.area, tt.effArea, tt.priority, tt.effPrior)
		}
	}
}

// rowIDs lists the IDs of rows, in order.
func rowIDs(rows []Row) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.ID
	}
	return out
}

func TestAncestors(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	obj := r.create(t, NewItem{Type: TypeObjective, Title: "objective", Area: AreaWork})
	kr := r.create(t, NewItem{Type: TypeKeyResult, Title: "key result", ParentID: obj.ID})
	epic := r.create(t, NewItem{Type: TypeEpic, Title: "epic", ParentID: kr.ID, Priority: PriorityP1})
	task := r.create(t, NewItem{Type: TypeTask, Title: "task", ParentID: epic.ID})
	rows, err := r.store.Ancestors(t.Context(), task.ID)
	if err != nil {
		t.Fatalf("Ancestors: %v", err)
	}
	if want := []string{obj.ID, kr.ID, epic.ID}; !slices.Equal(rowIDs(rows), want) {
		t.Fatalf("ancestors = %v, want %v", rowIDs(rows), want)
	}
	for i, row := range rows {
		if row.Depth != i || row.Children != 1 || row.EffectiveArea != AreaWork || row.Description != "" {
			t.Errorf("ancestor %d = %+v, want depth %d, one child, area work", i, row, i)
		}
	}
	if rows[2].EffectivePriority != PriorityP1 {
		t.Errorf("epic effective priority = %q, want p1", rows[2].EffectivePriority)
	}
	if rows, err := r.store.Ancestors(t.Context(), obj.ID); err != nil || len(rows) != 0 {
		t.Errorf("Ancestors of a root = %v, %v; want none", rows, err)
	}
	_, err = r.store.Ancestors(t.Context(), "it_nope")
	wantMissing(t, err, "item")
}

func TestChildren(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	epic := r.create(t, NewItem{Type: TypeEpic, Title: "epic", Area: AreaWork})
	var tasks []string
	for range 5 {
		tasks = append(tasks, r.create(t, NewItem{Type: TypeTask, Title: "task", ParentID: epic.ID}).ID)
	}
	if _, _, err := r.store.Delete(t.Context(), testActor, tasks[2], false); err != nil {
		t.Fatal(err)
	}
	live := []string{tasks[0], tasks[1], tasks[3], tasks[4]}
	for _, tt := range []struct {
		limit int
		want  []string
	}{{3, live[:3]}, {50, live}, {0, live}} {
		rows, total, err := r.store.Children(t.Context(), epic.ID, tt.limit)
		if err != nil || total != 4 || !slices.Equal(rowIDs(rows), tt.want) {
			t.Errorf("Children limit %d = %v of %d, %v; want %v of 4", tt.limit, rowIDs(rows), total, err, tt.want)
		}
		for _, row := range rows {
			if row.Depth != 0 {
				t.Errorf("child depth = %d, want 0", row.Depth)
			}
		}
	}
	if rows, total, err := r.store.Children(t.Context(), tasks[0], 50); err != nil || len(rows) != 0 || total != 0 {
		t.Errorf("Children of a leaf = %v of %d, %v", rows, total, err)
	}
	_, _, err := r.store.Children(t.Context(), "it_nope", 50)
	wantMissing(t, err, "item")
}

func TestSubtree(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	epic := r.create(t, NewItem{Type: TypeEpic, Title: "epic", Area: AreaWork})
	s1 := r.create(t, NewItem{Type: TypeStory, Title: "s1", ParentID: epic.ID})
	task := r.create(t, NewItem{Type: TypeTask, Title: "task", ParentID: s1.ID})
	s2 := r.create(t, NewItem{Type: TypeStory, Title: "s2", ParentID: epic.ID})
	r.create(t, NewItem{Type: TypeTask, Title: "under s2", ParentID: s2.ID})
	if _, _, err := r.store.Delete(t.Context(), testActor, s2.ID, true); err != nil {
		t.Fatal(err)
	}
	ids, err := r.store.Subtree(t.Context(), epic.ID)
	if want := []string{epic.ID, s1.ID, task.ID}; err != nil || !slices.Equal(ids, want) {
		t.Errorf("Subtree = %v, %v; want %v", ids, err, want)
	}
	for _, id := range []string{s2.ID, "it_nope"} {
		_, err := r.store.Subtree(t.Context(), id)
		wantMissing(t, err, "item")
	}
}

// Rows count live children, criteria and links.
func TestRowCounts(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	epic := r.create(t, NewItem{Type: TypeEpic, Title: "epic", Area: AreaWork})
	_, crit, _, err := r.store.Create(t.Context(), testActor, NewItem{Type: TypeTask, Title: "task", ParentID: epic.ID, Criteria: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(t.Context(), "UPDATE item_criteria SET ticked = 1, evidence = 'e' WHERE id = ?", crit[0].ID); err != nil {
		t.Fatal(err)
	}
	rows, _, err := r.store.Children(t.Context(), epic.ID, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("Children = %v, %v", rows, err)
	}
	for _, account := range []string{"work", "home"} {
		if _, err := r.db.ExecContext(t.Context(),
			"INSERT INTO item_links (item_id, account, thread_key, created_at, created_by) VALUES (?, ?, 'abc', ?, ?)",
			rows[0].ID, account, formatTime(r.clock.Now()), testActor.String()); err != nil {
			t.Fatal(err)
		}
	}
	rows, _, err = r.store.Children(t.Context(), epic.ID, 0)
	if err != nil || rows[0].CriteriaTotal != 2 || rows[0].CriteriaTicked != 1 || rows[0].Links != 2 || rows[0].Children != 0 {
		t.Errorf("task row = %+v, %v; want 2 criteria, 1 ticked, 2 links, no children", rows, err)
	}
	up, err := r.store.Ancestors(t.Context(), rows[0].ID)
	if err != nil || len(up) != 1 || up[0].Children != 1 || up[0].Links != 0 || up[0].CriteriaTotal != 0 {
		t.Errorf("epic row = %+v, %v; want one child and nothing else", up, err)
	}
}
