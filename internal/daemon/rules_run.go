package daemon

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/rules"
	"github.com/yaklabco/chainmail/internal/watch"
)

const (
	// newBatchSize is the most events a runner reads from its group at
	// once (spec 5.2).
	newBatchSize = 200
	// runFinishTimeout bounds the write that settles a run. It runs on a
	// detached context, so a run that its deadline or a shutdown ended is
	// still recorded with what it did.
	runFinishTimeout = 5 * time.Second
	// bodyWait bounds a new-mail batch's wait for the bodies its text
	// rules need (spec 5.2 step 3).
	bodyWait = 5 * time.Minute
	// bodyPoll is how often a batch waiting for bodies looks again. The
	// prefetcher has no signal, so the wait polls.
	bodyPoll = 5 * time.Second
	// reasonBodyNotCached is the run reason of a text rule whose message
	// had no cached body in time.
	reasonBodyNotCached = "body_not_cached"
	// bodyNotCachedHint is its fixed hint.
	bodyNotCachedHint = "the rule's query has text terms, which need message bodies the cache did not hold in time; " +
		"check [cache] prefetch_bodies, or run the rule by hand later"
	// matchPageSize is how many matches a scheduled or manual run reads
	// at once.
	matchPageSize = 200
	// ruleCacheWait bounds a scheduled or manual run's wait for the cache
	// builder to apply the log's tail. The cache counts as complete while
	// the builder lags by less than fresh_lag, so a healthy builder gets
	// there well inside it; one that does not reads as stale.
	ruleCacheWait = time.Minute
	// ruleCacheNotReadyHint is the fixed hint of a cache_not_ready run.
	// search_items.go's cacheNotReadyHint answers a refused search.
	ruleCacheNotReadyHint = "the cache cannot answer this rule completely yet; " +
		"run it again once chainmail cache status shows the account up to date"
	// ruleCacheDisabledHint is the fixed hint of a runner that leaves new
	// mail unread while the account's cache is off.
	ruleCacheDisabledHint = "the cache is off, as it turns off when the disk holding cache.db is full; " +
		"rules run on the new mail once the cache is back on, which the daemon log reports"
)

// errRuleAccount refuses a run of a rule on an account it does not cover.
// Its message is errRuleAccountNotCovered's, but its hint fits RunRule,
// whose account is named or left out, where that one fits a dry-run.
var errRuleAccount = &argError{
	msg:  "the rule does not cover that account",
	hint: "run the rule on an account it names, or on all of its accounts",
}

// The states a runner reports in RulesStatus. An account without a runner
// is reported as cache_disabled by the handler.
const (
	// runnerStarting is a runner that has not opened its group yet. Its
	// acked ID is -1 (amendment 8).
	runnerStarting        = "starting"
	runnerRunning         = "running"
	runnerWaitingForCache = "waiting_for_cache"
	runnerBackingOff      = "backing_off"
	runnerIdle            = "idle"
)

// cacheProgress is what a runner reads of its account's cache worker: the
// event log position the builder has applied to cache.db, a channel closed
// each time that moves, and whether the worker prefetches bodies at all.
// *cache.Account implements it; internal tests pass a fake.
type cacheProgress interface {
	Applied() int64
	AppliedChanged() <-chan struct{}
	PrefetchesBodies() bool
}

// ruleRunner runs the rules of one account that has a cache worker. It
// does one job at a time: a new-mail batch, or a scheduled or manual run
// of one rule, so two triggers never act on the account at once (spec
// 5.1). It acts as the rule client through the daemon's organize, compose
// and items paths, and never logs, records or returns rule text.
type ruleRunner struct {
	s    *Server
	acct config.ResolvedAccount
	ca   cacheProgress
	// group is the account's rules group in the event log.
	group string
	// turn holds a token while a job runs. Taking it waits on the job's
	// context, so a manual run waits for a batch within its deadline.
	turn chan struct{}
	// clock times the backoff and the body wait; jitter scales the
	// backoff.
	clock  watch.Clock
	jitter func() float64
	// mu guards st and jobs.
	mu sync.Mutex
	// st is the runner's state, as RulesStatus reports it.
	st runnerStatus
	// jobs counts the scheduled and manual runs holding the turn, which
	// show the runner as running whatever its loop last said.
	jobs int
}

