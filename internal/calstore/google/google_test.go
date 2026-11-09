package google_test

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the tests use IANA zones on hosts without zoneinfo

	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	"github.com/yaklabco/chainmail/internal/calstore/google"
	"github.com/yaklabco/chainmail/internal/testutil/fakegcal"
)

var _ calstore.Store = (*google.Store)(nil)

const (
	me       = "me@example.com"
	readOnly = "team#ro@group.calendar.google.com"
	la       = "America/Los_Angeles"
	// secretBody is response text no error may repeat.
	secretBody = "SECRET-BODY-TEXT do not echo"
)

func newStore(t *testing.T, opts ...google.Option) (*google.Store, *fakegcal.Server) {
	t.Helper()
	srv := fakegcal.Start(t)
	srv.AddCalendar(fakegcal.Calendar{ID: me, Summary: "Me", AccessRole: "owner", Primary: true, TimeZone: la})
	srv.AddCalendar(fakegcal.Calendar{ID: readOnly, Summary: "Team", AccessRole: "reader"})
	opts = append([]google.Option{google.WithBaseURL(srv.BaseURL())}, opts...)
	return google.New(&http.Client{}, opts...), srv
}

func ctx(t *testing.T) context.Context {
	t.Helper()
	return t.Context()
}

func TestObjectID(t *testing.T) {
	// Computed outside Go: printf %s abc123@chainmail | sha256sum, then
	// base32hex without padding, lowercased, first 32 characters.
	if got, want := google.ObjectID("abc123@chainmail"), "2ift10ggkeebhjm18561uc205u885lcc"; got != want {
		t.Errorf("ObjectID = %q, want %q", got, want)
	}
}

func TestInstanceID(t *testing.T) {
	rid := time.Date(2026, 11, 17, 17, 0, 0, 0, time.UTC)
	if got := google.InstanceID("series1", rid, false); got != "series1_20261117T170000Z" {
		t.Errorf("timed instance id = %q", got)
	}
	if got := google.InstanceID("series1", time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC), true); got != "series1_20261117" {
		t.Errorf("all-day instance id = %q", got)
	}
}

func TestListCalendars(t *testing.T) {
	st, srv := newStore(t)
	srv.AddCalendar(fakegcal.Calendar{ID: "w@group.calendar.google.com", Summary: "Shared", SummaryOverride: "My name", AccessRole: "writer", TimeZone: "Not/AZone"})
	srv.AddCalendar(fakegcal.Calendar{ID: "fb@example.com", Summary: "Busy only", AccessRole: "freeBusyReader", TimeZone: "UTC"})
	srv.AddCalendar(fakegcal.Calendar{ID: "etc@example.com", Summary: "Etc", AccessRole: "reader", TimeZone: "Etc/UTC"})
	srv.AddCalendar(fakegcal.Calendar{ID: "local@example.com", Summary: "Local", AccessRole: "reader", TimeZone: "Local"})
	srv.SetPageSize(1)
	cals, err := st.ListCalendars(ctx(t))
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	want := []calendar.Calendar{
		{ID: me, Name: "Me", Access: calendar.AccessWrite, Primary: true, TimeZone: la},
		{ID: readOnly, Name: "Team", Access: calendar.AccessRead},
		{ID: "w@group.calendar.google.com", Name: "My name", Access: calendar.AccessWrite},
		{ID: "fb@example.com", Name: "Busy only", Access: calendar.AccessRead, TimeZone: "UTC"},
		{ID: "etc@example.com", Name: "Etc", Access: calendar.AccessRead, TimeZone: "Etc/UTC"},
		{ID: "local@example.com", Name: "Local", Access: calendar.AccessRead},
	}
	if len(cals) != len(want) {
		t.Fatalf("got %d calendars, want %d: %+v", len(cals), len(want), cals)
	}
	for i := range want {
		if cals[i] != want[i] {
			t.Errorf("calendar %d = %+v, want %+v", i, cals[i], want[i])
		}
	}
	pages := 0
	for _, r := range srv.Requests() {
		if r.Path == "/users/me/calendarList" {
			pages++
		}
	}
	if pages != len(want) {
		t.Errorf("calendar list took %d requests, want %d pages", pages, len(want))
	}
}

