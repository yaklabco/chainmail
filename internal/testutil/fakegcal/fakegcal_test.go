package fakegcal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/testutil/fakegcal"
)

type client struct {
	t   *testing.T
	srv *fakegcal.Server
}

func (c client) do(method, path string, q url.Values, header http.Header, body any) (int, map[string]any) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	u := c.srv.BaseURL() + path
	if q != nil {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(context.Background(), method, u, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: body is not JSON: %v", method, path, err)
		}
	}
	return resp.StatusCode, out
}

func setup(t *testing.T) (*fakegcal.Server, client) {
	t.Helper()
	srv := fakegcal.Start(t)
	srv.AddCalendar(fakegcal.Calendar{ID: "me@example.com", Summary: "Me", AccessRole: "owner", Primary: true, TimeZone: "America/Los_Angeles"})
	srv.AddCalendar(fakegcal.Calendar{ID: "team#ro@group.calendar.google.com", Summary: "Team", AccessRole: "reader"})
	return srv, client{t: t, srv: srv}
}

func single(id string) map[string]any {
	return map[string]any{
		"id":    id,
		"start": map[string]any{"dateTime": "2026-11-10T09:00:00-08:00", "timeZone": "America/Los_Angeles"},
		"end":   map[string]any{"dateTime": "2026-11-10T10:00:00-08:00", "timeZone": "America/Los_Angeles"},
	}
}

