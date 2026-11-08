package rules

import (
	"slices"
	"testing"
)

// TestList checks the filters and the priority, ID order: an account
// filter matches rules naming the account and rules on every account, the
// enabled filter, deleted rules only with Deleted, and both combined.
func TestList(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	mk := func(name string, edit func(*Definition)) string {
		d := storeDef(name)
		if edit != nil {
			edit(&d)
		}
		return r.create(t, d).ID
	}
	a := mk("a", func(d *Definition) { d.Priority, d.Accounts = 50, []string{"work"} })
	b := mk("b", nil)
	c := mk("c", func(d *Definition) { d.Accounts, d.Enabled = []string{"gmail"}, false })
	gone := mk("gone", func(d *Definition) { d.Priority = 10 })
	if _, err := r.store.Delete(t.Context(), storeActor, gone); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tests := []struct {
		name string
		q    ListQuery
		want []string
	}{
		{"live", ListQuery{}, []string{a, b, c}},
		{"work", ListQuery{Account: "work"}, []string{a, b}},
		{"gmail", ListQuery{Account: "gmail"}, []string{b, c}},
		{"an account no rule names", ListQuery{Account: "other"}, []string{b}},
		{"enabled", ListQuery{Enabled: storePtr(true)}, []string{a, b}},
		{"disabled", ListQuery{Enabled: storePtr(false)}, []string{c}},
		{"disabled on gmail", ListQuery{Account: "gmail", Enabled: storePtr(false)}, []string{c}},
		{"deleted", ListQuery{Deleted: true}, []string{gone}},
		{"deleted on gmail", ListQuery{Deleted: true, Account: "gmail"}, []string{gone}},
		{"deleted and disabled", ListQuery{Deleted: true, Enabled: storePtr(false)}, nil},
	}
	for _, tc := range tests {
		got, err := r.store.List(t.Context(), tc.q)
		if err != nil {
			t.Fatalf("%s: List: %v", tc.name, err)
		}
		if ids := ruleIDs(got); !slices.Equal(ids, tc.want) {
			t.Errorf("%s: List = %v, want %v", tc.name, ids, tc.want)
		}
	}
	got, err := r.store.List(t.Context(), ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	sameRule(t, got[0], r.get(t, a))
}

// TestRevisions checks the revision history newest first, with the rule as
// it was after each change, the change kinds and actors, the limit, and
// lookup by ID only.
func TestRevisions(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	ctx := t.Context()
	if _, err := r.store.Update(ctx, otherActor, created.ID, Patch{Priority: storePtr(5)}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Update(ctx, otherActor, created.ID, Patch{Stop: storePtr(true)}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Delete(ctx, storeActor, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.Restore(ctx, otherActor, created.ID); err != nil {
		t.Fatal(err)
	}
	revs, err := r.store.Revisions(ctx, created.ID, 0)
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	changes := []string{ChangeRestored, ChangeDeleted, ChangeUpdated, ChangeUpdated, ChangeCreated}
	actors := []string{"1001/mcp", "1000/cli", "1001/mcp", "1001/mcp", "1000/cli"}
	if len(revs) != len(changes) {
		t.Fatalf("%d revisions, want %d", len(revs), len(changes))
	}
	for i, rev := range revs {
		want := int64(len(changes) - i)
		if rev.Revision != want || rev.Rule.Revision != want || rev.Change != changes[i] || rev.Actor != actors[i] {
			t.Errorf("revision %d = %d %s %s (rule revision %d), want %d %s %s",
				i, rev.Revision, rev.Change, rev.Actor, rev.Rule.Revision, want, changes[i], actors[i])
		}
		if !rev.At.Equal(rev.Rule.UpdatedAt) {
			t.Errorf("revision %d at %v, want the rule's updated_at %v", rev.Revision, rev.At, rev.Rule.UpdatedAt)
		}
	}
	sameRule(t, revs[0].Rule, r.get(t, created.ID))
	if revs[1].Rule.DeletedAt.IsZero() || revs[3].Rule.Stop || revs[3].Rule.Priority != 5 || revs[4].Rule.Priority != DefaultPriority {
		t.Errorf("revision rules do not hold the rule as it was after each change: %+v", revs)
	}
	two, err := r.store.Revisions(ctx, created.ID, 2)
	if err != nil || len(two) != 2 || two[0].Revision != 5 || two[1].Revision != 4 {
		t.Errorf("Revisions limit 2 = %+v, %v, want revisions 5 and 4", two, err)
	}
	for _, rule := range []string{"ru_unknown", "receipts"} {
		_, err := r.store.Revisions(ctx, rule, 0)
		wantRefused(t, err, ErrNotFound)
	}
}

// TestEnabledOnNew checks the rules a new-mail batch runs for an account:
// live, enabled and on_new, naming the account or every account, in
// priority then ID order.
func TestEnabledOnNew(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	mk := func(name string, edit func(*Definition)) string {
		d := storeDef(name)
		d.OnNew = true
		edit(&d)
		return r.create(t, d).ID
	}
	all := mk("all", func(d *Definition) { d.Priority = 200 })
	work := mk("work", func(d *Definition) { d.Priority, d.Accounts = 1, []string{"work"} })
	mk("scheduled-only", func(d *Definition) { d.OnNew = false })
	mk("disabled", func(d *Definition) { d.Enabled = false })
	gone := mk("gone", func(d *Definition) { d.Priority = -5 })
	gmail := mk("gmail", func(d *Definition) { d.Accounts = []string{"gmail", "other"} })
	if _, err := r.store.Delete(t.Context(), storeActor, gone); err != nil {
		t.Fatal(err)
	}
	for account, want := range map[string][]string{
		"work":    {work, all},
		"gmail":   {gmail, all},
		"other":   {gmail, all},
		"nothing": {all},
	} {
		got, err := r.store.EnabledOnNew(t.Context(), account)
		if err != nil {
			t.Fatalf("EnabledOnNew %s: %v", account, err)
		}
		if ids := ruleIDs(got); !slices.Equal(ids, want) {
			t.Errorf("EnabledOnNew %s = %v, want %v", account, ids, want)
		}
	}
}
