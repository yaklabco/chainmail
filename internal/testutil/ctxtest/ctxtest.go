// Package ctxtest has contexts for tests that must end at a moment the test
// picks rather than on a timer.
package ctxtest

import (
	"context"
	"time"
)

// Expiring returns a context whose deadline passes when expire is closed:
// Done is expire, and Err is context.DeadlineExceeded once it is closed, for
// it and for every context derived from it. Values come from parent. The
// parent's own end is not followed, so the test must close expire, for
// example in a Cleanup.
func Expiring(parent context.Context, expire <-chan struct{}) context.Context {
	return expiring{Context: parent, expire: expire}
}

// expiring keeps parent for Value only. Its Done channel differs from any
// cancelCtx's, so a context derived from it follows Done and copies Err,
// DeadlineExceeded.
type expiring struct {
	context.Context

	expire <-chan struct{}
}

func (e expiring) Deadline() (time.Time, bool) { return time.Time{}, false }

func (e expiring) Done() <-chan struct{} { return e.expire }

func (e expiring) Err() error {
	select {
	case <-e.expire:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
