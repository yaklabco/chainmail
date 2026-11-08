// Package google is a calstore.Store over the Google Calendar API v3, in
// plain REST over net/http with the account's authenticating client (see
// auth.Manager.HTTPClient).
//
// Mapping:
//
//   - A calendar's Access is write for the owner and writer access roles and
//     read for reader and freeBusyReader. Its Name is the user's override of
//     the summary when there is one.
//   - Events are listed with singleEvents=false, so a series comes as its
//     master and its exceptions. An exception becomes an override: the
//     master's ProviderID with RecurrenceID from originalStartTime. A
//     cancelled instance is a CANCELLED override. Every override carries
//     its master's UID, read from the master when Google leaves it out. A
//     cancelled event that is not an instance was deleted and becomes a
//     Delete, and the cancelled instances listed with a deleted series are
//     dropped.
//   - Raw is the event JSON. Updates PATCH only the fields that differ from
//     Raw, and keep what Google knows of each attendee (comments, guests,
//     flags) that chainmail does not model.
//   - Creates name the event ObjectID(UID). Google picks the iCalUID of an
//     event created with a client id, so the UID of the event Put returns
//     is Google's, not ev.UID.
//   - Writes send sendUpdates=all for calendar.NotifySend and none
//     otherwise. Respond always sends all, so the organizer hears.
//
// Errors follow calstore's rules. A 403 is read by its reasons: scope
// reasons (insufficientPermissions, ACCESS_TOKEN_SCOPE_INSUFFICIENT, or an
// insufficient_scope challenge) give ErrAuth, rate-limit reasons give
// ErrUnavailable, and any other 403 gives ErrReadOnly on a write and
// ErrAuth on a read. A response over its size cap gives calendar.ErrTooLarge.
// A request Google refuses as malformed (400 and other 4xx), or one this
// package refuses before sending, gives ErrRequest. No error carries
// response text.
package google

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

// DefaultBaseURL is the Calendar API v3 endpoint.
const DefaultBaseURL = "https://www.googleapis.com/calendar/v3"

// DefaultRequestTimeout bounds each request.
const DefaultRequestTimeout = 30 * time.Second

// Response size caps.
const (
	maxPageBytes  = 8 << 20              // a list page or a freeBusy answer
	maxEventBytes = calendar.MaxICSBytes // one event
	maxErrorBytes = 64 << 10             // an error body, read for its reasons only
)

// maxPages bounds the pages one listing reads, so a server that keeps
// handing out page tokens cannot keep a sync running for ever.
const maxPages = 1000

// ErrRequest means the request was refused as malformed: by Google, with a
// 400 or another client error calstore has no error for, or by this package
// before sending, such as an event without a UID or a Respond by someone who
// is not an attendee.
var ErrRequest = errors.New("calendar request refused")

// errBadResponse is a response that does not decode as the API's JSON.
var errBadResponse = fmt.Errorf("%w: invalid response", calstore.ErrUnavailable)

// Option configures a Store.
type Option func(*Store)

// WithBaseURL sets the API base URL, for tests. The default is
// DefaultBaseURL.
func WithBaseURL(u string) Option {
	return func(s *Store) { s.base = strings.TrimSuffix(u, "/") }
}

// WithRequestTimeout sets the deadline of each request. The default is
// DefaultRequestTimeout.
func WithRequestTimeout(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// Store is one Google account's calendars.
type Store struct {
	hc      *http.Client
	base    string
	timeout time.Duration
}

var _ calstore.Store = (*Store)(nil)

// New returns a Store whose requests go through hc, which authenticates
// them.
func New(hc *http.Client, opts ...Option) *Store {
	s := &Store{hc: hc, base: DefaultBaseURL, timeout: DefaultRequestTimeout}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ObjectID is the event id a create of an event with uid uses: the first 32
// characters of the lowercase, unpadded base32hex SHA-256 of uid, which
// Google accepts as a client-chosen id.
func ObjectID(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return strings.ToLower(base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))[:32]
}

// InstanceID is Google's id of the instance of series id that starts at
// rid: id_YYYYMMDDTHHMMSSZ, or id_YYYYMMDD for an all-day series. allDay is
// the series's kind (the instance's originalStartTime), not the kind an
// exception has now.
func InstanceID(id string, rid time.Time, allDay bool) string {
	if allDay {
		return id + "_" + rid.UTC().Format("20060102")
	}
	return id + "_" + rid.UTC().Format("20060102T150405Z")
}

// call is one API request.
type call struct {
	method  string
	path    string // escaped, below the base URL
	query   url.Values
	body    any
	ifMatch string
	limit   int64
	write   bool // a write: a plain 403 means the calendar is read-only
	create  bool // a create: 409 means the id is taken
	sync    bool // an events.list with a sync token: 410 means it expired
}

// do sends c and returns the response body, or the error its status maps
// to.
func (s *Store) do(ctx context.Context, c call) ([]byte, error) {
	var body io.Reader
	if c.body != nil {
		b, err := json.Marshal(c.body)
		if err != nil {
			return nil, fmt.Errorf("%w: encoding the request", ErrRequest)
		}
		body = bytes.NewReader(b)
	}
	u := s.base + c.path
	if len(c.query) > 0 {
		u += "?" + c.query.Encode()
	}
	rctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, c.method, u, body)
	if err != nil {
		return nil, fmt.Errorf("%w: building the request", ErrRequest)
	}
	req.Header.Set("Accept", "application/json")
	if c.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.ifMatch != "" {
		req.Header.Set("If-Match", c.ifMatch)
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }() // the body has been read or is not wanted
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(resp, c)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.limit+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if int64(len(data)) > c.limit {
		return nil, fmt.Errorf("%w: response over %d bytes", calendar.ErrTooLarge, c.limit)
	}
	return data, nil
}

