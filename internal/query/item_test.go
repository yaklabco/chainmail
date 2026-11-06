package query

import (
	"errors"
	"strings"
	"testing"
)

// TestParseItem checks that item: takes none or an item ID, prints
// canonically, and works under NOT, OR and groups like every operator.
func TestParseItem(t *testing.T) {
	t.Parallel()
	long := "it_" + strings.Repeat("a", 60)
	tests := []struct {
		in, want string
	}{
		{"item:none", "item:none"},
		{"ITEM:none", "item:none"},
		{"item:it_admin_work", "item:it_admin_work"},
		{"item:it_01jbzq0y8m5k3c9w2v7x4r6t1a", "item:it_01jbzq0y8m5k3c9w2v7x4r6t1a"},
		{`item:"it_a"`, "item:it_a"},
		{"item:" + long, "item:" + long},
		{"-item:none", "-item:none"},
		{"item:it_a OR item:it_b", "item:it_a OR item:it_b"},
		{"-(item:none OR from:x)", "-(item:none OR from:x)"},
		{"item:none newer_than:7d invoice", "item:none newer_than:7d invoice"},
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

// TestParseItemErrors checks that every other value is a syntax error at
// the value's column whose expected form names both item: forms.
func TestParseItemErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		pos     int
		message string
	}{
		{"item:", 6, "item: has no value"},
		{"item:NONE", 6, "item:NONE is not valid"},
		{"item:None", 6, "item:None is not valid"},
		{"item:it_", 6, "item:it_ is not valid"},
		{"item:it_ABC", 6, "item:it_ABC is not valid"},
		{"item:xx_abc", 6, "item:xx_abc is not valid"},
		{"item:it-abc", 6, "item:it-abc is not valid"},
		{"item:it_" + strings.Repeat("a", 61), 6, "is not valid"},
		{`item:"it_a b"`, 6, "is not valid"},
		{"a OR item:bad", 11, "item:bad is not valid"},
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
			if !strings.Contains(se.Expected, "item:none") || !strings.Contains(se.Expected, "item:it_") {
				t.Errorf("Parse(%q) expected %q, want it to name item:none and an item:it_ ID", tt.in, se.Expected)
			}
		})
	}
}

// TestValidateItemTrees checks API-built trees, which skip the lexer.
func TestValidateItemTrees(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		tree *Node
		ok   bool
	}{
		{"none", Term(FieldItem, ItemNone), true},
		{"id under not", Not(Term(FieldItem, "it_a")), true},
		{"id under or", Or(Term(FieldItem, "it_a"), Term(FieldText, "x")), true},
		{"bad id", Term(FieldItem, "bogus"), false},
		{"empty", Term(FieldItem, ""), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := Validate(tt.tree); (err == nil) != tt.ok {
				t.Errorf("Validate = %v, want ok %t", err, tt.ok)
			}
		})
	}
}

// TestSplitKeepsItem checks that item: is a filter, not a scope: Split
// leaves it in the rest of the tree.
func TestSplitKeepsItem(t *testing.T) {
	t.Parallel()
	n, err := Parse("item:none in:inbox account:gmail from:a")
	if err != nil {
		t.Fatal(err)
	}
	scope, rest := Split(n)
	if scope.In != "inbox" || len(scope.Accounts) != 1 {
		t.Errorf("scope = %+v, want in:inbox and one account", scope)
	}
	if got := rest.String(); got != "item:none from:a" {
		t.Errorf("rest = %q, want item:none from:a", got)
	}
}
