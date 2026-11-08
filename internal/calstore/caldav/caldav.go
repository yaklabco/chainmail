// Package caldav is a calstore.Store over CalDAV (RFC 4791) with
// collection sync (RFC 6578) and server scheduling (RFC 6638), for servers
// such as Mailwright.
//
// Calendar IDs are collection paths and provider IDs are object paths, both
// in a canonical escaped form: each segment unescaped and escaped again with
// url.PathEscape. Requests go only to the scheme and host of the account URL,
// whatever hosts a server names in its responses or redirects.
//
// The DAV client is written here rather than taken from go-webdav, whose
// client cannot send If-Match or If-None-Match, does not bound response
// bodies, decodes calendar data with a decoder that panics on some malformed
// input, and puts response body text into its errors. go-webdav's server is
// used in the tests.
//
// Every error is a calstore sentinel, ErrRejected or ErrNotAttendee, wrapped
// with an HTTP status or a fixed reason. No response body, event text or
// calendar name reaches an error.
package caldav

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

// Limits and defaults.
const (
	// maxMultistatus bounds one DAV XML response.
	maxMultistatus = 8 << 20
	// maxErrorBody bounds the part of an error response read for its
	// precondition elements.
	maxErrorBody = 64 << 10
	// defaultTimeout bounds each request.
	defaultTimeout = 30 * time.Second
	// maxRedirects bounds the redirects one request follows.
	maxRedirects = 5
	// multigetBatch is how many objects one calendar-multiget asks for.
	multigetBatch = 50
)

// Errors this package adds to the calstore ones.
var (
	// ErrRejected means the server refused a request for a reason other
	// than those calstore names, such as invalid calendar data or too many
	// recipients.
	ErrRejected = errors.New("calendar server rejected the request")
	// ErrNotAttendee means Respond found no attendee with the address.
	ErrNotAttendee = errors.New("address is not an attendee of the event")
	// errBadID is a calendar or provider ID that is not a path this store
	// made.
	errBadID = fmt.Errorf("%w: invalid calendar object id", calstore.ErrNotFound)
)

// Store is one account's calendars on one CalDAV server. It is safe for
// concurrent use. Discovery runs on first use and is kept.
type Store struct {
	client  *http.Client
	base    *url.URL
	now     func() time.Time
	timeout time.Duration

	mu   sync.Mutex
	disc *discovery
}

var _ calstore.Store = (*Store)(nil)

// Option configures a Store.
type Option func(*Store)