func items(t *testing.T, page map[string]any) []string {
	t.Helper()
	raw, _ := page["items"].([]any)
	ids := make([]string, 0, len(raw))
	for _, it := range raw {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	return ids
}

func TestCalendarListPages(t *testing.T) {
	srv, c := setup(t)
	srv.SetPageSize(1)
	code, p1 := c.do("GET", "/users/me/calendarList", nil, nil, nil)
	if code != 200 || len(items(t, p1)) != 1 || p1["nextPageToken"] == nil {
		t.Fatalf("first page: %d %v", code, p1)
	}
	code, p2 := c.do("GET", "/users/me/calendarList", url.Values{"pageToken": {p1["nextPageToken"].(string)}}, nil, nil)
	if code != 200 || len(items(t, p2)) != 1 || p2["nextPageToken"] != nil {
		t.Fatalf("second page: %d %v", code, p2)
	}
	if got := items(t, p2)[0]; got != "team#ro@group.calendar.google.com" {
		t.Errorf("second calendar = %q", got)
	}
}

func TestEventsSyncTokensAndPaging(t *testing.T) {
	srv, c := setup(t)
	srv.SetPageSize(2)
	for _, id := range []string{"aaaaa1", "aaaaa2", "aaaaa3"} {
		srv.AddEvent("me@example.com", single(id))
	}
	path := "/calendars/" + url.PathEscape("me@example.com") + "/events"
	var got []string
	q := url.Values{"showDeleted": {"true"}}
	var token string
	for {
		code, page := c.do("GET", path, q, nil, nil)
		if code != 200 {
			t.Fatalf("list: %d %v", code, page)
		}
		got = append(got, items(t, page)...)
		if next, ok := page["nextPageToken"].(string); ok {
			q.Set("pageToken", next)
			continue
		}
		token = page["nextSyncToken"].(string)
		break
	}
	if strings.Join(got, ",") != "aaaaa1,aaaaa2,aaaaa3" {
		t.Fatalf("full list = %v", got)
	}

	srv.CancelEvent("me@example.com", "aaaaa2")
	code, page := c.do("GET", path, url.Values{"syncToken": {token}}, nil, nil)
	if code != 200 || strings.Join(items(t, page), ",") != "aaaaa2" {
		t.Fatalf("incremental: %d %v", code, page)
	}
	code, page = c.do("GET", path, nil, nil, nil)
	if code != 200 || strings.Join(items(t, page), ",") != "aaaaa1,aaaaa3" {
		t.Fatalf("list without showDeleted lists a deleted event: %d %v", code, page)
	}

	srv.InvalidateSyncTokens()
	if code, _ := c.do("GET", path, url.Values{"syncToken": {token}}, nil, nil); code != http.StatusGone {
		t.Errorf("invalidated token: status %d, want 410", code)
	}
	if code, _ := c.do("GET", path, url.Values{"syncToken": {"bogus"}}, nil, nil); code != http.StatusGone {
		t.Errorf("bogus token: status %d, want 410", code)
	}
	if code, _ := c.do("GET", path, url.Values{"singleEvents": {"true"}}, nil, nil); code != http.StatusBadRequest {
		t.Errorf("singleEvents=true: status %d, want 400", code)
	}
}

func TestInsertPatchDelete(t *testing.T) {
	srv, c := setup(t)
	path := "/calendars/" + url.PathEscape("me@example.com") + "/events"
	ev := single("abcdef01")
	ev["summary"] = "plan"
	code, created := c.do("POST", path, url.Values{"sendUpdates": {"none"}}, nil, ev)
	if code != 200 || created["etag"] == nil || created["iCalUID"] != "abcdef01@google.com" {
		t.Fatalf("insert: %d %v", code, created)
	}
	if org := created["organizer"].(map[string]any); org["email"] != "me@example.com" || org["self"] != true {
		t.Errorf("organizer = %v", org)
	}
	if r, _ := srv.LastRequest("POST"); r.Query.Get("sendUpdates") != "none" {
		t.Errorf("recorded sendUpdates = %q", r.Query.Get("sendUpdates"))
	}
	if code, _ := c.do("POST", path, nil, nil, ev); code != http.StatusConflict {
		t.Errorf("insert taken id: %d, want 409", code)
	}
	if code, _ := c.do("POST", path, nil, nil, single("UPPER")); code != http.StatusBadRequest {
		t.Errorf("insert bad id: %d, want 400", code)
	}

	stale := http.Header{"If-Match": {`"1"`}}
	if code, _ := c.do("PATCH", path+"/abcdef01", nil, stale, map[string]any{"summary": "x"}); code != http.StatusPreconditionFailed {
		t.Errorf("patch with stale etag: %d, want 412", code)
	}
	fresh := http.Header{"If-Match": {created["etag"].(string)}}
	code, patched := c.do("PATCH", path+"/abcdef01", nil, fresh, map[string]any{"summary": "new", "location": nil})
	if code != 200 || patched["summary"] != "new" || patched["etag"] == created["etag"] {
		t.Fatalf("patch: %d %v", code, patched)
	}
	if code, _ := c.do("DELETE", path+"/abcdef01", nil, fresh, nil); code != http.StatusPreconditionFailed {
		t.Errorf("delete with old etag: %d, want 412", code)
	}
	if code, _ := c.do("DELETE", path+"/abcdef01", nil, nil, nil); code != http.StatusNoContent {
		t.Errorf("delete: %d, want 204", code)
	}
	if code, _ := c.do("DELETE", path+"/abcdef01", nil, nil, nil); code != http.StatusGone {
		t.Errorf("second delete: %d, want 410", code)
	}
	if code, _ := c.do("POST", path, nil, nil, ev); code != http.StatusConflict {
		t.Errorf("insert over a deleted id: %d, want 409", code)
	}

	ro := "/calendars/" + url.PathEscape("team#ro@group.calendar.google.com") + "/events"
	if code, body := c.do("POST", ro, nil, nil, single("abcdef02")); code != http.StatusForbidden || !strings.Contains(mustJSON(t, body), "requiredAccessLevel") {
		t.Errorf("insert on a reader calendar: %d %v", code, body)
	}
}

func TestInstances(t *testing.T) {
	srv, c := setup(t)
	master := single("series1")
	master["recurrence"] = []any{"RRULE:FREQ=WEEKLY"}
	srv.AddEvent("me@example.com", master)
	path := "/calendars/" + url.PathEscape("me@example.com") + "/events/"
	code, inst := c.do("GET", path+"series1_20261117T170000Z", nil, nil, nil)
	if code != 200 || inst["recurringEventId"] != "series1" || inst["recurrence"] != nil {
		t.Fatalf("instance: %d %v", code, inst)
	}
	if start := inst["start"].(map[string]any); start["dateTime"] != "2026-11-17T09:00:00-08:00" {
		t.Errorf("instance start = %v", start)
	}
	h := http.Header{"If-Match": {inst["etag"].(string)}}
	code, patched := c.do("PATCH", path+"series1_20261117T170000Z", nil, h, map[string]any{"summary": "moved"})
	if code != 200 || patched["summary"] != "moved" {
		t.Fatalf("patch instance: %d %v", code, patched)
	}
	if _, ok := srv.Event("me@example.com", "series1_20261117T170000Z"); !ok {
		t.Error("patched instance not stored as an exception")
	}
	if code, _ := c.do("GET", path+"series1_bogus", nil, nil, nil); code != http.StatusNotFound {
		t.Errorf("bad instance id: %d, want 404", code)
	}
}

func TestFreeBusy(t *testing.T) {
	srv, c := setup(t)
	day := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	srv.SetFreeBusy("Ann@Example.com", fakegcal.Busy{Start: day.Add(9 * time.Hour), End: day.Add(10 * time.Hour)})
	body := map[string]any{
		"timeMin": day.Format(time.RFC3339), "timeMax": day.Add(24 * time.Hour).Format(time.RFC3339),
		"items": []map[string]any{{"id": "ann@example.com"}, {"id": "nobody@example.com"}},
	}
	code, out := c.do("POST", "/freeBusy", nil, nil, body)
	if code != 200 {
		t.Fatalf("freeBusy: %d %v", code, out)
	}
	cals := out["calendars"].(map[string]any)
	ann := cals["ann@example.com"].(map[string]any)
	if len(ann["busy"].([]any)) != 1 || ann["errors"] != nil {
		t.Errorf("ann = %v", ann)
	}
	if nobody := cals["nobody@example.com"].(map[string]any); nobody["errors"] == nil {
		t.Errorf("unknown person has no error: %v", nobody)
	}
	var many []map[string]any
	for range fakegcal.MaxFreeBusyItems + 1 {
		many = append(many, map[string]any{"id": "x@example.com"})
	}
	body["items"] = many
	if code, _ := c.do("POST", "/freeBusy", nil, nil, body); code != http.StatusBadRequest {
		t.Errorf("too many people: %d, want 400", code)
	}
}

func TestTokenAndFaults(t *testing.T) {
	srv, c := setup(t)
	srv.RequireToken("tok")
	if code, _ := c.do("GET", "/users/me/calendarList", nil, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", code)
	}
	auth := http.Header{"Authorization": {"Bearer tok"}}
	srv.Inject(fakegcal.Fault{Method: "GET", Path: "/calendarList", Status: 503, Body: `{}`, Times: 2})
	for i := range 2 {
		if code, _ := c.do("GET", "/users/me/calendarList", nil, auth, nil); code != 503 {
			t.Errorf("fault %d: %d, want 503", i, code)
		}
	}
	if code, _ := c.do("GET", "/users/me/calendarList", nil, auth, nil); code != 200 {
		t.Errorf("after faults: %d, want 200", code)
	}

	srv.Inject(fakegcal.Fault{Block: true})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", srv.BaseURL()+"/users/me/calendarList", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("blocked request answered")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("blocked request: %v, want deadline exceeded", err)
	}
	if n := len(srv.Requests()); n != 5 {
		t.Errorf("recorded %d requests, want 5", n)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPatchMergesNestedObjects(t *testing.T) {
	srv, c := setup(t)
	srv.AddEvent("me@example.com", single("merge1"))
	path := "/calendars/" + url.PathEscape("me@example.com") + "/events/merge1"

	// A nested object merges: a date without nulling dateTime leaves both,
	// which Google refuses.
	code, _ := c.do("PATCH", path, nil, nil, map[string]any{
		"start": map[string]any{"date": "2026-11-10"}, "end": map[string]any{"date": "2026-11-11"},
	})
	if code != http.StatusBadRequest {
		t.Errorf("date over a dateTime without null: %d, want 400", code)
	}
	ev, _ := srv.Event("me@example.com", "merge1")
	if start := ev["start"].(map[string]any); start["dateTime"] != "2026-11-10T09:00:00-08:00" || start["date"] != nil {
		t.Errorf("refused patch changed the event: %v", start)
	}

	// Nulling the other fields converts the event to all-day.
	code, out := c.do("PATCH", path, nil, nil, map[string]any{
		"start": map[string]any{"date": "2026-11-10", "dateTime": nil, "timeZone": nil},
		"end":   map[string]any{"date": "2026-11-11", "dateTime": nil, "timeZone": nil},
	})
	if code != 200 {
		t.Fatalf("to all-day: %d %v", code, out)
	}
	if start := out["start"].(map[string]any); len(start) != 1 || start["date"] != "2026-11-10" {
		t.Errorf("all-day start = %v", start)
	}

	// Back to timed: the date is nulled, the other fields of the object
	// merge in.
	code, out = c.do("PATCH", path, nil, nil, map[string]any{
		"start": map[string]any{"date": nil, "dateTime": "2026-11-10T10:00:00-08:00", "timeZone": "America/Los_Angeles"},
		"end":   map[string]any{"date": nil, "dateTime": "2026-11-10T11:00:00-08:00", "timeZone": "America/Los_Angeles"},
	})
	if code != 200 {
		t.Fatalf("to timed: %d %v", code, out)
	}
	if start := out["start"].(map[string]any); len(start) != 2 || start["dateTime"] != "2026-11-10T10:00:00-08:00" {
		t.Errorf("timed start = %v", start)
	}

	// A nested field merges without touching its siblings, and arrays are
	// replaced whole.
	srv.ChangeEvent("me@example.com", "merge1", map[string]any{"attendees": []any{
		map[string]any{"email": "a@example.org"}, map[string]any{"email": "b@example.org"},
	}})
	code, out = c.do("PATCH", path, nil, nil, map[string]any{
		"start":     map[string]any{"timeZone": "America/New_York"},
		"attendees": []any{map[string]any{"email": "b@example.org"}},
	})
	if code != 200 {
		t.Fatalf("nested merge: %d %v", code, out)
	}
	if start := out["start"].(map[string]any); start["dateTime"] != "2026-11-10T10:00:00-08:00" || start["timeZone"] != "America/New_York" {
		t.Errorf("merged start = %v", start)
	}
	if got := out["attendees"].([]any); len(got) != 1 {
		t.Errorf("array not replaced: %v", got)
	}

	// Start and end must be of one kind.
	if code, _ := c.do("PATCH", path, nil, nil, map[string]any{
		"start": map[string]any{"date": "2026-11-10", "dateTime": nil, "timeZone": nil},
	}); code != http.StatusBadRequest {
		t.Errorf("all-day start with a timed end: %d, want 400", code)
	}
}

func withAttendees(id string) map[string]any {
	ev := single(id)
	ev["attendees"] = []any{
		map[string]any{"email": "boss@example.org", "organizer": true, "responseStatus": "accepted"},
		map[string]any{"email": "me@example.com", "self": true, "responseStatus": "needsAction", "additionalGuests": 1},
		map[string]any{"email": "ann@example.org", "responseStatus": "tentative"},
	}
	return ev
}

func TestAttendeesOmitted(t *testing.T) {
	srv, c := setup(t)
	srv.AddEvent("me@example.com", withAttendees("omit1"))
	path := "/calendars/" + url.PathEscape("me@example.com") + "/events/omit1"

	// Over the limit, only the participant is shown and attendeesOmitted is
	// set, as Google answers with maxAttendees.
	code, ev := c.do("GET", path, url.Values{"maxAttendees": {"2"}}, nil, nil)
	if code != 200 || ev["attendeesOmitted"] != true || len(ev["attendees"].([]any)) != 1 {
		t.Fatalf("maxAttendees=2: %d %v", code, ev)
	}
	srv.SetMaxAttendees(2)
	code, ev = c.do("GET", path, nil, nil, nil)
	if code != 200 || ev["attendeesOmitted"] != true || len(ev["attendees"].([]any)) != 1 {
		t.Fatalf("SetMaxAttendees(2): %d %v", code, ev)
	}
	code, page := c.do("GET", "/calendars/"+url.PathEscape("me@example.com")+"/events", nil, nil, nil)
	if it := page["items"].([]any)[0].(map[string]any); code != 200 || it["attendeesOmitted"] != true || len(it["attendees"].([]any)) != 1 {
		t.Fatalf("list with SetMaxAttendees(2): %d %v", code, page)
	}
	srv.SetMaxAttendees(0)

	// attendeesOmitted on a PATCH updates only the attendees it names.
	code, out := c.do("PATCH", path, nil, nil, map[string]any{
		"attendeesOmitted": true,
		"attendees":        []any{map[string]any{"email": "ME@example.com", "responseStatus": "accepted"}},
	})
	if code != 200 {
		t.Fatalf("participant patch: %d %v", code, out)
	}
	stored, _ := srv.Event("me@example.com", "omit1")
	atts := stored["attendees"].([]any)
	if len(atts) != 3 {
		t.Fatalf("participant patch replaced the list: %v", atts)
	}
	self := atts[1].(map[string]any)
	if self["responseStatus"] != "accepted" || self["additionalGuests"] != float64(1) || self["email"] != "me@example.com" {
		t.Errorf("self after participant patch = %v", self)
	}
	if _, ok := stored["attendeesOmitted"]; ok {
		t.Error("attendeesOmitted stored on the event")
	}

	// Without it the array is replaced.
	if code, _ := c.do("PATCH", path, nil, nil, map[string]any{
		"attendees": []any{map[string]any{"email": "me@example.com", "responseStatus": "declined"}},
	}); code != 200 {
		t.Fatalf("plain patch: %d", code)
	}
	stored, _ = srv.Event("me@example.com", "omit1")
	if n := len(stored["attendees"].([]any)); n != 1 {
		t.Errorf("plain attendee patch kept %d attendees, want 1", n)
	}
}
