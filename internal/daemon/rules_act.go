package daemon

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/rules"
)

const (
	// flagFlagged is the flag the flag and unflag actions set and clear.
	flagFlagged = `\Flagged`
	// ruleActionTimeout bounds one action of a rule on one message, as
	// organizeTimeout bounds an organize call whose client set none.
	ruleActionTimeout = 2 * time.Minute
	// pendingForwardMaxAge is the age past which a replayed pending forward
	// is settled as outcome_unknown without asking the idempotency store,
	// which keeps keys for 7 days (spec amendment 8).
	pendingForwardMaxAge = 6 * 24 * time.Hour
	// unknownActionHint answers an action this daemon does not run.
	unknownActionHint = "the rule holds an action this daemon does not know; edit the rule's actions"
)

// actClass is how a message's actions ended (spec 5.5).
type actClass int

const (
	// actOK: every action ran.
	actOK actClass = iota
	// actHeld: every action ran, and the forward was held as a draft.
	actHeld
	// actTransient: an action failed in a way a retry can fix. The run
	// stops and its batch is read again.
	actTransient
	// actPermanent: an action failed for good. The message counts as
	// failed and is not retried on this trigger.
	actPermanent
	// actGone: the matched message itself was gone when an action ran, as
	// when another client moved or deleted it after the match.
	actGone
)

// actResult is what act did to one message. It never holds server or
// rule text: reasons are API reasons and hints are the daemon's own.
type actResult struct {
	class actClass
	// note goes on the message's rule_applied row: replayed or
	// outcome_unknown, or "".
	note string
	// newRef is the message's ref after a relocating action, "" when it
	// is unknown or unchanged.
	newRef string
	// reason and hint are a failure's API reason and fixed hint.
	reason string
	hint   string
	// copyRef is the ref of the copy a copy action made, "" when it is
	// unknown or the copy is the message itself, as on Google.
	copyRef string
	// unguardedCopy is the act-once key of the copy a copy action made
	// when writing the rule's row there failed right after the copy, ""
	// otherwise. The runner owes that row until it is saved.
	unguardedCopy string
	// clearPending says a permanent failure, or a gone message, left a
	// pending row that this call wrote before its guarded action ran. The
	// runner drops it, so a later run does not take the action as done.
	clearPending bool
}

// actionStep is what one action did.
type actionStep struct {
	class  actClass
	note   string
	newRef string
	reason string
	hint   string
	// err is the failure as the daemon saw it, for runAction to tell what
	// it was about. It never leaves the daemon.
	err error
}

// messageGone reports whether err, which ended an action of kind, says the
// matched message itself is gone: STALE_REF always does, as it is about
// the ref. NOT_FOUND does when nothing else the action names can be
// missing: a missing destination or role folder is a FolderNotFoundError
// and a missing item items.ErrNotFound, and a label, folder or role folder
// that an action names can be missing in ways the store does not type, so
// a plain NOT_FOUND from those actions stays a failure.
func messageGone(kind rules.ActionKind, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, mailstore.ErrStaleRef):
		return true
	case !errors.Is(err, mailstore.ErrNotFound):
		return false
	}
	if _, ok := errors.AsType[*mailstore.FolderNotFoundError](err); ok {
		return false
	}
	switch kind {
	case rules.ActFlag, rules.ActUnflag, rules.ActRead, rules.ActUnread, rules.ActForward, rules.ActLinkItem:
		return true
	default:
		return false
	}
}

// classifyReason is the class of an action that ended with reason, ""
// for none. The account being down, time running out and the cache or the
// daemon failing are transient. A held send is held. A send whose outcome
// is unknown counts as done, since it may have gone and must never be
// sent again. Every other reason, including one added later, is
// permanent, so a rule never retries what it does not understand.
func classifyReason(reason string) actClass {
	switch reason {
	case "", api.ReasonSendOutcomeUnknown:
		return actOK
	case api.ReasonHeld:
		return actHeld
	case api.ReasonServerUnavailable, api.ReasonTimeout, api.ReasonAuthRequired, api.ReasonAuthRejected,
		api.ReasonCacheNotReady, api.ReasonInternal:
		return actTransient
	default:
		return actPermanent
	}
}

// errDetail is err's API reason and hint: a Connect error's own, as the
// action paths return them after their audit write, else what
// toConnectError makes of err.
func errDetail(err error) *chainmailv1.ErrorDetail {
	if _, ok := errors.AsType[*connect.Error](err); ok {
		return api.Detail(err)
	}
	return api.Detail(toConnectError(err))
}

// messageKeyOf is the act-once key of the message at m on acct (spec
// amendment 3).
func messageKeyOf(acct config.ResolvedAccount, m ref.Ref) string {
	return rules.MessageKey(acct.Provider.Gmail, m.GmailMsgID, m.Folder, m.UIDValidity, m.UID)
}

