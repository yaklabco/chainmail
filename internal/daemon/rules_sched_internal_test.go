package daemon

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/watch"
)

// schedConfig has two accounts and the rules time zone in UTC, so at
// schedules and due times land on the times the tests name.
const schedConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.work]
provider = "hostinger"
address = "me@work.example"

[rules]
timezone = "UTC"
`

// schedWait is one After call on a schedClock: how long it waits and the
// channel the test fires it on.
type schedWait struct {
	d    time.Duration
	fire chan time.Time
}

// schedClock is a watch.Clock whose time the test sets and whose timers
// fire only when the test fires them. It sends every After call on waits.
type schedClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan schedWait
}

func (c *schedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *schedClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func (c *schedClock) After(d time.Duration) <-chan time.Time {
	w := schedWait{d: d, fire: make(chan time.Time, 1)}
	c.waits <- w
	return w.fire
}

// schedServer is a server whose daemon clock and rules clock are one
// schedClock at at. No account has a rule runner.
func schedServer(t *testing.T, at time.Time) (*Server, *schedClock) {
	t.Helper()
	clk := &schedClock{now: at, waits: make(chan schedWait, 8)}
	s, _ := newTestServerOptions(t, schedConfig, Options{Now: clk.Now})
	s.rulesClock = clk
	return s, clk
}

// startScheduler runs the rule scheduler of s until the test ends.
func startScheduler(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { s.runRuleScheduler(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// nextSchedWait returns the next wait on clk. The bound only catches a
// loop that never waits; a working one asks at once.
func nextSchedWait(t *testing.T, clk *schedClock) schedWait {
	t.Helper()
	select {
	case w := <-clk.waits:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("nothing waited on the rules clock within 5s")
		return schedWait{}
	}
}

// schedRule creates a scheduled rule on the store directly, changed by
// edit when it is not nil.
func schedRule(t *testing.T, s *Server, name, schedule string, edit func(*rules.Definition)) rules.Rule {
	t.Helper()
	sc, err := rules.ParseSchedule(schedule)
	if err != nil {
		t.Fatal(err)
	}
	d := rules.Defaults()
	d.Name, d.Query, d.Schedule = name, "in:inbox", sc
	d.Actions = []rules.Action{{Kind: rules.ActFlag}}
	if edit != nil {
		edit(&d)
	}
	r, err := s.rules.Create(t.Context(), items.Actor{UID: 1000, Client: "test"}, d.Normalize())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// schedRuns returns a rule's runs, checking each is a scheduled
// cache_disabled run.
func schedRuns(t *testing.T, s *Server, ruleID string) []rules.Run {
	t.Helper()
	runs, _, err := s.rules.ListRuns(t.Context(), rules.RunQuery{RuleID: ruleID})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Trigger != rules.TriggerSchedule || r.Outcome != rules.OutcomeCacheNotReady || r.Reason != runnerCacheDisabled {
			t.Errorf("run = %+v, want a scheduled cache_disabled run", r)
		}
	}
	return runs
}

// An every rule and an at rule each fire once at their time, on every
// account they cover, and the scheduler then sleeps until the next one.
func TestRuleSchedulerFiresEachDueRuleOnce(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 7, 8, 30, 0, 0, time.UTC)
	s, clk := schedServer(t, t0)
	hourly := schedRule(t, s, "hourly", "every:1h", nil)   // next 09:30
	morning := schedRule(t, s, "morning", "at:09:00", nil) // next 09:00
	startScheduler(t, s)
	steps := []struct {
		at              time.Time
		wait            time.Duration
		hourly, morning int
	}{
		{at: t0, wait: 30 * time.Minute},
		{at: t0.Add(30 * time.Minute), wait: 30 * time.Minute, morning: 2},
		{at: t0.Add(time.Hour), wait: time.Hour, hourly: 2, morning: 2},
	}
	var w schedWait
	for i, st := range steps {
		if i > 0 {
			clk.set(st.at)
			w.fire <- st.at
		}
		w = nextSchedWait(t, clk)
		if w.d != st.wait {
			t.Errorf("step %d: waits %v, want %v", i, w.d, st.wait)
		}
		if n := len(schedRuns(t, s, hourly.ID)); n != st.hourly {
			t.Errorf("step %d: hourly ran %d times, want %d", i, n, st.hourly)
		}
		if n := len(schedRuns(t, s, morning.ID)); n != st.morning {
			t.Errorf("step %d: morning ran %d times, want %d", i, n, st.morning)
		}
	}
	got, err := s.rules.Get(t.Context(), morning.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC); !got.NextRunAt.Equal(want) {
		t.Errorf("morning next run = %v, want %v", got.NextRunAt, want)
	}
}

// After downtime a rule whose next run passed runs once, not once per
// missed run.
func TestRuleSchedulerCatchesUpOnce(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 7, 8, 30, 0, 0, time.UTC)
	s, clk := schedServer(t, t0)
	hourly := schedRule(t, s, "hourly", "every:1h", nil)
	clk.set(t0.Add(5 * time.Hour))
	startScheduler(t, s)
	if w := nextSchedWait(t, clk); w.d != time.Hour {
		t.Errorf("waits %v after catching up, want 1h", w.d)
	}
	if n := len(schedRuns(t, s, hourly.ID)); n != 2 {
		t.Errorf("hourly ran %d times, want once on each of 2 accounts", n)
	}
}

// With no scheduled rule the scheduler sleeps its longest; a rule change
// over the API wakes it to plan again.
func TestRuleSchedulerReplansOnRuleChanges(t *testing.T) {
	t.Parallel()
	s, clk := schedServer(t, time.Date(2026, 11, 7, 8, 30, 0, 0, time.UTC))
	startScheduler(t, s)
	if w := nextSchedWait(t, clk); w.d != ruleSchedulerIdle {
		t.Errorf("idle wait = %v, want %v", w.d, ruleSchedulerIdle)
	}
	rs := &rulesService{s: s}
	ctx := peerContext(t.Context())
	if _, err := rs.CreateRule(ctx, connect.NewRequest(&chainmailv1.CreateRuleRequest{Rule: &chainmailv1.RuleDefinition{
		Name: "often", Query: "in:inbox", Schedule: "every:15m", Actions: []*chainmailv1.RuleAction{{Kind: "flag"}},
	}})); err != nil {
		t.Fatal(err)
	}
	if w := nextSchedWait(t, clk); w.d != 15*time.Minute {
		t.Errorf("after the create the scheduler waits %v, want 15m", w.d)
	}
	if _, err := rs.UpdateRule(ctx, connect.NewRequest(&chainmailv1.UpdateRuleRequest{
		Rule: "often", Patch: &chainmailv1.RuleDefinition{}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"enabled"}},
	})); err != nil {
		t.Fatal(err)
	}
	if w := nextSchedWait(t, clk); w.d != ruleSchedulerIdle {
		t.Errorf("after disabling the scheduler waits %v, want %v", w.d, ruleSchedulerIdle)
	}
	s.pokeRuleScheduler()
	s.pokeRuleScheduler() // a second poke while one is pending must not block
	nextSchedWait(t, clk)
}

// A due rule that covers no account it can run on is tried again after
// ruleSchedulerRetry, not at once.
func TestRuleSchedulerRetriesARuleThatStaysDue(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 7, 8, 30, 0, 0, time.UTC)
	s, clk := schedServer(t, t0)
	stuck := schedRule(t, s, "stuck", "every:1h", func(d *rules.Definition) {
		d.Accounts, d.Query = []string{"host"}, "account:work"
	})
	clk.set(t0.Add(2 * time.Hour))
	startScheduler(t, s)
	if w := nextSchedWait(t, clk); w.d != ruleSchedulerRetry {
		t.Errorf("waits %v for a rule that stays due, want %v", w.d, ruleSchedulerRetry)
	}
	if n := len(schedRuns(t, s, stuck.ID)); n != 0 {
		t.Errorf("stuck ran %d times, want none", n)
	}
}

// startWatch starts a rule runner for each account with a cache worker,
// and stopWatch joins them and the scheduler.
func TestStartWatchStartsRuleRunners(t *testing.T) {
	t.Parallel()
	s, _ := newCacheServerOptions(t, cacheConfig, Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store { return fake.New(a.Name, a.Provider.Gmail) },
	})
	s.startWatch(t.Context())
	got := slices.Sorted(maps.Keys(s.ruleRunners))
	s.stopWatch()
	if !slices.Equal(got, []string{"host"}) {
		t.Errorf("rule runners = %v, want [host]: off has the cache turned off", got)
	}
}

// startBackground runs the due checker on the rules clock: it scans, then
// waits until the next due time, 08:00 the next day from noon.
func TestStartBackgroundRunsTheDueChecker(t *testing.T) {
	t.Parallel()
	s, clk := schedServer(t, time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	stop := s.startBackground(t.Context())
	defer stop()
	if w := nextSchedWait(t, clk); w.d != 20*time.Hour {
		t.Errorf("the due checker waits %v, want 20h", w.d)
	}
}

// The items trim loop also trims rule runs older than 30 days.
func TestItemsTrimTrimsRuleRuns(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServer(t)
	s.itemsTrimTick = make(chan time.Time)
	insert := func(rule string, age time.Duration) {
		t.Helper()
		stamp := watch.FormatTime(s.opts.Now().Add(-age))
		if _, err := stateDB.ExecContext(t.Context(),
			"INSERT INTO rule_runs (rule_id, account, trigger_kind, started_at, finished_at, outcome) VALUES (?, 'host', 'manual', ?, ?, 'ok')",
			rule, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	count := func(rule string) int {
		t.Helper()
		var n int
		if err := stateDB.QueryRowContext(t.Context(), "SELECT count(*) FROM rule_runs WHERE rule_id = ?", rule).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	insert("ru_old", ruleRunRetention+time.Hour)
	insert("ru_new", time.Hour)
	stop := s.startBackground(t.Context())
	defer stop()
	deadline := time.Now().Add(5 * time.Second)
	for count("ru_old") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the expired rule run is still there after 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count("ru_new") != 1 {
		t.Error("the trim removed a rule run inside the retention")
	}
}

// New wires the rules store, the due checker, the rules clock and the
// scheduler's wakeup.
func TestNewWiresRules(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	if s.rules == nil || s.dueChecker == nil || cap(s.rulesWake) != 1 {
		t.Errorf("rules %v, due checker %v, wake capacity %d", s.rules, s.dueChecker, cap(s.rulesWake))
	}
	if _, ok := s.rulesClock.(daemonClock); !ok {
		t.Errorf("rulesClock = %T, want daemonClock", s.rulesClock)
	}
}

// A rule that stays due warns once per revision; the scheduler's later
// tries of the same revision log at debug, so a stuck rule cannot fill the
// log with a warning a minute.
func TestRuleSchedulerWarnsOncePerRevision(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 7, 8, 30, 0, 0, time.UTC)
	s, clk := schedServer(t, t0)
	var buf syncBuffer
	s.opts.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	stuck := schedRule(t, s, "stuck", "every:1h", func(d *rules.Definition) {
		d.Accounts, d.Query = []string{"host"}, "account:work"
	})
	clk.set(t0.Add(2 * time.Hour))
	warned := schedWarned{}
	count := func(level string) int {
		n := 0
		for line := range strings.Lines(buf.String()) {
			if strings.Contains(line, "level="+level) && strings.Contains(line, "covers no account") {
				n++
			}
		}
		return n
	}
	for range 3 {
		if d := s.ruleSchedulerPass(t.Context(), warned); d != ruleSchedulerRetry {
			t.Errorf("pass waits %v, want %v", d, ruleSchedulerRetry)
		}
	}
	if w, d := count("WARN"), count("DEBUG"); w != 1 || d != 2 {
		t.Errorf("after 3 passes: %d warnings and %d debug lines, want 1 and 2", w, d)
	}
	warned[stuck.ID] = stuck.Revision - 1 // as if the rule changed since
	s.ruleSchedulerPass(t.Context(), warned)
	if w := count("WARN"); w != 2 {
		t.Errorf("after a new revision: %d warnings, want 2", w)
	}
}

// A scheduled round whose accounts end differently leaves the earliest
// next run any of them needs, whichever finishes last: the stale account's
// five-minute retry (contract amendment 7) beats the ready account's hour.
// Holding one account's turn makes the other finish first.
func TestRuleSchedulerKeepsARoundsRetry(t *testing.T) {
	t.Parallel()
	for _, last := range []string{"ready", "stale"} {
		t.Run(last+" finishes last", func(t *testing.T) {
			t.Parallel()
			g := newRunnerRig(t, "host", nil)
			gmail := g.f.s.newRuleRunner(g.acct(t, "gmail"), g.f.accounts["gmail"])
			gmail.ca = newFakeProgress()
			g.f.s.ruleRunners = map[string]*ruleRunner{"host": g.r, "gmail": gmail}
			g.f.ageSync(t, "host", "Archive", time.Hour)
			held := gmail // the ready account
			if last == "stale" {
				held = g.r
			}
			rule := g.newRule(t, "host", "hourly", func(d *rules.Definition) {
				d.Accounts, d.OnNew = []string{"host", "gmail"}, false
				d.Schedule = rules.Schedule{Kind: rules.ScheduleEvery, Every: time.Hour}
			})

			held.turn <- struct{}{}
			// The round runs on its own context, cancelled and joined at
			// cleanup, so a failing wait below never leaves it running
			// while the fixture closes.
			ctx, cancel := context.WithCancel(t.Context())
			var wg sync.WaitGroup
			t.Cleanup(func() { cancel(); wg.Wait() })
			wg.Go(func() { g.f.s.runScheduled(ctx, rule, schedWarned{}) })
			waitFinishedRuns(t, g.f.s, rule.ID, 1)
			<-held.turn
			wg.Wait()

			runs := waitFinishedRuns(t, g.f.s, rule.ID, 2)
			outcomes := map[string]rules.Outcome{}
			for _, r := range runs {
				outcomes[r.Account] = r.Outcome
			}
			if outcomes["host"] != rules.OutcomeCacheNotReady || outcomes["gmail"] != rules.OutcomeOK {
				t.Fatalf("outcomes = %v, want host cache_not_ready and gmail ok", outcomes)
			}
			got, err := g.f.s.rules.Get(t.Context(), rule.ID)
			if err != nil {
				t.Fatal(err)
			}
			if want := routeNow.Add(5 * time.Minute); !got.NextRunAt.Equal(want) {
				t.Errorf("next run %v, want the retry at %v", got.NextRunAt, want)
			}
		})
	}
}

// waitFinishedRuns waits until rule ruleID has n finished runs, and
// returns its runs.
func waitFinishedRuns(t *testing.T, s *Server, ruleID string, n int) []rules.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs, _, err := s.rules.ListRuns(t.Context(), rules.RunQuery{RuleID: ruleID})
		if err != nil {
			t.Fatal(err)
		}
		finished := 0
		for _, r := range runs {
			if r.Outcome != rules.OutcomeInterrupted {
				finished++
			}
		}
		if finished >= n {
			return runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d runs finished within 5s", finished, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
