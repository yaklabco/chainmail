package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/rules"
)

// seedBatches adds five messages to host's INBOX, oldest first, 9 to 5
// days old, and returns their refs in that order.
func seedBatches(f *routeFixture) []ref.Ref {
	out := make([]ref.Ref, 5)
	for i := range out {
		out[i] = f.fakes["host"].Add(fake.Message{
			Folder: "INBOX", Date: routeDaysAgo(9 - i), Subject: fmt.Sprintf("batch %d", i+1),
			MessageID: fmt.Sprintf("batch%d@x", i+1), Body: "part of a batch",
		})
	}
	return out
}

// newBatchRig is a host runner over the five batch messages, with a manual
// rule that flags them, at most 2 a run.
func newBatchRig(t *testing.T) (*runnerRig, []ref.Ref, rules.Rule) {
	t.Helper()
	var batch []ref.Ref
	g := newRunnerRig(t, "host", func(f *routeFixture) { batch = seedBatches(f) })
	rule := g.newRule(t, "host", "flag-batches", func(d *rules.Definition) {
		d.OnNew, d.Query, d.MaxPerRun = false, "subject:batch", 2
	})
	return g, batch, rule
}

// TestRunRuleCapsAndResumesOldestFirst checks spec 5.3 and 5.6: a manual
// run acts on the oldest matches up to the rule's cap and stops capped,
// the next run carries on after what the act-once table holds, and a run
// with nothing left acts on nothing.
func TestRunRuleCapsAndResumesOldestFirst(t *testing.T) {
	t.Parallel()
	g, batch, rule := newBatchRig(t)
	want := []struct {
		outcome                   rules.Outcome
		matched, applied, skipped int
		flagged                   []bool
	}{
		{rules.OutcomeCapped, 2, 2, 0, []bool{true, true, false, false, false}},
		{rules.OutcomeCapped, 4, 2, 2, []bool{true, true, true, true, false}},
		{rules.OutcomeOK, 5, 1, 4, []bool{true, true, true, true, true}},
		{rules.OutcomeOK, 5, 0, 5, []bool{true, true, true, true, true}},
	}
	for i, w := range want {
		run, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 0)
		if err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
		if run.Outcome != w.outcome || run.Matched != w.matched || run.Applied != w.applied || run.Skipped != w.skipped ||
			run.Failed != 0 || run.Trigger != rules.TriggerManual || run.Account != "host" {
			t.Errorf("run %d = %+v, want %s, %d matched, %d applied, %d skipped", i+1, run, w.outcome, w.matched, w.applied, w.skipped)
		}
		if got := g.flagged(batch); !slices.Equal(got, w.flagged) {
			t.Errorf("after run %d flagged %v, want %v", i+1, got, w.flagged)
		}
	}
	if runs := g.runsOf(t, rule.ID); len(runs) != len(want) {
		t.Errorf("%d runs recorded, want %d", len(runs), len(want))
	}
}

// TestRunRuleLimitOverridesTheRule checks spec 8.1: a manual run's max
// replaces the rule's max_per_run.
func TestRunRuleLimitOverridesTheRule(t *testing.T) {
	t.Parallel()
	g, batch, rule := newBatchRig(t)
	run, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 3)
	if err != nil {
		t.Fatal(err)
	}
	if run.Applied != 3 || run.Outcome != rules.OutcomeCapped {
		t.Errorf("run = %+v, want 3 applied, capped", run)
	}
	if got := g.flagged(batch); !slices.Equal(got, []bool{true, true, true, false, false}) {
		t.Errorf("flagged %v, want the 3 oldest", got)
	}
}

