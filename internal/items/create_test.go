package items

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestCreateStoresTheItem(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	it, crit, duplicate, err := r.store.Create(t.Context(), testActor, NewItem{
		Type: TypeObjective, Title: "  SECRET-TITLE ship it  ", Description: "SECRET-BODY",
		State: StateInProgress, Area: AreaWork, Priority: PriorityP1,
		StartDate: "2026-11-01", DueDate: "2026-12-31", Estimate: floatp(2.5),
		Criteria: []string{"SECRET-CRIT one", "SECRET-CRIT two"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if duplicate {
		t.Error("duplicate = true for a first create")
	}
	if !ValidID(it.ID) || !strings.HasPrefix(it.ID, "it_") || len(it.ID) != len("it_")+26 {
		t.Errorf("ID = %q, want it_ and 26 ULID characters", it.ID)
	}
	at := r.clock.Last()
	want := Item{
		ID: it.ID, Type: TypeObjective, Title: "SECRET-TITLE ship it", Description: "SECRET-BODY",
		State: StateInProgress, Area: AreaWork, Priority: PriorityP1,
		EffectiveArea: AreaWork, EffectivePriority: PriorityP1,
		StartDate: "2026-11-01", DueDate: "2026-12-31", Estimate: floatp(2.5),
		Revision: 1, CreatedAt: at, UpdatedAt: at, CreatedBy: "1000/cli", UpdatedBy: "1000/cli",
	}
	sameItem(t, it, want)
	sameItem(t, r.get(t, it.ID), want)

	texts := []string{"SECRET-CRIT one", "SECRET-CRIT two"}
	if len(crit) != len(texts) {
		t.Fatalf("criteria = %+v, want %d", crit, len(texts))
	}
	for i, c := range crit {
		if !strings.HasPrefix(c.ID, "cr_") || len(c.ID) != len("cr_")+26 || c.Position != i || c.Text != texts[i] ||
			c.Ticked || c.Evidence != "" || !c.TickedAt.IsZero() || c.TickedBy != "" {
			t.Errorf("criterion %d = %+v", i, c)
		}
	}
	stored, err := r.store.Criteria(t.Context(), it.ID)
	if err != nil || !reflect.DeepEqual(stored, crit) {
		t.Errorf("Criteria = %+v, %v; want %+v", stored, err, crit)
	}
	wantChanges(t, r.changes(t, 0), change(it.ID, TypeObjective, 1, OpCreated))
	r.noText(t, "SECRET")
	var keys int
	if err := r.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM item_requests").Scan(&keys); err != nil || keys != 0 {
		t.Errorf("item_requests rows = %d, %v; want 0 without a key", keys, err)
	}
}

// Every non-leaf type and task may be a root, and a root needs an area.
func TestCreateRoots(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	all := []Type{TypeObjective, TypeKeyResult, TypeInitiative, TypeEpic, TypeFeature, TypeStory, TypeBug, TypeSupportTicket, TypeTask}
	for _, typ := range all {
		t.Run(string(typ), func(t *testing.T) {
			needsParent := typ == TypeBug || typ == TypeSupportTicket
			it, _, _, err := r.store.Create(t.Context(), testActor, NewItem{Type: typ, Title: "root", Area: AreaPersonal})
			switch {
			case needsParent:
				wantErr(t, err, errNeedsParent)
			case err != nil:
				t.Fatalf("Create: %v", err)
			case it.ParentID != "" || it.EffectiveArea != AreaPersonal || it.EffectivePriority != PriorityP2:
				t.Errorf("root = %+v, want no parent, area personal, priority p2", it)
			}
			_, _, _, err = r.store.Create(t.Context(), testActor, NewItem{Type: typ, Title: "root"})
			if needsParent {
				wantErr(t, err, errNeedsParent)
			} else {
				wantErr(t, err, errRootArea)
			}
		})
	}
}

// place creates an item of type typ in area work: at the root when the type
// may be one, else under a new feature.
func (r *storeRig) place(t *testing.T, typ Type) Item {
	t.Helper()
	if CanBeRoot(typ) {
		return r.create(t, NewItem{Type: typ, Title: string(typ), Area: AreaWork})
	}
	feature := r.create(t, NewItem{Type: TypeFeature, Title: "feature", Area: AreaWork})
	return r.create(t, NewItem{Type: typ, Title: string(typ), ParentID: feature.ID})
}

// The store applies the parent rule to every create. The rule itself is
// tested pair by pair in T2's rules tests; these pairs cover each clause.
func TestCreatePlacement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		parent, child Type
		ok            bool
	}{
		{TypeObjective, TypeKeyResult, true},
		{TypeObjective, TypeStory, true},
		{TypeKeyResult, TypeInitiative, true},
		{TypeInitiative, TypeEpic, true},
		{TypeEpic, TypeFeature, true},
		{TypeFeature, TypeStory, true},
		{TypeFeature, TypeBug, true},
		{TypeObjective, TypeSupportTicket, true},
		{TypeStory, TypeTask, true},
		{TypeInitiative, TypeTask, true},
		{TypeStory, TypeBug, false},
		{TypeStory, TypeSupportTicket, false},
		{TypeStory, TypeStory, false},
		{TypeEpic, TypeInitiative, false},
		{TypeKeyResult, TypeObjective, false},
		{TypeTask, TypeTask, false},
		{TypeBug, TypeTask, false},
		{TypeSupportTicket, TypeTask, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.parent)+" holds "+string(tt.child), func(t *testing.T) {
			t.Parallel()
			r := newStoreRig(t)
			parent := r.place(t, tt.parent)
			revision, tail := r.revision(t), r.tail(t)
			got, _, _, err := r.store.Create(t.Context(), testActor, NewItem{Type: tt.child, Title: "child", ParentID: parent.ID})
			if !tt.ok {
				wantErr(t, err, errParentRule)
				r.unchanged(t, revision, tail)
				return
			}
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got.ParentID != parent.ID || got.Area != "" || got.EffectiveArea != AreaWork || got.Revision != revision+1 {
				t.Errorf("child = %+v, want under %s in area work at revision %d", got, parent.ID, revision+1)
			}
			wantChanges(t, r.changes(t, tail), change(got.ID, tt.child, revision+1, OpCreated))
		})
	}
}