// runnerStatus is a runner's state for RulesStatus: one of the runner
// states, the group's acknowledged event ID (-1 while starting), and the
// reason code of the failure it is backing off from.
type runnerStatus struct {
	State   string
	AckedID int64
	Reason  string
}

// newRuleRunner makes the runner of acct over its cache worker ca, which
// must not be nil: an account without a worker has no runner (amendment
// 12).
func (s *Server) newRuleRunner(acct config.ResolvedAccount, ca *cache.Account) *ruleRunner {
	return &ruleRunner{
		s: s, acct: acct, ca: ca, group: watch.RulesGroup(acct.Name),
		turn: make(chan struct{}, 1), clock: s.rulesClock, jitter: rand.Float64,
		st: runnerStatus{State: runnerStarting, AckedID: -1},
	}
}

// status returns the runner's state. While a scheduled or manual run
// holds the turn, it is running.
func (r *ruleRunner) status() runnerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.st
	if r.jobs > 0 {
		st.State, st.Reason = runnerRunning, ""
	}
	return st
}

func (r *ruleRunner) setState(state, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.State, r.st.Reason = state, reason
}

// setAcked records the group's acknowledged ID. The first one, which open
// records, moves a starting runner to idle.
func (r *ruleRunner) setAcked(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.AckedID = id
	if r.st.State == runnerStarting {
		r.st.State, r.st.Reason = runnerIdle, ""
	}
}