// ruleTargets are the audit targets every action of a run appends.
func ruleTargets(ruleID string, runID int64) []string {
	return []string{"rule=" + ruleID, "run=" + strconv.FormatInt(runID, 10)}
}

// act runs rule's actions on row in order as ruleCaller(), each with the
// targets rule=<id> and run=<id>, and classifies the outcome. The first
// failure ends it. A guarded action (copy, forward) gets a pending
// rule_applied row first, written by act; the runner records the outcome.
func (s *Server) act(ctx context.Context, rule rules.Rule, runID int64, acct config.ResolvedAccount, row cache.Row) actResult {
	return s.actOn(ctx, rule, runID, acct, row, nil)
}

// actOn is act for a message whose rule_applied row is replay, a pending
// row an earlier attempt left, or nil when it has none. A replay writes no
// pending row, skips a copy (amendment 5), asks the idempotency store
// about a forward by calling it again with the same key, and skips a
// forward whose row is older than pendingForwardMaxAge (amendment 8).
func (s *Server) actOn(ctx context.Context, rule rules.Rule, runID int64, acct config.ResolvedAccount, row cache.Row, replay *rules.AppliedRow) actResult {
	if row.Account != acct.Name {
		return actResult{class: actPermanent, reason: api.ReasonInvalidArgument}
	}
	key := messageKeyOf(acct, row.Ref())
	cur := row.Ref().String()
	targets := ruleTargets(rule.ID, runID)
	var res actResult
	pendingWritten, guardDone := false, false
	for _, a := range rule.Actions {
		if a.Kind.Guarded() {
			if replay != nil {
				if note, skip := replaySkip(a, *replay, s.opts.Now()); skip {
					res.note = note
					guardDone = true
					continue
				}
			} else if !pendingWritten {
				if err := s.rules.MarkPending(ctx, rule.ID, acct.Name, key, runID); err != nil {
					d := errDetail(err)
					return actResult{class: actTransient, reason: d.GetReason(), hint: d.GetHint()}
				}
				pendingWritten = true
			}
		}
		st := s.runAction(ctx, rule.ID, a, acct, cur, key, targets)
		if st.class == actTransient || st.class == actPermanent || st.class == actGone {
			res.class, res.reason, res.hint = st.class, st.reason, st.hint
			res.clearPending = st.class != actTransient && pendingWritten && !guardDone
			return res
		}
		if a.Kind.Guarded() {
			guardDone = true
		}
		if st.class == actHeld {
			res.class = actHeld
		}
		if st.note != "" {
			res.note = st.note
		}
		switch {
		case a.Kind == rules.ActCopy:
			res.copyRef = st.newRef
			res.unguardedCopy = s.guardCopy(ctx, rule.ID, runID, acct, key, st.newRef)
		case a.Kind.Relocates():
			res.newRef = st.newRef
		}
	}
	if replay != nil && res.note == "" {
		res.note = rules.NoteReplayed
	}
	return res
}

// guardCopy writes rule's act-once row at the key of the copy a copy
// action just made, in its own transaction, before the next action runs.
// A later action that fails, or a crash, then cannot leave the copy
// unguarded: the replay skips the copy (amendment 5) without learning its
// ref, so without this row the copy's own new event would be copied again.
// A failed write is logged and returns the copy's key, which the runner
// owes until the row is saved; it returns "" otherwise.
func (s *Server) guardCopy(ctx context.Context, ruleID string, runID int64, acct config.ResolvedAccount, key, copyRef string) string {
	alias := aliasOf(acct, copyRef, key)
	if alias == "" {
		return ""
	}
	if err := s.rules.MarkApplied(ctx, ruleID, acct.Name, alias, runID, rules.AppliedDone, ""); err != nil {
		s.opts.Logger.WarnContext(ctx, "a rule could not guard the copy it made yet; the runner owes the write",
			"rule", ruleID, "account", acct.Name, "run", runID, "err", err)
		return alias
	}
	return ""
}

// replaySkip reports whether a replay of the pending row prev skips the
// guarded action a, with the note to record: a copy is never made twice,
// and a forward older than the idempotency store's keys may have gone.
func replaySkip(a rules.Action, prev rules.AppliedRow, now time.Time) (string, bool) {
	switch {
	case a.Kind == rules.ActCopy:
		return rules.NoteReplayed, true
	case a.Kind == rules.ActForward && now.Sub(prev.At) > pendingForwardMaxAge:
		return rules.NoteOutcomeUnknown, true
	default:
		return "", false
	}
}

