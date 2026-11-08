package caldav

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

// seriesICS is a weekly series in America/New_York with one moved
// instance, a property chainmail does not model, an alarm and an
// attendee parameter only the server writes.
const seriesICS = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//Other//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:series-1@example.com\r\n" +
	"DTSTAMP:20261101T000000Z\r\n" +
	"DTSTART;TZID=America/New_York:20261102T090000\r\n" +
	"DTEND;TZID=America/New_York:20261102T100000\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=10\r\n" +
	"SUMMARY:Standup\r\n" +
	"SEQUENCE:2\r\n" +
	"X-OTHER-CLIENT:keep me\r\n" +
	"ORGANIZER;CN=Bob:mailto:bob@example.com\r\n" +
	"ATTENDEE;CN=Bob;PARTSTAT=ACCEPTED:mailto:bob@example.com\r\n" +
	"ATTENDEE;CN=Jane;PARTSTAT=NEEDS-ACTION;RSVP=TRUE;SCHEDULE-STATUS=1.2:mailto:jane@example.com\r\n" +
	"BEGIN:VALARM\r\n" +
	"ACTION:DISPLAY\r\n" +
	"TRIGGER:-PT10M\r\n" +
	"DESCRIPTION:Reminder\r\n" +
	"END:VALARM\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:series-1@example.com\r\n" +
	"DTSTAMP:20261101T000000Z\r\n" +
	"RECURRENCE-ID;TZID=America/New_York:20261109T090000\r\n" +
	"DTSTART;TZID=America/New_York:20261109T110000\r\n" +
	"DTEND;TZID=America/New_York:20261109T120000\r\n" +
	"SUMMARY:Standup (moved)\r\n" +
	"SEQUENCE:2\r\n" +
	"ORGANIZER;CN=Bob:mailto:bob@example.com\r\n" +
	"ATTENDEE;CN=Bob;PARTSTAT=ACCEPTED:mailto:bob@example.com\r\n" +
	"ATTENDEE;CN=Jane;PARTSTAT=NEEDS-ACTION:mailto:jane@example.com\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// singleICS is one event with no attendees.
const singleICS = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//Other//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:single-1@example.com\r\n" +
	"DTSTAMP:20261101T000000Z\r\n" +
	"DTSTART:20261110T150000Z\r\n" +
	"DTEND:20261110T160000Z\r\n" +
	"SUMMARY:Dentist\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestNewValidatesURL(t *testing.T) {
	for _, u := range []string{"", "mail.example.com", "ftp://x/", "https://user:pw@x/", "https://x/?q=1", "https://x/#f"} {
		if _, err := New(http.DefaultClient, u); err == nil {
			t.Errorf("New(%q) = nil error", u)
		} else if strings.Contains(err.Error(), "pw") {
			t.Errorf("New(%q) error echoes the URL: %v", u, err)
		}
	}
	if _, err := New(nil, "https://x/"); err == nil {
		t.Error("New(nil client) = nil error")
	}
	if _, err := New(http.DefaultClient, "https://mail.example.com/dav/"); err != nil {
		t.Errorf("New: %v", err)
	}
}

func TestListCalendarsFromGoWebDAV(t *testing.T) {
	f := newFakeServer(t)
	s := f.store(t)
	cals, err := s.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	if len(cals) != 1 {
		t.Fatalf("got %d calendars, want 1: %+v", len(cals), cals)
	}
	c := cals[0]
	if c.ID != testCal || c.Name != "Work" || c.Access != calendar.AccessWrite {
		t.Errorf("calendar = %+v, want ID %s, name Work, write access", c, testCal)
	}
	// Discovery went through the well-known path first and is kept.
	if got := f.seen("PROPFIND"); len(got) == 0 || got[0].path != wellKnown {
		t.Errorf("first PROPFIND = %+v, want %s", got, wellKnown)
	}
	f.reset()
	if _, err := s.ListCalendars(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.seen("PROPFIND") {
		if r.path == wellKnown || r.path == testPrincipal {
			t.Errorf("discovery ran again: %s %s", r.method, r.path)
		}
	}
}

// davServer is a scripted DAV server: each handler answers one method and
// path, and anything else is a 404.
type davServer struct {
	*httptest.Server
	routes map[string]func(w http.ResponseWriter, r *http.Request)
	calls  []string
}

func newDAVServer(t *testing.T) *davServer {
	t.Helper()
	d := &davServer{routes: map[string]func(http.ResponseWriter, *http.Request){}}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.EscapedPath()
		d.calls = append(d.calls, key)
		if h, ok := d.routes[key]; ok {
			h(w, r)
			return
		}
		http.Error(w, "no route", http.StatusNotFound)
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *davServer) handle(method, path string, h func(w http.ResponseWriter, r *http.Request)) {
	d.routes[method+" "+path] = h
}

func (d *davServer) multistatus(method, path, body string) {
	d.handle(method, path, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, body)
	})
}

