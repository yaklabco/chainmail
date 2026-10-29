// Package query parses chainmail's Gmail-style search language into a typed
// tree and checks it. It knows nothing about IMAP, so the local index in
// sub-project 7 can reuse it unchanged.
package query

import (
	"slices"
	"strings"
	"unicode"
)

// Field names a search operator. FieldText is a bare word or a phrase.
type Field string

// Fields. The string values are the operator names and the proto field names.
const (
	FieldText      Field = "text"
	FieldFrom      Field = "from"
	FieldTo        Field = "to"
	FieldCc        Field = "cc"
	FieldBcc       Field = "bcc"
	FieldSubject   Field = "subject"
	FieldIs        Field = "is"
	FieldNewerThan Field = "newer_than"
	FieldOlderThan Field = "older_than"
	FieldAfter     Field = "after"
	FieldBefore    Field = "before"
	FieldLarger    Field = "larger"
	FieldSmaller   Field = "smaller"
	FieldHas       Field = "has"
	FieldFilename  Field = "filename"
	FieldLabel     Field = "label"
	FieldIn        Field = "in"
	FieldAccount   Field = "account"
)

// Operators lists the fields a query string can name with a colon, in the
// order help text shows them.
var Operators = []Field{
	FieldFrom, FieldTo, FieldCc, FieldBcc, FieldSubject, FieldIs, FieldNewerThan, FieldOlderThan,
	FieldAfter, FieldBefore, FieldLarger, FieldSmaller, FieldHas, FieldFilename, FieldLabel, FieldIn, FieldAccount,
}

func isOperator(f Field) bool { return slices.Contains(Operators, f) }

// Limits on a query.
const (
	// MaxLength is the longest query string, in bytes.
	MaxLength = 4096
	// MaxDepth is the deepest nesting of AND, OR, NOT and parentheses.
	MaxDepth = 32
	// MaxTerms is the most terms one query may hold.
	MaxTerms = 256
)

// Op is the kind of a Node.
type Op int

// Node kinds.
const (
	OpTerm Op = iota + 1
	OpAnd
	OpOr
	OpNot
)

// Node is one node of a query tree. OpAnd and OpOr have one or more
// Children, OpNot has exactly one, and OpTerm has Field and Value.
type Node struct {
	Op       Op
	Children []*Node
	Field    Field
	Value    string
	// Pos is the 1-based column of the node in the query string, counted in
	// characters. Trees built from the API have 0.
	Pos int
}

// Term returns a term node.
func Term(f Field, value string) *Node { return &Node{Op: OpTerm, Field: f, Value: value} }

// And returns an AND of children.
func And(children ...*Node) *Node { return &Node{Op: OpAnd, Children: children} }

// Or returns an OR of children.
func Or(children ...*Node) *Node { return &Node{Op: OpOr, Children: children} }

// Not returns the negation of child.
func Not(child *Node) *Node { return &Node{Op: OpNot, Children: []*Node{child}} }

// Clone returns a deep copy of n.
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Children != nil {
		c.Children = make([]*Node, len(n.Children))
		for i, child := range n.Children {
			c.Children[i] = child.Clone()
		}
	}
	return &c
}

// Equal reports whether n and o are the same tree, ignoring positions.
func (n *Node) Equal(o *Node) bool {
	if n == nil || o == nil {
		return n == o
	}
	if n.Op != o.Op || n.Field != o.Field || n.Value != o.Value || len(n.Children) != len(o.Children) {
		return false
	}
	for i := range n.Children {
		if !n.Children[i].Equal(o.Children[i]) {
			return false
		}
	}
	return true
}

// Terms returns every term in n, depth first.
func (n *Node) Terms() []*Node {
	if n == nil {
		return nil
	}
	if n.Op == OpTerm {
		return []*Node{n}
	}
	var out []*Node
	for _, c := range n.Children {
		out = append(out, c.Terms()...)
	}
	return out
}

