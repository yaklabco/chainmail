package items

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// withCriteria creates a work task at the root with texts as its criteria.
func withCriteria(t *testing.T, r *storeRig, texts ...string) (Item, []Criterion) {
	t.Helper()
	it, crit, _, err := r.store.Create(t.Context(), testActor, NewItem{Type: TypeTask, Title: "task", Area: AreaWork, Criteria: texts})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return it, crit
}

// critTexts lists the texts of crit and fails the test unless positions
// run 0, 1, 2 and so on.
func critTexts(t *testing.T, crit []Criterion) []string {
	t.Helper()
	out := make([]string, len(crit))
	for i, c := range crit {
		if c.Position != i {
			t.Errorf("criterion %d has position %d", i, c.Position)
		}
		out[i] = c.Text
	}
	return out
}

func TestEditCriteria(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func([]Criterion) CriteriaEdit
		want []string
	}{
		{
			"remove", func(c []Criterion) CriteriaEdit { return CriteriaEdit{Remove: []string{c[0].ID}} },
			[]string{"b", "c", "d"},
		},
		{"edit", func(c []Criterion) CriteriaEdit {
			return CriteriaEdit{Edit: []CriterionText{{ID: c[1].ID, Text: "  b edited  "}}}
		}, []string{"a", "b edited", "c", "d"}},
		{"order", func(c []Criterion) CriteriaEdit {
			return CriteriaEdit{Order: []string{c[3].ID, c[2].ID, c[1].ID, c[0].ID}}
		}, []string{"d", "c", "b", "a"}},
		{
			"add", func([]Criterion) CriteriaEdit { return CriteriaEdit{Add: []string{"e"}} },
			[]string{"a", "b", "c", "d", "e"},
		},
		{"all four, in that order", func(c []Criterion) CriteriaEdit {
			return CriteriaEdit{
				Remove: []string{c[1].ID},
				Edit:   []CriterionText{{ID: c[2].ID, Text: "c edited"}},
				Order:  []string{c[3].ID, c[2].ID, c[0].ID},
				Add:    []string{"e", "f"},
			}
		}, []string{"d", "c edited", "a", "e", "f"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newStoreRig(t)
			it, crit := withCriteria(t, r, "a", "b", "c", "d")
			revision, tail := r.revision(t), r.tail(t)
			got, rev, err := r.store.EditCriteria(t.Context(), testActor, it.ID, tt.edit(crit))
			if err != nil {
				t.Fatalf("EditCriteria: %v", err)
			}
			if texts := critTexts(t, got); !slices.Equal(texts, tt.want) || rev != revision+1 {
				t.Errorf("criteria = %v at %d, want %v at %d", texts, rev, tt.want, revision+1)
			}
			if stored, err := r.store.Criteria(t.Context(), it.ID); err != nil || !reflect.DeepEqual(stored, got) {
				t.Errorf("stored = %+v, %v; want %+v", stored, err, got)
			}
			if item := r.get(t, it.ID); item.Revision != rev || !item.UpdatedAt.Equal(r.clock.Last()) {
				t.Errorf("item after the edit = %+v, want revision %d", item, rev)
			}
			wantChanges(t, r.changes(t, tail), change(it.ID, TypeTask, rev, OpCriteria))
			r.noText(t, "edited")
		})
	}
}

// Kept criteria keep their IDs and ticks; added ones get new IDs.
func TestEditCriteriaKeepsIDsAndTicks(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	it, crit := withCriteria(t, r, "a", "b")
	if _, _, err := r.store.TickCriterion(t.Context(), testActor, it.ID, crit[0].ID, true, "proof"); err != nil {
		t.Fatal(err)
	}
	got, _, err := r.store.EditCriteria(t.Context(), testActor, it.ID, CriteriaEdit{
		Edit:  []CriterionText{{ID: crit[0].ID, Text: "a2"}},
		Order: []string{crit[1].ID, crit[0].ID},
		Add:   []string{"c"},
	})
	if err != nil || len(got) != 3 {
		t.Fatalf("EditCriteria = %+v, %v", got, err)
	}
	if got[0].ID != crit[1].ID || got[1].ID != crit[0].ID || !got[1].Ticked || got[1].Evidence != "proof" || got[1].Text != "a2" {
		t.Errorf("kept criteria = %+v", got[:2])
	}
	if !strings.HasPrefix(got[2].ID, "cr_") || got[2].ID == crit[0].ID || got[2].ID == crit[1].ID || got[2].Ticked {
		t.Errorf("added criterion = %+v", got[2])
	}
}