func (d *davServer) store(t *testing.T, path string, opts ...Option) *Store {
	t.Helper()
	s, err := New(d.Client(), d.URL+path, append([]Option{WithClock(clock)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ms(responses ...string) string {
	return `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:cs="http://calendarserver.org/ns/">` +
		strings.Join(responses, "") + `</d:multistatus>`
}

func resp(href, props string) string {
	return `<d:response><d:href>` + href + `</d:href><d:propstat><d:prop>` + props +
		`</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
}

// principalRoutes scripts discovery: a 301 from the well-known path that a
// PROPFIND must survive, the principal and its scheduling collections.
func principalRoutes(d *davServer) {
	d.handle("PROPFIND", wellKnown, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dav/", http.StatusMovedPermanently)
	})
	d.multistatus("PROPFIND", "/dav/", ms(resp("/dav/", `<d:current-user-principal><d:href>/dav/principals/jane/</d:href></d:current-user-principal>`)))
	d.multistatus("PROPFIND", "/dav/principals/jane/", ms(resp("/dav/principals/jane/",
		`<c:calendar-home-set><d:href>/dav/cal/jane/</d:href></c:calendar-home-set>`+
			`<c:schedule-inbox-URL><d:href>/dav/itip/jane/inbox/</d:href></c:schedule-inbox-URL>`+
			`<c:schedule-outbox-URL><d:href>/dav/itip/jane/outbox/</d:href></c:schedule-outbox-URL>`+
			`<c:calendar-user-address-set><d:href>mailto:jane@example.com</d:href><d:href>/dav/principals/jane/</d:href></c:calendar-user-address-set>`)))
	d.multistatus("PROPFIND", "/dav/itip/jane/inbox/", ms(resp("/dav/itip/jane/inbox/",
		`<c:schedule-default-calendar-URL><d:href>/dav/cal/jane/default/</d:href></c:schedule-default-calendar-URL>`)))
}

func TestListCalendarsPrivilegesComponentsAndDefault(t *testing.T) {
	d := newDAVServer(t)
	principalRoutes(d)
	cal := `<d:resourcetype><d:collection/><c:calendar/></d:resourcetype>`
	events := `<c:supported-calendar-component-set><c:comp name="VEVENT"/><c:comp name="VTODO"/></c:supported-calendar-component-set>`
	d.multistatus("PROPFIND", "/dav/cal/jane/", ms(
		resp("/dav/cal/jane/", `<d:resourcetype><d:collection/></d:resourcetype>`),
		resp("/dav/cal/jane/default/", cal+events+`<d:displayname>Personal&#9;</d:displayname>`+
			`<d:current-user-privilege-set><d:privilege><d:read/></d:privilege><d:privilege><d:write-content/></d:privilege></d:current-user-privilege-set>`+
			`<c:calendar-timezone>BEGIN:VCALENDAR&#13;
BEGIN:VTIMEZONE&#13;
TZID:Europe/Berlin&#13;
END:VTIMEZONE&#13;
END:VCALENDAR</c:calendar-timezone>`),
		resp("/dav/cal/jane/shared%20one", cal+events+`<d:displayname>Shared</d:displayname>`+
			`<d:current-user-privilege-set><d:privilege><d:read/></d:privilege></d:current-user-privilege-set>`),
		resp("/dav/cal/jane/tasks/", cal+`<c:supported-calendar-component-set><c:comp name="VTODO"/></c:supported-calendar-component-set>`),
		resp("/dav/cal/jane/notes/", `<d:resourcetype><d:collection/></d:resourcetype>`),
		`<d:response><d:href>/dav/cal/jane/all/</d:href><d:propstat><d:prop>`+cal+
			`<d:current-user-privilege-set><d:privilege><d:all/></d:privilege></d:current-user-privilege-set></d:prop>`+
			`<d:status>HTTP/1.1 200 OK</d:status></d:propstat><d:propstat><d:prop><d:displayname/></d:prop>`+
			`<d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response>`,
	))
	s := d.store(t, "/")
	cals, err := s.ListCalendars(context.Background())
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	want := []calendar.Calendar{
		{ID: "/dav/cal/jane/default/", Name: "Personal", Access: calendar.AccessWrite, Primary: true, TimeZone: "Europe/Berlin"},
		{ID: "/dav/cal/jane/shared%20one/", Name: "Shared", Access: calendar.AccessRead},
		{ID: "/dav/cal/jane/all/", Access: calendar.AccessWrite},
	}
	if len(cals) != len(want) {
		t.Fatalf("got %+v, want %+v", cals, want)
	}
	for i := range want {
		if cals[i] != want[i] {
			t.Errorf("calendar %d = %+v, want %+v", i, cals[i], want[i])
		}
	}
	d2, err := s.discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d2.outbox != "/dav/itip/jane/outbox/" || len(d2.addresses) != 1 || d2.addresses[0] != "jane@example.com" {
		t.Errorf("discovery = %+v", d2)
	}
}

func TestRedirectToAnotherHostIsRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("credentials followed a redirect to another host")
		w.WriteHeader(http.StatusMultiStatus)
	}))
	defer other.Close()
	d := newDAVServer(t)
	d.handle("PROPFIND", wellKnown, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/dav/", http.StatusTemporaryRedirect)
	})
	_, err := d.store(t, "/").ListCalendars(context.Background())
	if !errors.Is(err, calstore.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestGetEvent(t *testing.T) {
	f := newFakeServer(t)
	etag := f.backend.put(t, testCal+"series.ics", seriesICS)
	s := f.store(t)
	evs, err := s.GetEvent(context.Background(), testCal, testCal+"series.ics")
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want master and override", len(evs))
	}
	for _, ev := range evs {
		if ev.ProviderID != testCal+"series.ics" || ev.ETag != etag || len(ev.Raw) == 0 {
			t.Errorf("event ProviderID %q ETag %q raw %d bytes, want %q %q", ev.ProviderID, ev.ETag, len(ev.Raw), testCal+"series.ics", etag)
		}
	}
	if !evs[0].IsMaster() || evs[1].RecurrenceID.IsZero() {
		t.Errorf("events = master %v, override rid %v", evs[0].IsMaster(), evs[1].RecurrenceID)
	}

	_, err = s.GetEvent(context.Background(), testCal, testCal+"missing.ics")
	if !errors.Is(err, calstore.ErrNotFound) {
		t.Errorf("missing object: err = %v, want ErrNotFound", err)
	}
	for _, bad := range []string{"/elsewhere/x.ics", testCal, testCal + "a/b.ics", "relative.ics", testCal + "../x.ics"} {
		if _, err := s.GetEvent(context.Background(), testCal, bad); !errors.Is(err, calstore.ErrNotFound) {
			t.Errorf("GetEvent(%q): err = %v, want ErrNotFound", bad, err)
		}
	}
}

