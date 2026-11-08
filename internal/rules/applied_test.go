package rules

import (
	"fmt"
	"testing"
)

// TestAppliedAcrossChunks checks a lookup of more keys than one query
// holds: every marked key is found whichever chunk it falls in, and rows of
// another rule or account never answer.
func TestAppliedAcrossChunks(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	keys := make([]string, 1201)
	for i := range keys {
		keys[i] = fmt.Sprintf("i:7:%d:INBOX", i+1)
	}
	marked := []int{0, 499, 500, 999, 1000, 1200}
	for n, i := range marked {
		if err := r.store.MarkApplied(ctx, "ru_a", "work", keys[i], int64(n+1), AppliedDone, "", ""); err != nil {
			t.Fatalf("MarkApplied: %v", err)
		}
	}
	if err := r.store.MarkApplied(ctx, "ru_b", "work", keys[1], 9, AppliedDone, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := r.store.MarkApplied(ctx, "ru_a", "gmail", keys[2], 9, AppliedDone, "", ""); err != nil {
		t.Fatal(err)
	}
	got, err := r.store.Applied(ctx, "ru_a", "work", keys)
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if len(got) != len(marked) {
		t.Errorf("Applied found %d keys, want %d", len(got), len(marked))
	}
	for n, i := range marked {
		if row, ok := got[keys[i]]; !ok || row.Outcome != AppliedDone || row.RunID != int64(n+1) {
			t.Errorf("key %d = %+v, %v, want applied by run %d", i, row, ok, n+1)
		}
	}
	empty, err := r.store.Applied(ctx, "ru_a", "work", nil)
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("Applied of no keys = %v, %v, want an empty map", empty, err)
	}
}

// TestMarkPendingThenApplied checks the guarded path: a pending row keeps
// its first run and time when marked pending again, then turns held or
// applied with a note.
func TestMarkPendingThenApplied(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	if _, ok := r.appliedRow(t, "ru_a", "work", "g:42"); ok {
		t.Fatal("a row exists before any mark")
	}
	if err := r.store.MarkPending(ctx, "ru_a", "work", "g:42", 1); err != nil {
		t.Fatalf("MarkPending: %v", err)
	}
	first := AppliedRow{Outcome: AppliedPending, RunID: 1, At: r.clock.Last()}
	r.wantApplied(t, "ru_a", "work", "g:42", first)
	if err := r.store.MarkPending(ctx, "ru_a", "work", "g:42", 2); err != nil {
		t.Fatalf("second MarkPending: %v", err)
	}
	r.wantApplied(t, "ru_a", "work", "g:42", first)

	if err := r.store.MarkApplied(ctx, "ru_a", "work", "g:42", 2, AppliedHeld, "", ""); err != nil {
		t.Fatalf("MarkApplied held: %v", err)
	}
	r.wantApplied(t, "ru_a", "work", "g:42", AppliedRow{Outcome: AppliedHeld, RunID: 2, At: r.clock.Last()})
	if err := r.store.MarkApplied(ctx, "ru_a", "work", "g:42", 3, AppliedDone, NoteReplayed, ""); err != nil {
		t.Fatalf("MarkApplied replayed: %v", err)
	}
	r.wantApplied(t, "ru_a", "work", "g:42", AppliedRow{Outcome: AppliedDone, Note: NoteReplayed, RunID: 3, At: r.clock.Last()})
	if err := r.store.MarkPending(ctx, "ru_a", "work", "g:42", 4); err != nil {
		t.Fatalf("MarkPending over applied: %v", err)
	}
	if row, _ := r.appliedRow(t, "ru_a", "work", "g:42"); row.Outcome != AppliedDone || row.RunID != 3 {
		t.Errorf("MarkPending over an applied row left %+v, want it applied by run 3", row)
	}
	if err := r.store.MarkApplied(ctx, "ru_a", "work", "g:43", 5, AppliedDone, NoteOutcomeUnknown, ""); err != nil {
		t.Fatalf("MarkApplied without a pending row: %v", err)
	}
	r.wantApplied(t, "ru_a", "work", "g:43", AppliedRow{Outcome: AppliedDone, Note: NoteOutcomeUnknown, RunID: 5, At: r.clock.Last()})
}

// TestMarkAppliedRefusesPending checks that MarkApplied writes only applied
// or held.
func TestMarkAppliedRefusesPending(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	for _, o := range []AppliedOutcome{AppliedPending, ""} {
		err := r.store.MarkApplied(t.Context(), "ru_a", "work", "g:42", 1, o, "", "")
		wantRefused(t, err, errAppliedOutcome)
	}
	if n := r.count(t, "SELECT count(*) FROM rule_applied"); n != 0 {
		t.Errorf("%d act-once rows, want 0", n)
	}
}

