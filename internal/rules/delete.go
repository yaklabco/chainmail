package rules

import (
	"context"
	"database/sql"
	"time"

	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/watch"
)

// Refusals of restore. Their text never holds rule text.
var (
	errRestoreNeedsID   = &InvalidError{Msg: "restore takes a rule ID", Hint: "find it with chainmail rule list --deleted"}
	errRuleLive         = &InvalidError{Msg: "the rule is not deleted", Hint: "only a deleted rule can be restored"}
	errRestoreNameTaken = &InvalidError{Msg: "a live rule has the deleted rule's name", Hint: "rename or delete the live rule, then restore this one"}
)

// Delete soft-deletes the rule, found by ID or live name, and returns it as
// stored. It stamps deleted_at, bumps the revision and clears next_run_at,
// so the scheduler never picks it. The rule's runs and act-once rows stay.
// Deleting a rule that is already deleted changes nothing and returns it.
func (s *Store) Delete(ctx context.Context, a items.Actor, rule string) (Rule, error) {
	now := s.now().UTC()
	var out Rule
	err := s.write(ctx, func(tx *sql.Tx) ([]watch.Event, error) {
		cur, err := findRule(ctx, tx, rule)
		if err != nil {
			return nil, err
		}
		if !cur.DeletedAt.IsZero() {
			out = cur
			return nil, nil
		}
		next := cur
		next.DeletedAt, next.NextRunAt = now, time.Time{}
		next.Revision++
		next.UpdatedAt, next.UpdatedBy = now, a.String()
		var events []watch.Event
		out, events, err = commitChange(ctx, tx, next, ChangeDeleted, a.String(), "", now)
		return events, err
	})
	if err != nil {
		return Rule{}, err
	}
	return out, nil
}

// Restore undeletes the rule with ID ruleID, bumps its revision and plans
// its next scheduled run. A deleted rule has no live name, so Restore takes
// an ID only and refuses any other value with errRestoreNeedsID. It refuses
// a live rule and a rule whose name a live rule now holds.
func (s *Store) Restore(ctx context.Context, a items.Actor, ruleID string) (Rule, error) {
	if !ValidID(ruleID) {
		return Rule{}, errRestoreNeedsID
	}
	now := s.now().UTC()
	var out Rule
	err := s.write(ctx, func(tx *sql.Tx) ([]watch.Event, error) {
		cur, err := findRuleByID(ctx, tx, ruleID)
		if err != nil {
			return nil, err
		}
		if cur.DeletedAt.IsZero() {
			return nil, errRuleLive
		}
		taken, err := nameTaken(ctx, tx, cur.Name, cur.ID)
		if err != nil {
			return nil, err
		}
		if taken {
			return nil, errRestoreNameTaken
		}
		next := cur
		next.DeletedAt = time.Time{}
		next.NextRunAt = s.nextRun(next.Definition, now)
		next.Revision++
		next.UpdatedAt, next.UpdatedBy = now, a.String()
		var events []watch.Event
		out, events, err = commitChange(ctx, tx, next, ChangeRestored, a.String(), "", now)
		return events, err
	})
	if err != nil {
		return Rule{}, err
	}
	return out, nil
}
