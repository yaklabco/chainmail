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
