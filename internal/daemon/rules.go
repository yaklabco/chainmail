package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/audit"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/rules"
)

const (
	// runRuleTimeout is the deadline of RunRule, of DryRunRule and of one
	// scheduled run when the client set none: a capped run can act on
	// 5000 messages.
	runRuleTimeout = 10 * time.Minute
	// ruleRunsShown is how many runs GetRule returns.
	ruleRunsShown = 10
	// ruleRevisionsShown is how many revisions GetRule returns with history.
	ruleRevisionsShown = 50
	// maxRuleRunsLimit is the largest ListRuleRuns page.
	maxRuleRunsLimit = 500
	// runnerCacheDisabled is the state of an account with no rule runner:
	// its cache is off, or the account is not synced.
	runnerCacheDisabled = "cache_disabled"
	// ruleListHint points at the list of rules, where IDs and names come from.
	ruleListHint = "run: chainmail rule list"
	// ruleMaskHint lists UpdateRule's mask paths.
	ruleMaskHint = "paths are name, description, accounts, query, on_new, schedule, priority, stop, actions, max_per_run and enabled"
	// actionKindHint lists the action kinds.
	actionKindHint = "action kinds are label, unlabel, copy, flag, unflag, read, unread, link_item, forward, move, archive and trash"
	// cacheDisabledHint is the hint of a run recorded for an account with
	// no rule runner.
	cacheDisabledHint = "the account's cache is off or the account is not synced; run: chainmail cache status"
)

// rulePaths are UpdateRule's mask paths: RuleDefinition's field names.
var rulePaths = []string{"name", "description", "accounts", "query", "on_new", "schedule", "priority", "stop", "actions", "max_per_run", "enabled"}

// The service's own refusals. Their text is fixed: none repeats a value
// from the request, since a rule's text is often copied from mail.
var (
	errRuleRef         = &argError{msg: "rule must be a rule ID or name", hint: "rule IDs start with ru_; " + ruleListHint}
	errRuleMask        = &argError{msg: "update_mask must name one or more fields", hint: ruleMaskHint}
	errRulePath        = &argError{msg: "update_mask names an unknown field", hint: ruleMaskHint}
	errActionKind      = &argError{msg: "unknown action kind", hint: actionKindHint}
	errRuleSchedule    = &argError{msg: "invalid schedule", hint: "use every: with 15m to 30d, such as every:6h or every:7d, or at:HH:MM, such as at:07:00"}
	errLinkItemUnknown = &argError{msg: "a link_item action names an unknown item", hint: itemListHint}
	errLinkItemDeleted = &argError{msg: "a link_item action names a deleted item", hint: "restore the item or name another; " + itemListHint}
	errDryRunSource    = &argError{msg: "set exactly one of rule and definition", hint: "dry-run a saved rule by ID or name, or an unsaved definition"}
	errRuleOutcome     = &argError{msg: "outcome must be ok, capped, failed, cache_not_ready or interrupted", hint: "leave outcome empty for every outcome"}
	errRuleMax         = &argError{msg: "max must be 0 to 5000", hint: "0 means the rule's max_per_run"}
	errRuleRunsLimit   = &argError{msg: "limit must be 0 to 500", hint: "0 means 50"}
	errRuleBeforeID    = &argError{msg: "before_id must be 0 or more", hint: "pass next_before_id from the previous page"}
	errRuleDeleted     = &argError{msg: "the rule is deleted", hint: "restore it first: chainmail rule restore <id>"}
	// errRunsTrigger refuses a runs filter's trigger. Its message is
	// errRuleTrigger's, but empty means every trigger here.
	errRunsTrigger = &argError{msg: "trigger must be new, schedule or manual", hint: "leave trigger empty for every trigger"}
)

// rulesErr is the error to report for a failed rules store call. The
// store's refusals pass unchanged: their text is fixed. Anything else reads
// as fixed text and keeps its cause, so its reason still maps (TIMEOUT for
// a deadline, else INTERNAL). The cause can quote rule text, so the log
// line records only its type.
func (s *Server) rulesErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*rules.InvalidError](err); ok {
		return err
	}
	if errors.Is(err, rules.ErrNotFound) {
		return err
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.opts.Logger.Error("a rules store call failed", "type", fmt.Sprintf("%T", err))
	}
	return &fixedTextError{msg: "the rule store failed", err: err}
}

