package query

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// accountRE matches account names, as internal/config does. query cannot
// import config, which would pull the config parser into the index later.
var accountRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// itemIDRE matches item IDs, as internal/items ValidID does. query cannot
// import items, which would pull state.db code into the parser.
var itemIDRE = regexp.MustCompile(`^it_[0-9a-z_]{1,60}$`)

var (
	ageRE  = regexp.MustCompile(`^([0-9]{1,4})([dwmyDWMY])$`)
	sizeRE = regexp.MustCompile(`^([0-9]{1,12})([kKmM]?)$`)
)

// Validate checks a tree, whether Parse built it or it came from the API.
// Every error is a *SyntaxError.
func Validate(n *Node) error {
	if n == nil {
		return nil
	}
	v := &validator{}
	return v.node(n, false, 1)
}

type validator struct {
	terms int
	ins   int
}

// node checks n. constrained says n sits under NOT or OR, where in: and
// account: are not allowed.
func (v *validator) node(n *Node, constrained bool, depth int) error {
	if n == nil {
		return &SyntaxError{Msg: "a node is missing", Expected: "and, or, not or term"}
	}
	if depth > MaxDepth {
		return &SyntaxError{Pos: n.Pos, Msg: "the query is nested too deeply", Expected: fmt.Sprintf("at most %d levels", MaxDepth)}
	}
	switch n.Op {
	case OpTerm:
		return v.term(n, constrained)
	case OpAnd, OpOr:
		if len(n.Children) == 0 {
			return &SyntaxError{Pos: n.Pos, Msg: "an and or or node has no children", Expected: "at least one child"}
		}
		for _, c := range n.Children {
			if err := v.node(c, constrained || n.Op == OpOr, depth+1); err != nil {
				return err
			}
		}
		return nil
	case OpNot:
		if len(n.Children) != 1 {
			return &SyntaxError{Pos: n.Pos, Msg: "a not node needs exactly one child", Expected: "one child"}
		}
		return v.node(n.Children[0], true, depth+1)
	default:
		return &SyntaxError{Pos: n.Pos, Msg: fmt.Sprintf("unknown node kind %d", n.Op), Expected: "and, or, not or term"}
	}
}

func (v *validator) term(n *Node, constrained bool) error {
	v.terms++
	if v.terms > MaxTerms {
		return &SyntaxError{Pos: n.Pos, Msg: "the query has too many terms", Expected: fmt.Sprintf("at most %d terms", MaxTerms)}
	}
	if n.Field != FieldText && !isOperator(n.Field) {
		return &SyntaxError{Pos: n.Pos, Msg: fmt.Sprintf("unknown field %q", n.Field), Expected: "text or one of " + operatorList()}
	}
	pos := valuePos(n)
	if n.Value == "" {
		return &SyntaxError{Pos: pos, Msg: fmt.Sprintf("%s has no value", label(n.Field)), Expected: expected(n.Field)}
	}
	if strings.ContainsFunc(n.Value, isControl) {
		return &SyntaxError{Pos: pos, Msg: "the value contains a control character", Expected: "printable text"}
	}
	invalid := &SyntaxError{Pos: pos, Msg: fmt.Sprintf("%s:%s is not valid", n.Field, n.Value), Expected: expected(n.Field)}
	switch n.Field {
	case FieldIs:
		if !slices.Contains([]string{"unread", "read", "starred"}, strings.ToLower(n.Value)) {
			return invalid
		}
	case FieldNewerThan, FieldOlderThan:
		if _, _, err := ParseAge(n.Value); err != nil {
			return invalid
		}
	case FieldAfter, FieldBefore:
		if _, err := ParseDate(n.Value); err != nil {
			return invalid
		}
	case FieldLarger, FieldSmaller:
		if _, err := ParseSize(n.Value); err != nil {
			return invalid
		}
	case FieldHas:
		if !strings.EqualFold(n.Value, "attachment") {
			return invalid
		}
	case FieldItem:
		if n.Value != ItemNone && !itemIDRE.MatchString(n.Value) {
			return invalid
		}
	case FieldIn, FieldAccount:
		if constrained {
			return &SyntaxError{
				Pos:      n.Pos,
				Msg:      fmt.Sprintf("%s: chooses where to search, so it cannot be negated or joined with OR", n.Field),
				Expected: fmt.Sprintf("%s: as a plain term, such as %s", n.Field, example(n.Field)),
			}
		}
		if n.Field == FieldIn {
			v.ins++
			if v.ins > 1 {
				return &SyntaxError{Pos: n.Pos, Msg: "only one in: term is allowed", Expected: "a single in: term; in:anywhere searches every folder"}
			}
		}
		if n.Field == FieldAccount && !accountRE.MatchString(n.Value) {
			return invalid
		}
	}
	return nil
}

// valuePos is the column of a term's value: after "field:" for operators.
func valuePos(n *Node) int {
	if n.Pos == 0 || n.Field == FieldText {
		return n.Pos
	}
	return n.Pos + utf8.RuneCountInString(string(n.Field)) + 1
}

func label(f Field) string {
	if f == FieldText {
		return "the term"
	}
	return string(f) + ":"
}

func example(f Field) string {
	if f == FieldIn {
		return "in:inbox"
	}
	return "account:gmail"
}

// expected describes the valid form of a field's value.
func expected(f Field) string {
	switch f {
	case FieldText:
		return "a word or a quoted phrase"
	case FieldFrom, FieldTo, FieldCc, FieldBcc:
		return "an address or a name, such as from:alex@example.com"
	case FieldSubject:
		return `text, such as subject:invoice or subject:"q3 report"`
	case FieldIs:
		return "is:unread, is:read or is:starred"
	case FieldNewerThan, FieldOlderThan:
		return "a number and d, w, m or y, such as newer_than:7d"
	case FieldAfter, FieldBefore:
		return "a date as YYYY-MM-DD, such as after:2026-01-31"
	case FieldLarger, FieldSmaller:
		return "a size in bytes, or with K or M, such as larger:5M"
	case FieldHas:
		return "has:attachment"
	case FieldFilename:
		return "part of a file name, such as filename:pdf"
	case FieldLabel:
		return "a Gmail label, such as label:receipts"
	case FieldItem:
		return "an item ID, such as item:it_admin_work, or item:none for mail in no linked thread"
	case FieldIn:
		return "inbox, sent, drafts, archive, trash, spam, anywhere or a folder name"
	case FieldAccount:
		return "an account name, such as account:gmail"
	default:
		return "a known operator"
	}
}

// isControl reports C0 and C1 control characters and DEL.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

// ParseAge parses a relative age such as 7d. unit is one of d, w, m or y.
func ParseAge(v string) (n int, unit byte, err error) {
	m := ageRE.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, fmt.Errorf("age %q: want a number and d, w, m or y", v)
	}
	n, _ = strconv.Atoi(m[1]) // the pattern allows only 1 to 4 digits
	if n == 0 {
		return 0, 0, errors.New("age must be at least 1")
	}
	return n, strings.ToLower(m[2])[0], nil
}

// ParseDate parses YYYY-MM-DD as midnight UTC.
func ParseDate(v string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q: want YYYY-MM-DD", v)
	}
	return t, nil
}

// ParseSize parses a size in bytes, optionally with K (KiB) or M (MiB).
func ParseSize(v string) (int64, error) {
	m := sizeRE.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("size %q: want a number, optionally with K or M", v)
	}
	n, _ := strconv.ParseInt(m[1], 10, 64) // at most 12 digits always fits
	switch strings.ToLower(m[2]) {
	case "k":
		n <<= 10
	case "m":
		n <<= 20
	}
	return n, nil
}
