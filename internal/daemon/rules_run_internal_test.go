package daemon

import (
	"context"
	"math"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/watch"
)

// fakeProgress is a cache worker's applied position and body setting, as
// the test sets them. It starts past every event, prefetching bodies.
type fakeProgress struct {
	mu       sync.Mutex
	applied  int64
	changed  chan struct{}
	prefetch bool
}

func newFakeProgress() *fakeProgress {
	return &fakeProgress{applied: math.MaxInt64, changed: make(chan struct{}), prefetch: true}
}

func (p *fakeProgress) Applied() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.applied
}

func (p *fakeProgress) AppliedChanged() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

func (p *fakeProgress) PrefetchesBodies() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prefetch
}

// set moves the applied position and signals it, as the builder does.
func (p *fakeProgress) set(v int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applied = v
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *fakeProgress) setPrefetch(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prefetch = on
}

// ruleWait is one wait a runner asked of its clock.
type ruleWait struct {
	d    time.Duration
	fire chan time.Time
}

// ruleClock is a watch.Clock whose timers fire only when the test fires
// them, and whose time moves only when the test advances it. It sends
// every After call on waits.
type ruleClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan ruleWait
}

func newRuleClock(now time.Time) *ruleClock {
	return &ruleClock{now: now, waits: make(chan ruleWait, 8)}
}

func (c *ruleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *ruleClock) After(d time.Duration) <-chan time.Time {
	w := ruleWait{d: d, fire: make(chan time.Time, 1)}
	c.waits <- w
	return w.fire
}