func TestEditCriteriaNoChange(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	it, crit := withCriteria(t, r, "a", "b")
	revision, tail := r.revision(t), r.tail(t)
	for _, e := range []CriteriaEdit{
		{},
		{Edit: []CriterionText{{ID: crit[0].ID, Text: " a "}}},
		{Order: []string{crit[0].ID, crit[1].ID}},
	} {
		got, rev, err := r.store.EditCriteria(t.Context(), testActor, it.ID, e)
		if err != nil || rev != revision || !reflect.DeepEqual(got, crit) {
			t.Errorf("EditCriteria(%+v) = %+v at %d, %v; want no change at %d", e, got, rev, err, revision)
		}
	}
	r.unchanged(t, revision, tail)
}

func TestEditCriteriaRefuses(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	it, crit := withCriteria(t, r, "a", "b", "c")
	_, otherCrit := withCriteria(t, r, "x")
	dead, _ := withCriteria(t, r, "z")
	r.markDeleted(t, dead.ID)
	a, b, c := crit[0].ID, crit[1].ID, crit[2].ID
	tests := []struct {
		name, id string
		e        CriteriaEdit
		want     error
		text     string
	}{
		{"an unknown item", "it_nope", CriteriaEdit{Add: []string{"n"}}, ErrNotFound, "not found: item"},
		{"a deleted item", dead.ID, CriteriaEdit{Add: []string{"n"}}, errItemDeleted, ""},
		{"remove an unknown criterion", it.ID, CriteriaEdit{Remove: []string{"cr_nope"}}, ErrNotFound, "not found: criterion"},
		{"remove another item's criterion", it.ID, CriteriaEdit{Remove: []string{otherCrit[0].ID}}, ErrNotFound, "not found: criterion"},
		{"edit an unknown criterion", it.ID, CriteriaEdit{Edit: []CriterionText{{ID: "cr_nope", Text: "n"}}}, ErrNotFound, "not found: criterion"},
		{"edit a removed criterion", it.ID, CriteriaEdit{Remove: []string{a}, Edit: []CriterionText{{ID: a, Text: "n"}}}, ErrNotFound, "not found: criterion"},
		{"edit to blank", it.ID, CriteriaEdit{Edit: []CriterionText{{ID: a, Text: " "}}}, nil, ""},
		{"add blank", it.ID, CriteriaEdit{Add: []string{""}}, nil, ""},
		{"add too long", it.ID, CriteriaEdit{Add: []string{strings.Repeat("x", MaxCriterion+1)}}, nil, ""},
		{"order missing one", it.ID, CriteriaEdit{Order: []string{a, b}}, errCriteriaOrder, ""},
		{"order naming one twice", it.ID, CriteriaEdit{Order: []string{a, b, b}}, errCriteriaOrder, ""},
		{"order naming a removed one", it.ID, CriteriaEdit{Remove: []string{c}, Order: []string{a, b, c}}, errCriteriaOrder, ""},
		{"order naming an unknown one", it.ID, CriteriaEdit{Order: []string{a, b, "cr_nope"}}, errCriteriaOrder, ""},
	}
	revision, tail := r.revision(t), r.tail(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := r.store.EditCriteria(t.Context(), testActor, tt.id, tt.e)
			wantRefusal(t, err, tt.want, tt.text)
		})
	}
	r.unchanged(t, revision, tail)
}