// ruleTarget is the audit target rule=<ref> for a rule ID or name, and the
// error when ref can be neither. Such a value is recorded as invalidTarget:
// it can be any bytes.
func ruleTarget(ref string) (string, error) {
	if !rules.ValidID(ref) && !rules.ValidName(ref) {
		return "rule=" + invalidTarget, errRuleRef
	}
	return "rule=" + ref, nil
}

// ruleMaskFields checks an UpdateRule mask's paths and returns them without
// repeats, in mask order, with their audit target. A path that is not one
// of rulePaths fails the whole mask and is never recorded.
func ruleMaskFields(paths []string) ([]string, string, error) {
	if len(paths) == 0 {
		return nil, "fields=", errRuleMask
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if !slices.Contains(rulePaths, p) {
			return nil, "fields=" + invalidTarget, errRulePath
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out, "fields=" + strings.Join(out, ","), nil
}

// parseTrigger checks a trigger name. Empty is empty's value: manual for a
// dry run, every trigger for a runs filter. Any other name is refused
// with refuse, whose hint should fit empty: errRuleTrigger for a dry run,
// errRunsTrigger for a runs filter.
func parseTrigger(s string, empty rules.Trigger, refuse error) (rules.Trigger, error) {
	if s == "" {
		return empty, nil
	}
	switch t := rules.Trigger(s); t {
	case rules.TriggerNew, rules.TriggerSchedule, rules.TriggerManual:
		return t, nil
	}
	return "", refuse
}

// parseOutcome checks a runs filter's outcome. Empty means every outcome.
func parseOutcome(s string) (rules.Outcome, error) {
	switch o := rules.Outcome(s); o {
	case "", rules.OutcomeOK, rules.OutcomeCapped, rules.OutcomeFailed, rules.OutcomeCacheNotReady, rules.OutcomeInterrupted:
		return o, nil
	}
	return "", errRuleOutcome
}

// rulesService implements chainmailv1connect.RulesServiceHandler. Every
// call is audited under the caller's own client kind, and no audit target,
// error message, hint or log line holds a description, query, folder,
// label, forward address or note: only IDs, names, field names, accounts,
// kinds and counts.
type rulesService struct {
	s *Server
}

var _ chainmailv1connect.RulesServiceHandler = (*rulesService)(nil)

// ruleCall runs one rule call the way every handler of the service does:
// it identifies the caller, gives the call timeout unless the client set a
// deadline, and runs run, which sets the audit entry's targets as it
// validates. It then records the entry, so refusals are audited too.
func ruleCall[Req, Resp any](ctx context.Context, s *Server, req *connect.Request[Req], action string, timeout time.Duration,
	run func(ctx context.Context, c caller, entry *audit.Entry) (*Resp, error),
) (*connect.Response[Resp], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, timeout)
	defer cancel()
	entry := audit.Entry{Action: action}
	resp, runErr := run(ctx, c, &entry)
	if err := s.record(ctx, c, entry, runErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// pokeRuleScheduler wakes the rule scheduler so it plans again. It never
// blocks: one pending wakeup covers any number of changes.
func (s *Server) pokeRuleScheduler() {
	select {
	case s.rulesWake <- struct{}{}:
	default:
	}
}

// checkLinkItems refuses link_item actions whose item is unknown or
// deleted. It reads the items store, so it runs outside any rules store
// transaction.
func (s *Server) checkLinkItems(ctx context.Context, acts []rules.Action) error {
	for _, a := range acts {
		if a.Kind != rules.ActLinkItem {
			continue
		}
		it, err := s.items.Get(ctx, a.ItemID)
		switch {
		case errors.Is(err, items.ErrNotFound):
			return errLinkItemUnknown
		case err != nil:
			return s.itemsErr(err)
		case !it.DeletedAt.IsZero():
			return errLinkItemDeleted
		}
	}
	return nil
}

// CreateRule implements chainmailv1connect.RulesServiceHandler. It is
// audited as rule.create with the new rule's ID and its number of actions.
func (r *rulesService) CreateRule(ctx context.Context, req *connect.Request[chainmailv1.CreateRuleRequest]) (*connect.Response[chainmailv1.CreateRuleResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.create", callTimeout, func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.CreateRuleResponse, error) {
		entry.Targets = []string{tally("actions", len(req.Msg.GetRule().GetActions()))}
		d, err := definitionOf(req.Msg.GetRule())
		if err != nil {
			return nil, err
		}
		if err := d.Validate(r.s.ruleEnv()); err != nil {
			return nil, err
		}
		if err := r.s.checkLinkItems(ctx, d.Actions); err != nil {
			return nil, err
		}
		rule, err := r.s.rules.Create(ctx, actorOf(c), d)
		if err != nil {
			return nil, r.s.rulesErr(err)
		}
		entry.Targets = []string{"rule=" + rule.ID, tally("actions", len(rule.Actions))}
		r.s.pokeRuleScheduler()
		return &chainmailv1.CreateRuleResponse{Rule: ruleProto(rule)}, nil
	})
}

// UpdateRule implements chainmailv1connect.RulesServiceHandler. It changes
// the fields update_mask names, refusing a stale expected_revision, and is
// audited as rule.update with the rule, the field names and on success the
// new revision. The store may run the check inside its transaction, so the
// check only validates; link_item items are checked before.
func (r *rulesService) UpdateRule(ctx context.Context, req *connect.Request[chainmailv1.UpdateRuleRequest]) (*connect.Response[chainmailv1.UpdateRuleResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.update", callTimeout, func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.UpdateRuleResponse, error) {
		m := req.Msg
		target, refErr := ruleTarget(m.GetRule())
		fields, fieldsTarget, maskErr := ruleMaskFields(m.GetUpdateMask().GetPaths())
		entry.Targets = []string{target, fieldsTarget}
		if err := cmp.Or(refErr, maskErr); err != nil {
			return nil, err
		}
		p, err := patchOf(m.GetPatch(), fields, m.ExpectedRevision)
		if err != nil {
			return nil, err
		}
		if p.Actions != nil {
			if err := r.s.checkLinkItems(ctx, *p.Actions); err != nil {
				return nil, err
			}
		}
		env := r.s.ruleEnv()
		rule, err := r.s.rules.Update(ctx, actorOf(c), m.GetRule(), p, func(d rules.Definition) error { return d.Validate(env) })
		if err != nil {
			return nil, r.s.rulesErr(err)
		}
		entry.Targets = []string{"rule=" + rule.ID, fieldsTarget, fmt.Sprintf("revision=%d", rule.Revision)}
		r.s.pokeRuleScheduler()
		return &chainmailv1.UpdateRuleResponse{Rule: ruleProto(rule)}, nil
	})
}

// DeleteRule implements chainmailv1connect.RulesServiceHandler. Deleting a
// deleted rule (by ID) changes nothing. Audited as rule.delete.
func (r *rulesService) DeleteRule(ctx context.Context, req *connect.Request[chainmailv1.DeleteRuleRequest]) (*connect.Response[chainmailv1.DeleteRuleResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.delete", callTimeout, func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.DeleteRuleResponse, error) {
		rule, err := r.ruleLifecycle(ctx, c, entry, req.Msg.GetRule(), r.s.rules.Delete)
		if err != nil {
			return nil, err
		}
		return &chainmailv1.DeleteRuleResponse{Rule: rule}, nil
	})
}