func TestCreateRefuses(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	parent := r.create(t, NewItem{Type: TypeEpic, Title: "epic", Area: AreaWork})
	gone := r.create(t, NewItem{Type: TypeEpic, Title: "gone", Area: AreaWork})
	r.markDeleted(t, gone.ID)
	valid := NewItem{Type: TypeTask, Title: "task", Area: AreaWork}
	tests := []struct {
		name string
		edit func(*NewItem)
		want error  // nil: any *InvalidError
		text string // when set, the exact error text
	}{
		{name: "blank title", edit: func(n *NewItem) { n.Title = "   " }},
		{name: "title too long", edit: func(n *NewItem) { n.Title = strings.Repeat("é", MaxTitle+1) }},
		{name: "no type", edit: func(n *NewItem) { n.Type = "" }},
		{name: "unknown type", edit: func(n *NewItem) { n.Type = "chore" }},
		{name: "unknown state", edit: func(n *NewItem) { n.State = "blocked" }},
		{name: "unknown area", edit: func(n *NewItem) { n.Area = "home" }},
		{name: "unknown priority", edit: func(n *NewItem) { n.Priority = "p9" }},
		{name: "impossible start date", edit: func(n *NewItem) { n.StartDate = "2026-02-30" }},
		{name: "malformed due date", edit: func(n *NewItem) { n.DueDate = "tomorrow" }},
		{name: "description too long", edit: func(n *NewItem) { n.Description = strings.Repeat("a", MaxDescription+1) }},
		{name: "blank criterion", edit: func(n *NewItem) { n.Criteria = []string{"fine", " "} }},
		{name: "negative estimate", edit: func(n *NewItem) { n.Estimate = floatp(-1) }, want: errBadEstimate},
		{name: "NaN estimate", edit: func(n *NewItem) { n.Estimate = floatp(math.NaN()) }, want: errBadEstimate},
		{name: "infinite estimate", edit: func(n *NewItem) { n.Estimate = floatp(math.Inf(1)) }, want: errBadEstimate},
		{name: "malformed key", edit: func(n *NewItem) { n.IdempotencyKey = "has space" }, want: errBadKey},
		{name: "key too long", edit: func(n *NewItem) { n.IdempotencyKey = strings.Repeat("k", 129) }, want: errBadKey},
		{name: "done with criteria", edit: func(n *NewItem) { n.State = StateDone; n.Criteria = []string{"c"} }, want: errDoneUnticked},
		{name: "unknown parent", edit: func(n *NewItem) { n.ParentID = "it_nope" }, want: ErrNotFound, text: "not found: parent"},
		{name: "deleted parent", edit: func(n *NewItem) { n.ParentID = gone.ID }, want: errParentDeleted},
		{name: "bug at the root", edit: func(n *NewItem) { n.Type = TypeBug }, want: errNeedsParent},
		{name: "root without area", edit: func(n *NewItem) { n.Area = "" }, want: errRootArea},
	}
	revision, tail := r.revision(t), r.tail(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := valid
			tt.edit(&n)
			_, _, _, err := r.store.Create(t.Context(), testActor, n)
			wantRefusal(t, err, tt.want, tt.text)
		})
	}
	r.unchanged(t, revision, tail)

	// The same request with a live parent, or done with no criteria, is fine.
	child := valid
	child.Area, child.ParentID = "", parent.ID
	r.create(t, child)
	done := valid
	done.State = StateDone
	if got := r.create(t, done); got.State != StateDone {
		t.Errorf("state = %q, want done", got.State)
	}
}

