package items

import "testing"

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