// RestoreRule implements chainmailv1connect.RulesServiceHandler. A live
// rule, or one whose name another live rule took, is refused. Audited as
// rule.restore.
func (r *rulesService) RestoreRule(ctx context.Context, req *connect.Request[chainmailv1.RestoreRuleRequest]) (*connect.Response[chainmailv1.RestoreRuleResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.restore", callTimeout, func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.RestoreRuleResponse, error) {
		rule, err := r.ruleLifecycle(ctx, c, entry, req.Msg.GetRule(), r.s.rules.Restore)
		if err != nil {
			return nil, err
		}
		return &chainmailv1.RestoreRuleResponse{Rule: rule}, nil
	})
}

// ruleLifecycle runs a delete or a restore of the rule ref names, and sets
// the entry's targets: the rule, then on success its revision.
func (r *rulesService) ruleLifecycle(ctx context.Context, c caller, entry *audit.Entry, ref string,
	op func(context.Context, items.Actor, string) (rules.Rule, error),
) (*chainmailv1.Rule, error) {
	target, err := ruleTarget(ref)
	entry.Targets = []string{target}
	if err != nil {
		return nil, err
	}
	rule, err := op(ctx, actorOf(c), ref)
	if err != nil {
		return nil, r.s.rulesErr(err)
	}
	entry.Targets = []string{"rule=" + rule.ID, fmt.Sprintf("revision=%d", rule.Revision)}
	r.s.pokeRuleScheduler()
	return ruleProto(rule), nil
}

// ruleAccountArg checks a request's optional account: empty passes, an
// invalid name is INVALID_ARGUMENT and an unknown one NOT_FOUND.
func (s *Server) ruleAccountArg(name string) error {
	if name == "" {
		return nil
	}
	_, err := s.resolveAccounts([]string{name})
	return err
}

