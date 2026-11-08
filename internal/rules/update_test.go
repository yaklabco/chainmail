package rules

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// TestUpdateEachField changes one field at a time and checks the returned
// and stored rule, revision 2 with its rule_revisions row, and one event
// with the change kind: disabled when only enabled turned off, updated
// otherwise.
func TestUpdateEachField(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		patch  Patch
		change string
		want   func(r *Rule, at time.Time)
	}{
		{
			"name",
			Patch{Name: storePtr("invoices")},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.Name = "invoices" },
		},
		{
			"description",
			Patch{Description: storePtr(markDescription + "-2")},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.Description = markDescription + "-2" },
		},
		{
			"query",
			Patch{Query: storePtr("subject:" + markQuery)},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.Query = "subject:" + markQuery },
		},
		{
			"accounts",
			Patch{Accounts: storePtr([]string{"work", "gmail", "gmail"})},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.Accounts = []string{"gmail", "work"} },
		},
		{
			"on_new",
			Patch{OnNew: storePtr(true)},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.OnNew = true },
		},
		{
			"stop",
			Patch{Stop: storePtr(true)},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.Stop = true },
		},
		{
			"disable",
			Patch{Enabled: storePtr(false)},
			ChangeDisabled,
			func(r *Rule, _ time.Time) { r.Enabled = false },
		},
		{
			"schedule",
			Patch{Schedule: &Schedule{Kind: ScheduleEvery, Every: time.Hour}},
			ChangeUpdated,
			func(r *Rule, at time.Time) {
				r.Schedule = Schedule{Kind: ScheduleEvery, Every: time.Hour}
				r.NextRunAt = at.Add(time.Hour)
			},
		},
		{
			"priority",
			Patch{Priority: storePtr(5)},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.Priority = 5 },
		},
		{
			"priority zero",
			Patch{Priority: storePtr(0)},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.Priority = 0 },
		},
		{
			"max_per_run",
			Patch{MaxPerRun: storePtr(50)},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.MaxPerRun = 50 },
		},
		{
			"actions",
			Patch{Actions: storePtr([]Action{{Kind: ActFlag}, {Kind: ActMove, Folder: markFolder}})},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.Actions = []Action{{Kind: ActFlag}, {Kind: ActMove, Folder: markFolder}} },
		},
		{
			"enabled with another field",
			Patch{Enabled: storePtr(false), Priority: storePtr(5)},
			ChangeUpdated,
			func(r *Rule, _ time.Time) { r.Enabled, r.Priority = false, 5 },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newStoreRig(t)
			created := r.create(t, storeDef("receipts"))
			tail := r.tail(t)
			got, err := r.store.Update(t.Context(), otherActor, "receipts", tc.patch, nil)
			if err != nil {
				t.Fatalf("Update: %v", err)
			}
			at := r.clock.Last()
			want := created
			want.Revision, want.UpdatedAt, want.UpdatedBy = 2, at, "1001/mcp"
			tc.want(&want, at)
			sameRule(t, got, want)
			sameRule(t, r.get(t, created.ID), want)
			rev := r.lastRevision(t, created.ID)
			if rev.Revision != 2 || rev.Change != tc.change || rev.Actor != "1001/mcp" || !rev.At.Equal(at) {
				t.Errorf("revision row = %d %s %s %v, want 2 %s 1001/mcp %v", rev.Revision, rev.Change, rev.Actor, rev.At, tc.change, at)
			}
			sameRule(t, rev.Rule, want)
			wantRuleChanges(t, r.ruleChanges(t, tail),
				watch.RuleChange{RuleID: created.ID, Name: want.Name, Change: tc.change, Revision: 2, Actor: "1001/mcp"})
			r.noRuleText(t)
		})
	}
}

