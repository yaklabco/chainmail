package rules

import (
	"errors"
	"strings"
	"testing"
)

// ruleMarker stands in for rule text a client wrote. No error text or hint
// may ever contain it, since error text reaches agents and the audit log
// outside the untrusted wrapping.
const ruleMarker = "zq-rule-text-marker"

// wantInvalid fails the test unless err is an *InvalidError with Msg msg
// and a hint, and neither holds ruleMarker. It returns the hint.
func wantInvalid(t *testing.T, what string, err error, msg string) string {
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
	if strings.Contains(inv.Error(), ruleMarker) || strings.Contains(inv.Hint, ruleMarker) {
		t.Errorf("%s: error %q with hint %q holds rule text", what, inv.Error(), inv.Hint)
	}
	return inv.Hint
}