// runAction runs one action on the message at cur through the daemon's
// own organize, items or compose path, within ruleActionTimeout. A
// permanent failure that says the message itself is gone (messageGone)
// is actGone instead.
func (s *Server) runAction(ctx context.Context, ruleID string, a rules.Action, acct config.ResolvedAccount, cur, key string, targets []string) actionStep {
	st := s.doAction(ctx, ruleID, a, acct, cur, key, targets)
	if st.class == actPermanent && messageGone(a.Kind, st.err) {
		st.class = actGone
	}
	return st
}

// doAction is runAction before a gone message is told apart.
func (s *Server) doAction(ctx context.Context, ruleID string, a rules.Action, acct config.ResolvedAccount, cur, key string, targets []string) actionStep {
	ctx, cancel := context.WithTimeout(ctx, ruleActionTimeout)
	defer cancel()
	refs := []string{cur}
	switch a.Kind {
	case rules.ActLabel:
		return s.organizeStep(ctx, labelsCall(refs, []string{a.Label}, nil), targets)
	case rules.ActUnlabel:
		return s.organizeStep(ctx, labelsCall(refs, nil, []string{a.Label}), targets)
	case rules.ActCopy:
		return s.organizeStep(ctx, copyCall(refs, a.Folder), targets)
	case rules.ActFlag:
		return s.organizeStep(ctx, flagsCall(refs, []string{flagFlagged}, nil), targets)
	case rules.ActUnflag:
		return s.organizeStep(ctx, flagsCall(refs, nil, []string{flagFlagged}), targets)
	case rules.ActRead:
		return s.organizeStep(ctx, flagsCall(refs, []string{flagSeen}, nil), targets)
	case rules.ActUnread:
		return s.organizeStep(ctx, flagsCall(refs, nil, []string{flagSeen}), targets)
	case rules.ActMove:
		return s.organizeStep(ctx, moveCall(refs, a.Folder), targets)
	case rules.ActArchive:
		return s.organizeStep(ctx, archiveCall(refs), targets)
	case rules.ActTrash:
		return s.organizeStep(ctx, trashCall(refs), targets)
	case rules.ActLinkItem:
		_, err := s.linkThreadAs(ctx, ruleCaller(), &chainmailv1.LinkThreadRequest{ItemId: a.ItemID, Ref: cur, Anchor: a.Anchor}, slices.Clone(targets))
		return stepOf(err)
	case rules.ActForward:
		return s.forwardStep(ctx, ruleID, a, acct, cur, key, targets)
	default:
		return actionStep{class: actPermanent, reason: api.ReasonInvalidArgument, hint: unknownActionHint}
	}
}

// organizeStep runs a one-ref organize call as ruleCaller() with the rule
// targets after the call's own, and reads its one outcome. A move or copy
// keeps its validated folder= target, as socket callers' rows do.
func (s *Server) organizeStep(ctx context.Context, c batchCall, targets []string) actionStep {
	c.targets = slices.Concat(c.targets, targets)
	outs, errs, err := (&organizeService{s: s}).batchErrsAs(ctx, ruleCaller(), c)
	if err != nil {
		return stepOf(err)
	}
	if len(outs) != 1 || len(errs) != 1 {
		return actionStep{class: actTransient, reason: api.ReasonInternal}
	}
	if e := outs[0].GetError(); e != nil {
		return actionStep{class: classifyReason(e.GetReason()), reason: e.GetReason(), hint: e.GetHint(), err: errs[0]}
	}
	return actionStep{newRef: outs[0].GetNewRef()}
}

// forwardStep forwards the message at cur to the action's recipients, with
// its note as plain text, under the rule's idempotency key for the
// message, so a replay returns the first attempt's outcome. A held forward
// is held; one whose outcome is unknown counts as done with a note.
func (s *Server) forwardStep(ctx context.Context, ruleID string, a rules.Action, acct config.ResolvedAccount, cur, key string, targets []string) actionStep {
	req := &chainmailv1.ForwardRequest{
		Ref:            cur,
		To:             slices.Clone(a.To),
		IdempotencyKey: rules.ForwardKey(ruleID, acct.Name, key),
	}
	if a.Note != "" {
		req.Content = &chainmailv1.Content{Body: &chainmailv1.ComposeBody{Kind: &chainmailv1.ComposeBody_Text{Text: a.Note}}}
	}
	res, err := s.compose.forwardAs(ctx, ruleCaller(), req, slices.Clone(targets))
	if err != nil {
		if errDetail(err).GetReason() == api.ReasonSendOutcomeUnknown {
			return actionStep{note: rules.NoteOutcomeUnknown}
		}
		return stepOf(err)
	}
	if res.GetHeld() != nil {
		return actionStep{class: actHeld}
	}
	return actionStep{}
}

// stepOf is the step of an action that ended with err, nil for success.
func stepOf(err error) actionStep {
	if err == nil {
		return actionStep{}
	}
	d := errDetail(err)
	return actionStep{class: classifyReason(d.GetReason()), reason: d.GetReason(), hint: d.GetHint(), err: err}
}
