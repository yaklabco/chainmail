package items

import "testing"

// family is the tree most update tests start from: obj (objective, work, p3)
// holding epic and epic2, and epic holding story.
type family struct{ obj, epic, story, epic2 Item }

func newFamily(t *testing.T, r *storeRig) family {
	t.Helper()
	obj := r.create(t, NewItem{Type: TypeObjective, Title: "objective", Area: AreaWork, Priority: PriorityP3})
	epic := r.create(t, NewItem{Type: TypeEpic, Title: "epic", ParentID: obj.ID})
	story := r.create(t, NewItem{Type: TypeStory, Title: "story", Description: "body", ParentID: epic.ID})
	epic2 := r.create(t, NewItem{Type: TypeEpic, Title: "epic two", ParentID: obj.ID})
	return family{obj: obj, epic: epic, story: story, epic2: epic2}
}

func TestUpdateEachField(t *testing.T) {
	t.Parallel()
	editor := Actor{UID: 1001, Client: "mcp"}
	tests := []struct {
		field  string
		values func(family) NewItem
		op     string
		want   func(*Item, family)
	}{
		{
			fieldType, func(family) NewItem { return NewItem{Type: TypeFeature} }, OpUpdated,
			func(it *Item, _ family) { it.Type = TypeFeature },
		},
		{
			fieldTitle, func(family) NewItem { return NewItem{Title: "  renamed  "} }, OpUpdated,
			func(it *Item, _ family) { it.Title = "renamed" },
		},
		{
			fieldDescription, func(family) NewItem { return NewItem{Description: "new body"} }, OpUpdated,
			func(it *Item, _ family) { it.Description = "new body" },
		},
		{
			fieldState, func(family) NewItem { return NewItem{State: StateInReview} }, OpUpdated,
			func(it *Item, _ family) { it.State = StateInReview },
		},
		{
			fieldArea, func(family) NewItem { return NewItem{Area: AreaPersonal} }, OpUpdated,
			func(it *Item, _ family) { it.Area, it.EffectiveArea = AreaPersonal, AreaPersonal },
		},
		{
			fieldPriority, func(family) NewItem { return NewItem{Priority: PriorityP0} }, OpUpdated,
			func(it *Item, _ family) { it.Priority, it.EffectivePriority = PriorityP0, PriorityP0 },
		},
		{
			fieldStartDate, func(family) NewItem { return NewItem{StartDate: "2026-11-09"} }, OpUpdated,
			func(it *Item, _ family) { it.StartDate = "2026-11-09" },
		},
		{
			fieldDueDate, func(family) NewItem { return NewItem{DueDate: "2026-11-30"} }, OpUpdated,
			func(it *Item, _ family) { it.DueDate = "2026-11-30" },
		},
		{
			fieldEstimate, func(family) NewItem { return NewItem{Estimate: floatp(3.5)} }, OpUpdated,
			func(it *Item, _ family) { it.Estimate = floatp(3.5) },
		},
		{
			fieldParent, func(f family) NewItem { return NewItem{ParentID: f.epic2.ID} }, OpMoved,
			func(it *Item, f family) { it.ParentID = f.epic2.ID },
		},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()
			r := newStoreRig(t)
			f := newFamily(t, r)
			before := r.get(t, f.story.ID)
			revision, tail := r.revision(t), r.tail(t)
			got, err := r.store.Update(t.Context(), editor, f.story.ID, Patch{Fields: []string{tt.field}, Values: tt.values(f)})
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			want := before
			tt.want(&want, f)
			want.Revision, want.UpdatedAt, want.UpdatedBy = revision+1, r.clock.Last(), "1001/mcp"
			sameItem(t, got, want)
			sameItem(t, r.get(t, f.story.ID), want)
			wantChanges(t, r.changes(t, tail), change(f.story.ID, want.Type, revision+1, tt.op, tt.field))
			r.noText(t, "renamed", "new body")
		})
	}
}

