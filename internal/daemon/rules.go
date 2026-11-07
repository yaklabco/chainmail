package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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
// dry run, every trigger for a runs filter.
func parseTrigger(s string, empty rules.Trigger) (rules.Trigger, error) {
	if s == "" {
		return empty, nil
	}
	switch t := rules.Trigger(s); t {
	case rules.TriggerNew, rules.TriggerSchedule, rules.TriggerManual:
		return t, nil
	}
	return "", errRuleTrigger
}

// parseOutcome checks a runs filter's outcome. Empty means every outcome.
func parseOutcome(s string) (rules.Outcome, error) {
	switch o := rules.Outcome(s); o {
	case "", rules.OutcomeOK, rules.OutcomeCapped, rules.OutcomeFailed, rules.OutcomeCacheNotReady, rules.OutcomeInterrupted:
		return o, nil
	}
	return "", errRuleOutcome
}