// GetRule implements chainmailv1connect.RulesServiceHandler: the rule, its
// last runs, its counts over the retained runs and, with history, its
// revisions newest first. Audited as rule.get.
func (r *rulesService) GetRule(ctx context.Context, req *connect.Request[chainmailv1.GetRuleRequest]) (*connect.Response[chainmailv1.GetRuleResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.get", callTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.GetRuleResponse, error) {
		m := req.Msg
		target, err := ruleTarget(m.GetRule())
		entry.Targets = []string{target}
		if m.GetHistory() {
			entry.Targets = append(entry.Targets, "history=1")
		}
		if err != nil {
			return nil, err
		}
		rule, err := r.s.rules.Get(ctx, m.GetRule())
		if err != nil {
			return nil, r.s.rulesErr(err)
		}
		entry.Targets[0] = "rule=" + rule.ID
		runs, err := r.s.rules.RecentRuns(ctx, rule.ID, ruleRunsShown)
		if err != nil {
			return nil, r.s.rulesErr(err)
		}
		counts, err := r.s.rules.Counts(ctx, rule.ID)
		if err != nil {
			return nil, r.s.rulesErr(err)
		}
		resp := &chainmailv1.GetRuleResponse{Rule: ruleProto(rule), Runs: ruleRunsProto(runs), Counts: ruleCountsProto(counts)}
		if m.GetHistory() {
			revs, err := r.s.rules.Revisions(ctx, rule.ID, ruleRevisionsShown)
			if err != nil {
				return nil, r.s.rulesErr(err)
			}
			resp.Revisions = ruleRevisionsProto(revs)
		}
		return resp, nil
	})
}

// ListRules implements chainmailv1connect.RulesServiceHandler. Rules come
// in priority, then ID, order. Audited as rule.list with the set filters
// and the number of rows; a valid account filter is the entry's account.
func (r *rulesService) ListRules(ctx context.Context, req *connect.Request[chainmailv1.ListRulesRequest]) (*connect.Response[chainmailv1.ListRulesResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.list", callTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.ListRulesResponse, error) {
		m := req.Msg
		accountErr := r.s.ruleAccountArg(m.GetAccount())
		if accountErr == nil {
			entry.Account = m.GetAccount()
		}
		if m.Enabled != nil {
			entry.Targets = append(entry.Targets, fmt.Sprintf("enabled=%t", m.GetEnabled()))
		}
		if m.GetDeleted() {
			entry.Targets = append(entry.Targets, "deleted=1")
		}
		if accountErr != nil {
			return nil, accountErr
		}
		list, err := r.s.rules.List(ctx, rules.ListQuery{Account: m.GetAccount(), Enabled: m.Enabled, Deleted: m.GetDeleted()})
		if err != nil {
			return nil, r.s.rulesErr(err)
		}
		entry.Targets = append(entry.Targets, tally("rows", len(list)))
		resp := &chainmailv1.ListRulesResponse{Rules: make([]*chainmailv1.Rule, 0, len(list))}
		for _, rule := range list {
			resp.Rules = append(resp.Rules, ruleProto(rule))
		}
		return resp, nil
	})
}

// ruleRunAccounts is the accounts a run of d acts on: every
// account it covers, or only account when set, which must be one of them.
func (s *Server) ruleRunAccounts(d rules.Definition, account string) ([]config.ResolvedAccount, error) {
	accts, err := s.ruleAccounts(d)
	if err != nil {
		return nil, err
	}
	if account == "" {
		return accts, nil
	}
	for _, a := range accts {
		if a.Name == account {
			return []config.ResolvedAccount{a}, nil
		}
	}
	return nil, errRuleAccount
}

// dryRunSubject is what a dry run matches: a saved rule's ID and
// definition, or an unsaved definition, validated, with no ID.
func (s *Server) dryRunSubject(ctx context.Context, m *chainmailv1.DryRunRuleRequest) (string, rules.Definition, error) {
	if m.GetRule() != "" {
		rule, err := s.rules.Get(ctx, m.GetRule())
		if err != nil {
			return "", rules.Definition{}, s.rulesErr(err)
		}
		return rule.ID, rule.Definition, nil
	}
	d, err := definitionOf(m.GetDefinition())
	if err != nil {
		return "", rules.Definition{}, err
	}
	if err := d.Validate(s.ruleEnv()); err != nil {
		return "", rules.Definition{}, err
	}
	return "", d, nil
}

