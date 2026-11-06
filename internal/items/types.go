package items

import "time"

// Type is the kind of an item, from the Lathe roadmap hierarchy plus task.
type Type string

// The item types, from the top of the hierarchy down. Bug, support-ticket
// and task are leaves.
const (
	TypeObjective     Type = "objective"
	TypeKeyResult     Type = "key-result"
	TypeInitiative    Type = "initiative"
	TypeEpic          Type = "epic"
	TypeFeature       Type = "feature"
	TypeStory         Type = "story"
	TypeBug           Type = "bug"
	TypeSupportTicket Type = "support-ticket"
	TypeTask          Type = "task"
)

// Hints for values the parse functions refuse. They list the values a
// request may send, never what it sent.
const (
	hintTypes      = "types: objective, key-result, initiative, epic, feature, story, bug, support-ticket, task"
	hintStates     = "states: backlog, in-progress, in-review, done"
	hintAreas      = "areas: work, personal; leave it empty to inherit the parent's"
	hintPriorities = "priorities: p0, p1, p2, p3; leave it empty to inherit the parent's"
	hintDates      = "dates are YYYY-MM-DD, such as 2026-11-30"
)

// ParseType returns s as a Type. It refuses anything but one of the nine
// type names, exactly, with an *InvalidError.
func ParseType(s string) (Type, error) {
	t := Type(s)
	if _, ok := typeRules[t]; !ok {
		return "", &InvalidError{Msg: "unknown item type", Hint: hintTypes}
	}
	return t, nil
}

// State is where an item's work stands.
type State string

// The states. A new item starts in backlog.
const (
	StateBacklog    State = "backlog"
	StateInProgress State = "in-progress"
	StateInReview   State = "in-review"
	StateDone       State = "done"
)

// Area is the part of life an item belongs to. "" inherits the nearest
// ancestor's area; a root item must have its own.
type Area string

// The areas.
const (
	AreaWork     Area = "work"
	AreaPersonal Area = "personal"
)

// Priority is how urgent an item is. "" inherits the nearest ancestor's
// priority.
type Priority string

// The priorities, most urgent first.
const (
	PriorityP0 Priority = "p0"
	PriorityP1 Priority = "p1"
	PriorityP2 Priority = "p2"
	PriorityP3 Priority = "p3"
)

// DefaultPriority is the effective priority of an item when neither it nor
// any ancestor has one.
const DefaultPriority = PriorityP2

// ParseState returns s as a State. "" is refused: callers default an
// unset state to StateBacklog before calling.
func ParseState(s string) (State, error) {
	switch st := State(s); st {
	case StateBacklog, StateInProgress, StateInReview, StateDone:
		return st, nil
	default:
		return "", &InvalidError{Msg: "unknown item state", Hint: hintStates}
	}
}

// ParseArea returns s as an Area. "" is accepted and means inherit.
func ParseArea(s string) (Area, error) {
	switch a := Area(s); a {
	case "", AreaWork, AreaPersonal:
		return a, nil
	default:
		return "", &InvalidError{Msg: "unknown area", Hint: hintAreas}
	}
}

// ParsePriority returns s as a Priority. "" is accepted and means inherit.
func ParsePriority(s string) (Priority, error) {
	switch p := Priority(s); p {
	case "", PriorityP0, PriorityP1, PriorityP2, PriorityP3:
		return p, nil
	default:
		return "", &InvalidError{Msg: "unknown priority", Hint: hintPriorities}
	}
}

// ParseDate checks that s is a real calendar date written YYYY-MM-DD and
// returns it unchanged. "" means no date and is accepted.
func ParseDate(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	d, err := time.Parse(time.DateOnly, s)
	if err != nil || d.Format(time.DateOnly) != s {
		return "", &InvalidError{Msg: "the date is not a real YYYY-MM-DD date", Hint: hintDates}
	}
	return s, nil
}
