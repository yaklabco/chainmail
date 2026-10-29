package query

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseCanonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"invoice", "invoice"},
		{"invoice from:acme", "invoice from:acme"},
		{`from:"Alex Doe" subject:report`, `from:"Alex Doe" subject:report`},
		{"a b OR c", "a b OR c"},
		{"(a b) OR c", "(a b) OR c"},
		{"-from:alex", "-from:alex"},
		{"-(a OR b)", "-(a OR b)"},
		{"-(a b)", "-(a b)"},
		{"FROM:alex", "from:alex"},
		{`"a phrase"`, `"a phrase"`},
		{"subject:re:hello", "subject:re:hello"},
		{`"OR"`, `"OR"`},
		{`"-x"`, `"-x"`},
		{`"a:b"`, `"a:b"`},
		{"((a))", "a"},
		{"a  \t b", "a b"},
		{"(a OR b) OR c", "a OR b OR c"},
		{`subject:"say \"hi\""`, `subject:"say \"hi\""`},
		{"e-mail", "e-mail"},
		{"newer_than:7d is:unread larger:5M", "newer_than:7d is:unread larger:5M"},
		{"in:inbox account:gmail invoice", "in:inbox account:gmail invoice"},
		{"café", "café"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			n, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.in, err)
			}
			if got := n.String(); got != tt.want {
				t.Errorf("Parse(%q).String() = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseEmpty(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "   ", "\t\n"} {
		n, err := Parse(in)
		if err != nil || n != nil {
			t.Errorf("Parse(%q) = %v, %v; want nil, nil", in, n, err)
		}
	}
}

func TestParseTree(t *testing.T) {
	t.Parallel()
	n, err := Parse("a b OR c -d")
	if err != nil {
		t.Fatal(err)
	}
	want := And(Term(FieldText, "a"), Or(Term(FieldText, "b"), Term(FieldText, "c")), Not(Term(FieldText, "d")))
	if !n.Equal(want) {
		t.Errorf("tree = %s, want %s", n, want)
	}
	if n.Children[0].Pos != 1 || n.Children[1].Children[1].Pos != 8 || n.Children[2].Pos != 10 {
		t.Errorf("positions = %d %d %d, want 1 8 10", n.Children[0].Pos, n.Children[1].Children[1].Pos, n.Children[2].Pos)
	}
}

func TestParseErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		pos     int
		message string
	}{
		{`"unclosed`, 1, "unclosed quote"},
		{"from:", 6, "from: has no value"},
		{"foo:bar", 1, `unknown operator "foo:"`},
		{"a OR", 3, "OR has nothing after it"},
		{"OR a", 1, "OR has nothing before it"},
		{"a OR OR b", 3, "OR has nothing after it"},
		{"(a", 1, "( is never closed"},
		{"a)", 2, "unmatched )"},
		{"()", 1, "() is empty"},
		{"- a", 1, "a lone - negates nothing"},
		{`""`, 1, "empty phrase"},
		{"is:maybe", 4, "is:maybe is not valid"},
		{"newer_than:5x", 12, "newer_than:5x is not valid"},
		{"after:2026-13-01", 7, "after:2026-13-01 is not valid"},
		{"larger:5G", 8, "larger:5G is not valid"},
		{"has:pdf", 5, "has:pdf is not valid"},
		{"-in:inbox", 2, "in: chooses where to search"},
		{"a OR account:gmail", 6, "account: chooses where to search"},
		{"in:inbox in:sent", 10, "only one in: term"},
		{"account:Bad_Name", 9, "account:Bad_Name is not valid"},
		{strings.Repeat("(", 33) + "a" + strings.Repeat(")", 33), 33, "nested too deeply"},
		{"a\x01b", 1, "control character"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tt.in)
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("Parse(%q) error = %v, want *SyntaxError", tt.in, err)
			}
			if se.Pos != tt.pos || !strings.Contains(se.Msg, tt.message) {
				t.Errorf("Parse(%q) = column %d %q, want column %d containing %q", tt.in, se.Pos, se.Msg, tt.pos, tt.message)
			}
			if se.Expected == "" {
				t.Errorf("Parse(%q) has no expected form", tt.in)
			}
		})
	}
}

func TestSyntaxErrorText(t *testing.T) {
	t.Parallel()
	_, err := Parse("from:")
	want := "column 6: from: has no value; expected an address or a name, such as from:alex@example.com"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
	err = Validate(Or(Term(FieldIn, "inbox"), Term(FieldText, "x")))
	if err == nil || strings.Contains(err.Error(), "column") {
		t.Errorf("API tree error = %v, want one without a column", err)
	}
}

