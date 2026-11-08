package rules

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/watch"
)

// TestBeginAndFinishRun checks that a begun run reads as interrupted until
// it finishes, that FinishRun stores the counts, outcome, reason and hint,
// and that a manual run changes no rule revision and writes no event.
func TestBeginAndFinishRun(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	rule := r.create(t, storeDef("receipts"))
	tail := r.tail(t)
	started := r.clock.Now()
	id := r.begin(t, rule.ID, "work", TriggerManual, started)
	begun := Run{ID: id, RuleID: rule.ID, Account: "work", Trigger: TriggerManual, StartedAt: started, FinishedAt: started, Outcome: OutcomeInterrupted}
	runs, next, err := r.store.ListRuns(ctx, RunQuery{RuleID: rule.ID})
	if err != nil || next != 0 {
		t.Fatalf("ListRuns = %v, %v", next, err)
	}
	sameRuns(t, runs, begun)

	done := begun
	done.FinishedAt, done.Outcome = r.clock.Now(), OutcomeOK
	done.Matched, done.Applied, done.Held, done.Skipped = 5, 3, 1, 1
	if r.finish(t, done) {
		t.Fatal("a clean run disabled the rule")
	}
	failed := Run{
		ID: r.begin(t, rule.ID, "gmail", TriggerManual, r.clock.Now()), RuleID: rule.ID, Account: "gmail",
		Trigger: TriggerManual, Matched: 3, Applied: 2, Failed: 1, Outcome: OutcomeOK,
		Reason: "FOLDER_NOT_FOUND", Hint: "run: chainmail folders",
	}
	failed.StartedAt = r.clock.Last()
	failed.FinishedAt = r.clock.Now()
	if r.finish(t, failed) {
		t.Fatal("one failure out of three disabled the rule")
	}
	recent, err := r.store.RecentRuns(ctx, rule.ID, 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	sameRuns(t, recent, failed, done)
	got := r.get(t, rule.ID)
	if got.Revision != 1 || got.FailedRuns != 1 || !got.LastRunAt.IsZero() || !got.Enabled {
		t.Errorf("rule after manual runs = %+v, want revision 1, one failed run, no last run, enabled", got)
	}
	wantRuleChanges(t, r.ruleChanges(t, tail))
}

// TestRunRefusals checks an unknown trigger, outcome and run.
func TestRunRefusals(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	rule := r.create(t, storeDef("receipts"))
	_, err := r.store.BeginRun(ctx, rule.ID, "work", Trigger("cron"), r.clock.Now())
	wantRefused(t, err, errRunTrigger)
	if n := r.count(t, "SELECT count(*) FROM rule_runs"); n != 0 {
		t.Errorf("%d runs, want 0", n)
	}
	id := r.begin(t, rule.ID, "work", TriggerNew, r.clock.Now())
	_, err = r.store.FinishRun(ctx, Run{ID: id, Outcome: Outcome("meh")})
	wantRefused(t, err, errRunOutcome)
	_, err = r.store.FinishRun(ctx, Run{ID: id + 100, Outcome: OutcomeOK})
	wantRefused(t, err, errUnknownRun)
}

// TestFailedRunsCountAndReset checks failed_runs: a run with a failure adds
// one, a clean ok or capped run resets it, and a run that acted on nothing
// or was interrupted without a failure leaves it.
func TestFailedRunsCountAndReset(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	rule := r.create(t, storeDef("receipts"))
	tail := r.tail(t)
	steps := []struct {
		name string
		run  Run
		want int
	}{
		{"a failure", Run{Matched: 3, Applied: 2, Failed: 1, Outcome: OutcomeOK}, 1},
		{"another while capped", Run{Matched: 3, Applied: 2, Failed: 1, Outcome: OutcomeCapped}, 2},
		{"cache not ready", Run{Outcome: OutcomeCacheNotReady}, 2},
		{"interrupted", Run{Matched: 2, Applied: 1, Outcome: OutcomeInterrupted}, 2},
		{"clean", Run{Matched: 2, Applied: 2, Outcome: OutcomeOK}, 0},
		{"a failed run", Run{Matched: 2, Applied: 1, Failed: 1, Outcome: OutcomeFailed}, 1},
		{"clean and capped", Run{Matched: 500, Applied: 500, Outcome: OutcomeCapped}, 0},
	}
	for _, st := range steps {
		run := st.run
		run.ID = r.begin(t, rule.ID, "work", TriggerNew, r.clock.Now())
		run.FinishedAt = r.clock.Now()
		if r.finish(t, run) {
			t.Fatalf("%s disabled the rule", st.name)
		}
		if got := r.get(t, rule.ID).FailedRuns; got != st.want {
			t.Errorf("after %s failed_runs = %d, want %d", st.name, got, st.want)
		}
	}
	if got := r.get(t, rule.ID); got.Revision != 1 || !got.Enabled {
		t.Errorf("rule = %+v, want revision 1 and enabled", got)
	}
	wantRuleChanges(t, r.ruleChanges(t, tail))
}

// TestAutoDisableWhenEveryActedOnMatchFailed checks the first auto-disable
// condition: a run where every match not skipped failed. The rule is
// disabled with reason all_failed by the daemon's rule actor, at a new
// revision with its row and event, its schedule is cleared, and enabling it
// clears the reason and the counter.
func TestAutoDisableWhenEveryActedOnMatchFailed(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	d := storeDef("hourly")
	d.Schedule = Schedule{Kind: ScheduleEvery, Every: time.Hour}
	rule := r.create(t, d)
	tail := r.tail(t)
	actor := fmt.Sprintf("%d/rule", os.Getuid())

	partly := Run{ID: r.begin(t, rule.ID, "work", TriggerSchedule, r.clock.Now()), Matched: 4, Applied: 1, Failed: 3, Outcome: OutcomeOK}
	partly.FinishedAt = r.clock.Now()
	if r.finish(t, partly) {
		t.Fatal("a run with one applied match disabled the rule")
	}
	all := Run{ID: r.begin(t, rule.ID, "work", TriggerSchedule, r.clock.Now()), Matched: 4, Skipped: 1, Failed: 3, Outcome: OutcomeFailed}
	all.FinishedAt = r.clock.Now()
	if !r.finish(t, all) {
		t.Fatal("a run where every acted-on match failed did not disable the rule")
	}
	at := r.clock.Last()
	want := rule
	want.Enabled, want.DisabledReason, want.FailedRuns = false, ReasonAllFailed, 2
	want.Revision, want.UpdatedAt, want.UpdatedBy = 2, at, actor
	want.NextRunAt, want.LastRunAt = time.Time{}, all.FinishedAt
	got := r.get(t, rule.ID)
	sameRule(t, got, want)
	rev := r.lastRevision(t, rule.ID)
	if rev.Change != ChangeDisabled || rev.Actor != actor || rev.Revision != 2 {
		t.Errorf("revision row = %d %s %s, want 2 disabled %s", rev.Revision, rev.Change, rev.Actor, actor)
	}
	sameRule(t, rev.Rule, want)
	wantRuleChanges(t, r.ruleChanges(t, tail),
		watch.RuleChange{RuleID: rule.ID, Name: "hourly", Change: ChangeDisabled, Revision: 2, Actor: actor, Reason: ReasonAllFailed})
	if _, ok, err := r.store.NextScheduled(t.Context()); err != nil || ok {
		t.Errorf("NextScheduled after the auto-disable = %v, %v, want none", ok, err)
	}

	enabled, err := r.store.Update(t.Context(), storeActor, rule.ID, Patch{Enabled: storePtr(true)}, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !enabled.Enabled || enabled.DisabledReason != "" || enabled.FailedRuns != 0 ||
		!enabled.NextRunAt.Equal(r.clock.Last().Add(time.Hour)) || !enabled.LastRunAt.Equal(all.FinishedAt) {
		t.Errorf("re-enabled rule = %+v, want enabled with no reason, no failed runs, a new next run and the last run kept", enabled)
	}
}

// TestAutoDisableAfterFiveFailingRuns checks the second condition: the
// fifth run in a row with a failure disables the rule with reason
// repeated_failures, and a later failing run on the disabled rule only
// counts.
func TestAutoDisableAfterFiveFailingRuns(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	rule := r.create(t, storeDef("receipts"))
	tail := r.tail(t)
	failing := func() Run {
		run := Run{ID: r.begin(t, rule.ID, "work", TriggerNew, r.clock.Now()), Matched: 2, Applied: 1, Failed: 1, Outcome: OutcomeOK}
		run.FinishedAt = r.clock.Now()
		return run
	}
	for i := 1; i <= 5; i++ {
		if disabled := r.finish(t, failing()); disabled != (i == 5) {
			t.Fatalf("run %d disabled = %v, want %v", i, disabled, i == 5)
		}
	}
	got := r.get(t, rule.ID)
	if got.Enabled || got.DisabledReason != ReasonRepeatedFailures || got.FailedRuns != 5 || got.Revision != 2 {
		t.Errorf("rule = %+v, want disabled for repeated_failures after 5 failed runs at revision 2", got)
	}
	actor := fmt.Sprintf("%d/rule", os.Getuid())
	change := watch.RuleChange{RuleID: rule.ID, Name: "receipts", Change: ChangeDisabled, Revision: 2, Actor: actor, Reason: ReasonRepeatedFailures}
	wantRuleChanges(t, r.ruleChanges(t, tail), change)
	if r.finish(t, failing()) {
		t.Error("a failing run on a disabled rule disabled it again")
	}
	if got := r.get(t, rule.ID); got.Revision != 2 || got.FailedRuns != 6 {
		t.Errorf("after a sixth failing run the rule is %+v, want revision 2 with 6 failed runs", got)
	}
	wantRuleChanges(t, r.ruleChanges(t, tail), change)
}

// TestFinishRunKeepsTheSchedule checks the schedule bookkeeping: a finished
// scheduled run sets last_run_at and plans next_run_at from its finish
// time, a manual run leaves both, and neither changes the revision.
func TestFinishRunKeepsTheSchedule(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	d := storeDef("hourly")
	d.Schedule = Schedule{Kind: ScheduleEvery, Every: time.Hour}
	rule := r.create(t, d)
	tail := r.tail(t)
	manual := Run{ID: r.begin(t, rule.ID, "work", TriggerManual, r.clock.Now()), Outcome: OutcomeOK}
	manual.FinishedAt = r.clock.Now()
	r.finish(t, manual)
	if got := r.get(t, rule.ID); !got.LastRunAt.IsZero() || !got.NextRunAt.Equal(rule.NextRunAt) {
		t.Errorf("after a manual run last %v next %v, want none and %v", got.LastRunAt, got.NextRunAt, rule.NextRunAt)
	}
	finished := storeStart.Add(3 * time.Hour)
	sched := Run{ID: r.begin(t, rule.ID, "work", TriggerSchedule, storeStart.Add(2*time.Hour)), FinishedAt: finished, Outcome: OutcomeOK}
	r.finish(t, sched)
	got := r.get(t, rule.ID)
	if !got.LastRunAt.Equal(finished) || !got.NextRunAt.Equal(finished.Add(time.Hour)) || got.Revision != 1 || !got.UpdatedAt.Equal(rule.UpdatedAt) {
		t.Errorf("after a scheduled run the rule is %+v, want last run %v, next %v, revision 1, updated_at kept",
			got, finished, finished.Add(time.Hour))
	}
	if next, ok, err := r.store.NextScheduled(t.Context()); err != nil || !ok || !next.Equal(finished.Add(time.Hour)) {
		t.Errorf("NextScheduled = %v, %v, %v, want %v", next, ok, err, finished.Add(time.Hour))
	}
	wantRuleChanges(t, r.ruleChanges(t, tail))
}

// TestFinishRunRetriesAScheduleTheCacheCouldNotAnswer checks amendment 7: a
// scheduled run that ends cache_not_ready for any reason but cache_disabled
// plans the next run five minutes after its finish, whatever the schedule.
// cache_disabled, a manual run and a disabled rule keep the usual planning.
func TestFinishRunRetriesAScheduleTheCacheCouldNotAnswer(t *testing.T) {
	t.Parallel()
	finished := storeStart.Add(3 * time.Hour)
	hourly := Schedule{Kind: ScheduleEvery, Every: time.Hour}
	daily := Schedule{Kind: ScheduleAt, At: ClockTime{Hour: 7}}
	tests := []struct {
		name     string
		schedule Schedule
		enabled  bool
		trig     Trigger
		reason   string
		want     func(created Rule) time.Time
	}{
		{
			"every, backfilling", hourly, true, TriggerSchedule, "backfilling",
			func(Rule) time.Time { return finished.Add(5 * time.Minute) },
		},
		{
			"every, no reason", hourly, true, TriggerSchedule, "",
			func(Rule) time.Time { return finished.Add(5 * time.Minute) },
		},
		{
			"at, stale", daily, true, TriggerSchedule, "stale",
			func(Rule) time.Time { return finished.Add(5 * time.Minute) },
		},
		{
			"every, cache disabled", hourly, true, TriggerSchedule, "cache_disabled",
			func(Rule) time.Time { return finished.Add(time.Hour) },
		},
		{
			"at, cache disabled", daily, true, TriggerSchedule, "cache_disabled",
			func(Rule) time.Time { return time.Date(2026, 11, 7, 7, 0, 0, 0, time.UTC) },
		},
		{
			"manual", hourly, true, TriggerManual, "backfilling",
			func(created Rule) time.Time { return created.NextRunAt },
		},
		{
			"disabled", hourly, false, TriggerSchedule, "backfilling",
			func(Rule) time.Time { return time.Time{} },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newStoreRig(t)
			d := storeDef("scheduled")
			d.Schedule, d.Enabled = tc.schedule, tc.enabled
			created := r.create(t, d)
			id := r.begin(t, created.ID, "work", tc.trig, storeStart.Add(2*time.Hour))
			if r.finish(t, Run{ID: id, FinishedAt: finished, Outcome: OutcomeCacheNotReady, Reason: tc.reason}) {
				t.Fatal("a cache_not_ready run disabled the rule")
			}
			got := r.get(t, created.ID)
			if want := tc.want(created); !got.NextRunAt.Equal(want) {
				t.Errorf("NextRunAt = %v, want %v", got.NextRunAt, want)
			}
			if got.Revision != 1 || got.FailedRuns != 0 {
				t.Errorf("rule = %+v, want revision 1 with no failed runs", got)
			}
		})
	}
}

// TestPlanRoundKeepsTheEarliestNextRun checks that a scheduled round, one
// run per account, leaves the earliest next run any of its runs needs
// whatever order they finish in: a five-minute retry survives an ok run
// that finishes later, and an ok run that finishes first gives way to the
// retry. FinishRun alone leaves the last finisher's plan, which PlanRound
// then lowers. PlanRound never moves next_run_at later, changes no
// revision and writes no event.
func TestPlanRoundKeepsTheEarliestNextRun(t *testing.T) {
	t.Parallel()
	started := storeStart.Add(2 * time.Hour)
	first, second := started.Add(time.Minute), started.Add(2*time.Minute)
	tests := []struct {
		name          string
		first, second Run
		want          time.Time
	}{
		{
			"retry finishes first",
			Run{Account: "stale", FinishedAt: first, Outcome: OutcomeCacheNotReady, Reason: "stale"},
			Run{Account: "ready", FinishedAt: second, Outcome: OutcomeOK},
			first.Add(5 * time.Minute),
		},
		{
			"ok finishes first",
			Run{Account: "ready", FinishedAt: first, Outcome: OutcomeOK},
			Run{Account: "stale", FinishedAt: second, Outcome: OutcomeCacheNotReady, Reason: "stale"},
			second.Add(5 * time.Minute),
		},
		{
			"both ok",
			Run{Account: "ready", FinishedAt: first, Outcome: OutcomeOK},
			Run{Account: "other", FinishedAt: second, Outcome: OutcomeOK},
			first.Add(6 * time.Hour),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newStoreRig(t)
			ctx := t.Context()
			d := storeDef("six-hourly")
			d.Schedule = Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}
			rule := r.create(t, d)
			tail := r.tail(t)
			round := []Run{tc.first, tc.second}
			for i := range round {
				round[i].ID = r.begin(t, rule.ID, round[i].Account, TriggerSchedule, started)
				round[i].RuleID, round[i].Trigger = rule.ID, TriggerSchedule
			}
			for _, run := range round {
				r.finish(t, run)
			}
			if err := r.store.PlanRound(ctx, rule.ID, round); err != nil {
				t.Fatalf("PlanRound: %v", err)
			}
			got := r.get(t, rule.ID)
			if !got.NextRunAt.Equal(tc.want) || !got.LastRunAt.Equal(second) || got.Revision != 1 {
				t.Errorf("after the round next %v last %v revision %d, want next %v last %v revision 1",
					got.NextRunAt, got.LastRunAt, got.Revision, tc.want, second)
			}
			if next, ok, err := r.store.NextScheduled(ctx); err != nil || !ok || !next.Equal(tc.want) {
				t.Errorf("NextScheduled = %v, %v, %v, want %v", next, ok, err, tc.want)
			}

			// A plan later than the stored one changes nothing.
			late := Run{ID: round[0].ID, RuleID: rule.ID, Trigger: TriggerSchedule, FinishedAt: second.Add(time.Hour), Outcome: OutcomeOK}
			if err := r.store.PlanRound(ctx, rule.ID, []Run{late}); err != nil {
				t.Fatalf("PlanRound: %v", err)
			}
			if again := r.get(t, rule.ID); !again.NextRunAt.Equal(tc.want) {
				t.Errorf("a later plan moved next_run_at to %v, want %v", again.NextRunAt, tc.want)
			}
			wantRuleChanges(t, r.ruleChanges(t, tail))
		})
	}
}