func TestErrorMapping(t *testing.T) {
	scopeDetails := `{"error":{"code":403,"message":"` + secretBody + `","status":"PERMISSION_DENIED",` +
		`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"ACCESS_TOKEN_SCOPE_INSUFFICIENT"}]}}`
	reason := func(code int, r string) string {
		return `{"error":{"code":` + itoa(code) + `,"message":"` + secretBody + `","errors":[{"domain":"global","reason":"` + r + `","message":"` + secretBody + `"}]}}`
	}
	cases := []struct {
		name   string
		op     string
		status int
		body   string
		header http.Header
		want   error
	}{
		{"401", "list", 401, reason(401, "authError"), nil, calstore.ErrAuth},
		{"403 insufficientPermissions", "list", 403, reason(403, "insufficientPermissions"), nil, calstore.ErrAuth},
		{"403 scope detail", "sync", 403, scopeDetails, nil, calstore.ErrAuth},
		{"403 insufficient_scope header", "get", 403, "{}", http.Header{"Www-Authenticate": {`Bearer error="insufficient_scope"`}}, calstore.ErrAuth},
		{"403 scope on a write", "create", 403, reason(403, "insufficientPermissions"), nil, calstore.ErrAuth},
		{"403 rateLimitExceeded", "sync", 403, reason(403, "rateLimitExceeded"), nil, calstore.ErrUnavailable},
		{"403 userRateLimitExceeded on a write", "create", 403, reason(403, "userRateLimitExceeded"), nil, calstore.ErrUnavailable},
		{"403 requiredAccessLevel on a write", "create", 403, reason(403, "requiredAccessLevel"), nil, calstore.ErrReadOnly},
		{"403 forbidden on a delete", "delete", 403, reason(403, "forbidden"), nil, calstore.ErrReadOnly},
		{"403 forbidden on a read", "get", 403, reason(403, "forbidden"), nil, calstore.ErrAuth},
		{"404 on get", "get", 404, reason(404, "notFound"), nil, calstore.ErrNotFound},
		{"410 on get", "get", 410, reason(410, "deleted"), nil, calstore.ErrNotFound},
		{"410 on delete", "delete", 410, reason(410, "deleted"), nil, calstore.ErrNotFound},
		{"410 on sync", "sync", 410, reason(410, "fullSyncRequired"), nil, calstore.ErrSyncTokenInvalid},
		{"409 on create", "create", 409, reason(409, "duplicate"), nil, calstore.ErrExists},
		{"412 on delete", "delete", 412, reason(412, "conditionNotMet"), nil, calstore.ErrConflict},
		{"429", "list", 429, reason(429, "rateLimitExceeded"), nil, calstore.ErrUnavailable},
		{"500", "freebusy", 500, secretBody, nil, calstore.ErrUnavailable},
		{"503", "sync", 503, secretBody, nil, calstore.ErrUnavailable},
		{"400", "create", 400, reason(400, "invalid"), nil, google.ErrRequest},
		{"302 redirect refused", "list", 302, "", nil, calstore.ErrUnavailable},
		{"304 on a write", "create", 304, "", nil, calstore.ErrUnavailable},
		{"200 not JSON", "list", 200, secretBody, nil, calstore.ErrUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, srv := newStore(t)
			srv.AddEvent(me, timedEvent("single1"))
			srv.Inject(fakegcal.Fault{Status: tc.status, Body: tc.body, Header: tc.header})
			err := runOp(t, st, tc.op)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error repeats the response body: %q", err)
			}
			if tc.status != 200 && !strings.Contains(err.Error(), "status "+itoa(tc.status)) {
				t.Errorf("error %q does not name status %d", err, tc.status)
			}
		})
	}
}

// runOp calls the Store method named by op and returns its error.
func runOp(t *testing.T, st *google.Store, op string) error {
	t.Helper()
	switch op {
	case "list":
		_, err := st.ListCalendars(ctx(t))
		return err
	case "sync":
		_, err := st.Sync(ctx(t), me, "")
		return err
	case "get":
		_, err := st.GetEvent(ctx(t), me, "single1")
		return err
	case "create":
		_, err := st.Put(ctx(t), me, newEvent("uid-1@chainmail"), "", calendar.NotifyNone)
		return err
	case "delete":
		return st.Delete(ctx(t), me, "single1", `"1"`, calendar.NotifyNone)
	case "freebusy":
		from := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
		_, err := st.FreeBusy(ctx(t), []string{"ann@example.com"}, from, from.Add(24*time.Hour))
		return err
	}
	t.Fatalf("unknown op %q", op)
	return nil
}

