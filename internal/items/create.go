package items

import (
	"context"
	"database/sql"
	"math"

	"github.com/jamesainslie/chainmail/internal/watch"
)

// Item fields, as UpdateItem mask paths and as the field names item events list.
const (
	fieldType        = "type"
	fieldTitle       = "title"
	fieldDescription = "description"
	fieldState       = "state"
	fieldArea        = "area"
	fieldPriority    = "priority"
	fieldStartDate   = "start_date"
	fieldDueDate     = "due_date"
	fieldEstimate    = "estimate"
	fieldParent      = "parent_id"
)

// itemFields lists every field, in the order UpdateItemRequest documents.
var itemFields = []string{
	fieldType, fieldTitle, fieldDescription, fieldState, fieldArea,
	fieldPriority, fieldStartDate, fieldDueDate, fieldEstimate, fieldParent,
}

// fieldsHint names the fields an update may change.
const fieldsHint = "fields are type, title, description, state, area, priority, start_date, due_date, estimate and parent_id"

// parentRuleHint states the parent rule; every placement refusal carries it.
const parentRuleHint = "a parent ranks above its child (objective, key-result, initiative, epic, feature, story); " +
	"bug and support-ticket sit under a feature or above; a task sits under any of those, or in the inbox with no parent"

// Refusals of create and placement. Their text never holds request values.
var (
	errUnknownField  = &InvalidError{Msg: "unknown field", Hint: fieldsHint}
	errBadEstimate   = &InvalidError{Msg: "estimate must be a number zero or more", Hint: "give a plain non-negative number, or clear the estimate"}
	errBadKey        = &InvalidError{Msg: "idempotency_key must be 1 to 128 characters of A-Z, a-z, 0-9, '.', '_', ':' or '-'", Hint: "use a fresh random key for each new item"}
	errNeedsParent   = &InvalidError{Msg: "this type needs a parent", Hint: parentRuleHint}
	errRootArea      = &InvalidError{Msg: "a root item needs an area", Hint: "set area to work or personal"}
	errParentDeleted = &InvalidError{Msg: "the parent is deleted", Hint: "restore the parent first, or choose another"}
	errParentRule    = &InvalidError{Msg: "the parent's type cannot hold this type", Hint: parentRuleHint}
	errDoneUnticked  = &InvalidError{Msg: "the item has unticked criteria", Hint: "tick every criterion with evidence before setting state done"}
)

// NewItem is a request for a new item. Title, Description and Criteria are
// untrusted text. An empty State means backlog. An empty ParentID puts the
// item at the root, which needs an Area; a task at the root is in the inbox.
type NewItem struct {
	Type               Type
	Title, Description string
	State              State
	Area               Area
	Priority           Priority
	StartDate, DueDate string
	Estimate           *float64
	ParentID           string
	Criteria           []string
	IdempotencyKey     string
}

// Create adds an item and its criteria as one revision with one created
// event, and returns them with duplicate false.
//
// With an IdempotencyKey, a request already made with that key returns the
// item it made, as it is now, with its criteria and duplicate true, and
// writes nothing. The same key with a different request is ErrKeyMismatch.
// Requests compare after cleaning, so a retry that differs only in spacing
// the cleaning removes is the same request. Keys are kept for
// RequestRetention.
func (s *Store) Create(ctx context.Context, a Actor, n NewItem) (Item, []Criterion, bool, error) {
	clean, err := cleanNewItem(n)
	if err != nil {
		return Item{}, nil, false, err
	}
	var hash string
	if clean.IdempotencyKey != "" {
		if hash, err = requestHash(clean); err != nil {
			return Item{}, nil, false, err
		}
	}
	now := s.now().UTC()
	var (
		it        Item
		crit      []Criterion
		duplicate bool
	)
	err = s.write(ctx, func(tx *sql.Tx, rev func() (int64, error)) ([]watch.Event, error) {
		if clean.IdempotencyKey != "" {
			id, found, err := lookupRequest(ctx, tx, clean.IdempotencyKey, hash)
			if err != nil {
				return nil, err
			}
			if found {
				duplicate = true
				it, crit, err = itemWithCriteria(ctx, tx, id)
				return nil, err
			}
		}
		f, err := loadForest(ctx, tx)
		if err != nil {
			return nil, err
		}
		if err := checkPlacement(f, clean.Type, clean.ParentID, clean.Area); err != nil {
			return nil, err
		}
		r, err := rev()
		if err != nil {
			return nil, err
		}
		actor := a.String()
		row := Item{
			ID: NewID("it_", now, s.rnd), Type: clean.Type, Title: clean.Title, Description: clean.Description,
			State: clean.State, Area: clean.Area, Priority: clean.Priority,
			StartDate: clean.StartDate, DueDate: clean.DueDate, Estimate: clean.Estimate, ParentID: clean.ParentID,
			Revision: r, CreatedAt: now, UpdatedAt: now, CreatedBy: actor, UpdatedBy: actor,
		}
		if err := insertItem(ctx, tx, row); err != nil {
			return nil, err
		}
		if err := putCriteria(ctx, tx, row.ID, newCriteria(clean.Criteria, 0, now, s.rnd)); err != nil {
			return nil, err
		}
		if clean.IdempotencyKey != "" {
			if err := putRequest(ctx, tx, clean.IdempotencyKey, hash, row.ID, now); err != nil {
				return nil, err
			}
		}
		if it, crit, err = itemWithCriteria(ctx, tx, row.ID); err != nil {
			return nil, err
		}
		return []watch.Event{itemEvent(now, row.ID, row.Type, r, OpCreated, nil)}, nil
	})
	if err != nil {
		return Item{}, nil, false, err
	}
	return it, crit, duplicate, nil
}