// WithClock sets the clock used for DTSTAMP and LAST-MODIFIED values.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// WithRequestTimeout sets the deadline of each request (default 30s).
func WithRequestTimeout(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// New returns a store for the CalDAV server at rawURL, which discovery
// starts from. client authenticates every request, as auth.Manager's
// HTTPClient does; New uses a copy of it that follows no redirects itself.
func New(client *http.Client, rawURL string, opts ...Option) (*Store, error) {
	if client == nil {
		return nil, errors.New("caldav: nil http client")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("caldav: url must be an absolute http or https URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("caldav: url must not have user info, a query or a fragment")
	}
	p, ok := canonPath(u.EscapedPath())
	if !ok {
		return nil, errors.New("caldav: url path is not valid")
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s := &Store{
		client:  &c,
		base:    &url.URL{Scheme: u.Scheme, Host: u.Host, Path: mustUnescape(p), RawPath: p},
		now:     time.Now,
		timeout: defaultTimeout,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// request is one DAV request.
type request struct {
	method      string
	path        string // canonical escaped path
	body        []byte
	contentType string
	depth       string
	header      map[string]string
	// limit bounds a success body; tooLarge is the error for a longer one.
	limit    int64
	tooLarge error
}

// response is a received response with its body read.
type response struct {
	status int
	header http.Header
	body   []byte
	path   string // the canonical path finally answered, after redirects
}

// do sends req, following redirects to the account URL's origin itself so
// that a redirected PROPFIND or REPORT keeps its method and body. Bodies of
// 2xx responses are read up to req.limit; other bodies up to maxErrorBody.
func (s *Store) do(ctx context.Context, req request) (*response, error) {
	path := req.path
	for range maxRedirects + 1 {
		resp, err := s.roundTrip(ctx, req, path)
		if err != nil {
			return nil, err
		}
		if !isRedirect(resp.status) {
			resp.path = path
			return resp, nil
		}
		next, ok := s.redirectPath(path, resp.header.Get("Location"))
		if !ok {
			return nil, fmt.Errorf("%w: redirect refused: status %d", calstore.ErrUnavailable, resp.status)
		}
		path = next
	}
	return nil, fmt.Errorf("%w: too many redirects", calstore.ErrUnavailable)
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// redirectPath resolves a Location against the current path and accepts it
// only on the account URL's scheme and host.
func (s *Store) redirectPath(current, location string) (string, bool) {
	if location == "" {
		return "", false
	}
	loc, err := url.Parse(location)
	if err != nil {
		return "", false
	}
	target := s.urlFor(current).ResolveReference(loc)
	if target.Scheme != s.base.Scheme || target.Host != s.base.Host {
		return "", false
	}
	return canonPath(target.EscapedPath())
}

func (s *Store) roundTrip(ctx context.Context, req request, path string) (*response, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	var body io.Reader
	if req.body != nil {
		body = bytes.NewReader(req.body)
	}
	hr, err := http.NewRequestWithContext(ctx, req.method, s.urlFor(path).String(), body)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot build request", calstore.ErrUnavailable)
	}
	if req.contentType != "" {
		hr.Header.Set("Content-Type", req.contentType)
	}
	if req.depth != "" {
		hr.Header.Set("Depth", req.depth)
	}
	for k, v := range req.header {
		hr.Header.Set(k, v)
	}
	resp, err := s.client.Do(hr)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }() // the body has been read; nothing to report
	out := &response{status: resp.StatusCode, header: resp.Header}
	limit := int64(maxErrorBody)
	if resp.StatusCode/100 == 2 && req.limit > 0 {
		limit = req.limit
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if int64(len(data)) > limit {
		if resp.StatusCode/100 == 2 {
			if req.tooLarge != nil {
				return nil, req.tooLarge
			}
			return nil, fmt.Errorf("%w: response too large", calstore.ErrUnavailable)
		}
		data = data[:limit]
	}
	out.body = data
	return out, nil
}

// transportError maps a failed request: missing credentials to ErrAuth, a
// cancelled caller to its context error, anything else to ErrUnavailable.
// The underlying error is not wrapped, since its text names URLs.
func transportError(ctx context.Context, err error) error {
	var required *auth.RequiredError
	switch {
	case errors.As(err, &required), errors.Is(err, auth.ErrAuthRequired):
		return fmt.Errorf("%w: credentials unavailable", calstore.ErrAuth)
	case errors.Is(err, auth.ErrPlainHTTP):
		return fmt.Errorf("%w: %w", calstore.ErrUnavailable, auth.ErrPlainHTTP)
	}
	if cerr := context.Cause(ctx); cerr != nil && !errors.Is(cerr, context.DeadlineExceeded) {
		return cerr
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: request timed out", calstore.ErrUnavailable)
	}
	return fmt.Errorf("%w: request failed", calstore.ErrUnavailable)
}

// urlFor builds the URL of a canonical path on the account URL's origin.
func (s *Store) urlFor(path string) *url.URL {
	return &url.URL{Scheme: s.base.Scheme, Host: s.base.Host, Path: mustUnescape(path), RawPath: path}
}

// opKind says how a status maps to a calstore error.
type opKind int

const (
	opRead opKind = iota
	opSync
	opCreate
	opWrite
)

// statusError maps a non-success status. body is the start of the error
// response, read only for DAV precondition element names.
func statusError(op opKind, code int, body []byte) error {
	var sentinel error
	switch {
	case code == http.StatusUnauthorized:
		sentinel = calstore.ErrAuth
	case code == http.StatusTooManyRequests || code >= 500:
		sentinel = calstore.ErrUnavailable
	case code >= 300 && code < 400:
		// A 3xx that do does not follow: the server did not answer.
		sentinel = calstore.ErrUnavailable
	case op == opSync && (code == http.StatusGone || code == http.StatusConflict ||
		(code == http.StatusForbidden && hasElement(body, "valid-sync-token"))):
		sentinel = calstore.ErrSyncTokenInvalid
	case code == http.StatusNotFound || code == http.StatusGone:
		sentinel = calstore.ErrNotFound
	case code == http.StatusPreconditionFailed && op == opCreate:
		sentinel = calstore.ErrExists
	case code == http.StatusPreconditionFailed:
		sentinel = calstore.ErrConflict
	case code == http.StatusConflict && op == opCreate:
		sentinel = calstore.ErrExists
	case code == http.StatusForbidden && (op == opWrite || op == opCreate):
		// A refusal for privileges, or one that names no precondition, is
		// a read-only calendar. A named precondition, such as too many
		// recipients, is a rejection of this object.
		names := elementNames(body)
		if names["need-privileges"] || len(names) == 0 || onlyErrorElement(names) {
			sentinel = calstore.ErrReadOnly
		} else {
			sentinel = ErrRejected
		}
	case code == http.StatusForbidden:
		sentinel = calstore.ErrAuth
	default:
		sentinel = ErrRejected
	}
	return fmt.Errorf("%w: status %d", sentinel, code)
}

// onlyErrorElement reports whether names holds nothing but the DAV error
// wrapper.
func onlyErrorElement(names map[string]bool) bool {
	return len(names) == 1 && names["error"]
}

// canonPath returns an escaped URL path, or the path of a URL, in canonical
// form: every segment unescaped and escaped again with url.PathEscape. It
// fails on a relative path, a dot segment or a bad escape.
func canonPath(s string) (string, bool) {
	if s == "" {
		return "/", true
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", false
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if seg == "" {
			continue
		}
		raw, err := url.PathUnescape(seg)
		if err != nil || raw == "." || raw == ".." {
			return "", false
		}
		segs[i] = url.PathEscape(raw)
	}
	return strings.Join(segs, "/"), true
}

// collectionPath is canonPath with a trailing slash.
func collectionPath(s string) (string, bool) {
	p, ok := canonPath(s)
	if !ok {
		return "", false
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p, true
}

// childOf reports whether p is a direct member of the collection coll.
func childOf(coll, p string) bool {
	rest, ok := strings.CutPrefix(p, coll)
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// objectPath checks a calendar ID and a provider ID and returns them in
// canonical form.
func objectPath(calID, providerID string) (coll, href string, err error) {
	coll, ok := collectionPath(calID)
	if !ok {
		return "", "", errBadID
	}
	href, ok = canonPath(providerID)
	if !ok || !childOf(coll, href) {
		return "", "", errBadID
	}
	return coll, href, nil
}

func mustUnescape(p string) string {
	raw, err := url.PathUnescape(p)
	if err != nil {
		return p
	}
	return raw
}

// normalizeETag returns an entity tag as a quoted string, so it can be sent
// back in If-Match whatever form the server wrote it in.
func normalizeETag(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, `"`) || strings.HasPrefix(s, `W/"`) {
		return s
	}
	return `"` + s + `"`
}

// errCalendarTooLarge is the error for one calendar object over MaxICSBytes.
var errCalendarTooLarge = fmt.Errorf("%w: calendar object", calendar.ErrTooLarge)
