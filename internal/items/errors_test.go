package items

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jamesainslie/chainmail/internal/idempotency"
)

func TestInvalidErrorText(t *testing.T) {
	t.Parallel()
	err := error(&InvalidError{Msg: "unknown item type", Hint: "types: task"})
	if got := err.Error(); got != "unknown item type" {
		t.Errorf("Error() = %q, want the message alone", got)
	}
	wrapped := fmt.Errorf("creating item: %w", err)
	var inv *InvalidError
	if !errors.As(wrapped, &inv) || inv.Hint != "types: task" {
		t.Errorf("errors.As through a wrap = %v, %+v; want the hint kept", errors.As(wrapped, &inv), inv)
	}
}

func TestErrorValues(t *testing.T) {
	t.Parallel()
	for _, what := range []string{"item", "parent", "criterion", "blocker", "link"} {
		err := fmt.Errorf("%w: %s", ErrNotFound, what)
		if !errors.Is(err, ErrNotFound) || err.Error() != "not found: "+what {
			t.Errorf("wrapped ErrNotFound = %q, Is %v; want %q", err, errors.Is(err, ErrNotFound), "not found: "+what)
		}
	}
	if got := ErrKeyMismatch.Error(); got != "idempotency key already used for a different request" {
		t.Errorf("ErrKeyMismatch = %q", got)
	}
	// The daemon gives the two mismatches different hints, so they must
	// not match each other.
	if errors.Is(ErrKeyMismatch, idempotency.ErrKeyMismatch) {
		t.Error("items.ErrKeyMismatch matches idempotency.ErrKeyMismatch")
	}
}
