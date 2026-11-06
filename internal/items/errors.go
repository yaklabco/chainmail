package items

import "errors"

// InvalidError is a request the item rules refuse. Msg and Hint are fixed
// text and never contain item text: error text reaches agents outside the
// untrusted wrapping that item text gets everywhere else.
type InvalidError struct {
	// Msg says what is wrong.
	Msg string
	// Hint says what to send instead.
	Hint string
}

// Error returns Msg. The daemon sends Hint separately, as the error's hint.
func (e *InvalidError) Error() string { return e.Msg }

// ErrNotFound reports an item, parent, criterion, blocker or link that does
// not exist. It is wrapped as fmt.Errorf("%w: <what>", ErrNotFound), where
// <what> is "item", "parent", "criterion", "blocker" or "link", never an ID
// or text from the request.
var ErrNotFound = errors.New("not found")

// ErrKeyMismatch reports an idempotency key first used with a different
// create request. It is not idempotency.ErrKeyMismatch: the daemon answers
// the two with different hints.
var ErrKeyMismatch = errors.New("idempotency key already used for a different request")
