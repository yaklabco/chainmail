package items

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