// TestRunRuleNeedsACompleteCache checks spec 5.3: when the cache cannot
// answer the rule completely, the run is recorded cache_not_ready with the
// cache's reason and acts on nothing.
func TestRunRuleNeedsACompleteCache(t *testing.T) {
	t.Parallel()
	g, _, rule := newBatchRig(t)
	g.f.ageSync(t, "host", "Archive", time.Hour)
	run, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 0)
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != rules.OutcomeCacheNotReady || run.Reason != "stale" || run.Hint != ruleCacheNotReadyHint || run.Matched != 0 {
		t.Errorf("run = %+v, want cache_not_ready, stale", run)
	}
	if n := g.fk.Calls("StoreFlags"); n != 0 {
		t.Errorf("flagged %d times", n)
	}
	if last := g.lastRun(t, rule.ID); last.ID != run.ID || last.Outcome != rules.OutcomeCacheNotReady {
		t.Errorf("stored run = %+v, want the cache_not_ready run", last)
	}
}

// TestRunRuleRecordsAScheduledRun checks that a scheduled run is recorded
// as one, so the store moves the rule's schedule on from it.
func TestRunRuleRecordsAScheduledRun(t *testing.T) {
	t.Parallel()
	var batch []ref.Ref
	g := newRunnerRig(t, "host", func(f *routeFixture) { batch = seedBatches(f) })
	rule := g.newRule(t, "host", "hourly", func(d *rules.Definition) {
		d.OnNew, d.Query = false, "subject:batch"
		d.Schedule = rules.Schedule{Kind: rules.ScheduleEvery, Every: time.Hour}
	})
	run, err := g.r.runRule(t.Context(), rule, rules.TriggerSchedule, 0)
	if err != nil {
		t.Fatal(err)
	}
	if run.Trigger != rules.TriggerSchedule || run.Applied != len(batch) || run.Outcome != rules.OutcomeOK {
		t.Errorf("run = %+v, want schedule, %d applied, ok", run, len(batch))
	}
	now, err := g.f.s.rules.Get(t.Context(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !now.LastRunAt.Equal(routeNow) || !now.NextRunAt.Equal(routeNow.Add(time.Hour)) {
		t.Errorf("last run %v, next %v, want %v and an hour later", now.LastRunAt, now.NextRunAt, routeNow)
	}
}

// TestRunRuleWaitsForItsTurn checks spec 5.1: a run waits while another
// job holds the runner, bounded by its own context.
func TestRunRuleWaitsForItsTurn(t *testing.T) {
	t.Parallel()
	g, _, rule := newBatchRig(t)
	g.r.turn <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := g.r.runRule(ctx, rule, rules.TriggerManual, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("runRule while busy = %v, want the deadline", err)
	}
	if runs := g.runsOf(t, rule.ID); len(runs) != 0 {
		t.Errorf("runs = %+v, want none written while waiting", runs)
	}
	<-g.r.turn
	if _, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 0); err != nil {
		t.Errorf("runRule once free: %v", err)
	}
}

// TestRunRuleRefusesAnAccountItDoesNotCover checks that a rule is not run
// on an account outside its accounts, and that nothing is recorded.
func TestRunRuleRefusesAnAccountItDoesNotCover(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "gmail", "gmail-only", func(d *rules.Definition) { d.OnNew = false })
	if _, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 0); !errors.Is(err, errRuleAccount) {
		t.Errorf("runRule = %v, want errRuleAccount", err)
	}
	if runs := g.runsOf(t, rule.ID); len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
}

// TestRunnerStatusReportsAJobAsRunning checks that a scheduled or manual
// run holding the turn shows as running, whatever the loop last said.
func TestRunnerStatusReportsAJobAsRunning(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	g.r.setState(runnerBackingOff, "SERVER_UNAVAILABLE")
	done := g.r.busy()
	if st := g.r.status(); st.State != runnerRunning || st.Reason != "" {
		t.Errorf("status during a job = %+v, want running", st)
	}
	done()
	if st := g.r.status(); st.State != runnerBackingOff || st.Reason != "SERVER_UNAVAILABLE" {
		t.Errorf("status after the job = %+v, want the loop's state back", st)
	}
}