func TestCreateIdempotent(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	n := NewItem{Type: TypeTask, Title: "call the bank", Area: AreaPersonal, Criteria: []string{"booked"}, IdempotencyKey: "k-1"}
	first, firstCrit, duplicate, err := r.store.Create(t.Context(), testActor, n)
	if err != nil || duplicate {
		t.Fatalf("first Create: duplicate %v, %v", duplicate, err)
	}
	r.create(t, NewItem{Type: TypeTask, Title: "something else", Area: AreaWork})
	revision, tail := r.revision(t), r.tail(t)

	// A retry, even with different spacing around the title, returns the
	// first item and writes nothing.
	for _, title := range []string{"call the bank", "  call the bank "} {
		retry := n
		retry.Title = title
		again, againCrit, duplicate, err := r.store.Create(t.Context(), Actor{UID: 1001, Client: "mcp"}, retry)
		if err != nil || !duplicate {
			t.Fatalf("retry %q: duplicate %v, %v; want the first item", title, duplicate, err)
		}
		sameItem(t, again, first)
		if !reflect.DeepEqual(againCrit, firstCrit) {
			t.Errorf("retry criteria = %+v, want %+v", againCrit, firstCrit)
		}
	}
	r.unchanged(t, revision, tail)

	// The same key with a different request is refused.
	other := n
	other.Title = "call the plumber"
	_, _, _, err = r.store.Create(t.Context(), testActor, other)
	wantErr(t, err, ErrKeyMismatch)
	r.unchanged(t, revision, tail)

	var hash, itemID string
	if err := r.db.QueryRowContext(t.Context(),
		"SELECT request_hash, item_id FROM item_requests WHERE key = 'k-1'").Scan(&hash, &itemID); err != nil {
		t.Fatalf("reading the key row: %v", err)
	}
	if len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" || itemID != first.ID {
		t.Errorf("key row = (%q, %q), want a hex SHA-256 and %s", hash, itemID, first.ID)
	}

	// A new key makes a new item.
	fresh := n
	fresh.IdempotencyKey = "k-2"
	second, _, duplicate, err := r.store.Create(t.Context(), testActor, fresh)
	if err != nil || duplicate || second.ID == first.ID || second.Revision != revision+1 {
		t.Errorf("new key: %+v, duplicate %v, %v; want a new item at revision %d", second, duplicate, err, revision+1)
	}
}
