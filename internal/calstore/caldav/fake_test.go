package caldav

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	gocaldav "github.com/emersion/go-webdav/caldav"
)

// Paths of the in-memory server. go-webdav's handler infers the resource
// type from the number of path segments.
const (
	testPrincipal = "/jane/"
	testHome      = "/jane/calendars/"
	testCal       = "/jane/calendars/work/"
)

// memBackend is an in-memory go-webdav CalDAV backend.
type memBackend struct {
	mu   sync.Mutex
	cals []gocaldav.Calendar
	objs map[string]*memObject
	n    int
}

type memObject struct {
	cal  *ical.Calendar
	etag string
}

func newMemBackend() *memBackend {
	return &memBackend{
		cals: []gocaldav.Calendar{{Path: testCal, Name: "Work", SupportedComponentSet: []string{"VEVENT"}}},
		objs: map[string]*memObject{},
	}
}

func (b *memBackend) CurrentUserPrincipal(context.Context) (string, error) { return testPrincipal, nil }
func (b *memBackend) CalendarHomeSetPath(context.Context) (string, error)  { return testHome, nil }

func (b *memBackend) CreateCalendar(_ context.Context, c *gocaldav.Calendar) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cals = append(b.cals, *c)
	return nil
}

func (b *memBackend) ListCalendars(context.Context) ([]gocaldav.Calendar, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]gocaldav.Calendar(nil), b.cals...), nil
}

func (b *memBackend) GetCalendar(_ context.Context, p string) (*gocaldav.Calendar, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.cals {
		if strings.TrimSuffix(c.Path, "/") == strings.TrimSuffix(p, "/") {
			return &c, nil
		}
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, nil)
}

func (b *memBackend) object(p string, o *memObject) gocaldav.CalendarObject {
	return gocaldav.CalendarObject{Path: p, ETag: o.etag, Data: o.cal, ModTime: time.Unix(1700000000, 0)}
}

func (b *memBackend) GetCalendarObject(_ context.Context, p string, _ *gocaldav.CalendarCompRequest) (*gocaldav.CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objs[p]
	if !ok {
		return nil, webdav.NewHTTPError(http.StatusNotFound, nil)
	}
	co := b.object(p, o)
	return &co, nil
}

func (b *memBackend) ListCalendarObjects(_ context.Context, coll string, _ *gocaldav.CalendarCompRequest) ([]gocaldav.CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []gocaldav.CalendarObject
	for p, o := range b.objs {
		if strings.HasPrefix(p, coll) {
			out = append(out, b.object(p, o))
		}
	}
	return out, nil
}

func (b *memBackend) QueryCalendarObjects(ctx context.Context, coll string, _ *gocaldav.CalendarQuery) ([]gocaldav.CalendarObject, error) {
	return b.ListCalendarObjects(ctx, coll, nil)
}

func (b *memBackend) PutCalendarObject(_ context.Context, p string, cal *ical.Calendar, opts *gocaldav.PutCalendarObjectOptions) (*gocaldav.CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	old, exists := b.objs[p]
	if opts.IfNoneMatch.IsWildcard() && exists {
		return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, nil)
	}
	if opts.IfMatch.IsSet() {
		if !exists {
			return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, nil)
		}
		if ok, err := opts.IfMatch.MatchETag(old.etag); err != nil || !ok {
			return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, nil)
		}
	}
	b.n++
	o := &memObject{cal: cal, etag: "etag-" + strconv.Itoa(b.n)}
	b.objs[p] = o
	co := b.object(p, o)
	return &co, nil
}

func (b *memBackend) DeleteCalendarObject(_ context.Context, p string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.objs[p]; !ok {
		return webdav.NewHTTPError(http.StatusNotFound, nil)
	}
	delete(b.objs, p)
	return nil
}

// put stores raw ICS directly, as another client would.
func (b *memBackend) put(t *testing.T, p, raw string) string {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(raw)).Decode()
	if err != nil {
		t.Fatalf("fixture %s: %v", p, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	etag := "etag-" + strconv.Itoa(b.n)
	b.objs[p] = &memObject{cal: cal, etag: etag}
	return `"` + etag + `"`
}

// raw returns a stored object encoded, unfolded, or "" when absent.
func (b *memBackend) raw(t *testing.T, p string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objs[p]
	if !ok {
		return ""
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(o.cal); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func (b *memBackend) etag(p string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if o, ok := b.objs[p]; ok {
		return `"` + o.etag + `"`
	}
	return ""
}

// recorded is one request the fake server saw.
type recorded struct {
	method, path string
	header       http.Header
	body         string
}

// fakeServer serves a memBackend through go-webdav's caldav.Handler, adds
// If-Match to DELETE (which go-webdav ignores), records every request, and
// lets a test intercept requests.
type fakeServer struct {
	*httptest.Server
	backend *memBackend

	mu        sync.Mutex
	requests  []recorded
	intercept func(w http.ResponseWriter, r *http.Request, body []byte) bool
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{backend: newMemBackend()}
	h := &gocaldav.Handler{Backend: f.backend}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.requests = append(f.requests, recorded{method: r.Method, path: r.URL.EscapedPath(), header: r.Header.Clone(), body: string(body)})
		intercept := f.intercept
		f.mu.Unlock()
		if intercept != nil && intercept(w, r, body) {
			return
		}
		if r.Method == http.MethodDelete {
			if m := r.Header.Get("If-Match"); m != "" && f.backend.etag(r.URL.Path) != m {
				if f.backend.etag(r.URL.Path) == "" {
					http.Error(w, "missing", http.StatusNotFound)
					return
				}
				http.Error(w, "etag mismatch", http.StatusPreconditionFailed)
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) store(t *testing.T, opts ...Option) *Store {
	t.Helper()
	s, err := New(f.Client(), f.URL+"/", opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fakeServer) seen(method string) []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recorded
	for _, r := range f.requests {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeServer) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = nil
}

// fixedNow is the clock of every test store.
var fixedNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func clock() time.Time { return fixedNow }

// unfoldICS joins folded lines, for assertions on stored objects.
func unfoldICS(s string) string {
	return strings.NewReplacer("\r\n ", "", "\r\n\t", "").Replace(s)
}

// line returns the unfolded content line of s that starts with prefix, or "".
func line(s, prefix string) string {
	for _, l := range strings.Split(unfoldICS(s), "\r\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// lines returns every unfolded content line of s that starts with prefix.
func lines(s, prefix string) []string {
	var out []string
	for _, l := range strings.Split(unfoldICS(s), "\r\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}