// TestRunRuleInterruptedMidPage checks spec 5.5 for a manual run: a
// transient failure after the page's first rows ends the run interrupted
// with the failure's reason and what it counted so far, and runRule still
// returns the run with a nil error.
func TestRunRuleInterruptedMidPage(t *testing.T) {
	t.Parallel()
	g, batch, rule := newBatchRig(t)
	if _, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 0); err != nil {
		t.Fatal(err)
	}
	g.fk.FailWrite("StoreFlags", mailstore.ErrUnavailable)
	run, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 0)
	if err != nil {
		t.Fatalf("runRule = %v, want the run with a nil error", err)
	}
	if run.Outcome != rules.OutcomeInterrupted || run.Reason != api.ReasonServerUnavailable ||
		run.Matched != 3 || run.Skipped != 2 || run.Applied != 0 || run.Failed != 0 {
		t.Errorf("run = %+v, want interrupted, SERVER_UNAVAILABLE, 3 matched, 2 skipped", run)
	}
	if last := g.lastRun(t, rule.ID); last.ID != run.ID || last.Outcome != rules.OutcomeInterrupted {
		t.Errorf("stored run = %+v, want the interrupted run", last)
	}
	if got := g.flagged(batch); !slices.Equal(got, []bool{true, true, false, false, false}) {
		t.Errorf("flagged %v, want the 2 oldest", got)
	}
}

// lateBatch adds a sixth batch message to host's server after the cache
// was warmed and publishes its new event, as a sync pass that has just
// found it does. The builder is left one event short of it, inside
// fresh_lag, so the cache still counts as complete. It returns the
// message and the event's ID.
func lateBatch(t *testing.T, g *runnerRig) (ref.Ref, int64) {
	t.Helper()
	late := g.uncached("host", fake.Message{
		Folder: "INBOX", Date: routeDaysAgo(4), Subject: "batch 6",
		MessageID: "batch6@x", Body: "part of a batch",
	})
	tail := g.publish(t, late)
	g.sig.set(tail - 1)
	return late, tail
}

// startRunRule runs rule once for trigger in the background and returns
// the channel its run and error arrive on.
func startRunRule(t *testing.T, g *runnerRig, rule rules.Rule, trigger rules.Trigger) <-chan runRuleResult {
	t.Helper()
	done := make(chan runRuleResult, 1)
	go func() {
		run, err := g.r.runRule(t.Context(), rule, trigger, 10)
		done <- runRuleResult{run, err}
	}()
	return done
}

// runRuleResult is what one runRule call returned.
type runRuleResult struct {
	run rules.Run
	err error
}

// awaitCacheWait waits until the runner asks its clock for the wait on the
// builder, failing the test if runRule returns first.
func awaitCacheWait(t *testing.T, g *runnerRig, done <-chan runRuleResult, tail int64) ruleWait {
	t.Helper()
	select {
	case res := <-done:
		t.Fatalf("runRule = %+v, %v before the builder applied event %d", res.run, res.err, tail)
	case w := <-g.clk.waits:
		if w.d != ruleCacheWait {
			t.Fatalf("runner waited %v, want %v for the builder", w.d, ruleCacheWait)
		}
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("runRule neither returned nor waited for the builder")
	}
	return ruleWait{}
}

// TestRunRuleWaitsForTheBuilder checks spec 5.3 and section 6 for a
// scheduled or manual run. The cache counts as complete while its builder
// lags the log by less than fresh_lag, so a run waits until the builder
// has applied the log's tail before it matches: mail the sync has just
// found, such as the mail a downtime catch-up is for, is acted on in this
// run and not an interval later.
func TestRunRuleWaitsForTheBuilder(t *testing.T) {
	t.Parallel()
	for _, trigger := range []rules.Trigger{rules.TriggerSchedule, rules.TriggerManual} {
		t.Run(string(trigger), func(t *testing.T) {
			t.Parallel()
			g, batch, rule := newBatchRig(t)
			late, tail := lateBatch(t, g)
			done := startRunRule(t, g, rule, trigger)
			awaitCacheWait(t, g, done, tail)
			if n := g.fk.Calls("StoreFlags"); n != 0 {
				t.Errorf("flagged %d times before the builder applied the tail", n)
			}
			// The builder applies the new event: the row reaches cache.db,
			// then its position moves.
			g.cacheHeader(t, late)
			g.sig.set(tail)
			res := <-done
			if res.err != nil {
				t.Fatal(res.err)
			}
			if run := res.run; run.Outcome != rules.OutcomeOK || run.Matched != 6 || run.Applied != 6 || run.Skipped != 0 || run.Failed != 0 {
				t.Errorf("run = %+v, want ok, 6 matched, 6 applied", run)
			}
			if got := g.flagged(append(slices.Clone(batch), late)); slices.Contains(got, false) {
				t.Errorf("flagged %v, want all six, the late one too", got)
			}
		})
	}
}

