package rules

import (
	"context"
	"database/sql"
	"slices"

	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/watch"
)

// Refusals of updates. Their text never holds rule text.
var (
	errStaleRevision = &InvalidError{Msg: "the rule changed since it was read", Hint: "read the rule again and send the change against its current revision"}
	errRuleDeleted   = &InvalidError{Msg: "the rule is deleted", Hint: "restore it first: chainmail rule restore"}
)

// updateAttempts bounds how often Update reads again after a concurrent
// write to the same rule committed between its read and its write.
const updateAttempts = 3

// Patch names the fields to change: a nil field is left as stored. It
// carries the optimistic ExpectedRevision too.
type Patch struct {
	Name, Description, Query *string
	Accounts                 *[]string
	OnNew, Stop, Enabled     *bool
	Schedule                 *Schedule
	Priority, MaxPerRun      *int
	Actions                  *[]Action
	ExpectedRevision         *int64
}

// apply returns d with p's set fields in place of its own.
func (p Patch) apply(d Definition) Definition {
	if p.Name != nil {
		d.Name = *p.Name
	}
	if p.Description != nil {
		d.Description = *p.Description
	}
	if p.Query != nil {
		d.Query = *p.Query
	}
	if p.Accounts != nil {
		d.Accounts = *p.Accounts
	}
	if p.OnNew != nil {
		d.OnNew = *p.OnNew
	}
	if p.Stop != nil {
		d.Stop = *p.Stop
	}
	if p.Enabled != nil {
		d.Enabled = *p.Enabled
	}
	if p.Schedule != nil {
		d.Schedule = *p.Schedule
	}
	if p.Priority != nil {
		d.Priority = *p.Priority
	}
	if p.MaxPerRun != nil {
		d.MaxPerRun = *p.MaxPerRun
	}
	if p.Actions != nil {
		d.Actions = *p.Actions
	}
	return d
}

// Update applies p to the live rule, found by ID or live name, normalizes
// the result, and calls check on it before writing. It refuses a deleted
// rule, a stale ExpectedRevision, a name another live rule holds, and what
// the table cannot hold, and returns check's error unchanged. When no field
// changes it writes nothing, does not call check, and returns the stored
// rule. Turning Enabled on clears DisabledReason and FailedRuns. A change to
// Schedule or Enabled plans next_run_at again.
//
// check runs outside every transaction, so it may read state.db (for
// example to look up link_item items). The write re-reads the rule and goes
// ahead only when its revision has not moved since the read, so a
// concurrent change is never overwritten: Update reads, merges and checks
// again, at most updateAttempts times in all.
func (s *Store) Update(ctx context.Context, a items.Actor, rule string, p Patch, check func(Definition) error) (Rule, error) {
	for range updateAttempts {
		cur, err := s.Get(ctx, rule)
		if err != nil {
			return Rule{}, err
		}
		if !cur.DeletedAt.IsZero() {
			return Rule{}, errRuleDeleted
		}
		if p.ExpectedRevision != nil && *p.ExpectedRevision != cur.Revision {
			return Rule{}, errStaleRevision
		}
		next := canonical(p.apply(cur.Definition))
		if sameDefinition(cur.Definition, next) {
			return cur, nil
		}
		if err := checkStored(next); err != nil {
			return Rule{}, err
		}
		if check != nil {
			if err := check(next); err != nil {
				return Rule{}, err
			}
		}
		out, done, err := s.commitUpdate(ctx, a, cur, next)
		if err != nil {
			return Rule{}, err
		}
		if done {
			return out, nil
		}
		rule = cur.ID // a racing rename must not lose the rule
	}
	return Rule{}, errStaleRevision
}

// commitUpdate writes next over cur in one transaction when the stored rule
// is still live at cur's revision. done is false, with nothing written,
// when it is not.
func (s *Store) commitUpdate(ctx context.Context, a items.Actor, cur Rule, next Definition) (Rule, bool, error) {
	now := s.now().UTC()
	var (
		out  Rule
		done bool
	)
	err := s.write(ctx, func(tx *sql.Tx) ([]watch.Event, error) {
		stored, err := findRuleByID(ctx, tx, cur.ID)
		if err != nil {
			return nil, err
		}
		if stored.Revision != cur.Revision || !stored.DeletedAt.IsZero() {
			return nil, nil
		}
		if next.Name != stored.Name {
			taken, err := nameTaken(ctx, tx, next.Name, stored.ID)
			if err != nil {
				return nil, err
			}
			if taken {
				return nil, errNameTaken
			}
		}
		r := stored
		r.Definition = next
		if !stored.Enabled && next.Enabled {
			r.DisabledReason, r.FailedRuns = "", 0
		}
		if next.Schedule != stored.Schedule || next.Enabled != stored.Enabled {
			r.NextRunAt = s.nextRun(next, now)
		}
		r.Revision++
		r.UpdatedAt, r.UpdatedBy = now, a.String()
		var events []watch.Event
		out, events, err = commitChange(ctx, tx, r, changeOf(stored.Definition, next), a.String(), "", now)
		if err != nil {
			return nil, err
		}
		done = true
		return events, nil
	})
	if err != nil {
		return Rule{}, false, err
	}
	return out, done, nil
}

// sameDefinition reports whether a and b hold the same values.
func sameDefinition(a, b Definition) bool {
	return a.Name == b.Name && a.Description == b.Description && slices.Equal(a.Accounts, b.Accounts) &&
		a.Query == b.Query && a.OnNew == b.OnNew && a.Schedule == b.Schedule && a.Priority == b.Priority &&
		a.Stop == b.Stop && slices.EqualFunc(a.Actions, b.Actions, sameAction) &&
		a.MaxPerRun == b.MaxPerRun && a.Enabled == b.Enabled
}

// sameAction reports whether a and b are the same action with the same
// arguments.
func sameAction(a, b Action) bool {
	return a.Kind == b.Kind && a.Label == b.Label && a.Folder == b.Folder && a.ItemID == b.ItemID &&
		a.Anchor == b.Anchor && slices.Equal(a.To, b.To) && a.Note == b.Note
}

// changeOf is the change kind of an update from cur to next: enabled or
// disabled when Enabled is the only field that changed, updated otherwise.
func changeOf(cur, next Definition) string {
	if cur.Enabled == next.Enabled {
		return ChangeUpdated
	}
	only := cur
	only.Enabled = next.Enabled
	if !sameDefinition(only, next) {
		return ChangeUpdated
	}
	if next.Enabled {
		return ChangeEnabled
	}
	return ChangeDisabled
}
