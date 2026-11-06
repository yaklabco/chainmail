package items

import (
	"errors"
	"fmt"
	"testing"

	"github.com/yaklabco/chainmail/internal/idempotency"
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

// The refusals for a deleted item read the same wherever they come from,
// and the one for a deleted blocker names the blocker. All are fixed text.
func TestDeletedRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       *InvalidError
		msg, hint string
	}{
		{"item", errItemDeleted, "the item is deleted", "restore it first: chainmail item restore"},
		{"link", errLinkDeleted, "the item is deleted", "restore it first: chainmail item restore"},
		{"blocker", errBlockerDeleted, "the blocker is deleted", "restore the blocker first: chainmail item restore"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.err.Msg != tt.msg || tt.err.Hint != tt.hint {
				t.Errorf("refusal = %q, %q; want %q, %q", tt.err.Msg, tt.err.Hint, tt.msg, tt.hint)
			}
		})
	}
}