// Only changed fields count, in mask order and once each. A patch that
// changes nothing writes nothing and returns the item as stored.
func TestUpdateChangedFields(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	f := newFamily(t, r)
	before := r.get(t, f.story.ID)
	revision, tail := r.revision(t), r.tail(t)
	got, err := r.store.Update(t.Context(), testActor, f.story.ID, Patch{
		Fields: []string{fieldTitle, fieldArea, fieldEstimate},
		Values: NewItem{Title: " story "},
	})
	if err != nil {
		t.Fatalf("no-op Update: %v", err)
	}
	sameItem(t, got, before)
	r.unchanged(t, revision, tail)

	got, err = r.store.Update(t.Context(), testActor, f.story.ID, Patch{
		Fields: []string{fieldPriority, fieldTitle, fieldArea, fieldPriority},
		Values: NewItem{Title: "story", Priority: PriorityP1, Area: AreaPersonal},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Priority != PriorityP1 || got.Area != AreaPersonal || got.Revision != revision+1 {
		t.Errorf("updated = %+v, want p1, personal, revision %d", got, revision+1)
	}
	wantChanges(t, r.changes(t, tail), change(f.story.ID, TypeStory, revision+1, OpUpdated, fieldPriority, fieldArea))
}

func TestUpdateClears(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	f := newFamily(t, r)
	full := r.create(t, NewItem{
		Type: TypeStory, Title: "full", Description: "body", Area: AreaPersonal, Priority: PriorityP0,
		StartDate: "2026-11-02", DueDate: "2026-11-20", Estimate: floatp(5), ParentID: f.epic.ID,
	})
	fields := []string{fieldDescription, fieldArea, fieldPriority, fieldStartDate, fieldDueDate, fieldEstimate}
	revision, tail := r.revision(t), r.tail(t)
	got, err := r.store.Update(t.Context(), testActor, full.ID, Patch{Fields: fields})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	want := full
	want.Description, want.Area, want.Priority, want.StartDate, want.DueDate, want.Estimate = "", "", "", "", "", nil
	want.EffectiveArea, want.EffectivePriority = AreaWork, PriorityP3
	want.Revision, want.UpdatedAt = revision+1, r.clock.Last()
	sameItem(t, got, want)
	wantChanges(t, r.changes(t, tail), change(full.ID, TypeStory, revision+1, OpUpdated, fields...))

	// A root's priority clears too; it and everything below fall back to p2.
	root, err := r.store.Update(t.Context(), testActor, f.obj.ID, Patch{Fields: []string{fieldPriority}})
	if err != nil || root.Priority != "" || root.EffectivePriority != PriorityP2 {
		t.Errorf("cleared root = %+v, %v; want no priority, effective p2", root, err)
	}
	if got := r.get(t, full.ID); got.EffectivePriority != PriorityP2 {
		t.Errorf("child effective priority = %q, want p2", got.EffectivePriority)
	}
}

func TestUpdateMovesAndRetypes(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	f := newFamily(t, r)
	task := r.create(t, NewItem{Type: TypeTask, Title: "task", ParentID: f.story.ID})
	revision, tail := r.revision(t), r.tail(t)
	update := func(id string, p Patch) Item {
		t.Helper()
		got, err := r.store.Update(t.Context(), testActor, id, p)
		if err != nil {
			t.Fatalf("Update %v: %v", p.Fields, err)
		}
		return got
	}

	// To the root, with the area the root needs in the same patch.
	got := update(task.ID, Patch{Fields: []string{fieldArea, fieldParent}, Values: NewItem{Area: AreaPersonal}})
	if got.ParentID != "" || got.EffectiveArea != AreaPersonal || got.EffectivePriority != PriorityP2 {
		t.Errorf("moved to the root = %+v", got)
	}
	// Back under a parent.
	if got = update(task.ID, Patch{Fields: []string{fieldParent}, Values: NewItem{ParentID: f.epic2.ID}}); got.ParentID != f.epic2.ID {
		t.Errorf("parent = %q, want %q", got.ParentID, f.epic2.ID)
	}
	// A move carries the subtree; only the moved item gets the revision.
	update(f.epic.ID, Patch{Fields: []string{fieldParent, fieldArea}, Values: NewItem{Area: AreaPersonal}})
	if story := r.get(t, f.story.ID); story.EffectiveArea != AreaPersonal || story.Revision != f.story.Revision {
		t.Errorf("story after its parent moved = %+v", story)
	}
	wantChanges(t, r.changes(t, tail),
		change(task.ID, TypeTask, revision+1, OpMoved, fieldArea, fieldParent),
		change(task.ID, TypeTask, revision+2, OpMoved, fieldParent),
		change(f.epic.ID, TypeEpic, revision+3, OpMoved, fieldParent, fieldArea))

	// A retype keeps children the new type can hold, and ignores deleted ones.
	if got = update(f.epic2.ID, Patch{Fields: []string{fieldType}, Values: NewItem{Type: TypeFeature}}); got.Type != TypeFeature {
		t.Errorf("type = %q, want feature", got.Type)
	}
	r.markDeleted(t, task.ID)
	if got = update(f.epic2.ID, Patch{Fields: []string{fieldType}, Values: NewItem{Type: TypeTask}}); got.Type != TypeTask {
		t.Errorf("type = %q, want task", got.Type)
	}
}

func TestUpdateRefuses(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	f := newFamily(t, r)
	if err := r.store.EnsureAdmin(t.Context(), testActor); err != nil {
		t.Fatal(err)
	}
	task := r.create(t, NewItem{Type: TypeTask, Title: "task", ParentID: f.story.ID, Criteria: []string{"c"}})
	bug := r.create(t, NewItem{Type: TypeBug, Title: "bug", ParentID: f.epic.ID})
	gone := r.create(t, NewItem{Type: TypeEpic, Title: "gone", ParentID: f.obj.ID})
	r.markDeleted(t, gone.ID)
	dead := r.create(t, NewItem{Type: TypeTask, Title: "dead", ParentID: f.epic2.ID})
	r.markDeleted(t, dead.ID)
	only := func(field string, v NewItem) Patch { return Patch{Fields: []string{field}, Values: v} }
	tests := []struct {
		name  string
		id    string
		patch Patch
		want  error  // nil: any *InvalidError
		text  string // when set, the exact error text
	}{
		{"no fields", f.story.ID, Patch{}, errNoFields, ""},
		{"unknown field", f.story.ID, only("colour", NewItem{}), errUnknownField, ""},
		{"clear type", f.story.ID, only(fieldType, NewItem{}), errClearType, ""},
		{"clear title", f.story.ID, only(fieldTitle, NewItem{}), nil, ""},
		{"clear state", f.story.ID, only(fieldState, NewItem{}), errClearState, ""},
		{"unknown area", f.story.ID, only(fieldArea, NewItem{Area: "home"}), nil, ""},
		{"negative estimate", f.story.ID, only(fieldEstimate, NewItem{Estimate: floatp(-2)}), errBadEstimate, ""},
		{"clear a root's area", f.obj.ID, only(fieldArea, NewItem{}), errRootArea, ""},
		{"to the root without an area", f.epic.ID, only(fieldParent, NewItem{}), errRootArea, ""},
		{"a bug to the root", bug.ID, Patch{Fields: []string{fieldParent, fieldArea}, Values: NewItem{Area: AreaWork}}, errNeedsParent, ""},
		{"retype past the parent", f.story.ID, only(fieldType, NewItem{Type: TypeInitiative}), errParentRule, ""},
		{"retype below a child", f.epic.ID, only(fieldType, NewItem{Type: TypeStory}), errChildRule, ""},
		{"retype to a leaf with children", f.epic.ID, only(fieldType, NewItem{Type: TypeTask}), errChildRule, ""},
		{"move under itself", f.epic.ID, only(fieldParent, NewItem{ParentID: f.epic.ID}), errMoveCycle, ""},
		{"move under its own child", f.epic.ID, only(fieldParent, NewItem{ParentID: f.story.ID}), errMoveCycle, ""},
		{"move under a deleted parent", f.story.ID, only(fieldParent, NewItem{ParentID: gone.ID}), errParentDeleted, ""},
		{"move under an unknown parent", f.story.ID, only(fieldParent, NewItem{ParentID: "it_nope"}), ErrNotFound, "not found: parent"},
		{"move under a leaf", f.story.ID, only(fieldParent, NewItem{ParentID: bug.ID}), errParentRule, ""},
		{"done with unticked criteria", task.ID, only(fieldState, NewItem{State: StateDone}), errDoneUnticked, ""},
		{"an ongoing item done", AdminWorkID, only(fieldState, NewItem{State: StateDone}), errOngoingDone, ""},
		{"move an ongoing item", AdminWorkID, only(fieldParent, NewItem{ParentID: f.obj.ID}), errOngoingFixed, ""},
		{"retype an ongoing item", AdminWorkID, only(fieldType, NewItem{Type: TypeEpic}), errOngoingFixed, ""},
		{"a deleted item", dead.ID, only(fieldTitle, NewItem{Title: "x"}), errItemDeleted, ""},
		{"an unknown item", "it_nope", only(fieldTitle, NewItem{Title: "x"}), ErrNotFound, "not found: item"},
	}
	revision, tail := r.revision(t), r.tail(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := r.store.Update(t.Context(), testActor, tt.id, tt.patch)
			wantRefusal(t, err, tt.want, tt.text)
		})
	}
	r.unchanged(t, revision, tail)
}