func TestResponseSizeCaps(t *testing.T) {
	big := `{"items":[],"x":"` + strings.Repeat("a", 8<<20) + `"}`
	st, srv := newStore(t)
	srv.Inject(fakegcal.Fault{Status: 200, Body: big})
	if _, err := st.Sync(ctx(t), me, ""); !errors.Is(err, calendar.ErrTooLarge) {
		t.Errorf("events page over 8 MiB: err = %v, want ErrTooLarge", err)
	}
	srv.Inject(fakegcal.Fault{Status: 200, Body: big})
	if _, err := st.ListCalendars(ctx(t)); !errors.Is(err, calendar.ErrTooLarge) {
		t.Errorf("calendar list over 8 MiB: err = %v, want ErrTooLarge", err)
	}
	srv.AddEvent(me, timedEvent("single1"))
	one := `{"id":"single1","x":"` + strings.Repeat("a", calendar.MaxICSBytes) + `"}`
	srv.Inject(fakegcal.Fault{Method: "GET", Path: "/events/single1", Status: 200, Body: one})
	if _, err := st.GetEvent(ctx(t), me, "single1"); !errors.Is(err, calendar.ErrTooLarge) {
		t.Errorf("one event over MaxICSBytes: err = %v, want ErrTooLarge", err)
	}
	// The fault is used up, so the same event reads normally.
	if _, err := st.GetEvent(ctx(t), me, "single1"); err != nil {
		t.Errorf("GetEvent after the fault: %v", err)
	}
}

func TestNetworkErrors(t *testing.T) {
	st, srv := newStore(t, google.WithRequestTimeout(20*time.Millisecond))
	srv.Inject(fakegcal.Fault{Block: true})
	if _, err := st.ListCalendars(ctx(t)); !errors.Is(err, calstore.ErrUnavailable) {
		t.Errorf("request past its deadline: err = %v, want ErrUnavailable", err)
	}

	c, cancel := context.WithCancel(ctx(t))
	cancel()
	if _, err := st.ListCalendars(c); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: err = %v, want context.Canceled", err)
	}

	dead := google.New(&http.Client{}, google.WithBaseURL("http://127.0.0.1:1/calendar/v3"))
	if _, err := dead.ListCalendars(ctx(t)); !errors.Is(err, calstore.ErrUnavailable) {
		t.Errorf("refused connection: err = %v, want ErrUnavailable", err)
	}
}

type headerTransport struct{ token string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	return http.DefaultTransport.RoundTrip(r)
}

type parkedTransport struct{}

func (parkedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_ = r.Body.Close()
	}
	return nil, &auth.RequiredError{Account: "acct", Hint: "run: chainmail auth login acct"}
}

func TestClientTransport(t *testing.T) {
	srv := fakegcal.Start(t)
	srv.AddCalendar(fakegcal.Calendar{ID: me, Summary: "Me"})
	srv.RequireToken("tok")

	good := google.New(&http.Client{Transport: headerTransport{token: "tok"}}, google.WithBaseURL(srv.BaseURL()))
	if _, err := good.ListCalendars(ctx(t)); err != nil {
		t.Errorf("with the token: %v", err)
	}
	bad := google.New(&http.Client{Transport: headerTransport{token: "wrong"}}, google.WithBaseURL(srv.BaseURL()))
	if _, err := bad.ListCalendars(ctx(t)); !errors.Is(err, calstore.ErrAuth) {
		t.Errorf("with a bad token: err = %v, want ErrAuth", err)
	}

	parked := google.New(&http.Client{Transport: parkedTransport{}}, google.WithBaseURL(srv.BaseURL()))
	_, err := parked.ListCalendars(ctx(t))
	var re *auth.RequiredError
	if !errors.Is(err, calstore.ErrAuth) || !errors.As(err, &re) {
		t.Errorf("parked account: err = %v, want ErrAuth carrying *auth.RequiredError", err)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	var got string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.URL.String()
		return nil, errors.New("stop")
	})}
	_, _ = google.New(hc).ListCalendars(ctx(t))
	if !strings.HasPrefix(got, "https://www.googleapis.com/calendar/v3/users/me/calendarList?") {
		t.Errorf("default request URL = %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func itoa(n int) string { return strconv.Itoa(n) }

// TestStoreObjectID checks that the store names a create's object as the
// package function does, whatever the calendar, so the daemon can read back
// an event a crashed create left.
func TestStoreObjectID(t *testing.T) {
	st := google.New(http.DefaultClient)
	if got, want := st.ObjectID("primary", "abc123@chainmail"), google.ObjectID("abc123@chainmail"); got != want {
		t.Errorf("ObjectID = %q, want %q", got, want)
	}
}
