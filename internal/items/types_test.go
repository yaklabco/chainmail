package items

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// modelMarker stands in for item text an agent wrote. No error text or hint
// may ever contain it, since error text reaches agents outside the
// untrusted wrapping.
const modelMarker = "zq-item-text-marker"

// checkInvalid fails the test unless err is an *InvalidError with Msg msg
// and a hint, and neither holds modelMarker. It returns the error.
func checkInvalid(t *testing.T, what string, err error, msg string) *InvalidError {
	t.Helper()
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("%s: err = %v (%T), want an *InvalidError", what, err, err)
	}
	if inv.Msg != msg {
		t.Errorf("%s: Msg = %q, want %q", what, inv.Msg, msg)
	}
	if inv.Hint == "" {
		t.Errorf("%s: Hint is empty", what)
	}
	if strings.Contains(inv.Error(), modelMarker) || strings.Contains(inv.Hint, modelMarker) {
		t.Errorf("%s: error %q with hint %q holds the request's text", what, inv.Error(), inv.Hint)
	}
	return inv
}

func TestParseType(t *testing.T) {
	t.Parallel()
	for _, want := range modelTypes {
		got, err := ParseType(string(want))
		if err != nil || got != want {
			t.Errorf("ParseType(%q) = %q, %v; want %q", want, got, err, want)
		}
	}
	const hint = "types: objective, key-result, initiative, epic, feature, story, bug, support-ticket, task"
	for _, bad := range []string{"", "Task", "TASK", " task", "task ", "key_result", "keyresult", "sub-task", "todo", modelMarker} {
		got, err := ParseType(bad)
		if got != "" {
			t.Errorf("ParseType(%q) = %q, want \"\"", bad, got)
		}
		if inv := checkInvalid(t, fmt.Sprintf("ParseType(%q)", bad), err, "unknown item type"); inv.Hint != hint {
			t.Errorf("ParseType(%q) hint = %q, want %q", bad, inv.Hint, hint)
		}
	}
}
