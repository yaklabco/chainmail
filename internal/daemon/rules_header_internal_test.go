package daemon

import (
	"testing"

	"github.com/yaklabco/chainmail/internal/query"
)

// TestRelaxText checks the header-only superset of a text query: a text
// term reads as true where it counts for a match and as false under a NOT,
// an AND or OR folds what that leaves, and the other terms stay.
func TestRelaxText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		query string
		want  string // the relaxed query, or "true" or "false"
	}{
		{query: "invoice", want: "true"},
		{query: "-invoice", want: "true"},
		{query: "subject:invoice plan", want: "subject:invoice"},
		{query: "subject:invoice OR plan", want: "true"},
		{query: "subject:invoice -(plan OR from:acme)", want: "subject:invoice -from:acme"},
		{query: "subject:invoice -(from:acme plan)", want: "subject:invoice"},
		{query: "from:acme (plan OR subject:trip)", want: "from:acme"},
		{query: "(plan from:acme) OR subject:trip", want: "from:acme OR subject:trip"},
		{query: "-(-plan)", want: "true"},
		{query: "from:acme is:unread", want: "from:acme is:unread"},
	}
	for _, tt := range tests {
		n, err := query.Parse(tt.query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.query, err)
		}
		got, v := relaxText(n, true)
		var s string
		switch v {
		case relaxedTrue:
			s = "true"
		case relaxedFalse:
			s = "false"
		default:
			s = got.String()
		}
		if s != tt.want {
			t.Errorf("relaxText(%q) = %q, want %q", tt.query, s, tt.want)
		}
	}
}