func TestParseTooLong(t *testing.T) {
	t.Parallel()
	if _, err := Parse(strings.Repeat("a ", MaxLength)); err == nil {
		t.Error("Parse accepted a query over MaxLength")
	}
	many := strings.TrimSpace(strings.Repeat("a ", MaxTerms+1))
	if _, err := Parse(many); err == nil || !strings.Contains(err.Error(), "too many terms") {
		t.Errorf("Parse(%d terms) = %v, want too many terms", MaxTerms+1, err)
	}
}

func TestValidateAPITrees(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tree *Node
		ok   bool
	}{
		{"term", Term(FieldFrom, "alex"), true},
		{"single child and", And(Term(FieldText, "x")), true},
		{"empty and", And(), false},
		{"not with two children", &Node{Op: OpNot, Children: []*Node{Term(FieldText, "a"), Term(FieldText, "b")}}, false},
		{"nil child", And(nil), false},
		{"unknown field", Term("bogus", "x"), false},
		{"unknown op", &Node{Op: 99}, false},
		{"in under not", Not(Term(FieldIn, "inbox")), false},
		{"in under and", And(Term(FieldIn, "inbox"), Term(FieldText, "x")), true},
		{"empty value", Term(FieldSubject, ""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tt.tree)
			if (err == nil) != tt.ok {
				t.Errorf("Validate = %v, want ok %t", err, tt.ok)
			}
		})
	}
}

func TestSplit(t *testing.T) {
	t.Parallel()
	n, err := Parse("invoice in:inbox account:gmail account:arsoculus from:a account:gmail")
	if err != nil {
		t.Fatal(err)
	}
	scope, rest := Split(n)
	if scope.In != "inbox" || !slices.Equal(scope.Accounts, []string{"arsoculus", "gmail"}) {
		t.Errorf("scope = %+v", scope)
	}
	if got := rest.String(); got != "invoice from:a" {
		t.Errorf("rest = %q", got)
	}
	n, err = Parse("in:inbox")
	if err != nil {
		t.Fatal(err)
	}
	if _, rest := Split(n); rest != nil {
		t.Errorf("rest of a scope-only query = %v, want nil", rest)
	}
	if scope, rest := Split(nil); rest != nil || scope.In != "" || scope.Accounts != nil {
		t.Errorf("Split(nil) = %+v, %v", scope, rest)
	}
}

func TestCloneIsDeep(t *testing.T) {
	t.Parallel()
	n, err := Parse("a OR b")
	if err != nil {
		t.Fatal(err)
	}
	c := n.Clone()
	c.Children[0].Value = "changed"
	if n.Children[0].Value != "a" {
		t.Error("Clone shares children with the original")
	}
	if got := len(n.Terms()); got != 2 {
		t.Errorf("Terms() = %d, want 2", got)
	}
}

func TestParseAge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		n    int
		unit byte
		ok   bool
	}{
		{"7d", 7, 'd', true},
		{"2w", 2, 'w', true},
		{"3m", 3, 'm', true},
		{"1y", 1, 'y', true},
		{"10D", 10, 'd', true},
		{"0d", 0, 0, false},
		{"d", 0, 0, false},
		{"7", 0, 0, false},
		{"7h", 0, 0, false},
		{"99999d", 0, 0, false},
	}
	for _, tt := range tests {
		n, unit, err := ParseAge(tt.in)
		if (err == nil) != tt.ok || n != tt.n || unit != tt.unit {
			t.Errorf("ParseAge(%q) = %d, %q, %v", tt.in, n, unit, err)
		}
	}
}

func TestParseDateAndSize(t *testing.T) {
	t.Parallel()
	d, err := ParseDate("2026-01-31")
	if err != nil || !d.Equal(time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ParseDate = %v, %v", d, err)
	}
	for _, bad := range []string{"2026/01/31", "31-01-2026", "2026-02-30"} {
		if _, err := ParseDate(bad); err == nil {
			t.Errorf("ParseDate(%q) succeeded", bad)
		}
	}
	sizes := map[string]int64{"100": 100, "10K": 10 << 10, "10k": 10 << 10, "5M": 5 << 20}
	for in, want := range sizes {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "5G", "1.5M", "-1"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) succeeded", bad)
		}
	}
}
