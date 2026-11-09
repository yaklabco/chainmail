package rules

import (
	"context"
	"database/sql"

	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/watch"
)

// Create stores a new rule from d, normalized, at revision 1, with a new
// ru_ ID, and plans its first scheduled run. It refuses a name a live rule
// holds and what the table cannot hold, and does not call Validate: the
// caller validates d against its accounts and policy first.
func (s *Store) Create(ctx context.Context, a items.Actor, d Definition) (Rule, error) {
	d = canonical(d)
	if err := checkStored(d); err != nil {
		return Rule{}, err
	}
	now := s.now().UTC()
	r := Rule{
		ID:         items.NewID(IDPrefix, now, s.rnd),
		Definition: d,
		Revision:   1,
		CreatedAt:  now,
		UpdatedAt:  now,
		CreatedBy:  a.String(),
		UpdatedBy:  a.String(),
		NextRunAt:  s.nextRun(d, now),
	}
	var out Rule
	err := s.write(ctx, func(tx *sql.Tx) ([]watch.Event, error) {
		taken, err := nameTaken(ctx, tx, d.Name, "")
		if err != nil {
			return nil, err
		}
		if taken {
			return nil, errNameTaken
		}
		if err := insertRule(ctx, tx, r); err != nil {
			return nil, err
		}
		var events []watch.Event
		out, events, err = recordChange(ctx, tx, r.ID, ChangeCreated, a.String(), "", now)
		return events, err
	})
	if err != nil {
		return Rule{}, err
	}
	return out, nil
}