func TestTickCriterion(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	it, crit := withCriteria(t, r, "a", "b")
	revision, tail := r.revision(t), r.tail(t)
	tick := func(ticked bool, evidence string) (Criterion, int64) {
		t.Helper()
		c, rev, err := r.store.TickCriterion(t.Context(), testActor, it.ID, crit[0].ID, ticked, evidence)
		if err != nil {
			t.Fatalf("TickCriterion(%v): %v", ticked, err)
		}
		return c, rev
	}
	c, rev := tick(true, "sent the invoice")
	if !c.Ticked || c.Evidence != "sent the invoice" || !c.TickedAt.Equal(r.clock.Last()) || c.TickedBy != "1000/cli" || rev != revision+1 {
		t.Errorf("ticked = %+v at %d", c, rev)
	}
	if _, rev := tick(true, "sent the invoice"); rev != revision+1 {
		t.Errorf("the same tick again moved the revision to %d", rev)
	}
	if c, rev := tick(true, "paid"); c.Evidence != "paid" || rev != revision+2 {
		t.Errorf("new evidence = %+v at %d", c, rev)
	}
	c, rev = tick(false, "")
	if c.Ticked || c.Evidence != "" || !c.TickedAt.IsZero() || c.TickedBy != "" || rev != revision+3 {
		t.Errorf("unticked = %+v at %d, want everything cleared", c, rev)
	}
	if _, rev := tick(false, ""); rev != revision+3 {
		t.Errorf("the same untick again moved the revision to %d", rev)
	}
	if stored, err := r.store.Criteria(t.Context(), it.ID); err != nil || !reflect.DeepEqual(stored[0], c) {
		t.Errorf("stored = %+v, %v; want %+v", stored, err, c)
	}
	wantChanges(t, r.changes(t, tail),
		change(it.ID, TypeTask, revision+1, OpCriteria),
		change(it.ID, TypeTask, revision+2, OpCriteria),
		change(it.ID, TypeTask, revision+3, OpCriteria))
	r.noText(t, "invoice", "paid")
}

func TestTickCriterionRefuses(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	it, crit := withCriteria(t, r, "a")
	_, otherCrit := withCriteria(t, r, "x")
	dead, deadCrit := withCriteria(t, r, "z")
	r.markDeleted(t, dead.ID)
	tests := []struct {
		name, item, criterion string
		ticked                bool
		evidence              string
		want                  error
		text                  string
	}{
		{"tick without evidence", it.ID, crit[0].ID, true, "", nil, ""},
		{"evidence too long", it.ID, crit[0].ID, true, strings.Repeat("e", MaxEvidence+1), nil, ""},
		{"evidence on an untick", it.ID, crit[0].ID, false, "why", errUntickEvidence, ""},
		{"an unknown criterion", it.ID, "cr_nope", true, "e", ErrNotFound, "not found: criterion"},
		{"another item's criterion", it.ID, otherCrit[0].ID, true, "e", ErrNotFound, "not found: criterion"},
		{"a deleted item", dead.ID, deadCrit[0].ID, true, "e", errItemDeleted, ""},
		{"an unknown item", "it_nope", crit[0].ID, true, "e", ErrNotFound, "not found: item"},
	}
	revision, tail := r.revision(t), r.tail(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := r.store.TickCriterion(t.Context(), testActor, tt.item, tt.criterion, tt.ticked, tt.evidence)
			wantRefusal(t, err, tt.want, tt.text)
		})
	}
	r.unchanged(t, revision, tail)
}

func TestDoneNeedsEveryCriterionTicked(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	it, crit := withCriteria(t, r, "a", "b")
	done := Patch{Fields: []string{fieldState}, Values: NewItem{State: StateDone}}
	for _, c := range crit {
		_, err := r.store.Update(t.Context(), testActor, it.ID, done)
		wantErr(t, err, errDoneUnticked)
		if _, _, err := r.store.TickCriterion(t.Context(), testActor, it.ID, c.ID, true, "proof"); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := r.store.Update(t.Context(), testActor, it.ID, done); err != nil || got.State != StateDone {
		t.Errorf("done after every tick = %+v, %v", got, err)
	}
}

// A criterion ID already held by another item is never rewritten: the
// write fails and the other item's criterion stays as it was.
func TestPutCriteriaRefusesAnotherItemsID(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	owner, ownerCrit := withCriteria(t, r, "owner's")
	other, _ := withCriteria(t, r)
	tx, err := r.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }() // the test reads the store after rolling back
	taken := []Criterion{{ID: ownerCrit[0].ID, Position: 5, Text: "stolen"}}
	if err := putCriteria(t.Context(), tx, other.ID, taken); err == nil {
		t.Fatal("putCriteria with another item's criterion ID succeeded, want an error")
	} else if strings.Contains(err.Error(), ownerCrit[0].ID) || strings.Contains(err.Error(), "stolen") {
		t.Errorf("err = %q, want fixed text", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	stored, err := r.store.Criteria(t.Context(), owner.ID)
	if err != nil || !reflect.DeepEqual(stored, ownerCrit) {
		t.Errorf("owner's criteria = %+v, %v; want %+v unchanged", stored, err, ownerCrit)
	}
	if got, err := r.store.Criteria(t.Context(), other.ID); err != nil || len(got) != 0 {
		t.Errorf("other's criteria = %+v, %v; want none", got, err)
	}
}