// take waits for the runner's turn, or for ctx to end.
func (r *ruleRunner) take(ctx context.Context) error {
	select {
	case r.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release ends the job holding the turn.
func (r *ruleRunner) release() { <-r.turn }

// Run reads watch.RulesGroup(acct) from its acked position (opening it at
// the tail on first start, recovering a group past the tail like the cache
// does), keeps new events, waits for ca.Applied() to reach the batch's
// last ID, matches each enabled on_new rule in priority order, acts,
// records, and acks. Transient failures stop without acking and back off
// with watch.Backoff. It returns when ctx ends.
func (r *ruleRunner) Run(ctx context.Context) {
	pos := int64(-1)
	failures := 0
	for ctx.Err() == nil {
		var err error
		if pos < 0 {
			if pos, err = r.open(ctx); err != nil {
				pos = -1
			}
		} else {
			pos, err = r.step(ctx, pos)
		}
		if err == nil {
			failures = 0
			continue
		}
		if ctx.Err() != nil {
			return
		}
		reason, _ := failureOf(err)
		state := runnerBackingOff
		if pos < 0 {
			// The group is not open yet: the runner is still starting.
			state = runnerStarting
		}
		r.setState(state, reason)
		r.s.opts.Logger.WarnContext(ctx, "a rules runner step failed; backing off",
			"account", r.acct.Name, "reason", reason, "failures", failures+1)
		select {
		case <-r.clock.After(watch.Backoff(failures, r.jitter())):
		case <-ctx.Done():
			return
		}
		failures++
	}
}

// open opens the rules group, at the log's tail when it is new. A group
// acknowledged past the tail, which a state.db reset leaves, moves back to
// the tail first, as the cache's recoverGroup does but without a rebuild:
// a rules group holds nothing but its position.
func (r *ruleRunner) open(ctx context.Context) (int64, error) {
	acked, _, err := r.s.watchLog.Group(ctx, r.group)
	if err != nil {
		return 0, err
	}
	tail, err := r.s.watchLog.Tail(ctx)
	if err != nil {
		return 0, err
	}
	if acked > tail {
		r.s.opts.Logger.WarnContext(ctx, "a rules group is past the event log's tail; reading from the tail", "account", r.acct.Name)
		moved, err := r.s.watchLog.ResetGroup(ctx, r.group, acked, tail)
		if err != nil {
			return 0, err
		}
		acked = tail
		if !moved {
			// Another reset moved it first: read where it is now.
			if acked, _, err = r.s.watchLog.Group(ctx, r.group); err != nil {
				return 0, err
			}
		}
	}
	r.setAcked(acked)
	return acked, nil
}

// step reads the next batch after pos, runs the on_new rules over its new
// messages once the cache holds them, and acknowledges it. Caught up, it
// waits for the log to grow and returns pos. A failure returns pos and
// acknowledges nothing, so the batch is read again.
func (r *ruleRunner) step(ctx context.Context, pos int64) (int64, error) {
	page, err := r.s.watchLog.Read(ctx, watch.Query{
		After:  pos,
		Filter: watch.Filter{Accounts: []string{r.acct.Name}, Kinds: []watch.Kind{watch.KindNew}},
		Limit:  newBatchSize,
	})
	if err != nil {
		return pos, err
	}
	if len(page.Events) == 0 && page.Next == pos {
		r.setState(runnerIdle, "")
		return pos, r.s.watchLog.Wait(ctx, pos)
	}
	if b := r.batchOf(page.Events); len(b.refs) > 0 {
		if err := r.waitForCache(ctx, b.last); err != nil {
			return pos, err
		}
		if err := r.runBatch(ctx, b); err != nil {
			return pos, err
		}
	}
	if err := r.s.watchLog.Ack(ctx, r.group, page.Next); err != nil {
		return pos, err
	}
	r.setAcked(page.Next)
	return page.Next, nil
}

// batchKey names one message of a batch.
type batchKey struct {
	folder      string
	uidValidity uint32
	uid         uint32
}

// newBatch is the new messages of one read of the group.
type newBatch struct {
	// byFolder are the messages' refs by folder, in log order.
	byFolder map[string][]ref.Ref
	// refs are the same refs by message.
	refs map[batchKey]ref.Ref
	// last is the ID of the batch's last new event.
	last int64
	// bodyDeadline ends the batch's wait for bodies; zero until a text
	// rule first waits.
	bodyDeadline time.Time
}

// batchOf keeps the account's new events of a page, once per message. An
// event whose ref does not parse, or names another account, is dropped.
func (r *ruleRunner) batchOf(events []watch.Event) *newBatch {
	b := &newBatch{byFolder: map[string][]ref.Ref{}, refs: map[batchKey]ref.Ref{}}
	for _, e := range events {
		if e.Kind != watch.KindNew || e.Account != r.acct.Name {
			continue
		}
		m, err := ref.Parse(e.Ref)
		if err != nil || m.Account != r.acct.Name {
			continue
		}
		k := batchKey{folder: m.Folder, uidValidity: m.UIDValidity, uid: m.UID}
		if _, dup := b.refs[k]; !dup {
			b.refs[k] = m
			b.byFolder[m.Folder] = append(b.byFolder[m.Folder], m)
		}
		b.last = e.ID
	}
	return b
}

// scoped is the batch's UIDs in each of sources, whose UIDVALIDITY they
// must have: a message of a folder reset since its event is not matched.
func (b *newBatch) scoped(sources []mailstore.Source) map[string][]uint32 {
	out := map[string][]uint32{}
	for _, src := range sources {
		for _, m := range b.byFolder[src.Folder] {
			if m.UIDValidity == src.UIDValidity {
				out[src.Folder] = append(out[src.Folder], m.UID)
			}
		}
	}
	return out
}

// waitForCache waits until the account's cache builder has applied the
// log up to last, so cache.db holds the batch's messages (spec 5.2 step
// 2). It does not hold the turn, so a manual run can go meanwhile.
func (r *ruleRunner) waitForCache(ctx context.Context, last int64) error {
	for {
		changed := r.ca.AppliedChanged()
		if r.ca.Applied() >= last {
			return nil
		}
		r.setState(runnerWaitingForCache, "")
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runBatch runs every enabled on_new rule covering the account over b, in
// priority order, on the runner's turn. A message a stop rule matched, or
// a relocating rule moved, is left alone by the rules after it. The first
// transient failure ends the batch.
func (r *ruleRunner) runBatch(ctx context.Context, b *newBatch) error {
	if err := r.take(ctx); err != nil {
		return err
	}
	defer r.release()
	r.setState(runnerRunning, "")
	list, err := r.s.rules.EnabledOnNew(ctx, r.acct.Name)
	if err != nil {
		return err
	}
	stopped := map[string]bool{}
	for _, rule := range list {
		if err := r.newRun(ctx, rule, b, stopped); err != nil {
			return err
		}
	}
	return nil
}

// newRun runs one on_new rule over the batch. A text rule first waits for
// the bodies it needs, and a message whose body does not come counts as a
// failed match (amendment 10). A rule that matched nothing writes no run
// (spec 3.3). It returns a transient failure, after recording the run as
// interrupted. While the account's cache is off it returns a
// cache_disabled failure and writes no run, so the batch stays unread.
func (r *ruleRunner) newRun(ctx context.Context, rule rules.Rule, b *newBatch, stopped map[string]bool) error {
	start := r.s.opts.Now()
	sc, covered, reason, err := r.scope(ctx, rule, false)
	switch {
	case err != nil && classifyReason(errDetail(err).GetReason()) == actTransient:
		return err
	case err != nil:
		_, err = r.scopeFailed(ctx, rule, rules.TriggerNew, start, err)
		return err
	case !covered:
		return nil
	case reason == reasonCacheDisabled:
		// The cache is down, as a full disk turns it off: the batch
		// would match nothing, so it stays unread until the cache is back
		// (spec 5.2).
		return &actionUnavailable{reason: reasonCacheDisabled, hint: ruleCacheDisabledHint}
	}
	uids := b.scoped(sc.sources)
	var bodyFailed []ref.Ref
	if sc.needsBodies {
		if uids, bodyFailed, err = r.bodyGate(ctx, sc, b, uids); err != nil {
			return err
		}
	}
	rows, err := r.s.matchBatch(ctx, sc, uids)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(rows)+len(bodyFailed))
	for _, row := range rows {
		keys = append(keys, messageKeyOf(r.acct, row.Ref()))
	}
	for _, m := range bodyFailed {
		keys = append(keys, messageKeyOf(r.acct, m))
	}
	if len(keys) == 0 {
		return nil
	}
	applied, err := r.s.rules.Applied(ctx, rule.ID, r.acct.Name, keys)
	if err != nil {
		return err
	}
	run, err := r.begin(ctx, rule, rules.TriggerNew, start)
	if err != nil {
		return err
	}
	var cause error
	for i, row := range rows {
		run.Matched++
		if stopped[keys[i]] {
			run.Skipped++
			continue
		}
		moved, err := r.apply(ctx, rule, &run, row, keys[i], applied)
		if err != nil {
			cause = err
			break
		}
		if moved || rule.Stop {
			stopped[keys[i]] = true
		}
	}
	if cause == nil {
		for _, key := range keys[len(rows):] {
			run.Matched++
			if prev, ok := applied[key]; stopped[key] || (ok && prev.Outcome != rules.AppliedPending) {
				run.Skipped++
				continue
			}
			run.Failed++
			if run.Reason == "" {
				run.Reason, run.Hint = reasonBodyNotCached, bodyNotCachedHint
			}
		}
	}
	if err := r.settle(ctx, &run, false, cause); err != nil {
		return err
	}
	return cause
}

// bodyGate splits a text rule's batch messages, uids, into those whose
// bodies the cache holds, which it returns to match, and those that fail
// with body_not_cached: a final body failure, any message when the
// account prefetches no bodies, and one still pending at the batch's body
// deadline. It waits while any is pending. A message without a row is in
// neither: it is not matched (amendment 11).
func (r *ruleRunner) bodyGate(ctx context.Context, sc ruleScope, b *newBatch, uids map[string][]uint32) (map[string][]uint32, []ref.Ref, error) {
	for {
		ready := map[string][]uint32{}
		var failed, pending []ref.Ref
		for _, src := range sc.sources {
			list := uids[src.Folder]
			if len(list) == 0 {
				continue
			}
			states, err := r.s.cache.BodyStates(ctx, r.acct.Name, src, list)
			if err != nil {
				return nil, nil, err
			}
			for _, uid := range list {
				st, ok := states[uid]
				if !ok {
					continue
				}
				m := b.refs[batchKey{folder: src.Folder, uidValidity: src.UIDValidity, uid: uid}]
				switch st {
				case cache.BodyHeld:
					ready[src.Folder] = append(ready[src.Folder], uid)
				case cache.BodyFailed:
					failed = append(failed, m)
				default:
					pending = append(pending, m)
				}
			}
		}
		if len(pending) == 0 {
			return ready, failed, nil
		}
		now := r.clock.Now()
		if b.bodyDeadline.IsZero() {
			b.bodyDeadline = now.Add(bodyWait)
		}
		if !r.ca.PrefetchesBodies() || !now.Before(b.bodyDeadline) {
			return ready, append(failed, pending...), nil
		}
		select {
		case <-r.clock.After(min(bodyPoll, b.bodyDeadline.Sub(now))):
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// scope resolves rule on the runner's account. covered is false when the
// rule's accounts or its query's account: terms leave the account out.
// With complete, reason says why the cache cannot answer the rule
// completely, "" when it can.
func (r *ruleRunner) scope(ctx context.Context, rule rules.Rule, complete bool) (ruleScope, bool, string, error) {
	accts, err := r.s.ruleAccounts(rule.Definition)
	if err != nil {
		return ruleScope{}, false, "", err
	}
	if !slices.ContainsFunc(accts, func(a config.ResolvedAccount) bool { return a.Name == r.acct.Name }) {
		return ruleScope{}, false, "", nil
	}
	sc, reason, err := r.s.scopeRule(ctx, rule.ID, rule.Definition, r.acct, complete)
	return sc, true, reason, err
}

// begin starts a run of rule on the account, stored as interrupted until
// it is settled.
func (r *ruleRunner) begin(ctx context.Context, rule rules.Rule, t rules.Trigger, start time.Time) (rules.Run, error) {
	id, err := r.s.rules.BeginRun(ctx, rule.ID, r.acct.Name, t, start)
	if err != nil {
		return rules.Run{}, err
	}
	return rules.Run{ID: id, RuleID: rule.ID, Account: r.acct.Name, Trigger: t, StartedAt: start}, nil
}

// scopeFailed records a run of rule that could not start for good, such
// as one whose item: term names a deleted item, with cause's reason and
// hint. It counts as one permanent failure, so five in a row disable the
// rule.
func (r *ruleRunner) scopeFailed(ctx context.Context, rule rules.Rule, t rules.Trigger, start time.Time, cause error) (rules.Run, error) {
	run, err := r.begin(ctx, rule, t, start)
	if err != nil {
		return rules.Run{}, err
	}
	run.Failed = 1
	run.Reason, run.Hint = failureOf(cause)
	err = r.settle(ctx, &run, false, nil)
	return run, err
}

// apply runs rule on one matched row and counts it in run. A row the
// act-once table holds as applied or held is skipped; a pending one is
// replayed. It returns whether later rules must leave the message alone
// because this rule moves messages, and a transient failure, which ends
// the run.
func (r *ruleRunner) apply(ctx context.Context, rule rules.Rule, run *rules.Run, row cache.Row, key string, applied map[string]rules.AppliedRow) (bool, error) {
	var replay *rules.AppliedRow
	if prev, ok := applied[key]; ok {
		if prev.Outcome != rules.AppliedPending {
			run.Skipped++
			return relocates(rule), nil
		}
		replay = &prev
	}
	res := r.s.actOn(ctx, rule, run.ID, r.acct, row, replay)
	switch res.class {
	case actOK, actHeld:
		outcome := rules.AppliedDone
		if res.class == actHeld {
			outcome = rules.AppliedHeld
			run.Held++
		} else {
			run.Applied++
		}
		return relocates(rule), r.markApplied(ctx, rule.ID, run.ID, key, outcome, res)
	case actPermanent:
		run.Failed++
		if run.Reason == "" {
			run.Reason, run.Hint = res.reason, res.hint
		}
		if res.clearPending {
			// A pending row left behind would make a replay of this batch
			// take the copy as made, so failing the batch here is worse
			// than keeping the failure counted and going on.
			if err := rules.ClearPending(ctx, r.s.opts.StateDB, rule.ID, r.acct.Name, key); err != nil {
				r.s.opts.Logger.WarnContext(ctx, "a failed rule action left a pending act-once row",
					"rule", rule.ID, "account", r.acct.Name, "run", run.ID, "err", err)
			}
		}
		return false, nil
	default:
		return false, &actionUnavailable{reason: res.reason, hint: res.hint}
	}
}

// markApplied records a message the rule acted on, then copies every
// rule's row of its key to the keys of its new ref and of the copy it
// made, so no rule acts on it again where it landed (amendment 6).
func (r *ruleRunner) markApplied(ctx context.Context, ruleID string, runID int64, key string, o rules.AppliedOutcome, res actResult) error {
	alias := aliasOf(r.acct, res.newRef, key)
	if err := r.s.rules.MarkApplied(ctx, ruleID, r.acct.Name, key, runID, o, res.note, alias); err != nil {
		return err
	}
	if copied := aliasOf(r.acct, res.copyRef, key); copied != "" && copied != alias {
		return r.s.rules.MarkApplied(ctx, ruleID, r.acct.Name, key, runID, o, res.note, copied)
	}
	return nil
}

// aliasOf is the act-once key of newRef when it names another message
// than key, else "".
func aliasOf(acct config.ResolvedAccount, newRef, key string) string {
	if newRef == "" {
		return ""
	}
	m, err := ref.Parse(newRef)
	if err != nil {
		return ""
	}
	if k := messageKeyOf(acct, m); k != key {
		return k
	}
	return ""
}

// settle sets run's outcome from what ended it, then writes it:
// interrupted with cause's reason on a transient failure, else failed when
// a message failed, else capped, else ok.
func (r *ruleRunner) settle(ctx context.Context, run *rules.Run, capped bool, cause error) error {
	switch {
	case cause != nil:
		run.Outcome = rules.OutcomeInterrupted
		run.Reason, run.Hint = failureOf(cause)
	case run.Failed > 0:
		run.Outcome = rules.OutcomeFailed
	case capped:
		run.Outcome = rules.OutcomeCapped
	default:
		run.Outcome = rules.OutcomeOK
	}
	return r.write(ctx, run)
}

// write finishes run in the store, on a detached context, and logs an
// auto-disable it caused.
func (r *ruleRunner) write(ctx context.Context, run *rules.Run) error {
	run.FinishedAt = r.s.opts.Now()
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runFinishTimeout)
	defer cancel()
	disabled, err := r.s.rules.FinishRun(wctx, *run)
	if err != nil {
		return err
	}
	if disabled {
		r.s.opts.Logger.InfoContext(ctx, "a rule disabled itself after permanent failures",
			"rule", run.RuleID, "account", run.Account, "run", run.ID)
	}
	return nil
}

// actionUnavailable is an action failure a retry can fix, which ends the
// run it happened in. It carries the failure's reason and fixed hint; its
// text is fixed.
type actionUnavailable struct {
	reason, hint string
}

func (e *actionUnavailable) Error() string { return "a rule action failed for now" }

// failureOf is the reason code and fixed hint to record for err.
func failureOf(err error) (string, string) {
	if u, ok := errors.AsType[*actionUnavailable](err); ok {
		return u.reason, u.hint
	}
	d := errDetail(err)
	return d.GetReason(), d.GetHint()
}

// relocates reports whether rule moves the messages it acts on.
func relocates(rule rules.Rule) bool {
	return slices.ContainsFunc(rule.Actions, func(a rules.Action) bool { return a.Kind.Relocates() })
}

// runRule runs one rule once on this account for TriggerSchedule or
// TriggerManual, taking the runner's lock. limit 0 means the rule's
// MaxPerRun. It matches the whole cache oldest first, needs the cache to
// answer the rule completely (spec 5.3), and returns the run it recorded
// with a nil error whatever its outcome. A complete cache can still lag
// the log by less than fresh_lag, so the run first waits for the builder
// to apply the log's tail (builderAtTail): mail the sync has just found,
// such as what a downtime catch-up is for, is matched in this run. An
// error means the turn was not taken, or the builder not waited for,
// before ctx ended, the rule does not cover the account (errRuleAccount),
// or the log's tail could not be read or the run written.
func (r *ruleRunner) runRule(ctx context.Context, rule rules.Rule, t rules.Trigger, limit int) (rules.Run, error) {
	if err := r.take(ctx); err != nil {
		return rules.Run{}, err
	}
	defer r.release()
	defer r.busy()()
	if limit <= 0 {
		limit = rule.MaxPerRun
	}
	start := r.s.opts.Now()
	sc, covered, reason, err := r.scope(ctx, rule, true)
	transient := err != nil && classifyReason(errDetail(err).GetReason()) == actTransient
	switch {
	case err != nil && !transient:
		return r.scopeFailed(ctx, rule, t, start, err)
	case err == nil && !covered:
		return rules.Run{}, errRuleAccount
	}
	if err == nil && reason == "" {
		at, werr := r.builderAtTail(ctx)
		if werr != nil {
			return rules.Run{}, werr
		}
		if !at {
			reason = cache.ReasonStale
		}
	}
	run, berr := r.begin(ctx, rule, t, start)
	if berr != nil {
		return rules.Run{}, berr
	}
	switch {
	case transient:
		err = r.settle(ctx, &run, false, err)
	case reason != "":
		run.Outcome, run.Reason, run.Hint = rules.OutcomeCacheNotReady, reason, ruleCacheNotReadyHint
		err = r.write(ctx, &run)
	default:
		capped, cause := r.page(ctx, rule, sc, &run, limit)
		err = r.settle(ctx, &run, capped, cause)
	}
	return run, err
}

// builderAtTail waits until the account's cache builder has applied the
// event log up to its tail as read now, and reports whether it got there
// within ruleCacheWait. The caller has already found the cache's folders
// fresh, and a sync pass stamps a folder in the same write as the events
// it logs, so the tail read after that covers every pass the freshness
// counted. Applied passes every ID in the log, other accounts' too, so it
// compares with the tail directly. The runner's state is left alone: a
// job holding the turn shows as running.
func (r *ruleRunner) builderAtTail(ctx context.Context) (bool, error) {
	tail, err := r.s.watchLog.Tail(ctx)
	if err != nil {
		return false, err
	}
	var timeout <-chan time.Time
	for {
		changed := r.ca.AppliedChanged()
		if r.ca.Applied() >= tail {
			return true, nil
		}
		if timeout == nil {
			timeout = r.clock.After(ruleCacheWait)
		}
		select {
		case <-changed:
		case <-timeout:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// page runs rule over the scope oldest first, a page at a time, acting on
// at most limit messages the act-once table does not skip. It reports
// whether it stopped at limit with a match left, and the transient
// failure that ended it.
func (r *ruleRunner) page(ctx context.Context, rule rules.Rule, sc ruleScope, run *rules.Run, limit int) (bool, error) {
	var after cache.MatchCursor
	acted := 0
	for {
		rows, err := r.s.matchPage(ctx, sc, after, matchPageSize)
		if err != nil || len(rows) == 0 {
			return false, err
		}
		after = cache.CursorOf(rows[len(rows)-1])
		keys := make([]string, len(rows))
		for i, row := range rows {
			keys[i] = messageKeyOf(r.acct, row.Ref())
		}
		applied, err := r.s.rules.Applied(ctx, rule.ID, r.acct.Name, keys)
		if err != nil {
			return false, err
		}
		for i, row := range rows {
			if prev, ok := applied[keys[i]]; ok && prev.Outcome != rules.AppliedPending {
				run.Matched++
				run.Skipped++
				continue
			}
			if acted == limit {
				return true, nil
			}
			run.Matched++
			acted++
			if _, err := r.apply(ctx, rule, run, row, keys[i], applied); err != nil {
				return false, err
			}
		}
	}
}

// busy counts a scheduled or manual run until the func it returns ends
// it.
func (r *ruleRunner) busy() func() {
	r.mu.Lock()
	r.jobs++
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.jobs--
		r.mu.Unlock()
	}
}
