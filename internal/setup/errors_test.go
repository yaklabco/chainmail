package setup

import (
	"errors"
	"testing"
)

func TestRefuse(t *testing.T) {
	err := refuse("try --name", "bad name %q", "x")
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("refuse returned %T, want *Refusal", err)
	}
	if r.Error() != `bad name "x"` || r.Hint != "try --name" {
		t.Fatalf("got %q, hint %q", r.Error(), r.Hint)
	}
}
