package daemon

import (
	"cmp"
	"context"
	"sync"

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
