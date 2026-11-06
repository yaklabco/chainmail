package items

import "testing"

// modelTypes is every item type, from the top of the hierarchy down, in the
// order the matrices below list them.
var modelTypes = []Type{
	TypeObjective, TypeKeyResult, TypeInitiative, TypeEpic, TypeFeature, TypeStory,
	TypeBug, TypeSupportTicket, TypeTask,
}

func TestTypeValues(t *testing.T) {
	t.Parallel()
	want := []string{"objective", "key-result", "initiative", "epic", "feature", "story", "bug", "support-ticket", "task"}
	for i, typ := range modelTypes {
		if string(typ) != want[i] {
			t.Errorf("type %d = %q, want %q", i, typ, want[i])
		}
	}
}

// Every ordered pair of the nine types, checked against the rule written
// out by hand: objective > key-result > initiative > epic > feature > story;
// bug and support-ticket under feature or above; task under any non-leaf;
// nothing under a leaf.
func TestCanParent(t *testing.T) {
	t.Parallel()
	// Each row is a parent. Its string has one character per child, in
	// modelTypes order (objective, key-result, initiative, epic, feature,
	// story, bug, support-ticket, task): T allowed, . refused.
	matrix := []struct {
		parent Type
		row    string
	}{
		{TypeObjective, ".TTTTTTTT"},
		{TypeKeyResult, "..TTTTTTT"},
		{TypeInitiative, "...TTTTTT"},
		{TypeEpic, "....TTTTT"},
		{TypeFeature, ".....TTTT"},
		{TypeStory, "........T"},
		{TypeBug, "........."},
		{TypeSupportTicket, "........."},
		{TypeTask, "........."},
	}
	if len(matrix) != len(modelTypes) {
		t.Fatalf("matrix has %d rows, want %d", len(matrix), len(modelTypes))
	}
	cases := 0
	for i, m := range matrix {
		if m.parent != modelTypes[i] || len(m.row) != len(modelTypes) {
			t.Fatalf("matrix row %d is %s %q, want %s with %d columns", i, m.parent, m.row, modelTypes[i], len(modelTypes))
		}
		for j, child := range modelTypes {
			want := m.row[j] == 'T'
			if got := CanParent(m.parent, child); got != want {
				t.Errorf("CanParent(%s, %s) = %v, want %v", m.parent, child, got, want)
			}
			cases++
		}
	}
	if cases != 81 {
		t.Errorf("checked %d pairs, want 81", cases)
	}
}

func TestLeafAndRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		typ  Type
		leaf bool
		root bool
	}{
		{TypeObjective, false, true},
		{TypeKeyResult, false, true},
		{TypeInitiative, false, true},
		{TypeEpic, false, true},
		{TypeFeature, false, true},
		{TypeStory, false, true},
		{TypeBug, true, false},
		{TypeSupportTicket, true, false},
		{TypeTask, true, true}, // a task with no parent is in the inbox
	}
	for _, tt := range tests {
		if got := Leaf(tt.typ); got != tt.leaf {
			t.Errorf("Leaf(%s) = %v, want %v", tt.typ, got, tt.leaf)
		}
		if got := CanBeRoot(tt.typ); got != tt.root {
			t.Errorf("CanBeRoot(%s) = %v, want %v", tt.typ, got, tt.root)
		}
	}
}

// A type the rules do not know is never a leaf, a root, a parent or a child.
func TestRulesRefuseUnknownTypes(t *testing.T) {
	t.Parallel()
	for _, bogus := range []Type{"", "Task", "subtask"} {
		if Leaf(bogus) || CanBeRoot(bogus) {
			t.Errorf("Leaf(%q) or CanBeRoot(%q) is true", bogus, bogus)
		}
		for _, typ := range modelTypes {
			if CanParent(bogus, typ) || CanParent(typ, bogus) {
				t.Errorf("CanParent with %q and %s is true", bogus, typ)
			}
		}
	}
}
