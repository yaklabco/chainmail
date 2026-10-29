package query

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SyntaxError is a query that does not parse or validate. Pos is the 1-based
// column, counted in characters, or 0 for trees built from the API.
type SyntaxError struct {
	Pos      int
	Msg      string
	Expected string
}

func (e *SyntaxError) Error() string {
	var b strings.Builder
	if e.Pos > 0 {
		fmt.Fprintf(&b, "column %d: ", e.Pos)
	}
	b.WriteString(e.Msg)
	if e.Expected != "" {
		b.WriteString("; expected ")
		b.WriteString(e.Expected)
	}
	return b.String()
}

type tokKind int

const (
	tokWord tokKind = iota + 1
	tokPhrase
	tokField
	tokLParen
	tokRParen
	tokOr
	tokMinus
)

type token struct {
	kind  tokKind
	pos   int
	field Field
	value string
	// valuePos is where a field's value starts, for error messages.
	valuePos int
}

// Parse parses a query string. An empty or all-space query returns a nil
// tree, which matches every message. Every error is a *SyntaxError.
func Parse(s string) (*Node, error) {
	if len(s) > MaxLength {
		return nil, &SyntaxError{Pos: 1, Msg: fmt.Sprintf("the query is %d bytes long", len(s)), Expected: fmt.Sprintf("at most %d bytes", MaxLength)}
	}
	if !utf8.ValidString(s) {
		return nil, &SyntaxError{Pos: 1, Msg: "the query is not valid UTF-8", Expected: "UTF-8 text"}
	}
	toks, err := lex(s)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks}
	n, err := p.sequence()
	if err != nil {
		return nil, err
	}
	if t, ok := p.peek(); ok {
		// Only an unmatched ) stops the top-level sequence early.
		return nil, &SyntaxError{Pos: t.pos, Msg: "unmatched )", Expected: "a ( before it, or no )"}
	}
	if err := Validate(n); err != nil {
		return nil, err
	}
	return n, nil
}

func lex(s string) ([]token, error) {
	rs := []rune(s)
	var toks []token
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case unicode.IsSpace(r):
			i++
		case r == '(':
			toks = append(toks, token{kind: tokLParen, pos: i + 1})
			i++
		case r == ')':
			toks = append(toks, token{kind: tokRParen, pos: i + 1})
			i++
		case r == '-' && i+1 < len(rs) && !unicode.IsSpace(rs[i+1]) && rs[i+1] != ')':
			toks = append(toks, token{kind: tokMinus, pos: i + 1})
			i++
		case r == '-':
			return nil, &SyntaxError{Pos: i + 1, Msg: "a lone - negates nothing", Expected: "a term right after -, such as -from:alex"}
		case r == '"':
			v, next, err := lexQuoted(rs, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{kind: tokPhrase, pos: i + 1, value: v})
			i = next
		default:
			t, next, err := lexWord(rs, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, t)
			i = next
		}
	}
	return toks, nil
}

// lexWord reads a bare word, OR, or an operator with its value, which may be
// quoted: from:"Alex Doe".
func lexWord(rs []rune, start int) (token, int, error) {
	i := start
	for i < len(rs) && !unicode.IsSpace(rs[i]) && rs[i] != '(' && rs[i] != ')' && rs[i] != '"' {
		i++
	}
	word := string(rs[start:i])
	if word == "OR" {
		return token{kind: tokOr, pos: start + 1}, i, nil
	}
	name, value, hasColon := strings.Cut(word, ":")
	if !hasColon {
		return token{kind: tokWord, pos: start + 1, value: word}, i, nil
	}
	f := Field(strings.ToLower(name))
	if !isOperator(f) {
		return token{}, 0, &SyntaxError{
			Pos:      start + 1,
			Msg:      fmt.Sprintf("unknown operator %q", name+":"),
			Expected: fmt.Sprintf("one of %s; to search for the text itself, quote it: %s", operatorList(), quote(word, true)),
		}
	}
	valuePos := start + 1 + utf8.RuneCountInString(name) + 1
	if value == "" && i < len(rs) && rs[i] == '"' {
		v, next, err := lexQuoted(rs, i)
		if err != nil {
			return token{}, 0, err
		}
		value, valuePos, i = v, i+1, next
	}
	if value == "" {
		return token{}, 0, &SyntaxError{Pos: valuePos, Msg: fmt.Sprintf("%s: has no value", f), Expected: expected(f)}
	}
	return token{kind: tokField, pos: start + 1, field: f, value: value, valuePos: valuePos}, i, nil
}