// DryRunRule implements chainmailv1connect.RulesServiceHandler. It matches
// a saved rule, or an unsaved definition, without acting or writing. A
// deleted saved rule can be dry-run by ID, to see what it would do before
// it is restored; RunRule refuses one, since a run acts.
// Audited as rule.dry_run with the rule or "unsaved", the trigger, and the
// number of accounts and matches.
func (r *rulesService) DryRunRule(ctx context.Context, req *connect.Request[chainmailv1.DryRunRuleRequest]) (*connect.Response[chainmailv1.DryRunRuleResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.dry_run", runRuleTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.DryRunRuleResponse, error) {
		m := req.Msg
		var (
			target string
			refErr error
		)
		switch {
		case (m.GetRule() == "") == (m.GetDefinition() == nil):
			target, refErr = "rule="+invalidTarget, errDryRunSource
		case m.GetRule() != "":
			target, refErr = ruleTarget(m.GetRule())
		default:
			target = "rule=unsaved"
		}
		trigger, trigErr := parseTrigger(m.GetTrigger(), rules.TriggerManual, errRuleTrigger)
		accountErr := r.s.ruleAccountArg(m.GetAccount())
		if accountErr == nil {
			entry.Account = m.GetAccount()
		}
		entry.Targets = []string{target, enumTarget("trigger", trigger, trigErr)}
		if err := cmp.Or(refErr, trigErr, accountErr); err != nil {
			return nil, err
		}
		ruleID, d, err := r.s.dryRunSubject(ctx, m)
		if err != nil {
			return nil, err
		}
		if ruleID != "" {
			entry.Targets[0] = "rule=" + ruleID
		}
		resp, err := r.s.dryRun(ctx, ruleID, d, trigger, m.GetAccount())
		if err != nil {
			return nil, err
		}
		var matched int64
		for _, a := range resp.GetAccounts() {
			matched += a.GetMatched()
		}
		entry.Targets = append(entry.Targets, tally("accounts", len(resp.GetAccounts())), fmt.Sprintf("matched=%d", matched))
		return resp, nil
	})
}

// RunRule implements chainmailv1connect.RulesServiceHandler. It runs a
// live rule once on every account it covers, or the one named, through
// each account's runner, within 10 minutes unless the client set a
// deadline. A disabled rule runs, since the call is explicit. If any
// account's run fails, the call returns the first failure in account
// order and no runs, although the other accounts' runs are recorded and
// may have acted: ListRuleRuns shows them, and the audit entry counts them.
// Audited as rule.run with the rule, max and the number of runs.
func (r *rulesService) RunRule(ctx context.Context, req *connect.Request[chainmailv1.RunRuleRequest]) (*connect.Response[chainmailv1.RunRuleResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.run", runRuleTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.RunRuleResponse, error) {
		m := req.Msg
		target, refErr := ruleTarget(m.GetRule())
		limit := int(m.GetMax())
		var maxErr error
		if limit < 0 || limit > rules.MaxMaxPerRun {
			maxErr = errRuleMax
		}
		accountErr := r.s.ruleAccountArg(m.GetAccount())
		if accountErr == nil {
			entry.Account = m.GetAccount()
		}
		entry.Targets = []string{target, tally("max", limit)}
		if err := cmp.Or(refErr, maxErr, accountErr); err != nil {
			return nil, err
		}
		rule, err := r.s.rules.Get(ctx, m.GetRule())
		if err != nil {
			return nil, r.s.rulesErr(err)
		}
		entry.Targets[0] = "rule=" + rule.ID
		if !rule.DeletedAt.IsZero() {
			return nil, errRuleDeleted
		}
		accts, err := r.s.ruleRunAccounts(rule.Definition, m.GetAccount())
		if err != nil {
			return nil, err
		}
		runs, err := r.s.runOnAccounts(ctx, rule, accts, rules.TriggerManual, limit)
		entry.Targets = append(entry.Targets, tally("runs", len(runs)))
		if err != nil {
			return nil, err
		}
		return &chainmailv1.RunRuleResponse{Runs: ruleRunsProto(runs)}, nil
	})
}

