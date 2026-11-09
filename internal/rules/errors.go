package rules

import "errors"

// InvalidError is a rule definition the rules refuse. Msg and Hint are
// fixed text and never contain rule text: error text reaches agents and the
// audit log outside the untrusted wrapping rule text gets everywhere else.
type InvalidError struct {
	// Msg says what is wrong.
	Msg string
	// Hint says what to send instead.
	Hint string
}

// Error returns Msg. The daemon sends Hint separately, as the error's hint.
func (e *InvalidError) Error() string { return e.Msg }

// ErrNotFound reports a rule that does not exist. Callers wrap it with
// fixed text only, never with the ID or name they looked up.
var ErrNotFound = errors.New("rule not found")