// lexQuoted reads a quoted string starting at rs[start] == '"'. Inside it,
// \" is a quote and \\ a backslash.
func lexQuoted(rs []rune, start int) (string, int, error) {
	var b strings.Builder
	for i := start + 1; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			if i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\') {
				i++
			}
			b.WriteRune(rs[i])
		case '"':
			if b.Len() == 0 {
				return "", 0, &SyntaxError{Pos: start + 1, Msg: "empty phrase", Expected: `some text between the quotes, such as "q3 report"`}
			}
			return b.String(), i + 1, nil
		default:
			b.WriteRune(rs[i])
		}
	}
	return "", 0, &SyntaxError{Pos: start + 1, Msg: "unclosed quote", Expected: `a closing "`}
}

func operatorList() string {
	names := make([]string, len(Operators))
	for i, f := range Operators {
		names[i] = string(f) + ":"
	}
	return strings.Join(names, " ")
}

type parser struct {
	toks  []token
	i     int
	depth int
}

func (p *parser) peek() (token, bool) {
	if p.i >= len(p.toks) {
		return token{}, false
	}
	return p.toks[p.i], true
}

// sequence parses terms joined by implicit AND, up to ) or the end.
func (p *parser) sequence() (*Node, error) {
	var items []*Node
	for {
		t, ok := p.peek()
		if !ok || t.kind == tokRParen {
			return and(items), nil
		}
		n, err := p.or()
		if err != nil {
			return nil, err
		}
		items = append(items, n)
	}
}

// or parses unary terms joined by OR.
func (p *parser) or() (*Node, error) {
	first, err := p.unary()
	if err != nil {
		return nil, err
	}
	items := []*Node{first}
	for {
		t, ok := p.peek()
		if !ok || t.kind != tokOr {
			break
		}
		p.i++
		next, ok := p.peek()
		if !ok || next.kind == tokRParen || next.kind == tokOr {
			return nil, &SyntaxError{Pos: t.pos, Msg: "OR has nothing after it", Expected: "a term on both sides of OR"}
		}
		n, err := p.unary()
		if err != nil {
			return nil, err
		}
		items = append(items, n)
	}
	return or(items), nil
}

func (p *parser) unary() (*Node, error) {
	t, ok := p.peek()
	if !ok {
		return nil, &SyntaxError{Msg: "the query ends too early", Expected: "a term"}
	}
	p.i++
	switch t.kind {
	case tokMinus:
		child, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &Node{Op: OpNot, Children: []*Node{child}, Pos: t.pos}, nil
	case tokLParen:
		p.depth++
		if p.depth > MaxDepth {
			return nil, &SyntaxError{Pos: t.pos, Msg: "the query is nested too deeply", Expected: fmt.Sprintf("at most %d levels of parentheses", MaxDepth)}
		}
		inner, err := p.sequence()
		if err != nil {
			return nil, err
		}
		closing, ok := p.peek()
		if !ok || closing.kind != tokRParen {
			return nil, &SyntaxError{Pos: t.pos, Msg: "( is never closed", Expected: "a matching )"}
		}
		p.i++
		p.depth--
		if inner == nil {
			return nil, &SyntaxError{Pos: t.pos, Msg: "() is empty", Expected: "a term inside the parentheses"}
		}
		return inner, nil
	case tokOr:
		return nil, &SyntaxError{Pos: t.pos, Msg: "OR has nothing before it", Expected: "a term on both sides of OR"}
	case tokRParen:
		return nil, &SyntaxError{Pos: t.pos, Msg: "unmatched )", Expected: "a ( before it"}
	case tokField:
		return &Node{Op: OpTerm, Field: t.field, Value: t.value, Pos: t.pos}, nil
	default:
		return &Node{Op: OpTerm, Field: FieldText, Value: t.value, Pos: t.pos}, nil
	}
}
