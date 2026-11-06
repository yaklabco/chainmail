package items

// typeRule is one type's place in the hierarchy.
type typeRule struct {
	// rank orders the types that may have children: objective 6 down to
	// story 1. A leaf has rank 0 and cannot be a parent.
	rank int
	// minParentRank is the lowest rank a parent of this type may have.
	minParentRank int
	// root says the type may have no parent. A task with none is in the
	// inbox.
	root bool
}

// typeRules is the parent rule, kept in this one table so the Lathe roadmap
// refactor can change it in one place. A type that may have children sits
// under any type ranked above it (an objective under nothing), bug and
// support-ticket under feature or above, and task under any type that may
// have children. Every type that may have children may be a root, and so
// may a task.
var typeRules = map[Type]typeRule{
	TypeObjective:     {rank: 6, minParentRank: 7, root: true},
	TypeKeyResult:     {rank: 5, minParentRank: 6, root: true},
	TypeInitiative:    {rank: 4, minParentRank: 5, root: true},
	TypeEpic:          {rank: 3, minParentRank: 4, root: true},
	TypeFeature:       {rank: 2, minParentRank: 3, root: true},
	TypeStory:         {rank: 1, minParentRank: 2, root: true},
	TypeBug:           {rank: 0, minParentRank: 2},
	TypeSupportTicket: {rank: 0, minParentRank: 2},
	TypeTask:          {rank: 0, minParentRank: 1, root: true},
}

// Leaf reports whether t can have no children: bug, support-ticket, task.
// An unknown type is not a leaf.
func Leaf(t Type) bool {
	r, ok := typeRules[t]
	return ok && r.rank == 0
}

// CanParent reports whether a child of type child may sit under parent:
// parent must not be a leaf; task may sit under any non-leaf; bug and
// support-ticket under feature or above; any other type under a type that
// ranks above it (objective > key-result > initiative > epic > feature >
// story). An unknown type on either side is refused.
func CanParent(parent, child Type) bool {
	p, okParent := typeRules[parent]
	c, okChild := typeRules[child]
	return okParent && okChild && p.rank > 0 && p.rank >= c.minParentRank
}

// CanBeRoot reports whether t may have no parent: every non-leaf, and task
// (the inbox). An unknown type is refused.
func CanBeRoot(t Type) bool {
	r, ok := typeRules[t]
	return ok && r.root
}