func (c *ruleClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// next is the runner's next wait. The bound only catches a runner that
// never waits on the clock; a working one asks at once.
func (c *ruleClock) next(t *testing.T) ruleWait {
	t.Helper()
	select {
	case w := <-c.waits:
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("the runner never waited on its clock")
		return ruleWait{}
	}
}

// runnerRig is one account's runner over an actRig, with a fake cache
// signal, a ruleClock and fixed jitter, its group opened at the log's
// tail before any rule exists.
type runnerRig struct {
	*actRig
	account string
	fk      *fake.Store
	r       *ruleRunner
	sig     *fakeProgress
	clk     *ruleClock
	// pos is the position the next step reads after.
	pos int64
}

func newRunnerRig(t *testing.T, account string, seed func(*routeFixture)) *runnerRig {
	t.Helper()
	g := newActRig(t, seed)
	clk := newRuleClock(routeNow)
	g.f.s.rulesClock = clk
	r := g.f.s.newRuleRunner(g.acct(t, account), g.f.accounts[account])
	sig := newFakeProgress()
	r.ca = sig
	r.jitter = func() float64 { return 0.5 }
	pos, err := r.open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return &runnerRig{actRig: g, account: account, fk: g.f.fakes[account], r: r, sig: sig, clk: clk, pos: pos}
}

// publish appends a new event for each ref, as a sync pass does, and
// returns the last event's ID.
func (g *runnerRig) publish(t *testing.T, refs ...ref.Ref) int64 {
	t.Helper()
	events := make([]watch.Event, len(refs))
	for i, m := range refs {
		events[i] = watch.Event{Kind: watch.KindNew, Account: m.Account, Folder: m.Folder, Ref: m.String(), Message: &watch.Message{Subject: "new mail"}}
	}
	if err := g.f.s.watchLog.Publish(t.Context(), events...); err != nil {
		t.Fatal(err)
	}
	tail, err := g.f.s.watchLog.Tail(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return tail
}

// step runs one runner step from the rig's position, and moves it on.
func (g *runnerRig) step(ctx context.Context) error {
	next, err := g.r.step(ctx, g.pos)
	if err == nil {
		g.pos = next
	}
	return err
}

// rewind moves the group back to to, as a crash before the ack leaves it.
func (g *runnerRig) rewind(t *testing.T, to int64) {
	t.Helper()
	moved, err := g.f.s.watchLog.ResetGroup(t.Context(), g.r.group, g.pos, to)
	if err != nil || !moved {
		t.Fatalf("rewinding %s from %d to %d: %t, %v", g.r.group, g.pos, to, moved, err)
	}
	g.pos = to
}

// acked is the group's acknowledged ID in state.db.
func (g *runnerRig) acked(t *testing.T) int64 {
	t.Helper()
	acked, _, err := g.f.s.watchLog.Group(t.Context(), g.r.group)
	if err != nil {
		t.Fatal(err)
	}
	return acked
}

// runsOf is rule's runs, newest first.
func (g *runnerRig) runsOf(t *testing.T, ruleID string) []rules.Run {
	t.Helper()
	runs, _, err := g.f.s.rules.ListRuns(t.Context(), rules.RunQuery{RuleID: ruleID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// lastRun is rule's newest run.
func (g *runnerRig) lastRun(t *testing.T, ruleID string) rules.Run {
	t.Helper()
	runs := g.runsOf(t, ruleID)
	if len(runs) == 0 {
		t.Fatalf("rule %s has no runs", ruleID)
	}
	return runs[0]
}

// flagged reports which of refs carry \Flagged on the fake.
func (g *runnerRig) flagged(refs []ref.Ref) []bool {
	out := make([]bool, len(refs))
	for i, m := range refs {
		out[i] = slices.Contains(g.fk.Flags(m), flagFlagged)
	}
	return out
}

// uncached adds a message to account's server after the cache was warmed:
// the cache has no row of it.
func (g *runnerRig) uncached(account string, m fake.Message) ref.Ref {
	return g.f.fakes[account].Add(m)
}

// cacheHeader puts m's row in the cache without its body, as the builder
// does for a new message.
func (g *runnerRig) cacheHeader(t *testing.T, m ref.Ref) {
	t.Helper()
	headers, err := g.f.fakes[m.Account].FetchHeaders(t.Context(), mailstore.Source{Folder: m.Folder, UIDValidity: m.UIDValidity}, []uint32{m.UID})
	if err != nil || len(headers) != 1 {
		t.Fatalf("headers of %s: %v, %v", m, headers, err)
	}
	if err := g.f.store.PutRows(t.Context(), []cache.Row{cache.RowFromHeader(headers[0])}); err != nil {
		t.Fatal(err)
	}
}

// waitStatus polls the runner until its status satisfies ok.
func waitStatus(t *testing.T, r *ruleRunner, ok func(runnerStatus) bool) runnerStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := r.status()
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner status stayed %+v", st)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestRuleBatchWaitsForTheCache checks spec 5.2 step 2: a batch is matched
// only once the cache builder has applied the log past it, and it is
// acknowledged after its rules ran.
func TestRuleBatchWaitsForTheCache(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	g.newRule(t, "host", "flag-lunch", func(d *rules.Definition) { d.Query = "subject:lunch" })
	start := g.pos
	g.sig.set(0)
	last := g.publish(t, g.refs["host lunch"])
	var wg sync.WaitGroup
	var err error
	wg.Go(func() { err = g.step(t.Context()) })
	waitStatus(t, g.r, func(st runnerStatus) bool { return st.State == runnerWaitingForCache })
	if n := g.fk.Calls("StoreFlags"); n != 0 {
		t.Errorf("acted %d times before the cache had the batch", n)
	}
	if got := g.acked(t); got != start {
		t.Errorf("acked %d while waiting, want %d", got, start)
	}
	g.sig.set(last)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if !g.flagged([]ref.Ref{g.refs["host lunch"]})[0] {
		t.Error("lunch is not flagged")
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
}

// TestRuleBatchLabelsAndArchivesOnGoogle runs a Stripe-style rule on new
// mail of a Google account: the matching message is labelled and
// archived, the other is left alone, the run and the act-once row are
// recorded, and both actions are audited under client kind rule with the
// rule and run.
func TestRuleBatchLabelsAndArchivesOnGoogle(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "gmail", nil)
	rule := g.newRule(t, "gmail", "receipts", func(d *rules.Definition) {
		d.Query = "from:acme"
		d.Actions = []rules.Action{{Kind: rules.ActLabel, Label: "Receipts"}, {Kind: rules.ActArchive}}
	})
	inv, trip := g.refs["gmail invoice"], g.refs["gmail trip"]
	last := g.publish(t, inv, trip)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
	if labels := labelsOf(t, g.fk, inv); !slices.Contains(labels, "Receipts") || slices.Contains(labels, `\Inbox`) {
		t.Errorf("invoice labels = %q, want Receipts, archived", labels)
	}
	if labels := labelsOf(t, g.fk, trip); !slices.Equal(labels, []string{`\Inbox`}) {
		t.Errorf("trip labels = %q, want it untouched", labels)
	}
	run := g.lastRun(t, rule.ID)
	if run.Trigger != rules.TriggerNew || run.Matched != 1 || run.Applied != 1 || run.Outcome != rules.OutcomeOK {
		t.Errorf("run = %+v, want new, 1 matched, 1 applied, ok", run)
	}
	if row, ok := g.appliedRow(t, rule.ID, "gmail", keyOf(true, inv)); !ok || row.Outcome != rules.AppliedDone || row.RunID != run.ID {
		t.Errorf("act-once row = %+v, %t, want applied by run %d", row, ok, run.ID)
	}
	rows := g.audit(t)
	if got := auditActions(rows); !slices.Equal(got, []string{"labels", "archive"}) {
		t.Fatalf("audited %v, want labels, archive", got)
	}
	want := []string{inv.String(), "rule=" + rule.ID, "run=" + strconv.FormatInt(run.ID, 10)}
	for _, r := range rows {
		if r.uid != uint32(os.Getuid()) || r.account != "gmail" || r.result != "ok" || !slices.Equal(r.targets, want) {
			t.Errorf("audit row %+v, want targets %v", r, want)
		}
	}
}

// TestRuleBatchKeepsPriorityAndStop runs three rules on one message: they
// act in priority order, and the stop rule keeps the one after it off the
// message.
func TestRuleBatchKeepsPriorityAndStop(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	third := g.newRule(t, "host", "unread-lunch", func(d *rules.Definition) {
		d.Priority, d.Query, d.Actions = 30, "subject:lunch", []rules.Action{{Kind: rules.ActUnread}}
	})
	first := g.newRule(t, "host", "read-lunch", func(d *rules.Definition) {
		d.Priority, d.Query, d.Actions = 10, "subject:lunch", []rules.Action{{Kind: rules.ActRead}}
	})
	second := g.newRule(t, "host", "flag-lunch", func(d *rules.Definition) {
		d.Priority, d.Query, d.Stop = 20, "subject:lunch", true
	})
	lunch := g.refs["host lunch"]
	g.publish(t, lunch)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	rows := g.audit(t)
	if len(rows) != 2 || !slices.Contains(rows[0].targets, "rule="+first.ID) || !slices.Contains(rows[1].targets, "rule="+second.ID) {
		t.Fatalf("audit = %+v, want the read rule, then the flag rule", rows)
	}
	if flags := g.fk.Flags(lunch); !slices.Contains(flags, flagSeen) || !slices.Contains(flags, flagFlagged) {
		t.Errorf("flags = %v, want seen and flagged", flags)
	}
	if run := g.lastRun(t, third.ID); run.Matched != 1 || run.Skipped != 1 || run.Applied != 0 {
		t.Errorf("stopped rule's run = %+v, want 1 matched, 1 skipped", run)
	}
}

// flagEchoStore is a fake mail store whose flag writes reach the cache at
// once, as a cache builder that applies their flags events between two
// rules of a batch would leave it.
type flagEchoStore struct {
	*fake.Store
	t     *testing.T
	cache *cache.Store
}

func (w *flagEchoStore) StoreFlags(ctx context.Context, refs []ref.Ref, add, remove []string) []mailstore.RefResult {
	res := w.Store.StoreFlags(ctx, refs, add, remove)
	for i, m := range refs {
		if res[i].Err != nil {
			continue
		}
		if ok, err := w.cache.SetFlags(ctx, m, w.Flags(m), nil); err != nil || !ok {
			w.t.Errorf("echoing %s's flags to the cache: %t, %v", m, ok, err)
		}
	}
	return res
}

// TestRuleBatchMatchesEveryRuleFirst checks spec amendment 24: every rule
// of a batch matches the cache as it was when the batch was matched. The
// first rule marks the message read, and its flag reaches the cache before
// the second rule acts, but the second rule, which wants read mail, saw
// the message unread and leaves it alone.
func TestRuleBatchMatchesEveryRuleFirst(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	echo := &flagEchoStore{Store: g.fk, t: t, cache: g.f.store}
	g.f.s.stores = &storeSet{build: func(a config.ResolvedAccount) mailstore.Store {
		if a.Name == "host" {
			return echo
		}
		return g.f.fakes[a.Name]
	}}
	g.newRule(t, "host", "read-lunch", func(d *rules.Definition) {
		d.Priority, d.Query, d.Actions = 10, "subject:lunch", []rules.Action{{Kind: rules.ActRead}}
	})
	later := g.newRule(t, "host", "flag-read-lunch", func(d *rules.Definition) {
		d.Priority, d.Query = 20, "is:read subject:lunch"
	})
	lunch := g.refs["host lunch"]
	g.publish(t, lunch)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	row, ok, err := g.f.store.Row(t.Context(), lunch)
	if err != nil || !ok || !slices.Contains(row.Flags, flagSeen) {
		t.Fatalf("cached lunch = %+v, %t, %v, want it read in the cache after the batch", row.Flags, ok, err)
	}
	if g.flagged([]ref.Ref{lunch})[0] {
		t.Error("the read-mail rule flagged lunch, which was unread when the batch was matched")
	}
	if runs := g.runsOf(t, later.ID); len(runs) != 0 {
		t.Errorf("read-mail rule's runs = %+v, want none: it matched nothing", runs)
	}
}

// TestRuleRelocationStopsLaterRules checks spec 4: a rule that moves a
// message keeps later rules off it, whatever stop says.
func TestRuleRelocationStopsLaterRules(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	g.newRule(t, "host", "archive-lunch", func(d *rules.Definition) {
		d.Priority, d.Query, d.Actions = 10, "subject:lunch", []rules.Action{{Kind: rules.ActArchive}}
	})
	later := g.newRule(t, "host", "flag-lunch", func(d *rules.Definition) { d.Priority, d.Query = 20, "subject:lunch" })
	g.publish(t, g.refs["host lunch"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	g.foundIn(t, "host", "Archive", "lunch@x")
	if n := g.fk.Calls("StoreFlags"); n != 0 {
		t.Errorf("the later rule flagged %d times", n)
	}
	if run := g.lastRun(t, later.ID); run.Matched != 1 || run.Skipped != 1 {
		t.Errorf("later rule's run = %+v, want 1 matched, 1 skipped", run)
	}
}

// TestRuleBatchActsOnceOnReplay reads a batch again, as a restart before
// its ack does: the act-once table skips the message.
func TestRuleBatchActsOnceOnReplay(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "flag-lunch", func(d *rules.Definition) { d.Query = "subject:lunch" })
	start := g.pos
	g.publish(t, g.refs["host lunch"])
	for range 2 {
		if err := g.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if g.pos != start {
			g.rewind(t, start)
		}
	}
	if n := g.fk.Calls("StoreFlags"); n != 1 {
		t.Errorf("flagged %d times, want 1", n)
	}
	runs := g.runsOf(t, rule.ID)
	if len(runs) != 2 || runs[0].Matched != 1 || runs[0].Skipped != 1 || runs[0].Applied != 0 || runs[0].Outcome != rules.OutcomeOK {
		t.Errorf("runs = %+v, want the replay to skip its one match", runs)
	}
}

// TestRuleMoveAliasesEveryRulesRow checks amendment 6: when a move gives
// the message a new ref, every rule's act-once row follows it, so its new
// event in the destination is skipped by both rules.
func TestRuleMoveAliasesEveryRulesRow(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	flag := g.newRule(t, "host", "flag-lunch", func(d *rules.Definition) { d.Priority, d.Query = 10, "subject:lunch" })
	move := g.newRule(t, "host", "file-lunch", func(d *rules.Definition) {
		d.Priority, d.Query, d.Actions = 20, "subject:lunch", []rules.Action{{Kind: rules.ActMove, Folder: "Archive"}}
	})
	g.publish(t, g.refs["host lunch"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	moved := g.foundIn(t, "host", "Archive", "lunch@x")
	alias := keyOf(false, moved)
	for _, id := range []string{flag.ID, move.ID} {
		if row, ok := g.appliedRow(t, id, "host", alias); !ok || row.Outcome != rules.AppliedDone {
			t.Errorf("rule %s's row at the new key = %+v, %t, want applied", id, row, ok)
		}
	}
	g.cacheHeader(t, moved)
	g.publish(t, moved)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f, m := g.fk.Calls("StoreFlags"), g.fk.Calls("Move"); f != 1 || m != 1 {
		t.Errorf("flags %d, moves %d, want 1 each", f, m)
	}
	for _, id := range []string{flag.ID, move.ID} {
		if run := g.lastRun(t, id); run.Matched != 1 || run.Skipped != 1 {
			t.Errorf("rule %s's run on the moved message = %+v, want it skipped", id, run)
		}
	}
}

// TestRuleBatchSkipsMissingRows checks amendment 11: a batch message the
// cache holds no row of is not matched and is no failure.
func TestRuleBatchSkipsMissingRows(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "flag-new", nil)
	missing := g.uncached("host", fake.Message{Folder: "INBOX", Date: routeNow, Subject: "unread by the cache", MessageID: "gap@x", Body: "x"})
	last := g.publish(t, g.refs["host lunch"], missing)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := g.flagged([]ref.Ref{g.refs["host lunch"], missing}); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("flagged = %v, want lunch only", got)
	}
	if run := g.lastRun(t, rule.ID); run.Matched != 1 || run.Failed != 0 || run.Outcome != rules.OutcomeOK {
		t.Errorf("run = %+v, want 1 matched, no failure", run)
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
}

// TestRuleBatchWithoutMatchesWritesNoRun checks spec 3.3: a new-mail run
// that matched nothing leaves no row, and the batch is acknowledged.
func TestRuleBatchWithoutMatchesWritesNoRun(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "never", func(d *rules.Definition) { d.Query = "subject:nomatch" })
	last := g.publish(t, g.refs["host lunch"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if runs := g.runsOf(t, rule.ID); len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
}

// TestRuleTransientFailureLeavesTheBatchUnacked checks spec 5.5: a
// transient failure ends the batch unacknowledged with an interrupted run,
// and the replay acts on nothing the first attempt finished.
func TestRuleTransientFailureLeavesTheBatchUnacked(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "gmail", nil)
	label := g.newRule(t, "gmail", "label-acme", func(d *rules.Definition) {
		d.Priority, d.Query, d.Actions = 10, "from:acme", []rules.Action{{Kind: rules.ActLabel, Label: "Receipts"}}
	})
	archive := g.newRule(t, "gmail", "archive-acme", func(d *rules.Definition) {
		d.Priority, d.Query, d.Actions = 20, "from:acme", []rules.Action{{Kind: rules.ActArchive}}
	})
	start := g.pos
	inv := g.refs["gmail invoice"]
	g.publish(t, inv)
	g.fk.FailWrite("Archive", mailstore.ErrUnavailable)
	err := g.step(t.Context())
	if reason, _ := failureOf(err); reason != api.ReasonServerUnavailable {
		t.Fatalf("step = %v, want a SERVER_UNAVAILABLE failure", err)
	}
	if g.pos != start || g.acked(t) != start {
		t.Errorf("position %d, acked %d, want both %d", g.pos, g.acked(t), start)
	}
	if run := g.lastRun(t, archive.ID); run.Outcome != rules.OutcomeInterrupted || run.Reason != api.ReasonServerUnavailable {
		t.Errorf("archive run = %+v, want interrupted, SERVER_UNAVAILABLE", run)
	}
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if l, a := g.fk.Calls("StoreLabels"), g.fk.Calls("Archive"); l != 1 || a != 2 {
		t.Errorf("labels %d, archives %d, want 1 and 2", l, a)
	}
	if run := g.lastRun(t, label.ID); run.Skipped != 1 || run.Applied != 0 {
		t.Errorf("label replay = %+v, want its match skipped", run)
	}
	if run := g.lastRun(t, archive.ID); run.Applied != 1 || run.Outcome != rules.OutcomeOK {
		t.Errorf("archive replay = %+v, want applied", run)
	}
}

// TestRulePermanentFailureCountsAndDisables checks spec 5.5 and 5.6: a
// permanent failure counts against the run with its reason and fixed hint,
// leaves no act-once row, and a run whose every match failed disables the
// rule.
func TestRulePermanentFailureCountsAndDisables(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "file-nowhere", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:lunch", []rules.Action{{Kind: rules.ActMove, Folder: "Nowhere"}}
	})
	last := g.publish(t, g.refs["host lunch"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	run := g.lastRun(t, rule.ID)
	if run.Outcome != rules.OutcomeFailed || run.Matched != 1 || run.Failed != 1 || run.Reason != api.ReasonNotFound || run.Hint != folderHint {
		t.Errorf("run = %+v, want failed, NOT_FOUND, %q", run, folderHint)
	}
	if _, ok := g.appliedRow(t, rule.ID, "host", keyOf(false, g.refs["host lunch"])); ok {
		t.Error("a failed message has an act-once row")
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
	now, err := g.f.s.rules.Get(t.Context(), rule.ID)
	if err != nil || now.Enabled || now.DisabledReason != rules.ReasonAllFailed {
		t.Errorf("rule = enabled %t, reason %q, %v, want disabled: all_failed", now.Enabled, now.DisabledReason, err)
	}
}

// TestRuleSkipsAMessageGoneSinceTheMatch checks that a message another
// client deleted after the cache matched it counts as skipped: the run
// has no failure, the rule stays enabled, which an all_failed run would
// not leave it, and no act-once row is written. Both triggers are checked.
func TestRuleSkipsAMessageGoneSinceTheMatch(t *testing.T) {
	t.Parallel()
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "manual"}[manual], func(t *testing.T) {
			t.Parallel()
			g := newRunnerRig(t, "host", nil)
			rule := g.newRule(t, "host", "flag-lunch", func(d *rules.Definition) { d.Query = "subject:lunch" })
			lunch := g.refs["host lunch"]
			if res := g.fk.Expunge(t.Context(), []ref.Ref{lunch}); res[0].Err != nil {
				t.Fatal(res[0].Err)
			}
			var run rules.Run
			if manual {
				var err error
				if run, err = g.r.runRule(t.Context(), rule, rules.TriggerManual, 0); err != nil {
					t.Fatal(err)
				}
			} else {
				last := g.publish(t, lunch)
				if err := g.step(t.Context()); err != nil {
					t.Fatal(err)
				}
				if got := g.acked(t); got != last {
					t.Errorf("acked %d, want %d", got, last)
				}
				run = g.lastRun(t, rule.ID)
			}
			if run.Outcome != rules.OutcomeOK || run.Matched != 1 || run.Skipped != 1 || run.Failed != 0 || run.Applied != 0 {
				t.Errorf("run = %+v, want ok, 1 matched, 1 skipped", run)
			}
			if _, ok := g.appliedRow(t, rule.ID, "host", keyOf(false, lunch)); ok {
				t.Error("the gone message has an act-once row")
			}
			now, err := g.f.s.rules.Get(t.Context(), rule.ID)
			if err != nil || !now.Enabled || now.DisabledReason != "" {
				t.Errorf("rule = enabled %t, reason %q, %v, want enabled", now.Enabled, now.DisabledReason, err)
			}
		})
	}
}

// TestRuleHeldForwardCountsAsHeld checks spec 5.5 and amendment 9: a
// forward the policy holds as a draft counts as held, not applied, and its
// act-once row has outcome held.
func TestRuleHeldForwardCountsAsHeld(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	g.password(t)
	g.failDial(t, false)
	rule := g.newRule(t, "host", "fwd-blocked", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:invoice", []rules.Action{{Kind: rules.ActForward, To: []string{"you@blocked.invalid"}}}
	})
	g.publish(t, g.refs["host invoice"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if run := g.lastRun(t, rule.ID); run.Held != 1 || run.Applied != 0 || run.Failed != 0 || run.Outcome != rules.OutcomeOK {
		t.Errorf("run = %+v, want 1 held", run)
	}
	if row, ok := g.appliedRow(t, rule.ID, "host", keyOf(false, g.refs["host invoice"])); !ok || row.Outcome != rules.AppliedHeld {
		t.Errorf("act-once row = %+v, %t, want held", row, ok)
	}
	if n := len(g.fk.Appended()); n != 1 {
		t.Errorf("drafts = %d, want 1", n)
	}
}

// TestRuleCopyAliasesTheCopy checks amendments 5 and 6 for a copy that
// ran: its pending row becomes applied, and the copy's key gets the row
// too, so the copy's own new event is skipped.
func TestRuleCopyAliasesTheCopy(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "copy-lunch", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:lunch", []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}}
	})
	g.publish(t, g.refs["host lunch"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	run := g.lastRun(t, rule.ID)
	if run.Applied != 1 {
		t.Fatalf("run = %+v, want 1 applied", run)
	}
	for name, key := range map[string]string{
		"original": keyOf(false, g.refs["host lunch"]),
		"copy":     keyOf(false, g.foundIn(t, "host", "Archive", "lunch@x")),
	} {
		if row, ok := g.appliedRow(t, rule.ID, "host", key); !ok || row.Outcome != rules.AppliedDone || row.RunID != run.ID {
			t.Errorf("%s's row = %+v, %t, want applied by run %d", name, row, ok, run.ID)
		}
	}
}

// TestRuleCopyFailureClearsItsPendingRow checks that a copy that failed
// for good leaves no pending row, so the next attempt copies instead of
// taking the copy as made.
func TestRuleCopyFailureClearsItsPendingRow(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "copy-nowhere", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:lunch", []rules.Action{{Kind: rules.ActCopy, Folder: "Nowhere"}}
	})
	start := g.pos
	g.publish(t, g.refs["host lunch"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if run := g.lastRun(t, rule.ID); run.Failed != 1 || run.Reason != api.ReasonNotFound {
		t.Errorf("run = %+v, want 1 failed, NOT_FOUND", run)
	}
	if _, ok := g.appliedRow(t, rule.ID, "host", keyOf(false, g.refs["host lunch"])); ok {
		t.Error("the failed copy left an act-once row")
	}
	// A run whose every match failed disabled the rule: turn it on again,
	// so the replay runs it.
	on := true
	if _, err := g.f.s.rules.Update(t.Context(), ruleActor, rule.ID, rules.Patch{Enabled: &on}, nil); err != nil {
		t.Fatal(err)
	}
	g.rewind(t, start)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := g.fk.Calls("Copy"); n != 2 {
		t.Errorf("copies tried %d times, want 2", n)
	}
}

// TestRulePendingCopyIsReplayed checks amendment 5: a pending row of a
// copy rule means the copy may exist, so the replay skips it and records
// the message applied with note replayed.
func TestRulePendingCopyIsReplayed(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "copy-lunch", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:lunch", []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}}
	})
	key := keyOf(false, g.refs["host lunch"])
	if err := g.f.s.rules.MarkPending(t.Context(), rule.ID, "host", key, 0); err != nil {
		t.Fatal(err)
	}
	g.publish(t, g.refs["host lunch"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := g.fk.Calls("Copy"); n != 0 {
		t.Errorf("copied %d times on the replay", n)
	}
	if row, ok := g.appliedRow(t, rule.ID, "host", key); !ok || row.Outcome != rules.AppliedDone || row.Note != rules.NoteReplayed {
		t.Errorf("act-once row = %+v, %t, want applied, replayed", row, ok)
	}
	if run := g.lastRun(t, rule.ID); run.Applied != 1 || run.Skipped != 0 {
		t.Errorf("run = %+v, want 1 applied", run)
	}
}

// TestRulePendingForwardAsksTheIdempotencyStore checks amendment 8: a
// pending forward whose attempt crashed is asked of the idempotency store,
// which answers outcome unknown without sending, and the message is
// recorded applied with note outcome_unknown.
func TestRulePendingForwardAsksTheIdempotencyStore(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	g.failDial(t, true)
	rule := g.newRule(t, "host", "fwd-books", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:invoice", []rules.Action{{Kind: rules.ActForward, To: []string{"you@example.net"}}}
	})
	key := keyOf(false, g.refs["host invoice"])
	if err := g.f.s.rules.MarkPending(t.Context(), rule.ID, "host", key, 0); err != nil {
		t.Fatal(err)
	}
	g.crashedForward(t, rule.ID, "host", key)
	g.publish(t, g.refs["host invoice"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if row, ok := g.appliedRow(t, rule.ID, "host", key); !ok || row.Outcome != rules.AppliedDone || row.Note != rules.NoteOutcomeUnknown {
		t.Errorf("act-once row = %+v, %t, want applied, outcome_unknown", row, ok)
	}
}

// TestRuleRunnerRecoversAGroupPastTheTail opens a rules group that a reset
// state.db left past the log's tail: it moves back to the tail.
func TestRuleRunnerRecoversAGroupPastTheTail(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	ctx := t.Context()
	tail, err := g.f.s.watchLog.Tail(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.f.stateDB.ExecContext(ctx, "UPDATE event_groups SET acked_id = ? WHERE name = ?", tail+50, g.r.group); err != nil {
		t.Fatal(err)
	}
	pos, err := g.r.open(ctx)
	if err != nil || pos != tail {
		t.Fatalf("open = %d, %v, want the tail %d", pos, err, tail)
	}
	if got := g.acked(t); got != tail {
		t.Errorf("acked %d, want %d", got, tail)
	}
	if st := g.r.status(); st.AckedID != tail {
		t.Errorf("status = %+v, want acked %d", st, tail)
	}
}

// TestRuleRunnerBacksOffAndRetries runs the runner's loop: it starts idle
// at its group's position, a transient failure backs off for
// watch.Backoff's wait with the failure's reason and without acking, and
// the retry acts and acknowledges, then idles caught up.
func TestRuleRunnerBacksOffAndRetries(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "gmail", nil)
	start := g.pos
	if st := g.r.status(); st.State != runnerIdle || st.AckedID != start {
		t.Errorf("new runner status = %+v, want idle at %d", st, start)
	}
	g.newRule(t, "gmail", "receipts", func(d *rules.Definition) {
		d.Query = "from:acme"
		d.Actions = []rules.Action{{Kind: rules.ActLabel, Label: "Receipts"}, {Kind: rules.ActArchive}}
	})
	g.fk.FailWrite("Archive", mailstore.ErrUnavailable)
	last := g.publish(t, g.refs["gmail invoice"])
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { g.r.Run(ctx) })
	defer wg.Wait()
	defer cancel()
	w := g.clk.next(t)
	if want := watch.Backoff(0, 0.5); w.d != want {
		t.Errorf("backoff %v, want %v", w.d, want)
	}
	if st := g.r.status(); st.State != runnerBackingOff || st.Reason != api.ReasonServerUnavailable || st.AckedID != start {
		t.Errorf("status = %+v, want backing_off, SERVER_UNAVAILABLE, acked %d", st, start)
	}
	w.fire <- g.clk.Now()
	waitStatus(t, g.r, func(st runnerStatus) bool { return st.State == runnerIdle && st.AckedID == last })
	if labels := labelsOf(t, g.fk, g.refs["gmail invoice"]); slices.Contains(labels, `\Inbox`) {
		t.Errorf("labels = %q, want archived", labels)
	}
}

// TestRuleRunnerStartsUnopened checks amendment 8: a runner that has not
// opened its group yet reports starting with acked ID -1, and opening the
// group makes it idle at the group's position.
func TestRuleRunnerStartsUnopened(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	g.f.s.rulesClock = newRuleClock(routeNow)
	r := g.f.s.newRuleRunner(g.acct(t, "host"), g.f.accounts["host"])
	if st := r.status(); st.State != runnerStarting || st.AckedID != -1 || st.Reason != "" {
		t.Errorf("unopened runner status = %+v, want starting at -1", st)
	}
	pos, err := r.open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st := r.status(); st.State != runnerIdle || st.AckedID != pos {
		t.Errorf("opened runner status = %+v, want idle at %d", st, pos)
	}
}

// TestRuleBatchLeavesTheGroupUnreadWhileTheCacheIsDown checks spec 5.2:
// while the account's cache is disabled, as a full disk turns it off, a
// batch is left unacknowledged with reason cache_disabled and no rule acts
// or writes a run, and once the cache is back the batch runs.
func TestRuleBatchLeavesTheGroupUnreadWhileTheCacheIsDown(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "flag-lunch", func(d *rules.Definition) { d.Query = "subject:lunch" })
	start := g.pos
	last := g.publish(t, g.refs["host lunch"])
	g.f.s.cacheDown.Store(true)
	err := g.step(t.Context())
	if reason, _ := failureOf(err); reason != reasonCacheDisabled {
		t.Fatalf("step = %v, want a cache_disabled failure", err)
	}
	if g.pos != start || g.acked(t) != start {
		t.Errorf("position %d, acked %d, want both %d", g.pos, g.acked(t), start)
	}
	if n := g.fk.Calls("StoreFlags"); n != 0 {
		t.Errorf("flagged %d times while the cache was down", n)
	}
	if runs := g.runsOf(t, rule.ID); len(runs) != 0 {
		t.Errorf("runs = %+v, want none", runs)
	}
	g.f.s.cacheDown.Store(false)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !g.flagged([]ref.Ref{g.refs["host lunch"]})[0] {
		t.Error("lunch is not flagged once the cache is back")
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
}

// TestRuleCopyClearFailureHoldsTheBatch checks that a failed copy whose
// pending row cannot be dropped is never acknowledged, even once the
// failure has disabled the rule: the drop is owed apart from any rule, each
// step retries it on watch.Backoff before it reads a batch, and the batch
// is acknowledged only once the row is gone. A later run then tries the
// copy again instead of taking the pending row as a copy made.
func TestRuleCopyClearFailureHoldsTheBatch(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "copy-nowhere", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:lunch", []rules.Action{{Kind: rules.ActCopy, Folder: "Nowhere"}}
	})
	if _, err := g.f.stateDB.ExecContext(t.Context(),
		`CREATE TRIGGER keep_applied BEFORE DELETE ON rule_applied BEGIN SELECT RAISE(ABORT, 'kept'); END`); err != nil {
		t.Fatal(err)
	}
	start := g.pos
	last := g.publish(t, g.refs["host lunch"])
	key := keyOf(false, g.refs["host lunch"])
	heldStep := func(name string) {
		t.Helper()
		var wg sync.WaitGroup
		var err error
		wg.Go(func() { err = g.step(t.Context()) })
		for i := range owedWriteTries - 1 {
			w := g.clk.next(t)
			if want := watch.Backoff(i, 0.5); w.d != want {
				t.Errorf("%s: retry %d waited %v, want %v", name, i+1, w.d, want)
			}
			w.fire <- g.clk.Now()
		}
		wg.Wait()
		if reason, _ := failureOf(err); reason != api.ReasonInternal {
			t.Fatalf("%s: step = %v, want an INTERNAL failure", name, err)
		}
		if g.pos != start || g.acked(t) != start {
			t.Errorf("%s: position %d, acked %d, want both %d", name, g.pos, g.acked(t), start)
		}
		if row, ok := g.appliedRow(t, rule.ID, "host", key); !ok || row.Outcome != rules.AppliedPending {
			t.Errorf("%s: act-once row = %+v, %t, want the pending row state.db kept", name, row, ok)
		}
	}
	heldStep("first step")
	if run := g.lastRun(t, rule.ID); run.Outcome != rules.OutcomeFailed || run.Failed != 1 || run.Applied != 0 || run.Reason != api.ReasonNotFound {
		t.Errorf("run = %+v, want failed with 1 failed, NOT_FOUND", run)
	}
	now, err := g.f.s.rules.Get(t.Context(), rule.ID)
	if err != nil {
		t.Fatal(err)
	}
	if now.Enabled || now.DisabledReason != rules.ReasonAllFailed {
		t.Fatalf("rule = enabled %t, reason %q, want disabled for all_failed", now.Enabled, now.DisabledReason)
	}
	// The rule is off, so no rule would retry the drop: the runner still
	// owes it, and holds the batch.
	heldStep("step with the rule off")
	if _, err := g.f.stateDB.ExecContext(t.Context(), `DROP TRIGGER keep_applied`); err != nil {
		t.Fatal(err)
	}
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := g.appliedRow(t, rule.ID, "host", key); ok {
		t.Error("the refused copy's pending row is still there")
	}
	if n := g.fk.Calls("Copy"); n != 1 {
		t.Errorf("copies tried %d times with the rule off, want 1", n)
	}
	// The disable logged a rule event after the batch, which the step
	// reads past too.
	if got := g.acked(t); got < last || got != g.pos {
		t.Errorf("acked %d at position %d, want both at %d or past it", got, g.pos, last)
	}
	run, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := g.fk.Calls("Copy"); n != 2 {
		t.Errorf("copies tried %d times, want 2", n)
	}
	if row, ok := g.appliedRow(t, rule.ID, "host", key); ok {
		t.Errorf("act-once row = %+v, want none after the refused copy", row)
	}
	if run.Failed != 1 || run.Applied != 0 || run.Outcome != rules.OutcomeFailed {
		t.Errorf("manual run = %+v, want failed, 1 failed", run)
	}
}

// TestRuleBatchKeepsTheCap checks spec 5.6: a new-mail run acts on at most
// the rule's max_per_run messages after the act-once filter, oldest first,
// and records outcome capped when matches remain. The batch is still
// acknowledged: a scheduled or manual run picks up the rest.
func TestRuleBatchKeepsTheCap(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "flag-new", func(d *rules.Definition) { d.MaxPerRun = 1 })
	refs := []ref.Ref{g.refs["host invoice"], g.refs["host lunch"]}
	last := g.publish(t, refs...)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := g.flagged(refs); !slices.Equal(got, []bool{true, false}) {
		t.Errorf("flagged = %v, want the oldest only", got)
	}
	if run := g.lastRun(t, rule.ID); run.Outcome != rules.OutcomeCapped || run.Applied != 1 || run.Matched != 1 || run.Failed != 0 {
		t.Errorf("run = %+v, want capped with 1 applied", run)
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
}

// TestRuleCopyGuardSurvivesALaterFailure checks amendments 5 and 6 for a
// copy followed by an action that fails for now: the copy's act-once row is
// written as soon as the copy is made, so when the replay skips the copy
// and the copy's own new event arrives, the copy is not copied again.
func TestRuleCopyGuardSurvivesALaterFailure(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "copy-then-flag", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:lunch", []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}, {Kind: rules.ActFlag}}
	})
	g.publish(t, g.refs["host lunch"])
	g.fk.FailWrite("StoreFlags", mailstore.ErrUnavailable)
	err := g.step(t.Context())
	if reason, _ := failureOf(err); reason != api.ReasonServerUnavailable {
		t.Fatalf("step = %v, want a SERVER_UNAVAILABLE failure", err)
	}
	copied := g.foundIn(t, "host", "Archive", "lunch@x")
	if row, ok := g.appliedRow(t, rule.ID, "host", keyOf(false, copied)); !ok || row.Outcome != rules.AppliedDone {
		t.Errorf("copy's row after the failed flag = %+v, %t, want applied", row, ok)
	}
	g.cacheHeader(t, copied)
	g.publish(t, copied)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := g.fk.Calls("Copy"); n != 1 {
		t.Errorf("copied %d times, want 1", n)
	}
	if !g.flagged([]ref.Ref{g.refs["host lunch"]})[0] {
		t.Error("the original is not flagged after the replay")
	}
}

// TestRuleCopyGuardIsOwedUntilSaved checks a copy whose act-once row
// state.db refuses right after the copy, followed by an action that fails
// for now: the runner keeps the copy's key until the row is saved, so once
// the store recovers, the copy's own new event is skipped and the copy is
// made once.
func TestRuleCopyGuardIsOwedUntilSaved(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "copy-then-flag", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:lunch", []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}, {Kind: rules.ActFlag}}
	})
	if _, err := g.f.stateDB.ExecContext(t.Context(),
		`CREATE TRIGGER refuse_applied BEFORE INSERT ON rule_applied WHEN NEW.outcome = 'applied'
		 BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	g.publish(t, g.refs["host lunch"])
	g.fk.FailWrite("StoreFlags", mailstore.ErrUnavailable)
	err := g.step(t.Context())
	if reason, _ := failureOf(err); reason != api.ReasonServerUnavailable {
		t.Fatalf("step = %v, want a SERVER_UNAVAILABLE failure", err)
	}
	copied := g.foundIn(t, "host", "Archive", "lunch@x")
	if row, ok := g.appliedRow(t, rule.ID, "host", keyOf(false, copied)); ok {
		t.Fatalf("copy's row = %+v, want none while state.db refuses it", row)
	}
	if _, err := g.f.stateDB.ExecContext(t.Context(), `DROP TRIGGER refuse_applied`); err != nil {
		t.Fatal(err)
	}
	g.cacheHeader(t, copied)
	last := g.publish(t, copied)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := g.fk.Calls("Copy"); n != 1 {
		t.Errorf("copied %d times, want 1", n)
	}
	if row, ok := g.appliedRow(t, rule.ID, "host", keyOf(false, copied)); !ok || row.Outcome != rules.AppliedDone {
		t.Errorf("copy's row = %+v, %t, want applied", row, ok)
	}
	if !g.flagged([]ref.Ref{g.refs["host lunch"]})[0] {
		t.Error("the original is not flagged after the replay")
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
}

// TestRunRuleSkipsAnOwedCopy checks that a manual run leaves alone a copy
// whose act-once row the runner still owes: it reads the copy as applied,
// as the saved row would, and replays the original without copying again.
func TestRunRuleSkipsAnOwedCopy(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "copy-then-flag", func(d *rules.Definition) {
		d.Query, d.Actions = "subject:lunch", []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}, {Kind: rules.ActFlag}}
	})
	if _, err := g.f.stateDB.ExecContext(t.Context(),
		`CREATE TRIGGER refuse_applied BEFORE INSERT ON rule_applied WHEN NEW.outcome = 'applied'
		 BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	g.publish(t, g.refs["host lunch"])
	g.fk.FailWrite("StoreFlags", mailstore.ErrUnavailable)
	if err := g.step(t.Context()); err == nil {
		t.Fatal("step = nil, want the flag's failure")
	}
	if _, err := g.f.stateDB.ExecContext(t.Context(), `DROP TRIGGER refuse_applied`); err != nil {
		t.Fatal(err)
	}
	copied := g.foundIn(t, "host", "Archive", "lunch@x")
	g.cacheHeader(t, copied)
	run, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n := g.fk.Calls("Copy"); n != 1 {
		t.Errorf("copied %d times, want 1", n)
	}
	if run.Outcome != rules.OutcomeOK || run.Matched != 2 || run.Applied != 1 || run.Skipped != 1 {
		t.Errorf("run = %+v, want ok with 2 matched, 1 applied, 1 skipped", run)
	}
	if !g.flagged([]ref.Ref{g.refs["host lunch"]})[0] {
		t.Error("the original is not flagged after the manual run")
	}
}

// refuseDone makes state.db refuse every applied or held act-once row that
// when allows, a SQL condition on NEW, until the func it returns drops the
// refusal. Pending rows still go in.
func refuseDone(t *testing.T, g *runnerRig, when string) func() {
	t.Helper()
	for _, op := range []string{"INSERT", "UPDATE"} {
		if _, err := g.f.stateDB.ExecContext(t.Context(), `CREATE TRIGGER refuse_done_`+op+` BEFORE `+op+` ON rule_applied
			WHEN NEW.outcome != 'pending' AND (`+when+`) BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		for _, op := range []string{"INSERT", "UPDATE"} {
			if _, err := g.f.stateDB.ExecContext(t.Context(), `DROP TRIGGER refuse_done_`+op); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// moveAfterFailedMark runs a batch of host's invoice through rules whose
// act-once write state.db refuses where refuse allows, after the rules
// forwarded the invoice, which the policy holds as a draft, and moved it
// to Archive. The store then recovers, and the cache builder drops the
// original's row and caches the moved message, as the move's events make
// it. It returns the moved message.
func moveAfterFailedMark(t *testing.T, g *runnerRig, refuse string) ref.Ref {
	t.Helper()
	allow := refuseDone(t, g, refuse)
	invoice := g.refs["host invoice"]
	g.publish(t, invoice)
	if err := g.step(t.Context()); err == nil {
		t.Fatal("step = nil, want the refused act-once write's failure")
	}
	moved := g.foundIn(t, "host", "Archive", "inv42@x")
	if n := len(g.fk.Appended()); n != 1 {
		t.Fatalf("drafts after the first batch = %d, want the one held forward", n)
	}
	allow()
	if err := g.f.store.DeleteRow(t.Context(), invoice); err != nil {
		t.Fatal(err)
	}
	g.cacheHeader(t, moved)
	return moved
}

// TestRuleOwesAFailedActOnceWrite checks that an act-once write that fails
// after a forward and a move is owed whole, the moved message's key with
// it, and made before the next batch is matched: once the original's row
// is gone from the cache, the moved message's own new event is skipped
// and the invoice is not forwarded again.
func TestRuleOwesAFailedActOnceWrite(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	g.password(t)
	g.failDial(t, false)
	rule := g.newRule(t, "host", "fwd-and-file", func(d *rules.Definition) {
		d.Query = "subject:invoice"
		d.Actions = []rules.Action{{Kind: rules.ActForward, To: []string{"you@blocked.invalid"}}, {Kind: rules.ActMove, Folder: "Archive"}}
	})
	moved := moveAfterFailedMark(t, g, "1")
	last := g.publish(t, moved)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(g.fk.Appended()); n != 1 {
		t.Errorf("drafts = %d, want 1: the moved invoice was forwarded again", n)
	}
	for name, key := range map[string]string{"original": keyOf(false, g.refs["host invoice"]), "moved": keyOf(false, moved)} {
		if row, ok := g.appliedRow(t, rule.ID, "host", key); !ok || row.Outcome != rules.AppliedHeld {
			t.Errorf("%s's row = %+v, %t, want held", name, row, ok)
		}
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
}

// TestRunRuleSkipsWhatAnOwedWriteCovers checks that while the runner owes
// a rule's act-once write, a manual run reads its message's keys as the
// write will leave them: the moving rule's own row at both keys, and
// every other rule's row of the original copied to the moved key. A rule
// that forwarded the invoice before another moved it does not forward
// the moved invoice again.
func TestRunRuleSkipsWhatAnOwedWriteCovers(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	g.password(t)
	g.failDial(t, false)
	fwd := g.newRule(t, "host", "fwd-invoices", func(d *rules.Definition) {
		d.Priority, d.Query = 10, "subject:invoice"
		d.Actions = []rules.Action{{Kind: rules.ActForward, To: []string{"you@blocked.invalid"}}}
	})
	file := g.newRule(t, "host", "file-invoices", func(d *rules.Definition) {
		d.Priority, d.Query = 20, "subject:invoice"
		d.Actions = []rules.Action{{Kind: rules.ActMove, Folder: "Archive"}}
	})
	moved := moveAfterFailedMark(t, g, "NEW.rule_id = '"+file.ID+"'")
	for _, rule := range []rules.Rule{fwd, file} {
		run, err := g.r.runRule(t.Context(), rule, rules.TriggerManual, 0)
		if err != nil {
			t.Fatal(err)
		}
		if run.Outcome != rules.OutcomeOK || run.Matched != 1 || run.Skipped != 1 || run.Applied+run.Held != 0 {
			t.Errorf("rule %s's manual run = %+v, want the moved invoice skipped", rule.Name, run)
		}
	}
	if n := len(g.fk.Appended()); n != 1 {
		t.Errorf("drafts = %d, want 1: the moved invoice was forwarded again", n)
	}
	if err := g.r.payOwed(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{fwd.ID, file.ID} {
		if _, ok := g.appliedRow(t, id, "host", keyOf(false, moved)); !ok {
			t.Errorf("rule %s has no row at the moved key once the write is made", id)
		}
	}
}

// TestRunRuleDrainsOwedWritesBeforeMatching checks that a manual run makes
// the act-once writes the runner owes before it matches. A forward rule's
// final write fails, leaving the invoice's pending row and the write owed.
// While state.db still refuses it, a manual move rule's run is interrupted
// with INTERNAL and moves nothing. Once state.db takes it, the manual run
// makes the owed write first, so its move copies the forward rule's held
// row, not the pending one, to the moved key, and the moved invoice's own
// new event is not forwarded again.
func TestRunRuleDrainsOwedWritesBeforeMatching(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	g.password(t)
	g.failDial(t, false)
	fwd := g.newRule(t, "host", "fwd-invoices", func(d *rules.Definition) {
		d.Query = "subject:invoice"
		d.Actions = []rules.Action{{Kind: rules.ActForward, To: []string{"you@blocked.invalid"}}}
	})
	file := g.newRule(t, "host", "file-invoices", func(d *rules.Definition) {
		d.OnNew, d.Query = false, "subject:invoice"
		d.Actions = []rules.Action{{Kind: rules.ActMove, Folder: "Archive"}}
	})
	allow := refuseDone(t, g, "NEW.rule_id = '"+fwd.ID+"'")
	invoice := g.refs["host invoice"]
	g.publish(t, invoice)
	if err := g.step(t.Context()); err == nil {
		t.Fatal("step = nil, want the refused act-once write's failure")
	}
	if n := len(g.fk.Appended()); n != 1 {
		t.Fatalf("drafts after the first batch = %d, want the one held forward", n)
	}

	// state.db still refuses the write: the manual run fails before it
	// matches.
	var wg sync.WaitGroup
	var held rules.Run
	var heldErr error
	wg.Go(func() { held, heldErr = g.r.runRule(t.Context(), file, rules.TriggerManual, 0) })
	for range owedWriteTries - 1 {
		g.clk.next(t).fire <- g.clk.Now()
	}
	wg.Wait()
	if heldErr != nil {
		t.Fatal(heldErr)
	}
	if held.Outcome != rules.OutcomeInterrupted || held.Reason != api.ReasonInternal || held.Hint != owedWriteHint || held.Matched != 0 {
		t.Errorf("manual run with the write refused = %+v, want interrupted, INTERNAL, nothing matched", held)
	}
	if n := g.fk.Calls("Move"); n != 0 {
		t.Errorf("moved %d times while the write was owed", n)
	}

	allow()
	run, err := g.r.runRule(t.Context(), file, rules.TriggerManual, 0)
	if err != nil {
		t.Fatal(err)
	}
	if run.Outcome != rules.OutcomeOK || run.Applied != 1 {
		t.Fatalf("manual run = %+v, want ok, 1 applied", run)
	}
	moved := g.foundIn(t, "host", "Archive", "inv42@x")
	if row, ok := g.appliedRow(t, fwd.ID, "host", keyOf(false, moved)); !ok || row.Outcome != rules.AppliedHeld {
		t.Errorf("the forward rule's row at the moved key = %+v, %t, want held", row, ok)
	}
	if err := g.f.store.DeleteRow(t.Context(), invoice); err != nil {
		t.Fatal(err)
	}
	g.cacheHeader(t, moved)
	last := g.publish(t, moved)
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := len(g.fk.Appended()); n != 1 {
		t.Errorf("drafts = %d, want 1: the moved invoice was forwarded again", n)
	}
	if got := g.acked(t); got != last {
		t.Errorf("acked %d, want %d", got, last)
	}
}

// TestRuleCopyAndMoveAliasBothKeys checks amendment 6 for a rule that
// copies and then moves: the acting row, the copy's key and the moved
// message's key all hold the rule's row.
func TestRuleCopyAndMoveAliasBothKeys(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "copy-and-file", func(d *rules.Definition) {
		d.Query = "subject:lunch"
		d.Actions = []rules.Action{{Kind: rules.ActCopy, Folder: "Archive"}, {Kind: rules.ActMove, Folder: "Trash"}}
	})
	g.publish(t, g.refs["host lunch"])
	if err := g.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	run := g.lastRun(t, rule.ID)
	if run.Applied != 1 {
		t.Fatalf("run = %+v, want 1 applied", run)
	}
	for name, key := range map[string]string{
		"original": keyOf(false, g.refs["host lunch"]),
		"copy":     keyOf(false, g.foundIn(t, "host", "Archive", "lunch@x")),
		"moved":    keyOf(false, g.foundIn(t, "host", "Trash", "lunch@x")),
	} {
		if row, ok := g.appliedRow(t, rule.ID, "host", key); !ok || row.Outcome != rules.AppliedDone || row.RunID != run.ID {
			t.Errorf("%s's row = %+v, %t, want applied by run %d", name, row, ok, run.ID)
		}
	}
}

// TestRuleBatchScopeFailureCountsAndDisables checks the behaviour table's
// row for a rule that cannot be scoped for good: each batch records a
// failed run with one failure, nothing matched and the scope's reason, the
// batch is acknowledged, and the fifth such run in a row disables the
// rule with repeated_failures.
func TestRuleBatchScopeFailureCountsAndDisables(t *testing.T) {
	t.Parallel()
	g := newRunnerRig(t, "host", nil)
	rule := g.newRule(t, "host", "gone-item", func(d *rules.Definition) { d.Query = "item:it_zqxgone" })
	for i := range 5 {
		last := g.publish(t, g.refs["host lunch"])
		if err := g.step(t.Context()); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if got := g.acked(t); got != last {
			t.Errorf("step %d acked %d, want %d", i+1, got, last)
		}
		run := g.lastRun(t, rule.ID)
		if run.Outcome != rules.OutcomeFailed || run.Failed != 1 || run.Matched != 0 || run.Reason != api.ReasonNotFound || run.Hint == "" {
			t.Errorf("run %d = %+v, want failed, 1 failed, 0 matched, NOT_FOUND with a hint", i+1, run)
		}
		now, err := g.f.s.rules.Get(t.Context(), rule.ID)
		if err != nil {
			t.Fatal(err)
		}
		if wantOn := i < 4; now.Enabled != wantOn {
			t.Errorf("after run %d enabled %t, want %t", i+1, now.Enabled, wantOn)
		}
		if i == 4 && now.DisabledReason != rules.ReasonRepeatedFailures {
			t.Errorf("disabled reason %q, want %s", now.DisabledReason, rules.ReasonRepeatedFailures)
		}
	}
	if n := g.fk.Calls("StoreFlags"); n != 0 {
		t.Errorf("flagged %d times", n)
	}
}

// TestRuleRunnerStaysStartingWhileOpenFails checks amendment 8: a runner
// whose group does not open backs off as starting, with acked ID -1 and
// the failure's reason, and opens once the log answers.
func TestRuleRunnerStaysStartingWhileOpenFails(t *testing.T) {
	t.Parallel()
	g := newActRig(t, nil)
	clk := newRuleClock(routeNow)
	g.f.s.rulesClock = clk
	r := g.f.s.newRuleRunner(g.acct(t, "host"), g.f.accounts["host"])
	r.jitter = func() float64 { return 0.5 }
	if _, err := g.f.stateDB.ExecContext(t.Context(), `ALTER TABLE event_groups RENAME TO event_groups_away`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { r.Run(ctx) })
	defer wg.Wait()
	defer cancel()
	w := clk.next(t)
	if want := watch.Backoff(0, 0.5); w.d != want {
		t.Errorf("backoff %v, want %v", w.d, want)
	}
	if st := r.status(); st.State != runnerStarting || st.AckedID != -1 || st.Reason != api.ReasonInternal {
		t.Errorf("status = %+v, want starting at -1 with INTERNAL", st)
	}
	if _, err := g.f.stateDB.ExecContext(t.Context(), `ALTER TABLE event_groups_away RENAME TO event_groups`); err != nil {
		t.Fatal(err)
	}
	w.fire <- clk.Now()
	waitStatus(t, r, func(st runnerStatus) bool { return st.State == runnerIdle && st.AckedID >= 0 && st.Reason == "" })
}