// TestUpdateRenames checks that the old name stops finding the rule, the
// new one finds it, and the old name is free for another rule.
func TestUpdateRenames(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	if _, err := r.store.Update(t.Context(), storeActor, created.ID, Patch{Name: storePtr("invoices")}, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	_, err := r.store.Get(t.Context(), "receipts")
	wantRefused(t, err, ErrNotFound)
	if got := r.get(t, "invoices"); got.ID != created.ID {
		t.Errorf("invoices is %s, want %s", got.ID, created.ID)
	}
	if again := r.create(t, storeDef("receipts")); again.ID == created.ID {
		t.Errorf("the freed name went to the renamed rule")
	}
}

// TestUpdateWithoutAChangeWritesNothing checks patches that change nothing
// once normalized: no revision, no row, no event, and check is never called.
func TestUpdateWithoutAChangeWritesNothing(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	tail := r.tail(t)
	calls := 0
	check := func(Definition) error { calls++; return nil }
	patches := []Patch{
		{},
		{Name: storePtr("receipts"), Priority: storePtr(DefaultPriority), Enabled: storePtr(true)},
		{Accounts: storePtr([]string{})},
		{MaxPerRun: storePtr(0)}, // Normalize fills a zero max_per_run with the default
		{Actions: storePtr([]Action{{Kind: ActLabel, Label: markLabel}})},
	}
	for i, p := range patches {
		got, err := r.store.Update(t.Context(), otherActor, created.ID, p, check)
		if err != nil {
			t.Fatalf("patch %d: Update: %v", i, err)
		}
		sameRule(t, got, created)
	}
	if calls != 0 {
		t.Errorf("check called %d times, want 0", calls)
	}
	sameRule(t, r.get(t, created.ID), created)
	if n := r.count(t, "SELECT count(*) FROM rule_revisions WHERE rule_id = ?", created.ID); n != 1 {
		t.Errorf("%d revision rows, want 1", n)
	}
	if got := r.tail(t); got != tail {
		t.Errorf("log tail = %d, want %d unchanged", got, tail)
	}
}

// TestUpdateRefusesAStaleRevision checks ExpectedRevision: a mismatch is
// refused with the fixed text and writes nothing, even for a patch that
// would change nothing, and the current revision goes through.
func TestUpdateRefusesAStaleRevision(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	if errStaleRevision.Msg != "the rule changed since it was read" {
		t.Errorf("stale revision text = %q", errStaleRevision.Msg)
	}
	tail := r.tail(t)
	_, err := r.store.Update(t.Context(), otherActor, created.ID,
		Patch{Priority: storePtr(5), ExpectedRevision: storePtr(int64(2))}, nil)
	wantRefused(t, err, errStaleRevision)
	if got := r.tail(t); got != tail {
		t.Fatalf("log tail = %d, want %d unchanged", got, tail)
	}
	got, err := r.store.Update(t.Context(), otherActor, created.ID,
		Patch{Priority: storePtr(5), ExpectedRevision: storePtr(int64(1))}, nil)
	if err != nil || got.Revision != 2 || got.Priority != 5 {
		t.Fatalf("Update at revision 1 = %+v, %v, want revision 2 with priority 5", got, err)
	}
	for _, p := range []Patch{
		{Stop: storePtr(true), ExpectedRevision: storePtr(int64(1))},
		{ExpectedRevision: storePtr(int64(1))},
	} {
		_, err := r.store.Update(t.Context(), otherActor, created.ID, p, nil)
		wantRefused(t, err, errStaleRevision)
	}
	if got := r.get(t, created.ID); got.Revision != 2 || got.Stop {
		t.Errorf("after refused updates the rule is %+v, want revision 2 without stop", got)
	}
}

// TestUpdateCheckSeesTheMergedDefinition checks that check gets the stored
// definition with the patch applied and normalized, and that its refusal is
// returned unchanged with nothing written.
func TestUpdateCheckSeesTheMergedDefinition(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	tail := r.tail(t)
	refusal := errors.New("refused by the caller")
	var seen Definition
	_, err := r.store.Update(t.Context(), otherActor, "receipts",
		Patch{Priority: storePtr(5), Accounts: storePtr([]string{"work", "gmail"})},
		func(d Definition) error { seen = d; return refusal })
	wantRefused(t, err, refusal)
	want := created.Definition
	want.Priority, want.Accounts = 5, []string{"gmail", "work"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("check saw\n%+v\nwant\n%+v", seen, want)
	}
	sameRule(t, r.get(t, created.ID), created)
	if got := r.tail(t); got != tail {
		t.Errorf("log tail = %d, want %d unchanged", got, tail)
	}
}

// TestUpdateRefusesATakenName checks that a rename onto another live rule's
// name is refused and writes nothing.
func TestUpdateRefusesATakenName(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	r.create(t, storeDef("receipts"))
	other := r.create(t, storeDef("invoices"))
	_, err := r.store.Update(t.Context(), otherActor, "invoices", Patch{Name: storePtr("receipts")}, nil)
	wantRefused(t, err, errNameTaken)
	sameRule(t, r.get(t, other.ID), other)
}

// TestUpdateRefusesWhatTheTableCannotHold checks the store floor on the
// merged definition.
func TestUpdateRefusesWhatTheTableCannotHold(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	for _, tc := range []struct {
		patch Patch
		want  error
	}{
		{Patch{Name: storePtr("Bad Name")}, errStoreName},
		{Patch{Actions: storePtr([]Action{})}, errStoreActions},
		{Patch{MaxPerRun: storePtr(MaxMaxPerRun + 1)}, errStoreMaxPerRun},
		{Patch{Schedule: storePtr(Schedule{Kind: "weekly", Every: time.Hour})}, errStoreSchedule},
		{Patch{Schedule: storePtr(Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 7}, Every: time.Hour})}, errStoreSchedule},
		{Patch{Actions: storePtr([]Action{{Kind: ActArchive, Label: "Receipts"}})}, errStoreAction},
		{Patch{Actions: storePtr([]Action{{Kind: ActMove, Folder: "Archive", Note: "lost"}})}, errStoreAction},
	} {
		_, err := r.store.Update(t.Context(), otherActor, created.ID, tc.patch, nil)
		wantRefused(t, err, tc.want)
	}
	sameRule(t, r.get(t, created.ID), created)
}

