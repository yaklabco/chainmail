package items

import (
	"encoding/base64"
	"fmt"
	"slices"
	"testing"
)

func boolp(v bool) *bool    { return &v }
func int64p(v int64) *int64 { return &v }

// rawToken encodes s as a page token, for tests that build tokens by hand.
func rawToken(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// listTree builds this tree and returns its IDs by name. Revisions follow
// creation: the Admin items share 1, then obj 2, kr 3, epic 4, t1 5, init 6,
// e2 7, gone 8, inbox 9. Ticking a criterion of t1 moves it to 10, and
// deleting gone moves it to 11.
//
//	admin-personal, admin-work  ongoing initiatives
//	obj     objective, work
//	  kr      key-result
//	    epic    epic, p1, in-progress, one linked thread
//	      t1      task, two criteria, one ticked
//	  init    initiative
//	e2      epic, personal
//	  gone    story, deleted
//	inbox   task, work, no parent
func listTree(t *testing.T) (*storeRig, map[string]string) {
	t.Helper()
	r := newStoreRig(t)
	if err := r.store.EnsureAdmin(t.Context(), testActor); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{"admin-personal": AdminPersonalID, "admin-work": AdminWorkID}
	add := func(name string, n NewItem) {
		n.ParentID = ids[n.ParentID]
		ids[name] = r.create(t, n).ID
	}
	add("obj", NewItem{Type: TypeObjective, Title: "obj", Area: AreaWork})
	add("kr", NewItem{Type: TypeKeyResult, Title: "kr", ParentID: "obj"})
	add("epic", NewItem{Type: TypeEpic, Title: "epic", ParentID: "kr", Priority: PriorityP1, State: StateInProgress})
	add("t1", NewItem{Type: TypeTask, Title: "t1", ParentID: "epic", Criteria: []string{"a", "b"}})
	add("init", NewItem{Type: TypeInitiative, Title: "init", ParentID: "obj"})
	add("e2", NewItem{Type: TypeEpic, Title: "e2", Area: AreaPersonal})
	add("gone", NewItem{Type: TypeStory, Title: "gone", ParentID: "e2"})
	add("inbox", NewItem{Type: TypeTask, Title: "inbox", Area: AreaWork})
	crit, err := r.store.Criteria(t.Context(), ids["t1"])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.store.TickCriterion(t.Context(), testActor, ids["t1"], crit[0].ID, true, "done"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.store.Delete(t.Context(), testActor, ids["gone"], false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(t.Context(),
		"INSERT INTO item_links (item_id, account, thread_key, created_at, created_by) VALUES (?, 'work', 'abc', ?, ?)",
		ids["epic"], formatTime(r.clock.Now()), testActor.String()); err != nil {
		t.Fatal(err)
	}
	return r, ids
}

// listed runs q and renders each row as name@depth, with the names listTree gave.
func listed(t *testing.T, r *storeRig, ids map[string]string, q ListQuery) ([]string, string) {
	t.Helper()
	names := make(map[string]string, len(ids))
	for name, id := range ids {
		names[id] = name
	}
	rows, next, _, err := r.store.List(t.Context(), q)
	if err != nil {
		t.Fatalf("List(%+v): %v", q, err)
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = fmt.Sprintf("%s@%d", names[row.ID], row.Depth)
	}
	return out, next
}

var treeOrder = []string{"admin-personal@0", "admin-work@0", "obj@0", "kr@1", "epic@2", "t1@3", "init@1", "e2@0", "inbox@0"}

func TestListTreeOrder(t *testing.T) {
	t.Parallel()
	r, ids := listTree(t)
	rows, next, revision, err := r.store.List(t.Context(), ListQuery{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got, _ := listed(t, r, ids, ListQuery{}); !slices.Equal(got, treeOrder) || next != "" || revision != 11 {
		t.Errorf("List = %v, next %q, revision %d; want %v, no next, revision 11", got, next, revision, treeOrder)
	}
	byID := make(map[string]Row, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
		if row.Description != "" {
			t.Errorf("row %s carries a description", row.ID)
		}
	}
	checks := []struct {
		name string
		ok   func(Row) bool
	}{
		{"epic", func(x Row) bool {
			return x.Children == 1 && x.Links == 1 && x.EffectivePriority == PriorityP1 && x.State == StateInProgress
		}},
		{"t1", func(x Row) bool {
			return x.CriteriaTotal == 2 && x.CriteriaTicked == 1 && x.EffectiveArea == AreaWork && x.EffectivePriority == PriorityP1
		}},
		{"obj", func(x Row) bool { return x.Children == 2 && x.Revision == 2 }},
		{"e2", func(x Row) bool { return x.Children == 0 && x.EffectiveArea == AreaPersonal }},
		{"inbox", func(x Row) bool { return x.EffectivePriority == PriorityP2 && x.ParentID == "" }},
		{"admin-work", func(x Row) bool { return x.Ongoing && x.Type == TypeInitiative }},
	}
	for _, c := range checks {
		if row := byID[ids[c.name]]; !c.ok(row) {
			t.Errorf("row %s = %+v", c.name, row)
		}
	}
}

func TestListFilters(t *testing.T) {
	t.Parallel()
	r, ids := listTree(t)
	tests := []struct {
		name string
		q    ListQuery
		want []string
	}{
		{"type", ListQuery{Type: TypeEpic}, []string{"epic@2", "e2@0"}},
		{"state", ListQuery{State: StateInProgress}, []string{"epic@2"}},
		{"effective area", ListQuery{Area: AreaPersonal}, []string{"admin-personal@0", "e2@0"}},
		{"under", ListQuery{Under: ids["obj"]}, []string{"kr@0", "epic@1", "t1@2", "init@0"}},
		{"under and type", ListQuery{Under: ids["obj"], Type: TypeTask}, []string{"t1@2"}},
		{"inbox", ListQuery{Inbox: true}, []string{"inbox@0"}},
		{"ongoing", ListQuery{Ongoing: boolp(true)}, []string{"admin-personal@0", "admin-work@0"}},
		{"not ongoing", ListQuery{Ongoing: boolp(false)}, treeOrder[2:]},
		{"deleted included", ListQuery{IncludeDeleted: true, Area: AreaPersonal}, []string{"admin-personal@0", "e2@0", "gone@1"}},
		{"under a parent of deleted items", ListQuery{Under: ids["e2"]}, nil},
		{"under with deleted", ListQuery{Under: ids["e2"], IncludeDeleted: true}, []string{"gone@0"}},
		{"changed since", ListQuery{ChangedSince: int64p(6)}, []string{"e2@0", "inbox@0", "t1@3", "gone@1"}},
		{"changed since, with a type", ListQuery{ChangedSince: int64p(0), Type: TypeTask}, []string{"inbox@0", "t1@3"}},
		{"changed since, under", ListQuery{ChangedSince: int64p(0), Under: ids["obj"]}, []string{"kr@0", "epic@1", "init@0", "t1@2"}},
		{"changed since the latest", ListQuery{ChangedSince: int64p(11)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, next := listed(t, r, ids, tt.q); !slices.Equal(got, tt.want) || next != "" {
				t.Errorf("List = %v, next %q; want %v", got, next, tt.want)
			}
		})
	}
}

func TestListTreePages(t *testing.T) {
	t.Parallel()
	r, ids := listTree(t)
	q := ListQuery{Limit: 4}
	var all []string
	for _, wantNext := range []string{rawToken("t:4"), rawToken("t:8"), ""} {
		got, next := listed(t, r, ids, q)
		if next != wantNext {
			t.Fatalf("page after %v: next %q, want %q", all, next, wantNext)
		}
		all = append(all, got...)
		q.PageToken = next
	}
	if !slices.Equal(all, treeOrder) {
		t.Errorf("pages = %v, want %v", all, treeOrder)
	}
}

// changed_since pages by position, so an item that changes between pages
// moves to the end: nothing repeats and nothing is skipped.
func TestListChangedSincePages(t *testing.T) {
	t.Parallel()
	r, ids := listTree(t)
	q := ListQuery{ChangedSince: int64p(0), Limit: 3}
	first, next := listed(t, r, ids, q)
	if want := rawToken("r:2:" + ids["obj"]); next != want {
		t.Fatalf("first page next = %q, want %q", next, want)
	}
	if _, err := r.store.Update(t.Context(), testActor, ids["kr"], Patch{Fields: []string{fieldTitle}, Values: NewItem{Title: "kr two"}}); err != nil {
		t.Fatal(err)
	}
	all := first
	for pages := 1; next != ""; pages++ {
		if pages > 5 {
			t.Fatal("too many pages")
		}
		q.PageToken = next
		var got []string
		got, next = listed(t, r, ids, q)
		all = append(all, got...)
	}
	want := []string{"admin-personal@0", "admin-work@0", "obj@0", "epic@2", "init@1", "e2@0", "inbox@0", "t1@3", "gone@1", "kr@1"}
	if !slices.Equal(all, want) {
		t.Errorf("pages = %v, want %v", all, want)
	}
}

func TestListRefuses(t *testing.T) {
	t.Parallel()
	r, ids := listTree(t)
	type listCase struct {
		name string
		q    ListQuery
		want error
		text string
	}
	tests := []listCase{
		{"limit below one", ListQuery{Limit: -1}, errListLimit, ""},
		{"limit above 500", ListQuery{Limit: 501}, errListLimit, ""},
		{"unknown type", ListQuery{Type: "chore"}, nil, ""},
		{"unknown state", ListQuery{State: "blocked"}, nil, ""},
		{"unknown area", ListQuery{Area: "home"}, nil, ""},
		{"inbox under an item", ListQuery{Inbox: true, Under: ids["obj"]}, errInboxUnder, ""},
		{"negative changed_since", ListQuery{ChangedSince: int64p(-1)}, errChangedSince, ""},
		{"unknown under", ListQuery{Under: "it_nope"}, ErrNotFound, "not found: item"},
		{"tree token in a changed_since list", ListQuery{ChangedSince: int64p(0), PageToken: rawToken("t:4")}, errPageToken, ""},
		{"revision token in a tree list", ListQuery{PageToken: rawToken("r:5:it_a")}, errPageToken, ""},
	}
	malformed := []string{
		"!!!", "dDoxMA==", rawToken("x:1"), rawToken("t:"), rawToken("t:-1"), rawToken("t:abc"),
		rawToken("t:+3"), rawToken("t:04"), rawToken("r:5"), rawToken("r:x:it_a"), rawToken("r:-1:it_a"), rawToken("r:5:IT_A"),
	}
	for _, bad := range malformed {
		tests = append(tests,
			listCase{"tree token " + bad, ListQuery{PageToken: bad}, errPageToken, ""},
			listCase{"revision token " + bad, ListQuery{ChangedSince: int64p(0), PageToken: bad}, errPageToken, ""})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := r.store.List(t.Context(), tt.q)
			wantRefusal(t, err, tt.want, tt.text)
		})
	}
	for _, limit := range []int{1, 500} {
		if rows, _, _, err := r.store.List(t.Context(), ListQuery{Limit: limit}); err != nil || len(rows) != min(limit, len(treeOrder)) {
			t.Errorf("List limit %d = %d rows, %v", limit, len(rows), err)
		}
	}
}