// ListRuleRuns implements chainmailv1connect.RulesServiceHandler: runs
// newest first, paged by before_id. Audited as rule.runs with the set
// filters and the number of rows.
func (r *rulesService) ListRuleRuns(ctx context.Context, req *connect.Request[chainmailv1.ListRuleRunsRequest]) (*connect.Response[chainmailv1.ListRuleRunsResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.runs", callTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.ListRuleRunsResponse, error) {
		m := req.Msg
		var refErr error
		if m.GetRule() != "" {
			var target string
			target, refErr = ruleTarget(m.GetRule())
			entry.Targets = append(entry.Targets, target)
		}
		trigger, trigErr := parseTrigger(m.GetTrigger(), "", errRunsTrigger)
		if m.GetTrigger() != "" {
			entry.Targets = append(entry.Targets, enumTarget("trigger", trigger, trigErr))
		}
		outcome, outErr := parseOutcome(m.GetOutcome())
		if m.GetOutcome() != "" {
			entry.Targets = append(entry.Targets, enumTarget("outcome", outcome, outErr))
		}
		var limitErr, beforeErr error
		if l := m.GetLimit(); l < 0 || l > maxRuleRunsLimit {
			limitErr = errRuleRunsLimit
		}
		if m.GetBeforeId() < 0 {
			beforeErr = errRuleBeforeID
		}
		accountErr := r.s.ruleAccountArg(m.GetAccount())
		if accountErr == nil {
			entry.Account = m.GetAccount()
		}
		if err := cmp.Or(refErr, accountErr, trigErr, outErr, limitErr, beforeErr); err != nil {
			return nil, err
		}
		q := rules.RunQuery{Account: m.GetAccount(), Trigger: trigger, Outcome: outcome, Limit: int(m.GetLimit()), BeforeID: m.GetBeforeId()}
		if m.GetRule() != "" {
			rule, err := r.s.rules.Get(ctx, m.GetRule())
			if err != nil {
				return nil, r.s.rulesErr(err)
			}
			entry.Targets[0] = "rule=" + rule.ID
			q.RuleID = rule.ID
		}
		runs, next, err := r.s.rules.ListRuns(ctx, q)
		if err != nil {
			return nil, r.s.rulesErr(err)
		}
		entry.Targets = append(entry.Targets, tally("rows", len(runs)))
		return &chainmailv1.ListRuleRunsResponse{Runs: ruleRunsProto(runs), NextBeforeId: next}, nil
	})
}

// RulesStatus implements chainmailv1connect.RulesServiceHandler. Audited
// as rule.status with the number of runners reported. The scheduler starts
// with watch, so while watch is off (DisableWatch, or no watched account)
// scheduled rules do not run and next_scheduled_at can lie in the past;
// RunRule still runs a rule by hand.
func (r *rulesService) RulesStatus(ctx context.Context, req *connect.Request[chainmailv1.RulesStatusRequest]) (*connect.Response[chainmailv1.RulesStatusResponse], error) {
	return ruleCall(ctx, r.s, req, "rule.status", callTimeout, func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.RulesStatusResponse, error) {
		resp, err := r.s.rulesStatus(ctx)
		if err != nil {
			return nil, err
		}
		entry.Targets = []string{tally("runners", len(resp.GetRunners()))}
		return resp, nil
	})
}

// rulesStatus reports every configured account in name order: its
// runner's state, acknowledged position and lag behind the log's tail, or
// cache_disabled for an account with no runner; then the next scheduled
// run and the due checker's next wake.
func (s *Server) rulesStatus(ctx context.Context) (*chainmailv1.RulesStatusResponse, error) {
	tail, err := s.watchLog.Tail(ctx)
	if err != nil {
		return nil, s.watchLogErr(err)
	}
	names := s.opts.Config.AccountNames()
	resp := &chainmailv1.RulesStatusResponse{Runners: make([]*chainmailv1.RulesRunnerStatus, 0, len(names))}
	for _, name := range names {
		st := &chainmailv1.RulesRunnerStatus{Account: name, State: runnerCacheDisabled}
		if r := s.ruleRunners[name]; r != nil {
			rs := r.status()
			st.State, st.AckedId, st.Lag, st.Reason = rs.State, rs.AckedID, runnerLag(tail, rs.AckedID), rs.Reason
		}
		resp.Runners = append(resp.Runners, st)
	}
	next, ok, err := s.rules.NextScheduled(ctx)
	if err != nil {
		return nil, s.rulesErr(err)
	}
	if ok {
		resp.NextScheduledAt = timestamp(next)
	}
	resp.NextDueCheckAt = timestamp(s.dueChecker.NextWake())
	return resp, nil
}

// runnerLag is how many events lie past a runner's acknowledged position,
// 0 before it has one.
func runnerLag(tail, acked int64) int64 {
	if acked < 0 || tail <= acked {
		return 0
	}
	return tail - acked
}