// TestUpdateEnablingClearsTheDisabledReason checks that a rule disabled by
// the runner keeps its reason and counter through other updates, and that
// enabling it clears both with change enabled.
func TestUpdateEnablingClearsTheDisabledReason(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	r.exec(t, "UPDATE rules SET enabled = 0, disabled_reason = ?, failed_runs = 5 WHERE id = ?",
		ReasonRepeatedFailures, created.ID)
	got, err := r.store.Update(t.Context(), otherActor, created.ID, Patch{Priority: storePtr(5)}, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Enabled || got.DisabledReason != ReasonRepeatedFailures || got.FailedRuns != 5 {
		t.Fatalf("after a priority change the rule is %+v, want it still disabled with its reason and counter", got)
	}
	tail := r.tail(t)
	got, err = r.store.Update(t.Context(), otherActor, created.ID, Patch{Enabled: storePtr(true)}, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !got.Enabled || got.DisabledReason != "" || got.FailedRuns != 0 || got.Revision != 3 {
		t.Errorf("enabled rule = %+v, want enabled, no reason, no failed runs, revision 3", got)
	}
	r.wantColumns(t, created.ID, map[string]string{"enabled": "1", "disabled_reason": "", "failed_runs": "0"})
	wantRuleChanges(t, r.ruleChanges(t, tail),
		watch.RuleChange{RuleID: created.ID, Name: "receipts", Change: ChangeEnabled, Revision: 3, Actor: "1001/mcp"})
}

// TestUpdateReplansTheSchedule checks next_run_at across updates: kept by
// an unrelated change, cleared by disabling and by removing the schedule,
// and set again by enabling and by a new schedule.
func TestUpdateReplansTheSchedule(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	d := storeDef("hourly")
	d.Schedule = Schedule{Kind: ScheduleEvery, Every: time.Hour}
	created := r.create(t, d)
	update := func(p Patch) Rule {
		t.Helper()
		got, err := r.store.Update(t.Context(), otherActor, created.ID, p, nil)
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		return got
	}
	if got := update(Patch{Description: storePtr("other")}); !got.NextRunAt.Equal(created.NextRunAt) {
		t.Errorf("after a description change NextRunAt = %v, want %v kept", got.NextRunAt, created.NextRunAt)
	}
	if got := update(Patch{Enabled: storePtr(false)}); !got.NextRunAt.IsZero() {
		t.Errorf("disabled NextRunAt = %v, want none", got.NextRunAt)
	}
	r.wantColumns(t, created.ID, map[string]string{"next_run_at": "NULL"})
	if got, at := update(Patch{Enabled: storePtr(true)}), r.clock.Last(); !got.NextRunAt.Equal(at.Add(time.Hour)) {
		t.Errorf("enabled NextRunAt = %v, want %v", got.NextRunAt, at.Add(time.Hour))
	}
	daily := Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 7}}
	if got := update(Patch{Schedule: &daily}); !got.NextRunAt.Equal(time.Date(2026, 11, 7, 7, 0, 0, 0, time.UTC)) {
		t.Errorf("at 07:00 NextRunAt = %v, want 2026-11-07 07:00 UTC", got.NextRunAt)
	}
	if got := update(Patch{Schedule: &Schedule{}}); !got.NextRunAt.IsZero() {
		t.Errorf("unscheduled NextRunAt = %v, want none", got.NextRunAt)
	}
}

