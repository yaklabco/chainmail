// Package calstore defines the calendar store interface that chainmail's
// calendar backends implement: Google Calendar API v3 (calstore/google),
// CalDAV with RFC 6638 scheduling (calstore/caldav) and an in-memory fake
// for tests (calstore/fake).
//
// # Object names
//
// A create (Put with etag "") names the new object after ev.UID, so a retry
// after a crash finds the object it already made and gets ErrExists:
//
//   - Google: the event id is
//     strings.ToLower(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(sha256(UID)))[:32].
//   - CalDAV: the object href is <collection path>/<url.PathEscape(UID)>.ics.
//
// # Errors
//
// Backends map provider responses to the errors below:
//
//   - 401, and a 403 for insufficient scope: ErrAuth.
//   - 404 and 410 on an object: ErrNotFound.
//   - 409 on a create: ErrExists.
//   - 412: ErrConflict, or ErrExists on a create sent with If-None-Match: *.
//   - 403 on a write to a read-only calendar: ErrReadOnly.
//   - 410 on a sync: ErrSyncTokenInvalid.
//   - 429, 5xx and network errors: ErrUnavailable.
//
// They wrap only with fmt.Errorf("%w: status %d", ...). No response body,
// event text or calendar name reaches an error, since all of it is
// untrusted.
package calstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// Errors every backend returns, possibly wrapped with an HTTP status.
var (
	ErrNotFound         = errors.New("calendar object not found")
	ErrExists           = errors.New("calendar object already exists")
	ErrConflict         = errors.New("calendar object changed since it was read")
	ErrReadOnly         = errors.New("calendar is read-only")
	ErrSyncTokenInvalid = errors.New("sync token no longer valid")
	ErrAuth             = errors.New("calendar access not authorized")
	ErrUnavailable      = errors.New("calendar server unavailable")
	// ErrSyncTooLarge is a Sync that would keep more than MaxSyncBytes of
	// server data or MaxSyncEvents events. It is ErrUnavailable, so the
	// caller keeps its token, backs off and reports the calendar in error.
	ErrSyncTooLarge = fmt.Errorf("%w: calendar too large to sync", ErrUnavailable)
)

// Bounds on what one Sync keeps.
const (
	// MaxSyncBytes bounds the server data one Sync reads and keeps.
	MaxSyncBytes = 64 << 20
	// MaxSyncEvents bounds the events, or for CalDAV the objects, one Sync
	// keeps.
	MaxSyncEvents = 40000
)

// Store is one account's calendars on one backend. Every method makes
// network requests under ctx and holds no transaction.
type Store interface {
	// ListCalendars returns the account's calendars.
	ListCalendars(ctx context.Context) ([]calendar.Calendar, error)
	// Sync returns changes since token; "" means a full sync.
	Sync(ctx context.Context, calID, token string) (calendar.Changes, error)
	// GetEvent returns an object: its master and overrides, or one event.
	GetEvent(ctx context.Context, calID, providerID string) ([]calendar.Event, error)
	// FreeBusy returns the busy time of each address in people over [from, to).
	FreeBusy(ctx context.Context, people []string, from, to time.Time) (map[string]calendar.FreeBusy, error)
	// Put creates when etag is "" (the backend derives the object name from
	// ev.UID and returns ErrExists if it is taken) or replaces the object
	// ev.ProviderID with If-Match etag. mode NotifyNone sends no invitations.
	Put(ctx context.Context, calID string, ev calendar.Event, etag string, mode calendar.Notify) (calendar.Event, error)
	// Respond sets r.Self's participation status on an object or one of its
	// instances, so the organizer is told.
	Respond(ctx context.Context, calID, providerID string, r calendar.Response) (calendar.Event, error)
	// Delete removes an object with If-Match etag. mode NotifyNone sends no
	// cancellations.
	Delete(ctx context.Context, calID, providerID, etag string, mode calendar.Notify) error
}
