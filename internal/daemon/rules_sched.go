package daemon

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/rules"
)

// runOnAccounts runs rule once on each of accts, all at once: through the
// account's runner, which takes its turn, or as a cache_not_ready run
// with reason cache_disabled when the account has none. It returns the
// recorded runs in account order and the first error in that order.
func (s *Server) runOnAccounts(ctx context.Context, rule rules.Rule, accts []config.ResolvedAccount, t rules.Trigger, limit int) ([]rules.Run, error) {
	runs := make([]rules.Run, len(accts))
	errs := make([]error, len(accts))
	var wg sync.WaitGroup
	for i, acct := range accts {
		wg.Go(func() {
			if r := s.ruleRunners[acct.Name]; r != nil {
				runs[i], errs[i] = r.runRule(ctx, rule, t, limit)
				errs[i] = s.runnerErr(errs[i])
				return
			}
			runs[i], errs[i] = s.recordCacheDisabled(ctx, rule, acct.Name, t)
		})
	}
	wg.Wait()
	out := make([]rules.Run, 0, len(runs))
	for i, run := range runs {
		if errs[i] == nil {
			out = append(out, run)
		}
	}
	return out, cmp.Or(errs...)
}

// runnerErr is the error to report for a runner's runRule error. A
// refusal, such as errRuleAccount, passes unchanged. Anything else comes
// from the rules store, whose text can quote its driver, so it goes
// through rulesErr like every other store failure. A context error keeps
// its code there, since rulesErr keeps the cause.
func (s *Server) runnerErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*argError](err); ok {
		return err
	}
	return s.rulesErr(err)
}

// recordCacheDisabled records a run of rule on an account with no runner:
// outcome cache_not_ready, reason cache_disabled, nothing acted on. It
// takes its times from the rules clock, so a scheduled rule's next run
// follows it.
func (s *Server) recordCacheDisabled(ctx context.Context, rule rules.Rule, account string, t rules.Trigger) (rules.Run, error) {
	at := s.rulesClock.Now()
	id, err := s.rules.BeginRun(ctx, rule.ID, account, t, at)
	if err != nil {
		return rules.Run{}, s.rulesErr(err)
	}
	run := rules.Run{
		ID: id, RuleID: rule.ID, Account: account, Trigger: t, StartedAt: at, FinishedAt: at,
		Outcome: rules.OutcomeCacheNotReady, Reason: runnerCacheDisabled, Hint: cacheDisabledHint,
	}
	if _, err := s.rules.FinishRun(ctx, run); err != nil {
		return rules.Run{}, s.rulesErr(err)
	}
	return run, nil
}

const (
	// ruleSchedulerIdle is the scheduler's longest sleep. A rule change
	// wakes it sooner.
	ruleSchedulerIdle = time.Hour
	// ruleSchedulerRetry is the scheduler's sleep after a failure, or when
	// a rule stays due after its pass because it covers no account it can
	// run on, so such a rule cannot spin the loop.
	ruleSchedulerRetry = time.Minute
)

// startRuleRunners starts a rule runner, on wg, for each of accts with a
// cache worker, and records them in ruleRunners. startWatch calls it after
// startCacheAccounts, with the supervisors' context and WaitGroup, so the
// runners stop with the cache workers, before drain closes the stores. It
// does no IO.
func (s *Server) startRuleRunners(ctx context.Context, accts []config.ResolvedAccount, wg *sync.WaitGroup) {
	runners := make(map[string]*ruleRunner, len(accts))
	var names []string
	for _, acct := range accts {
		ca := s.cacheAccounts[acct.Name]
		if ca == nil {
			continue
		}
		r := s.newRuleRunner(acct, ca)
		runners[acct.Name] = r
		names = append(names, acct.Name)
		wg.Go(func() { r.Run(ctx) })
	}
	s.ruleRunners = runners
	if len(names) > 0 {
		s.opts.Logger.Info("running rules on accounts", "accounts", names)
	}
}

// schedWarned holds, by rule ID, the revision of each rule the scheduler
// has warned about, so a rule that stays due warns once per revision and
// its later tries log at debug. Only the scheduler goroutine uses it.
type schedWarned map[string]int64

// level is the level to log a scheduler failure of rule at: warn the
// first time for its revision, debug after.
func (w schedWarned) level(rule rules.Rule) slog.Level {
	if rev, ok := w[rule.ID]; ok && rev == rule.Revision {
		return slog.LevelDebug
	}
	w[rule.ID] = rule.Revision
	return slog.LevelWarn
}

// runRuleScheduler runs each scheduled rule when its next run comes, until
// ctx ends. It sleeps on the rules clock until the earliest next run,
// for at most ruleSchedulerIdle, and wakes early when a rule changes. A
// rule whose next run passed while the daemon was down runs once.
func (s *Server) runRuleScheduler(ctx context.Context) {
	warned := schedWarned{}
	for {
		wait := s.ruleSchedulerPass(ctx, warned)
		select {
		case <-ctx.Done():
			return
		case <-s.rulesClock.After(wait):
		case <-s.rulesWake:
		}
	}
}

// ruleSchedulerPass runs every due rule once, in priority order, and
// returns how long to sleep before the next pass.
func (s *Server) ruleSchedulerPass(ctx context.Context, warned schedWarned) time.Duration {
	due, err := s.rules.DueScheduled(ctx, s.rulesClock.Now())
	if err != nil {
		if ctx.Err() == nil {
			s.opts.Logger.Error("reading the scheduled rules failed", "reason", apiReason(err))
		}
		return ruleSchedulerRetry
	}
	for _, rule := range due {
		if ctx.Err() != nil {
			return ruleSchedulerRetry
		}
		s.runScheduled(ctx, rule, warned)
	}
	next, ok, err := s.rules.NextScheduled(ctx)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.opts.Logger.Error("reading the next scheduled rule failed", "reason", apiReason(err))
		}
		return ruleSchedulerRetry
	case !ok:
		return ruleSchedulerIdle
	}
	d := next.Sub(s.rulesClock.Now())
	switch {
	case d <= 0:
		return ruleSchedulerRetry
	case d > ruleSchedulerIdle:
		return ruleSchedulerIdle
	}
	return d
}

// runScheduled runs one due rule on each account it covers, within
// runRuleTimeout, through each account's runner. Each run's finish sets the
// rule's next run. Failures are logged by reason, never with rule text, at
// the level warned gives, and a run that works forgets the rule's warning.
func (s *Server) runScheduled(ctx context.Context, rule rules.Rule, warned schedWarned) {
	ctx, cancel := context.WithTimeout(ctx, runRuleTimeout)
	defer cancel()
	accts, err := s.ruleAccounts(rule.Definition)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			s.opts.Logger.Log(ctx, warned.level(rule), "a scheduled rule's accounts did not resolve", "rule", rule.ID, "reason", apiReason(err))
		}
		return
	case len(accts) == 0:
		s.opts.Logger.Log(ctx, warned.level(rule), "a scheduled rule covers no account it can run on", "rule", rule.ID)
		return
	}
	_, err = s.runOnAccounts(ctx, rule, accts, rules.TriggerSchedule, 0)
	switch {
	case err == nil:
		delete(warned, rule.ID)
	case ctx.Err() == nil:
		s.opts.Logger.Log(ctx, warned.level(rule), "a scheduled rule run failed", "rule", rule.ID, "reason", apiReason(err))
	}
}