// TestMarkAppliedCopiesEveryRulesRowToTheAlias checks amendment 6: after a
// relocating action, every rule's row for the old key is copied to the new
// key as it is, a row already at the new key is never overwritten, rows of
// another account stay put, and an alias equal to the key copies nothing.
func TestMarkAppliedCopiesEveryRulesRowToTheAlias(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	const old, moved = "i:7:10:INBOX", "i:7:3:Archive"
	mark := func(ruleID, account, key string, runID int64, o AppliedOutcome) {
		t.Helper()
		if err := r.store.MarkApplied(ctx, ruleID, account, key, runID, o, "", ""); err != nil {
			t.Fatalf("MarkApplied: %v", err)
		}
	}
	mark("ru_b", "work", old, 1, AppliedDone)
	bRow, _ := r.appliedRow(t, "ru_b", "work", old)
	mark("ru_c", "work", old, 2, AppliedDone)
	mark("ru_c", "work", moved, 3, AppliedHeld)
	cRow, _ := r.appliedRow(t, "ru_c", "work", moved)
	if err := r.store.MarkPending(ctx, "ru_d", "work", old, 4); err != nil {
		t.Fatal(err)
	}
	dRow, _ := r.appliedRow(t, "ru_d", "work", old)
	mark("ru_e", "gmail", old, 5, AppliedDone)

	if err := r.store.MarkApplied(ctx, "ru_a", "work", old, 6, AppliedDone, "", moved); err != nil {
		t.Fatalf("MarkApplied with an alias: %v", err)
	}
	aRow := AppliedRow{Outcome: AppliedDone, RunID: 6, At: r.clock.Last()}
	r.wantApplied(t, "ru_a", "work", old, aRow)
	r.wantApplied(t, "ru_a", "work", moved, aRow)
	r.wantApplied(t, "ru_b", "work", moved, bRow)
	r.wantApplied(t, "ru_c", "work", moved, cRow)
	r.wantApplied(t, "ru_d", "work", moved, dRow)
	if _, ok := r.appliedRow(t, "ru_e", "gmail", moved); ok {
		t.Error("a row of another account was copied")
	}
	if n := r.count(t, "SELECT count(*) FROM rule_applied WHERE account = 'work' AND message_key = ?", moved); n != 4 {
		t.Errorf("%d rows at the alias, want 4", n)
	}

	if err := r.store.MarkApplied(ctx, "ru_f", "work", "g:9", 7, AppliedDone, "", "g:9"); err != nil {
		t.Fatalf("MarkApplied with the key as alias: %v", err)
	}
	if n := r.count(t, "SELECT count(*) FROM rule_applied WHERE message_key = 'g:9'"); n != 1 {
		t.Errorf("%d rows for g:9, want 1", n)
	}
}

// TestMarkAppliedWritesEveryAliasAtOnce checks spec amendment 6 for a rule
// that both copies and relocates: the acting row and the rows at every
// alias commit in one transaction, so a failure writing any of them writes
// none, and an acting rule's row already at an alias, as the runner writes
// it right after a copy, is upserted without error.
func TestMarkAppliedWritesEveryAliasAtOnce(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	const old, copied, moved = "i:7:10:INBOX", "i:7:4:Saved", "i:7:3:Archive"
	if err := r.store.MarkApplied(ctx, "ru_b", "work", old, 1, AppliedDone, "", ""); err != nil {
		t.Fatal(err)
	}
	bRow, _ := r.appliedRow(t, "ru_b", "work", old)
	if err := r.store.MarkPending(ctx, "ru_a", "work", old, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.store.MarkApplied(ctx, "ru_a", "work", copied, 2, AppliedDone, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.ExecContext(ctx,
		`CREATE TRIGGER refuse_moved BEFORE INSERT ON rule_applied WHEN NEW.message_key = '`+moved+`'
		 BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.store.MarkApplied(ctx, "ru_a", "work", old, 3, AppliedHeld, NoteReplayed, copied, moved); err == nil {
		t.Fatal("MarkApplied wrote through a refused alias")
	}
	if row, _ := r.appliedRow(t, "ru_a", "work", old); row.Outcome != AppliedPending {
		t.Errorf("acting row after a failed alias = %+v, want it still pending", row)
	}
	if _, ok := r.appliedRow(t, "ru_b", "work", copied); ok {
		t.Error("another rule's row reached the copy although the move's alias failed")
	}
	if _, err := r.db.ExecContext(ctx, `DROP TRIGGER refuse_moved`); err != nil {
		t.Fatal(err)
	}
	if err := r.store.MarkApplied(ctx, "ru_a", "work", old, 3, AppliedHeld, NoteReplayed, copied, moved, ""); err != nil {
		t.Fatalf("MarkApplied with two aliases: %v", err)
	}
	aRow := AppliedRow{Outcome: AppliedHeld, Note: NoteReplayed, RunID: 3, At: r.clock.Last()}
	for _, key := range []string{old, copied, moved} {
		r.wantApplied(t, "ru_a", "work", key, aRow)
	}
	r.wantApplied(t, "ru_b", "work", copied, bRow)
	r.wantApplied(t, "ru_b", "work", moved, bRow)
}