// TestRunRuleGivesUpOnAStuckBuilder checks that a run whose builder does
// not reach the log's tail within ruleCacheWait is recorded cache_not_ready
// with reason stale and acts on nothing, so a scheduled rule retries soon
// (amendment 7).
func TestRunRuleGivesUpOnAStuckBuilder(t *testing.T) {
	t.Parallel()
	g, _, rule := newBatchRig(t)
	_, tail := lateBatch(t, g)
	done := startRunRule(t, g, rule, rules.TriggerSchedule)
	w := awaitCacheWait(t, g, done, tail)
	w.fire <- routeNow
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if run := res.run; run.Outcome != rules.OutcomeCacheNotReady || run.Reason != "stale" || run.Hint != ruleCacheNotReadyHint || run.Matched != 0 {
		t.Errorf("run = %+v, want cache_not_ready, stale", run)
	}
	if n := g.fk.Calls("StoreFlags"); n != 0 {
		t.Errorf("flagged %d times", n)
	}
	if last := g.lastRun(t, rule.ID); last.ID != res.run.ID || last.Outcome != rules.OutcomeCacheNotReady {
		t.Errorf("stored run = %+v, want the cache_not_ready run", last)
	}
}

// TestRunRuleRescopesAfterTheBuilderWait checks that a run scopes the rule
// again once the builder has applied the log's tail. A UIDVALIDITY gap the
// builder applies during the wait drops the folder's rows and starts its
// backfill again under the new UIDVALIDITY, so the run is recorded
// cache_not_ready, backfilling, and acts on nothing, rather than paging
// the old UIDVALIDITY and finding nothing to do.
func TestRunRuleRescopesAfterTheBuilderWait(t *testing.T) {
	t.Parallel()
	g, _, rule := newBatchRig(t)
	_, tail := lateBatch(t, g)
	done := startRunRule(t, g, rule, rules.TriggerSchedule)
	awaitCacheWait(t, g, done, tail)
	// The sync found INBOX's UIDVALIDITY changed, and the builder applies
	// the gap: the folder is reset under the new UIDVALIDITY.
	row, ok, err := g.f.s.watchState.Folder(t.Context(), "host", "INBOX")
	if err != nil || !ok {
		t.Fatalf("sync row host/INBOX: %v, %t", err, ok)
	}
	row.UIDValidity++
	g.f.putSync(t, row)
	if _, err := g.f.accounts["host"].Rebuild(t.Context(), "INBOX"); err != nil {
		t.Fatal(err)
	}
	g.sig.set(tail)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if run := res.run; run.Outcome != rules.OutcomeCacheNotReady || run.Reason != "backfilling" || run.Hint != ruleCacheNotReadyHint || run.Matched != 0 {
		t.Errorf("run = %+v, want cache_not_ready, backfilling", run)
	}
	if n := g.fk.Calls("StoreFlags"); n != 0 {
		t.Errorf("flagged %d times", n)
	}
	if last := g.lastRun(t, rule.ID); last.ID != res.run.ID || last.Outcome != rules.OutcomeCacheNotReady {
		t.Errorf("stored run = %+v, want the cache_not_ready run", last)
	}
}