// String prints n in canonical query syntax. Parse(n.String()) returns a tree
// Equal to n for any tree Parse produced. The Gmail dialect sends this text as
// X-GM-RAW, so it is also valid Gmail search syntax.
func (n *Node) String() string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	n.write(&b, false)
	return b.String()
}

// write prints n. inOr says n is a direct child of an OR, where an AND needs
// parentheses because OR binds tighter.
func (n *Node) write(b *strings.Builder, inOr bool) {
	switch n.Op {
	case OpTerm:
		if n.Field == FieldText {
			b.WriteString(quote(n.Value, true))
			return
		}
		b.WriteString(string(n.Field))
		b.WriteByte(':')
		b.WriteString(quote(n.Value, false))
	case OpNot:
		b.WriteByte('-')
		child := n.Children[0]
		if child.Op == OpTerm || child.Op == OpNot {
			child.write(b, false)
			return
		}
		b.WriteByte('(')
		child.write(b, false)
		b.WriteByte(')')
	case OpAnd:
		if inOr {
			b.WriteByte('(')
		}
		for i, c := range n.Children {
			if i > 0 {
				b.WriteByte(' ')
			}
			c.write(b, false)
		}
		if inOr {
			b.WriteByte(')')
		}
	case OpOr:
		for i, c := range n.Children {
			if i > 0 {
				b.WriteString(" OR ")
			}
			c.write(b, true)
		}
	}
}

// quote returns v as it must appear in a query. text says v is a bare word,
// which also needs quotes when it would read as an operator, a negation or OR.
func quote(v string, text bool) string {
	if !needsQuotes(v, text) {
		return v
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

func needsQuotes(v string, text bool) bool {
	if v == "" {
		return true
	}
	if text && (v == "OR" || strings.HasPrefix(v, "-") || strings.ContainsRune(v, ':')) {
		return true
	}
	return strings.ContainsFunc(v, func(r rune) bool {
		return unicode.IsSpace(r) || r == '(' || r == ')' || r == '"'
	})
}

// Scope is where a search runs: the accounts named with account: and the
// folder named with in:. Neither is sent to a mail server.
type Scope struct {
	// Accounts is sorted and has no duplicates. Empty means not narrowed.
	Accounts []string
	// In is the in: value, or empty.
	In string
}

// Split takes the in: and account: terms out of a validated tree and returns
// them with the rest of the tree, which is nil when nothing is left.
func Split(n *Node) (Scope, *Node) {
	var scope Scope
	rest := split(n, &scope)
	slices.Sort(scope.Accounts)
	scope.Accounts = slices.Compact(scope.Accounts)
	return scope, rest
}

func split(n *Node, scope *Scope) *Node {
	switch {
	case n == nil:
		return nil
	case n.Op == OpTerm && n.Field == FieldIn:
		scope.In = n.Value
		return nil
	case n.Op == OpTerm && n.Field == FieldAccount:
		scope.Accounts = append(scope.Accounts, n.Value)
		return nil
	case n.Op == OpAnd:
		var kept []*Node
		for _, c := range n.Children {
			if r := split(c, scope); r != nil {
				kept = append(kept, r)
			}
		}
		return and(kept)
	default:
		return n
	}
}

// and joins items with AND, flattening nested ANDs. No items is nil and one
// item is that item.
func and(items []*Node) *Node {
	return join(OpAnd, items)
}

// or joins items with OR, flattening nested ORs.
func or(items []*Node) *Node {
	return join(OpOr, items)
}

func join(op Op, items []*Node) *Node {
	var flat []*Node
	for _, it := range items {
		if it.Op == op {
			flat = append(flat, it.Children...)
			continue
		}
		flat = append(flat, it)
	}
	switch len(flat) {
	case 0:
		return nil
	case 1:
		return flat[0]
	default:
		return &Node{Op: op, Children: flat, Pos: flat[0].Pos}
	}
}