func TestStatusErrors(t *testing.T) {
	needPriv := []byte(`<d:error xmlns:d="DAV:"><d:need-privileges/></d:error>`)
	maxRecip := []byte(`<d:error xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><c:max-recipients/></d:error>`)
	syncTok := []byte(`<d:error xmlns:d="DAV:"><d:valid-sync-token/></d:error>`)
	tests := []struct {
		op   opKind
		code int
		body []byte
		want error
	}{
		{opRead, 401, nil, calstore.ErrAuth},
		{opWrite, 401, nil, calstore.ErrAuth},
		{opRead, 403, nil, calstore.ErrAuth},
		{opRead, 404, nil, calstore.ErrNotFound},
		{opRead, 410, nil, calstore.ErrNotFound},
		{opWrite, 404, nil, calstore.ErrNotFound},
		{opCreate, 409, nil, calstore.ErrExists},
		{opCreate, 412, nil, calstore.ErrExists},
		{opWrite, 412, nil, calstore.ErrConflict},
		{opWrite, 409, nil, ErrRejected},
		{opWrite, 403, nil, calstore.ErrReadOnly},
		{opWrite, 403, needPriv, calstore.ErrReadOnly},
		{opCreate, 403, needPriv, calstore.ErrReadOnly},
		{opWrite, 403, maxRecip, ErrRejected},
		{opSync, 410, nil, calstore.ErrSyncTokenInvalid},
		{opSync, 409, nil, calstore.ErrSyncTokenInvalid},
		{opSync, 403, syncTok, calstore.ErrSyncTokenInvalid},
		{opSync, 403, nil, calstore.ErrAuth},
		{opSync, 404, nil, calstore.ErrNotFound},
		{opRead, 429, nil, calstore.ErrUnavailable},
		{opWrite, 500, nil, calstore.ErrUnavailable},
		{opRead, 503, nil, calstore.ErrUnavailable},
		{opWrite, 400, nil, ErrRejected},
		{opWrite, 415, nil, ErrRejected},
	}
	for _, tc := range tests {
		err := statusError(tc.op, tc.code, tc.body)
		if !errors.Is(err, tc.want) {
			t.Errorf("statusError(%d, %d) = %v, want %v", tc.op, tc.code, err, tc.want)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("status %d", tc.code)) {
			t.Errorf("statusError(%d, %d) = %q, want the status", tc.op, tc.code, err)
		}
	}
}