// cleanNewItem validates n and returns it cleaned: text trimmed, values
// parsed, State defaulted to backlog, an empty criteria list as nil.
func cleanNewItem(n NewItem) (NewItem, error) {
	if n.State == "" {
		n.State = StateBacklog
	}
	out, err := cleanFields(n, itemFields)
	if err != nil {
		return NewItem{}, err
	}
	for _, c := range n.Criteria {
		text, err := CleanCriterion(c)
		if err != nil {
			return NewItem{}, err
		}
		out.Criteria = append(out.Criteria, text)
	}
	if n.IdempotencyKey != "" && !ValidKey(n.IdempotencyKey) {
		return NewItem{}, errBadKey
	}
	out.IdempotencyKey = n.IdempotencyKey
	if out.State == StateDone && len(out.Criteria) > 0 {
		return NewItem{}, errDoneUnticked
	}
	return out, nil
}

// cleanFields validates the named fields of v and returns them cleaned in
// a NewItem; other fields stay zero. An empty value clears the field where
// the parser allows it.
func cleanFields(v NewItem, fields []string) (NewItem, error) {
	var out NewItem
	for _, field := range fields {
		var err error
		switch field {
		case fieldType:
			out.Type, err = ParseType(string(v.Type))
		case fieldTitle:
			out.Title, err = CleanTitle(v.Title)
		case fieldDescription:
			out.Description, err = CleanDescription(v.Description)
		case fieldState:
			out.State, err = ParseState(string(v.State))
		case fieldArea:
			out.Area, err = ParseArea(string(v.Area))
		case fieldPriority:
			out.Priority, err = ParsePriority(string(v.Priority))
		case fieldStartDate:
			out.StartDate, err = ParseDate(v.StartDate)
		case fieldDueDate:
			out.DueDate, err = ParseDate(v.DueDate)
		case fieldEstimate:
			out.Estimate, err = cleanEstimate(v.Estimate)
		case fieldParent:
			out.ParentID = v.ParentID
		default:
			err = errUnknownField
		}
		if err != nil {
			return NewItem{}, err
		}
	}
	return out, nil
}

// cleanEstimate copies a finite, non-negative estimate. Nil stays nil.
func cleanEstimate(p *float64) (*float64, error) {
	if p == nil {
		return nil, nil
	}
	v := *p
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return nil, errBadEstimate
	}
	return &v, nil
}

// checkPlacement applies the parent rule to an item of type t under
// parentID, or at the root when parentID is empty, where it needs area.
func checkPlacement(f *forest, t Type, parentID string, area Area) error {
	if parentID == "" {
		switch {
		case !CanBeRoot(t):
			return errNeedsParent
		case area == "":
			return errRootArea
		}
		return nil
	}
	p, err := f.find(parentID, "parent")
	if err != nil {
		return err
	}
	if !p.live() {
		return errParentDeleted
	}
	if !CanParent(p.item.Type, t) {
		return errParentRule
	}
	return nil
}

// itemWithCriteria reads an item and its criteria through q.
func itemWithCriteria(ctx context.Context, q queryer, id string) (Item, []Criterion, error) {
	it, err := getItem(ctx, q, id)
	if err != nil {
		return Item{}, nil, err
	}
	crit, err := criteriaOf(ctx, q, id)
	if err != nil {
		return Item{}, nil, err
	}
	return it, crit, nil
}