// transportError maps a failure to get a response. The caller's own
// cancellation is returned as is; a parked account is ErrAuth carrying the
// *auth.RequiredError and its hint; anything else is ErrUnavailable. The
// underlying error's text, which names the URL, is not repeated.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var re *auth.RequiredError
	if errors.As(err, &re) {
		return fmt.Errorf("%w: %w", calstore.ErrAuth, re)
	}
	if errors.Is(err, auth.ErrPlainHTTP) {
		return fmt.Errorf("%w: %w", calstore.ErrUnavailable, auth.ErrPlainHTTP)
	}
	return fmt.Errorf("%w: request failed", calstore.ErrUnavailable)
}

// statusError maps an unsuccessful response to calstore's errors.
func statusError(resp *http.Response, c call) error {
	code := resp.StatusCode
	var sentinel error
	switch {
	case code == http.StatusUnauthorized:
		sentinel = calstore.ErrAuth
	case code == http.StatusForbidden:
		sentinel = forbiddenError(resp, c)
	case code == http.StatusNotFound:
		sentinel = calstore.ErrNotFound
	case code == http.StatusGone && c.sync:
		sentinel = calstore.ErrSyncTokenInvalid
	case code == http.StatusGone:
		sentinel = calstore.ErrNotFound
	case code == http.StatusConflict && c.create:
		sentinel = calstore.ErrExists
	case code == http.StatusConflict, code == http.StatusPreconditionFailed:
		sentinel = calstore.ErrConflict
	case code == http.StatusTooManyRequests, code >= 500:
		sentinel = calstore.ErrUnavailable
	default:
		sentinel = ErrRequest
	}
	return fmt.Errorf("%w: status %d", sentinel, code)
}

// apiError is the part of Google's error body this package reads: the
// reasons, never the messages.
type apiError struct {
	Error struct {
		Errors []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
		Details []struct {
			Reason string `json:"reason"`
		} `json:"details"`
	} `json:"error"`
}

// Reasons Google gives in a 403.
var (
	scopeReasons = map[string]bool{
		"insufficientPermissions":         true,
		"ACCESS_TOKEN_SCOPE_INSUFFICIENT": true,
	}
	rateReasons = map[string]bool{
		"rateLimitExceeded":     true,
		"userRateLimitExceeded": true,
		"quotaExceeded":         true,
		"RATE_LIMIT_EXCEEDED":   true,
	}
)

// forbiddenError reads why Google refused a request with 403.
func forbiddenError(resp *http.Response, c call) error {
	if strings.Contains(resp.Header.Get("WWW-Authenticate"), "insufficient_scope") {
		return calstore.ErrAuth
	}
	var reasons []string
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	if err == nil {
		var e apiError
		if json.Unmarshal(data, &e) == nil {
			for _, x := range e.Error.Errors {
				reasons = append(reasons, x.Reason)
			}
			for _, x := range e.Error.Details {
				reasons = append(reasons, x.Reason)
			}
		}
	}
	for _, r := range reasons {
		if scopeReasons[r] {
			return calstore.ErrAuth
		}
	}
	for _, r := range reasons {
		if rateReasons[r] {
			return calstore.ErrUnavailable
		}
	}
	if c.write {
		return calstore.ErrReadOnly
	}
	return calstore.ErrAuth
}

// eventsPath is the escaped path of a calendar's events collection.
func eventsPath(calID string) string {
	return "/calendars/" + url.PathEscape(calID) + "/events"
}

// eventPath is the escaped path of one event.
func eventPath(calID, id string) string {
	return eventsPath(calID) + "/" + url.PathEscape(id)
}

// sendUpdates is the sendUpdates value for mode. Only NotifySend sends.
func sendUpdates(mode calendar.Notify) string {
	if mode == calendar.NotifySend {
		return "all"
	}
	return "none"
}
