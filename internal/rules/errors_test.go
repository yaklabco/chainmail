package rules

import (
	"errors"
	"fmt"
	"testing"
)

func TestInvalidErrorText(t *testing.T) {
	t.Parallel()
	err := error(&InvalidError{Msg: "the rule has no actions", Hint: "give 1 to 16 actions"})
	if got := err.Error(); got != "the rule has no actions" {
		t.Errorf("Error() = %q, want the message alone", got)
	}
	wrapped := fmt.Errorf("creating rule: %w", err)
	var inv *InvalidError
	if !errors.As(wrapped, &inv) || inv.Hint != "give 1 to 16 actions" {
		t.Errorf("errors.As through a wrap = %v, %+v; want the hint kept", errors.As(wrapped, &inv), inv)
	}
}

func TestErrNotFound(t *testing.T) {
	t.Parallel()
	if got := ErrNotFound.Error(); got != "rule not found" {
		t.Errorf("ErrNotFound = %q, want %q", got, "rule not found")
	}
	if !errors.Is(fmt.Errorf("getting rule: %w", ErrNotFound), ErrNotFound) {
		t.Error("a wrapped ErrNotFound does not match ErrNotFound")
	}
}
