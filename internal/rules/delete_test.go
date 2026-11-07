package rules

import (
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// TestDeleteAndRestore checks a soft delete by name and a restore by ID:
// revisions, timestamps, actors, next_run_at cleared and planned again,
// lookup by name only while live, the second delete a no-op, and one event
// for each change.
func TestDeleteAndRestore(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	d := storeDef("hourly")
	d.Schedule = Schedule{Kind: ScheduleEvery, Every: time.Hour}
	created := r.create(t, d)
	tail := r.tail(t)

	deleted, err := r.store.Delete(t.Context(), otherActor, "hourly")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	delAt := r.clock.Last()
	want := created
	want.Revision, want.DeletedAt, want.UpdatedAt, want.UpdatedBy, want.NextRunAt = 2, delAt, delAt, "1001/mcp", time.Time{}
	sameRule(t, deleted, want)
	sameRule(t, r.get(t, created.ID), want)
	sameRule(t, r.lastRevision(t, created.ID).Rule, want)
	r.wantColumns(t, created.ID, map[string]string{"deleted_at": watch.FormatTime(delAt), "next_run_at": "NULL"})
	_, err = r.store.Get(t.Context(), "hourly")
	wantRefused(t, err, ErrNotFound)
	afterDelete := r.tail(t)

	again, err := r.store.Delete(t.Context(), storeActor, created.ID)
	if err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	sameRule(t, again, want)
	if got := r.tail(t); got != afterDelete {
		t.Errorf("a second delete moved the log tail to %d, want %d", got, afterDelete)
	}

	restored, err := r.store.Restore(t.Context(), storeActor, created.ID)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	resAt := r.clock.Last()
	want.Revision, want.DeletedAt, want.UpdatedAt, want.UpdatedBy, want.NextRunAt = 3, time.Time{}, resAt, "1000/cli", resAt.Add(time.Hour)
	sameRule(t, restored, want)
	sameRule(t, r.get(t, "hourly"), want)
	sameRule(t, r.lastRevision(t, created.ID).Rule, want)
	wantRuleChanges(t, r.ruleChanges(t, tail),
		watch.RuleChange{RuleID: created.ID, Name: "hourly", Change: ChangeDeleted, Revision: 2, Actor: "1001/mcp"},
		watch.RuleChange{RuleID: created.ID, Name: "hourly", Change: ChangeRestored, Revision: 3, Actor: "1000/cli"})
	r.noRuleText(t)
}

// TestDeleteFreesTheName checks that a deleted rule's name can be reused,
// and that restoring the deleted rule then fails and writes nothing.
func TestDeleteFreesTheName(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	old := r.create(t, storeDef("receipts"))
	if _, err := r.store.Delete(t.Context(), storeActor, old.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	fresh := r.create(t, storeDef("receipts"))
	if fresh.ID == old.ID {
		t.Fatal("the new rule reused the deleted rule's ID")
	}
	tail := r.tail(t)
	_, err := r.store.Restore(t.Context(), storeActor, old.ID)
	wantRefused(t, err, errRestoreNameTaken)
	if got := r.get(t, old.ID); got.DeletedAt.IsZero() || got.Revision != 2 {
		t.Errorf("after a refused restore the old rule is %+v, want it deleted at revision 2", got)
	}
	if got := r.tail(t); got != tail {
		t.Errorf("log tail = %d, want %d unchanged", got, tail)
	}
	if got := r.get(t, "receipts"); got.ID != fresh.ID {
		t.Errorf("receipts is %s, want the new rule %s", got.ID, fresh.ID)
	}
}

// TestRestoreRefusesALiveRule checks restore of a live rule by ID.
func TestRestoreRefusesALiveRule(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	_, err := r.store.Restore(t.Context(), storeActor, created.ID)
	wantRefused(t, err, errRuleLive)
	sameRule(t, r.get(t, created.ID), created)
}

// TestRestoreTakesAnIDOnly checks that restore refuses a value that is not
// a rule ID, live name or not, with the fixed text and hint, and writes
// nothing.
func TestRestoreTakesAnIDOnly(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	live := r.create(t, storeDef("receipts"))
	gone := r.create(t, storeDef("invoices"))
	if _, err := r.store.Delete(t.Context(), storeActor, gone.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if errRestoreNeedsID.Msg != "restore takes a rule ID" || errRestoreNeedsID.Hint != "find it with chainmail rule list --deleted" {
		t.Errorf("restore by name refusal = %q, %q", errRestoreNeedsID.Msg, errRestoreNeedsID.Hint)
	}
	tail := r.tail(t)
	for _, rule := range []string{"receipts", "invoices", "unknown", ""} {
		_, err := r.store.Restore(t.Context(), storeActor, rule)
		wantRefused(t, err, errRestoreNeedsID)
	}
	sameRule(t, r.get(t, live.ID), live)
	if got := r.get(t, gone.ID); got.DeletedAt.IsZero() {
		t.Errorf("a refused restore undeleted %s", gone.ID)
	}
	if got := r.tail(t); got != tail {
		t.Errorf("log tail = %d, want %d unchanged", got, tail)
	}
}

// TestUpdateRefusesADeletedRule checks that a deleted rule, found by ID,
// cannot be changed, even by a patch that would change nothing.
func TestUpdateRefusesADeletedRule(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	if _, err := r.store.Delete(t.Context(), storeActor, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, p := range []Patch{{Priority: storePtr(5)}, {}} {
		_, err := r.store.Update(t.Context(), otherActor, created.ID, p, nil)
		wantRefused(t, err, errRuleDeleted)
	}
}

// TestDeleteAndRestoreUnknown checks unknown rules: an unknown ID or name
// is not found for delete, and a deleted rule's name finds nothing to
// delete, since names find live rules only. Restore of an unknown ID is not
// found.
func TestDeleteAndRestoreUnknown(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	if _, err := r.store.Delete(t.Context(), storeActor, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, rule := range []string{"ru_unknown", "unknown", "receipts"} {
		_, err := r.store.Delete(t.Context(), storeActor, rule)
		wantRefused(t, err, ErrNotFound)
	}
	_, err := r.store.Restore(t.Context(), storeActor, "ru_unknown")
	wantRefused(t, err, ErrNotFound)
}
