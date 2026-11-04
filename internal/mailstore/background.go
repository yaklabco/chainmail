package mailstore

import (
	"context"
	"errors"
)

// ErrBusy means a call made for background work found no connection it may
// take: every connection is in use, or a foreground call is waiting for
// one. The caller tries again later. It never wraps ErrUnavailable, since
// the server is fine, and a Store never retries it.
var ErrBusy = errors.New("no idle connection for background work")

// backgroundKey marks a context for background work.
type backgroundKey struct{}

// WithBackground marks ctx for background work, such as the cache filling
// itself. A Store borrows a connection for such a call only when one is
// idle, or when it may open one with no foreground call waiting, and
// otherwise fails at once with ErrBusy instead of waiting. Foreground
// calls, made for a client, keep the connections first.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports whether WithBackground marked ctx or a context ctx
// derives from.
func IsBackground(ctx context.Context) bool {
	on, _ := ctx.Value(backgroundKey{}).(bool)
	return on
}
