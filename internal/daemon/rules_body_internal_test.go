package daemon

import (
	"slices"
	"sync"
	"testing"

	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/rules"
)

// newBodyRig is a host runner with one new message whose row the cache
// holds without its body, and a text rule only that body matches.
func newBodyRig(t *testing.T) (*runnerRig, ref.Ref, rules.Rule) {
	t.Helper()
	g := newRunnerRig(t, "host", nil)
	notes := g.uncached("host", fake.Message{
		Folder: "INBOX", Date: routeNow, Subject: "weekly notes", MessageID: "notes@x", Body: "the plan for the week",
	})
	g.cacheHeader(t, notes)
	rule := g.newRule(t, "host", "flag-plans", func(d *rules.Definition) { d.Query = "plan" })
	g.publish(t, notes)
	return g, notes, rule
}

// putBody stores m's body in the cache, as the prefetcher does.
func (g *runnerRig) putBody(t *testing.T, m ref.Ref) {
	t.Helper()
	res := g.f.fakes[m.Account].FetchMessages(t.Context(), []ref.Ref{m})
	if res[0].Err != nil {
		t.Fatal(res[0].Err)
	}
	if _, err := g.f.store.PutBody(t.Context(), m, cache.Body{Raw: res[0].Message.Raw, TextClipped: res[0].Message.TextClipped}, false); err != nil {
		t.Fatal(err)
	}
}

// noWait fails when the runner asked its clock for a wait.
func (g *runnerRig) noWait(t *testing.T) {
	t.Helper()
	select {
	case w := <-g.clk.waits:
		t.Errorf("the runner waited %v", w.d)
	default:
	}
}

// wantBodyFailure checks that rule's run failed its one message with
// body_not_cached and left it alone.
func (g *runnerRig) wantBodyFailure(t *testing.T, rule rules.Rule, notes ref.Ref) {
	t.Helper()
	run := g.lastRun(t, rule.ID)
	if run.Matched != 1 || run.Failed != 1 || run.Reason != reasonBodyNotCached || run.Hint != bodyNotCachedHint {
		t.Errorf("run = %+v, want 1 failed with %s", run, reasonBodyNotCached)
	}
	if slices.Contains(g.fk.Flags(notes), flagFlagged) {
		t.Error("a message without its body was acted on")
	}
}

// TestRuleBatchWaitsForABody checks amendment 10: a text rule waits while
// a batch message's body is pending, polling on the clock, and matches it
// once the body lands.
func TestRuleBatchWaitsForABody(t *testing.T) {
	t.Parallel()
	g, notes, rule := newBodyRig(t)
	var wg sync.WaitGroup
	var err error
	wg.Go(func() { err = g.step(t.Context()) })
	w := g.clk.next(t)
	if w.d != bodyPoll {
		t.Errorf("body wait %v, want %v", w.d, bodyPoll)
	}
	g.putBody(t, notes)
	w.fire <- g.clk.Now()
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.fk.Flags(notes), flagFlagged) {
		t.Error("the message is not flagged once its body landed")
	}
	if run := g.lastRun(t, rule.ID); run.Applied != 1 || run.Failed != 0 {
		t.Errorf("run = %+v, want 1 applied", run)
	}
}

// TestRuleBatchBodyFailsAfterTheWait checks spec 5.2 step 3: a body that
// has not landed 5 minutes into the batch fails the rule with
// body_not_cached.
func TestRuleBatchBodyFailsAfterTheWait(t *testing.T) {
	t.Parallel()
	g, notes, rule := newBodyRig(t)
	var wg sync.WaitGroup
	var err error
	wg.Go(func() { err = g.step(t.Context()) })
	w := g.clk.next(t)
	g.clk.advance(bodyWait)
	w.fire <- g.clk.Now()
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	g.noWait(t)
	g.wantBodyFailure(t, rule, notes)
}

// TestRuleBatchBodyFailsAtOnce checks amendment 10: a final body failure,
// or an account that prefetches no bodies, fails the rule at once.
func TestRuleBatchBodyFailsAtOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		spoil func(t *testing.T, g *runnerRig, notes ref.Ref)
	}{
		{name: "final failure", spoil: func(t *testing.T, g *runnerRig, notes ref.Ref) {
			for range 3 {
				if _, err := g.f.store.RecordBodyFailure(t.Context(), notes, "fetch_failed"); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{name: "no prefetch", spoil: func(_ *testing.T, g *runnerRig, _ ref.Ref) { g.sig.setPrefetch(false) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g, notes, rule := newBodyRig(t)
			tt.spoil(t, g, notes)
			if err := g.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			g.noWait(t)
			g.wantBodyFailure(t, rule, notes)
		})
	}
}

// TestRuleBodyFailuresNeverDisableTheRule checks that a message whose body
// the cache does not hold counts as a body_not_cached failure without
// disabling the rule: no action was attempted on it, so it is not an
// acted-on match for all_failed.
func TestRuleBodyFailuresNeverDisableTheRule(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	plan := g.uncached("host", fake.Message{
		Folder: "INBOX", Date: routeNow, Subject: "invoice plan", MessageID: "invplan@x", Body: "the plan to pay",
	})
	g.cacheHeader(t, plan)
	rule := g.newRule(t, "host", "flag-invoice-plans", func(d *rules.Definition) { d.Query = "subject:invoice plan" })
	g.sig.setPrefetch(false)
	g.publish(t, plan)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.wantBodyFailure(t, rule, plan)
	if now, err := g.f.s.rules.Get(t.Context(), rule.ID); err != nil || !now.Enabled {
		t.Errorf("rule = enabled %t, reason %q, %v, want enabled", now.Enabled, now.DisabledReason, err)
	}
}

// TestRuleBodyGateSkipsDefiniteNonmatches checks that a text rule drops,
// before it waits for bodies, the batch messages its query rules out
// whatever their bodies say: a header the query's other terms refuse, and
// a Google message with a label the rule excludes. They are neither
// matched nor failed, so the rule writes no run and stays enabled.
func TestRuleBodyGateSkipsDefiniteNonmatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, account, query string
		msg                  fake.Message
	}{
		{name: "header nonmatch", account: "host", query: "subject:invoice plan", msg: fake.Message{
			Folder: "INBOX", Date: routeNow, Subject: "weekly notes", MessageID: "notes@x", Body: "the plan for the week",
		}},
		{name: "excluded label", account: "gmail", query: "plan", msg: fake.Message{
			Folder: "[Gmail]/All Mail", Date: routeNow, Subject: "my plan", MessageID: "sentplan@x", Body: "the plan", Labels: []string{`\Sent`},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := newRunnerRig(t, tt.account, nil)
			m := g.uncached(tt.account, tt.msg)
			g.cacheHeader(t, m)
			rule := g.newRule(t, tt.account, "flag-plans", func(d *rules.Definition) { d.Query = tt.query })
			g.sig.setPrefetch(false)
			last := g.publish(t, m)
			if err := g.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			g.noWait(t)
			if runs := g.runsOf(t, rule.ID); len(runs) != 0 {
				t.Errorf("runs = %+v, want none", runs)
			}
			if now, err := g.f.s.rules.Get(t.Context(), rule.ID); err != nil || !now.Enabled {
				t.Errorf("rule = enabled %t, reason %q, %v, want enabled", now.Enabled, now.DisabledReason, err)
			}
			if got := g.acked(t); got != last {
				t.Errorf("acked %d, want %d", got, last)
			}
		})
	}
}