// TestUpdateRetriesAfterARacingWrite checks the optimistic write: a change
// that commits between Update's read and its write makes Update read again,
// check again and apply the patch over the newer rule, unless
// ExpectedRevision pins the older one.
func TestUpdateRetriesAfterARacingWrite(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	created := r.create(t, storeDef("receipts"))
	tail := r.tail(t)
	racer := func(calls *int) func(Definition) error {
		return func(Definition) error {
			*calls++
			if *calls > 1 {
				return nil
			}
			_, err := r.store.Update(t.Context(), storeActor, created.ID, Patch{Stop: storePtr(!r.get(t, created.ID).Stop)}, nil)
			return err
		}
	}
	calls := 0
	got, err := r.store.Update(t.Context(), otherActor, "receipts", Patch{Priority: storePtr(5)}, racer(&calls))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if calls != 2 || got.Revision != 3 || got.Priority != 5 || !got.Stop {
		t.Errorf("Update = %+v after %d checks, want revision 3 with priority 5 and the racer's stop, after 2 checks", got, calls)
	}
	wantRuleChanges(t, r.ruleChanges(t, tail),
		watch.RuleChange{RuleID: created.ID, Name: "receipts", Change: ChangeUpdated, Revision: 2, Actor: "1000/cli"},
		watch.RuleChange{RuleID: created.ID, Name: "receipts", Change: ChangeUpdated, Revision: 3, Actor: "1001/mcp"})
	pinned := 0
	_, err = r.store.Update(t.Context(), otherActor, created.ID,
		Patch{Priority: storePtr(9), ExpectedRevision: storePtr(int64(3))}, racer(&pinned))
	wantRefused(t, err, errStaleRevision)
	if got := r.get(t, created.ID); got.Revision != 4 || got.Priority != 5 {
		t.Errorf("after a pinned update lost the race the rule is %+v, want the racer's revision 4 with priority 5", got)
	}
}

// TestUpdateUnknownRule checks that updating a rule that does not exist is
// not found.
func TestUpdateUnknownRule(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	_, err := r.store.Update(t.Context(), otherActor, "receipts", Patch{Priority: storePtr(5)}, nil)
	wantRefused(t, err, ErrNotFound)
}