func TestErrorsCarryNoResponseText(t *testing.T) {
	const secret = "Board meeting about the merger"
	for _, code := range []int{400, 401, 403, 404, 409, 412, 500} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(code)
			_, _ = fmt.Fprintf(w, `<d:error xmlns:d="DAV:"><d:responsedescription>%s</d:responsedescription></d:error>`, secret)
		}))
		s, err := New(srv.Client(), srv.URL+"/dav/", WithClock(clock))
		if err != nil {
			t.Fatal(err)
		}
		checks := map[string]error{}
		_, checks["list"] = s.ListCalendars(context.Background())
		_, checks["get"] = s.GetEvent(context.Background(), "/dav/cal/", "/dav/cal/x.ics")
		_, checks["sync"] = s.Sync(context.Background(), "/dav/cal/", "s1:abc")
		checks["delete"] = s.Delete(context.Background(), "/dav/cal/", "/dav/cal/x.ics", `"e"`, calendar.NotifySend)
		for name, err := range checks {
			if err == nil {
				t.Errorf("%d %s: nil error", code, name)
			} else if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "127.0.0.1") {
				t.Errorf("%d %s: error %q carries response text or the URL", code, name, err)
			}
		}
		srv.Close()
	}
}

func TestResponseCaps(t *testing.T) {
	d := newDAVServer(t)
	big := strings.Repeat("X", calendar.MaxICSBytes+1)
	d.handle("GET", "/cal/big.ics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"1"`)
		_, _ = fmt.Fprint(w, big)
	})
	d.handle("REPORT", "/cal/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:">`+strings.Repeat(" ", maxMultistatus)+`</d:multistatus>`)
	})
	s := d.store(t, "/")
	if _, err := s.GetEvent(context.Background(), "/cal/", "/cal/big.ics"); !errors.Is(err, calendar.ErrTooLarge) {
		t.Errorf("GetEvent of a large object: err = %v, want calendar.ErrTooLarge", err)
	}
	if _, err := s.Sync(context.Background(), "/cal/", "s1:abc"); !errors.Is(err, calstore.ErrUnavailable) {
		t.Errorf("Sync with a large multistatus: err = %v, want ErrUnavailable", err)
	}
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

func TestTransportErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"parked account", &auth.RequiredError{Account: "mw", Hint: "run: chainmail auth login mw"}, calstore.ErrAuth},
		{"plain http", auth.ErrPlainHTTP, calstore.ErrUnavailable},
		{"network", errors.New("dial tcp 10.0.0.1:443: connection refused"), calstore.ErrUnavailable},
	}
	for _, tc := range tests {
		s, err := New(&http.Client{Transport: failingTransport{tc.err}}, "https://mail.example.com/")
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.GetEvent(context.Background(), "/cal/", "/cal/x.ics")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if strings.Contains(err.Error(), "10.0.0.1") || strings.Contains(err.Error(), "mail.example.com") {
			t.Errorf("%s: error %q names the host", tc.name, err)
		}
	}
}

func TestRequestTimeoutAndCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer srv.Close()
	defer close(release)
	s, err := New(srv.Client(), srv.URL+"/", WithRequestTimeout(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEvent(context.Background(), "/cal/", "/cal/x.ics"); !errors.Is(err, calstore.ErrUnavailable) {
		t.Errorf("timed out request: err = %v, want ErrUnavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetEvent(ctx, "/cal/", "/cal/x.ics"); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled request: err = %v, want context.Canceled", err)
	}
}

func TestCanonPath(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"/dav/cal/jane/", "/dav/cal/jane/", true},
		{"/dav/cal/a%40b.ics", "/dav/cal/a@b.ics", true},
		{"/dav/cal/a@b.ics", "/dav/cal/a@b.ics", true},
		{"/dav/cal/a%2Fb.ics", "/dav/cal/a%2Fb.ics", true},
		{"/dav/cal/my%20cal/", "/dav/cal/my%20cal/", true},
		{"/dav/cal/my cal/", "/dav/cal/my%20cal/", true},
		{"https://other.example/dav/x.ics", "/dav/x.ics", true},
		{"", "/", true},
		{"relative/x", "", false},
		{"/dav/../x", "", false},
		{"/dav/%2e%2e/x", "", false},
		{"/dav/%zz", "", false},
	}
	for _, tc := range tests {
		got, ok := canonPath(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("canonPath(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if !childOf("/c/", "/c/x.ics") || childOf("/c/", "/c/") || childOf("/c/", "/c/a/b.ics") || childOf("/c/", "/d/x.ics") {
		t.Error("childOf")
	}
	if normalizeETag("abc") != `"abc"` || normalizeETag(`"abc"`) != `"abc"` || normalizeETag(`W/"abc"`) != `W/"abc"` {
		t.Error("normalizeETag")
	}
}