// TestPlanRoundLeavesARuleWithNoNextRun checks that PlanRound plans
// nothing for a disabled or deleted rule, or an unknown one.
func TestPlanRoundLeavesARuleWithNoNextRun(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	d := storeDef("six-hourly")
	d.Schedule = Schedule{Kind: ScheduleEvery, Every: 6 * time.Hour}
	d.Enabled = false
	disabled := r.create(t, d)
	retry := Run{
		RuleID: disabled.ID, Trigger: TriggerSchedule, FinishedAt: storeStart.Add(time.Hour),
		Outcome: OutcomeCacheNotReady, Reason: "stale",
	}
	if err := r.store.PlanRound(ctx, disabled.ID, []Run{retry}); err != nil {
		t.Fatalf("PlanRound: %v", err)
	}
	if got := r.get(t, disabled.ID); !got.NextRunAt.IsZero() {
		t.Errorf("a disabled rule has next run %v, want none", got.NextRunAt)
	}

	d.Name, d.Enabled = "deleted", true
	live := r.create(t, d)
	if _, err := r.store.Delete(ctx, storeActor, live.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	retry.RuleID = live.ID
	if err := r.store.PlanRound(ctx, live.ID, []Run{retry}); err != nil {
		t.Fatalf("PlanRound: %v", err)
	}
	if got := r.get(t, live.ID); !got.NextRunAt.IsZero() {
		t.Errorf("a deleted rule has next run %v, want none", got.NextRunAt)
	}
	if err := r.store.PlanRound(ctx, "r_unknown", []Run{retry}); err != nil {
		t.Errorf("PlanRound of an unknown rule = %v, want nil", err)
	}
}

// TestFinishRunOnADeletedRulePlansNothing checks that a scheduled run that
// finishes after its rule was deleted records the run but plans no next
// run, so the deleted rule keeps next_run_at NULL.
func TestFinishRunOnADeletedRulePlansNothing(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	d := storeDef("hourly")
	d.Schedule = Schedule{Kind: ScheduleEvery, Every: time.Hour}
	created := r.create(t, d)
	id := r.begin(t, created.ID, "work", TriggerSchedule, r.clock.Now())
	deleted, err := r.store.Delete(t.Context(), storeActor, created.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tail := r.tail(t)
	if r.finish(t, Run{ID: id, Matched: 1, Failed: 1, Outcome: OutcomeFailed}) {
		t.Fatal("a run on a deleted rule disabled it")
	}
	r.wantColumns(t, created.ID, map[string]string{"next_run_at": "NULL", "revision": "2", "enabled": "1"})
	if got := r.get(t, created.ID); !got.DeletedAt.Equal(deleted.DeletedAt) {
		t.Errorf("DeletedAt = %v, want %v", got.DeletedAt, deleted.DeletedAt)
	}
	wantRuleChanges(t, r.ruleChanges(t, tail))
}

// TestListRuns checks the filters, newest-first order, keyset paging with
// next_before_id, and the refusals.
func TestListRuns(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	a := r.create(t, storeDef("a")).ID
	b := r.create(t, storeDef("b")).ID
	specs := []struct {
		rule, account string
		trig          Trigger
		outcome       Outcome
	}{
		{a, "work", TriggerNew, OutcomeOK},
		{a, "gmail", TriggerSchedule, OutcomeCapped},
		{b, "work", TriggerManual, OutcomeFailed},
		{a, "work", TriggerManual, OutcomeCacheNotReady},
		{b, "gmail", TriggerNew, OutcomeOK},
		{a, "work", TriggerSchedule, OutcomeOK},
	}
	ids := make([]int64, len(specs))
	for i, sp := range specs {
		ids[i] = r.begin(t, sp.rule, sp.account, sp.trig, r.clock.Now())
		r.finish(t, Run{ID: ids[i], FinishedAt: r.clock.Now(), Outcome: sp.outcome})
	}
	tests := []struct {
		name string
		q    RunQuery
		want []int64
		next int64
	}{
		{"all", RunQuery{}, []int64{ids[5], ids[4], ids[3], ids[2], ids[1], ids[0]}, 0},
		{"rule a", RunQuery{RuleID: a}, []int64{ids[5], ids[3], ids[1], ids[0]}, 0},
		{"gmail", RunQuery{Account: "gmail"}, []int64{ids[4], ids[1]}, 0},
		{"manual", RunQuery{Trigger: TriggerManual}, []int64{ids[3], ids[2]}, 0},
		{"ok", RunQuery{Outcome: OutcomeOK}, []int64{ids[5], ids[4], ids[0]}, 0},
		{"rule a, work, new", RunQuery{RuleID: a, Account: "work", Trigger: TriggerNew}, []int64{ids[0]}, 0},
		{"first page", RunQuery{Limit: 2}, []int64{ids[5], ids[4]}, ids[4]},
		{"second page", RunQuery{Limit: 2, BeforeID: ids[4]}, []int64{ids[3], ids[2]}, ids[2]},
		{"last page", RunQuery{Limit: 2, BeforeID: ids[2]}, []int64{ids[1], ids[0]}, 0},
		{"rule a, paged", RunQuery{RuleID: a, Limit: 3}, []int64{ids[5], ids[3], ids[1]}, ids[1]},
		{"the most a page holds", RunQuery{Limit: 500}, []int64{ids[5], ids[4], ids[3], ids[2], ids[1], ids[0]}, 0},
	}
	for _, tc := range tests {
		runs, next, err := r.store.ListRuns(ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: ListRuns: %v", tc.name, err)
		}
		if got := runIDs(runs); !slices.Equal(got, tc.want) || next != tc.next {
			t.Errorf("%s: ListRuns = %v next %d, want %v next %d", tc.name, got, next, tc.want, tc.next)
		}
	}
	for _, tc := range []struct {
		q    RunQuery
		want error
	}{
		{RunQuery{Limit: -1}, errRunLimit},
		{RunQuery{Limit: 501}, errRunLimit},
		{RunQuery{BeforeID: -1}, errRunBefore},
		{RunQuery{Trigger: "cron"}, errRunTrigger},
		{RunQuery{Outcome: "meh"}, errRunOutcome},
	} {
		_, _, err := r.store.ListRuns(ctx, tc.q)
		wantRefused(t, err, tc.want)
	}
}

// TestRecentRunsAndCounts checks the newest runs of one rule and the totals
// over its retained runs.
func TestRecentRunsAndCounts(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	a := r.create(t, storeDef("a")).ID
	b := r.create(t, storeDef("b")).ID
	for i := range 12 {
		id := r.begin(t, a, "work", TriggerNew, r.clock.Now())
		r.finish(t, Run{ID: id, FinishedAt: r.clock.Now(), Matched: i + 1, Applied: i, Held: 1, Outcome: OutcomeOK})
	}
	id := r.begin(t, b, "work", TriggerNew, r.clock.Now())
	r.finish(t, Run{ID: id, FinishedAt: r.clock.Now(), Matched: 2, Failed: 2, Outcome: OutcomeFailed})

	recent, err := r.store.RecentRuns(ctx, a, 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}
	if len(recent) != 10 {
		t.Fatalf("RecentRuns = %d runs, want 10", len(recent))
	}
	if recent[0].Matched != 12 || recent[9].Matched != 3 {
		t.Errorf("RecentRuns go from matched %d to %d, want 12 to 3", recent[0].Matched, recent[9].Matched)
	}
	if none, err := r.store.RecentRuns(ctx, a, 0); err != nil || len(none) != 0 {
		t.Errorf("RecentRuns 0 = %v, %v, want none", none, err)
	}
	for _, tc := range []struct {
		rule string
		want Counts
	}{
		{a, Counts{Runs: 12, Matched: 78, Applied: 66, Held: 12}},
		{b, Counts{Runs: 1, Matched: 2, Failed: 2}},
		{"ru_unknown", Counts{}},
	} {
		got, err := r.store.Counts(ctx, tc.rule)
		if err != nil || got != tc.want {
			t.Errorf("Counts %s = %+v, %v, want %+v", tc.rule, got, err, tc.want)
		}
	}
}

// TestTrimRuns checks that runs started before the cutoff are deleted and
// runs started at or after it stay.
func TestTrimRuns(t *testing.T) {
	t.Parallel()
	r := newStoreRig(t)
	ctx := t.Context()
	rule := r.create(t, storeDef("receipts")).ID
	cut := storeStart.Add(-30 * 24 * time.Hour)
	r.begin(t, rule, "work", TriggerNew, cut.Add(-time.Nanosecond))
	edge := r.begin(t, rule, "work", TriggerNew, cut)
	fresh := r.begin(t, rule, "work", TriggerNew, storeStart)
	n, err := r.store.TrimRuns(ctx, cut)
	if err != nil || n != 1 {
		t.Fatalf("TrimRuns = %d, %v, want 1", n, err)
	}
	runs, _, err := r.store.ListRuns(ctx, RunQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got := runIDs(runs); !slices.Equal(got, []int64{fresh, edge}) {
		t.Errorf("runs after the trim = %v, want %v", got, []int64{fresh, edge})
	}
	if n, err := r.store.TrimRuns(ctx, cut); err != nil || n != 0 {
		t.Errorf("second TrimRuns = %d, %v, want 0", n, err)
	}
}
