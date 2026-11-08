// Package fakegcal runs an in-memory Google Calendar API v3 server for
// calstore/google tests, as oauthtest runs a token endpoint. It keeps
// calendars and events as JSON objects and serves the endpoints the client
// uses:
//
//   - GET users/me/calendarList, paged.
//   - GET calendars/{id}/events with syncToken, pageToken, maxResults,
//     showDeleted, singleEvents=false and iCalUID. A sync token from before
//     InvalidateSyncTokens, or one the fake did not issue, answers 410.
//     Cancelled instances of a series are listed even without showDeleted,
//     as Google does with singleEvents=false; deleted events need
//     showDeleted or a sync token.
//   - GET, PATCH and DELETE calendars/{id}/events/{eventId}, with If-Match
//     (412 when it does not match). An instance id, <id>_<YYYYMMDDTHHMMSSZ>
//     or <id>_<YYYYMMDD>, names one occurrence of a series: a GET makes it up
//     from the master, a PATCH stores it as an exception and a DELETE stores
//     a cancelled exception.
//   - POST calendars/{id}/events with a client id (409 when taken, 400 when
//     it is not 5 to 1024 characters of lowercase base32hex).
//   - POST freeBusy for people set with SetFreeBusy; anyone else gets a
//     notFound error, as Google answers for a calendar it cannot see.
//
// Writes to a calendar whose access role is reader or freeBusyReader answer
// 403 requiredAccessLevel. PATCH merges top-level fields and a JSON null
// removes one, as Google's patch semantics do. Every request is recorded,
// including its sendUpdates parameter, so tests can prove what a write asked
// Google to send.
//
// Tests can make it fail: Inject answers matching requests with a fixed
// status, body and headers, or blocks them until the client gives up.
package fakegcal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// BasePath is the path under the server URL where the API is served.
const BasePath = "/calendar/v3"

// MaxFreeBusyItems is the most people one freeBusy query may name.
const MaxFreeBusyItems = 50

// defaultPageSize is the page size when neither the request nor
// SetPageSize sets a smaller one.
const defaultPageSize = 250

// Calendar is one entry of the fake's calendar list.
type Calendar struct {
	ID              string
	Summary         string
	SummaryOverride string
	TimeZone        string
	AccessRole      string // owner, writer, reader or freeBusyReader
	Primary         bool
	// Owner is the organizer address of events created through the API.
	// It defaults to ID.
	Owner string
}

// Busy is one busy block of a person.
type Busy struct{ Start, End time.Time }

// Request is one request the fake received.
type Request struct {
	Method        string
	Path          string // unescaped, below BasePath
	Query         url.Values
	IfMatch       string
	Authorization string
	Body          []byte
}

// Fault answers requests that match Method and Path instead of the fake.
type Fault struct {
	Method string // "" matches every method
	Path   string // a suffix of Request.Path; "" matches every path
	Status int
	Body   string
	Header http.Header
	// Block holds the request until the client cancels it, and then
	// answers nothing.
	Block bool
	// Times is how many requests the fault answers; 0 means one.
	Times int
}

// Server is a fake Calendar API server.
type Server struct {
	URL string

	t testing.TB

	mu       sync.Mutex
	cals     map[string]*fakeCalendar
	order    []string
	busy     map[string][]Busy
	reqs     []Request
	faults   []Fault
	pageSize int
	token    string
	gen      int
	etags    int
	now      func() time.Time
}

type fakeCalendar struct {
	Calendar
	seq    int
	events map[string]*storedEvent
}

type storedEvent struct {
	obj map[string]any
	seq int
}

// Start runs a Server until the test ends.
func Start(t testing.TB) *Server {
	t.Helper()
	s := &Server{
		t:     t,
		cals:  map[string]*fakeCalendar{},
		busy:  map[string][]Busy{},
		now:   func() time.Time { return time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC) },
		etags: 3393000,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+BasePath+"/users/me/calendarList", s.calendarList)
	mux.HandleFunc("GET "+BasePath+"/calendars/{cal}/events", s.listEvents)
	mux.HandleFunc("POST "+BasePath+"/calendars/{cal}/events", s.insertEvent)
	mux.HandleFunc("GET "+BasePath+"/calendars/{cal}/events/{id}", s.getEvent)
	mux.HandleFunc("PATCH "+BasePath+"/calendars/{cal}/events/{id}", s.patchEvent)
	mux.HandleFunc("DELETE "+BasePath+"/calendars/{cal}/events/{id}", s.deleteEvent)
	mux.HandleFunc("POST "+BasePath+"/freeBusy", s.freeBusy)
	srv := httptest.NewServer(s.front(mux))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// BaseURL is the API base URL to give the client.
func (s *Server) BaseURL() string { return s.URL + BasePath }

// SetPageSize caps every page at n entries, so a test can force paging.
func (s *Server) SetPageSize(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pageSize = n
}

// RequireToken makes every request need "Authorization: Bearer tok".
func (s *Server) RequireToken(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = tok
}

// SetNow sets the clock used for updated and created times.
func (s *Server) SetNow(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// InvalidateSyncTokens makes every sync token issued so far answer 410.
func (s *Server) InvalidateSyncTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
}

// Inject queues a fault. Faults are tried in the order they were queued.
func (s *Server) Inject(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Times <= 0 {
		f.Times = 1
	}
	s.faults = append(s.faults, f)
}

// Requests returns the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// LastRequest returns the last request with method, and false when there
// was none.
func (s *Server) LastRequest(method string) (Request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.reqs) - 1; i >= 0; i-- {
		if s.reqs[i].Method == method {
			return s.reqs[i], true
		}
	}
	return Request{}, false
}

// AddCalendar adds a calendar to the list.
func (s *Server) AddCalendar(c Calendar) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.Owner == "" {
		c.Owner = c.ID
	}
	if c.AccessRole == "" {
		c.AccessRole = "owner"
	}
	if _, ok := s.cals[c.ID]; !ok {
		s.order = append(s.order, c.ID)
	}
	s.cals[c.ID] = &fakeCalendar{Calendar: c, events: map[string]*storedEvent{}}
}

// AddEvent stores ev in calendar calID as someone else's client would, and
// returns its etag. It fills etag, updated, status and iCalUID when absent.
func (s *Server) AddEvent(calID string, ev map[string]any) string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.mustCalendar(calID)
	obj := deepCopy(ev)
	id, _ := obj["id"].(string)
	if id == "" {
		s.t.Fatalf("fakegcal: AddEvent needs an id")
	}
	if _, ok := obj["status"]; !ok {
		obj["status"] = "confirmed"
	}
	if _, ok := obj["iCalUID"]; !ok {
		obj["iCalUID"] = id + "@google.com"
	}
	obj["kind"] = "calendar#event"
	s.store(c, id, obj)
	return obj["etag"].(string)
}

// ChangeEvent merges patch into a stored event, as an organizer's change
// would, and returns the new etag.
func (s *Server) ChangeEvent(calID, id string, patch map[string]any) string {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.mustCalendar(calID)
	se, ok := c.events[id]
	if !ok {
		s.t.Fatalf("fakegcal: no event %q", id)
	}
	obj := deepCopy(se.obj)
	mergePatch(obj, deepCopy(patch))
	s.store(c, id, obj)
	return obj["etag"].(string)
}

// CancelEvent marks a stored event cancelled, as deleting it in Google
// Calendar does.
func (s *Server) CancelEvent(calID, id string) {
	s.ChangeEvent(calID, id, map[string]any{"status": "cancelled"})
}

// Event returns a copy of a stored event, and false when there is none.
func (s *Server) Event(calID, id string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cals[calID]
	if !ok {
		return nil, false
	}
	se, ok := c.events[id]
	if !ok {
		return nil, false
	}
	return deepCopy(se.obj), true
}

// SetFreeBusy sets the busy blocks freeBusy reports for email.
func (s *Server) SetFreeBusy(email string, busy ...Busy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy[strings.ToLower(email)] = slices.Clone(busy)
}

func (s *Server) mustCalendar(calID string) *fakeCalendar {
	c, ok := s.cals[calID]
	if !ok {
		s.t.Fatalf("fakegcal: no calendar %q", calID)
	}
	return c
}

// store saves obj under id with a new etag, updated time and change
// sequence. s.mu is held.
func (s *Server) store(c *fakeCalendar, id string, obj map[string]any) {
	s.etags++
	obj["etag"] = `"` + strconv.Itoa(s.etags) + `"`
	obj["updated"] = s.now().UTC().Format("2006-01-02T15:04:05.000Z")
	obj["id"] = id
	c.seq++
	c.events[id] = &storedEvent{obj: obj, seq: c.seq}
}

// front records each request, checks the token and applies faults before
// the API handlers run.
func (s *Server) front(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		req := Request{
			Method:        r.Method,
			Path:          strings.TrimPrefix(r.URL.Path, BasePath),
			Query:         r.URL.Query(),
			IfMatch:       r.Header.Get("If-Match"),
			Authorization: r.Header.Get("Authorization"),
			Body:          body,
		}
		s.mu.Lock()
		s.reqs = append(s.reqs, req)
		token := s.token
		authorized := token == "" || req.Authorization == "Bearer "+token
		var f Fault
		var faulted bool
		if authorized {
			f, faulted = s.takeFault(req)
		}
		s.mu.Unlock()
		if !authorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://accounts.google.com/", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "authError", "Invalid Credentials")
			return
		}
		if faulted {
			if f.Block {
				<-r.Context().Done()
				return
			}
			maps.Copy(w.Header(), f.Header)
			w.WriteHeader(f.Status)
			_, _ = io.WriteString(w, f.Body) // the client sees a short body
			return
		}
		next.ServeHTTP(w, r)
	})
}

// takeFault returns the first queued fault matching req and uses up one of
// its times. s.mu is held.
func (s *Server) takeFault(req Request) (Fault, bool) {
	for i, f := range s.faults {
		if (f.Method == "" || f.Method == req.Method) && strings.HasSuffix(req.Path, f.Path) {
			s.faults[i].Times--
			if s.faults[i].Times <= 0 {
				s.faults = slices.Delete(s.faults, i, i+1)
			}
			return f, true
		}
	}
	return Fault{}, false
}

func (s *Server) pageLimit(r *http.Request) int {
	size := defaultPageSize
	if n, err := strconv.Atoi(r.URL.Query().Get("maxResults")); err == nil && n > 0 && n < size {
		size = n
	}
	if s.pageSize > 0 && s.pageSize < size {
		size = s.pageSize
	}
	return size
}

func (s *Server) calendarList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	offset := 0
	if pt := r.URL.Query().Get("pageToken"); pt != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(pt, "cl:"))
		if err != nil || !strings.HasPrefix(pt, "cl:") || n < 0 || n > len(s.order) {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid page token")
			return
		}
		offset = n
	}
	end := min(offset+s.pageLimit(r), len(s.order))
	items := []map[string]any{}
	for _, id := range s.order[offset:end] {
		c := s.cals[id]
		item := map[string]any{
			"kind":       "calendar#calendarListEntry",
			"etag":       `"cl` + strconv.Itoa(len(id)) + `"`,
			"id":         c.ID,
			"summary":    c.Summary,
			"accessRole": c.AccessRole,
		}
		if c.SummaryOverride != "" {
			item["summaryOverride"] = c.SummaryOverride
		}
		if c.TimeZone != "" {
			item["timeZone"] = c.TimeZone
		}
		if c.Primary {
			item["primary"] = true
		}
		items = append(items, item)
	}
	resp := map[string]any{"kind": "calendar#calendarList", "items": items}
	if end < len(s.order) {
		resp["nextPageToken"] = "cl:" + strconv.Itoa(end)
	} else {
		resp["nextSyncToken"] = "clsync"
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) calendar(w http.ResponseWriter, r *http.Request) (*fakeCalendar, bool) {
	c, ok := s.cals[r.PathValue("cal")]
	if !ok {
		writeError(w, http.StatusNotFound, "notFound", "Not Found")
		return nil, false
	}
	return c, true
}

func (s *Server) writable(w http.ResponseWriter, c *fakeCalendar) bool {
	if c.AccessRole == "owner" || c.AccessRole == "writer" {
		return true
	}
	writeError(w, http.StatusForbidden, "requiredAccessLevel", "You need to have writer access to this calendar.")
	return false
}

// listPage is the cursor a page token carries.
type listPage struct {
	offset, snapshot, since, gen int
	showDeleted                  bool
	uid                          string
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calendar(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if q.Get("singleEvents") == "true" {
		writeError(w, http.StatusBadRequest, "invalid", "singleEvents is not supported by the fake")
		return
	}
	cur := listPage{snapshot: c.seq, since: -1, gen: s.gen, showDeleted: q.Get("showDeleted") == "true", uid: q.Get("iCalUID")}
	if tok := q.Get("syncToken"); tok != "" {
		gen, seq, ok := parseSyncToken(tok)
		if !ok || gen != s.gen || seq > c.seq {
			writeError(w, http.StatusGone, "fullSyncRequired", "Sync token is no longer valid, a full sync is required.")
			return
		}
		cur.since = seq
		cur.showDeleted = true
	}
	if pt := q.Get("pageToken"); pt != "" {
		p, ok := parsePageToken(pt)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid", "Invalid page token")
			return
		}
		if p.gen != s.gen {
			writeError(w, http.StatusGone, "fullSyncRequired", "Sync token is no longer valid, a full sync is required.")
			return
		}
		cur = p
	}
	ids := make([]string, 0, len(c.events))
	for id, se := range c.events {
		if se.seq > cur.snapshot || se.seq <= cur.since {
			continue
		}
		if cur.uid != "" && se.obj["iCalUID"] != cur.uid {
			continue
		}
		if !cur.showDeleted && isDeleted(se.obj) {
			continue
		}
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if d := c.events[a].seq - c.events[b].seq; d != 0 {
			return d
		}
		return strings.Compare(a, b)
	})
	if cur.offset > len(ids) {
		cur.offset = len(ids)
	}
	end := min(cur.offset+s.pageLimit(r), len(ids))
	items := make([]map[string]any, 0, end-cur.offset)
	for _, id := range ids[cur.offset:end] {
		items = append(items, c.events[id].obj)
	}
	resp := map[string]any{"kind": "calendar#events", "summary": c.Summary, "items": items}
	if c.TimeZone != "" {
		resp["timeZone"] = c.TimeZone
	}
	if end < len(ids) {
		next := cur
		next.offset = end
		resp["nextPageToken"] = pageToken(next)
	} else {
		resp["nextSyncToken"] = fmt.Sprintf("sync:%d:%d", s.gen, cur.snapshot)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calendar(w, r)
	if !ok {
		return
	}
	obj, ok := s.lookup(c, r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "notFound", "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) insertEvent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calendar(w, r)
	if !ok || !s.writable(w, c) {
		return
	}
	var obj map[string]any
	if err := json.NewDecoder(r.Body).Decode(&obj); err != nil || obj == nil {
		writeError(w, http.StatusBadRequest, "parseError", "Parse Error")
		return
	}
	id, _ := obj["id"].(string)
	if id == "" {
		s.etags++
		id = "gen" + strconv.Itoa(s.etags)
	}
	if !validID(id) {
		writeError(w, http.StatusBadRequest, "invalid", "Invalid resource id value.")
		return
	}
	if _, taken := c.events[id]; taken {
		writeError(w, http.StatusConflict, "duplicate", "The requested identifier already exists.")
		return
	}
	if _, ok := obj["start"]; !ok {
		writeError(w, http.StatusBadRequest, "required", "Missing start time.")
		return
	}
	if _, ok := obj["iCalUID"]; !ok {
		obj["iCalUID"] = id + "@google.com"
	}
	if _, ok := obj["status"]; !ok {
		obj["status"] = "confirmed"
	}
	owner := map[string]any{"email": c.Owner, "self": true}
	obj["organizer"] = owner
	obj["creator"] = deepCopy(owner)
	obj["kind"] = "calendar#event"
	obj["sequence"] = 0
	obj["created"] = s.now().UTC().Format("2006-01-02T15:04:05.000Z")
	s.store(c, id, obj)
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) patchEvent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calendar(w, r)
	if !ok || !s.writable(w, c) {
		return
	}
	id := r.PathValue("id")
	if se, ok := c.events[id]; ok && isDeleted(se.obj) {
		writeError(w, http.StatusGone, "deleted", "Resource has been deleted")
		return
	}
	obj, ok := s.lookup(c, id)
	if !ok {
		writeError(w, http.StatusNotFound, "notFound", "Not Found")
		return
	}
	if m := r.Header.Get("If-Match"); m != "" && m != obj["etag"] {
		writeError(w, http.StatusPreconditionFailed, "conditionNotMet", "Precondition Failed")
		return
	}
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "parseError", "Parse Error")
		return
	}
	delete(patch, "id")
	mergePatch(obj, patch)
	s.store(c, id, obj)
	writeJSON(w, http.StatusOK, obj)
}

func (s *Server) deleteEvent(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.calendar(w, r)
	if !ok || !s.writable(w, c) {
		return
	}
	id := r.PathValue("id")
	if se, ok := c.events[id]; ok && isDeleted(se.obj) {
		writeError(w, http.StatusGone, "deleted", "Resource has been deleted")
		return
	}
	obj, ok := s.lookup(c, id)
	if !ok {
		writeError(w, http.StatusNotFound, "notFound", "Not Found")
		return
	}
	if m := r.Header.Get("If-Match"); m != "" && m != obj["etag"] {
		writeError(w, http.StatusPreconditionFailed, "conditionNotMet", "Precondition Failed")
		return
	}
	obj["status"] = "cancelled"
	s.store(c, id, obj)
	w.WriteHeader(http.StatusNoContent)
}

// lookup returns a copy of the event id, or of the instance it names. s.mu
// is held.
func (s *Server) lookup(c *fakeCalendar, id string) (map[string]any, bool) {
	if se, ok := c.events[id]; ok {
		return deepCopy(se.obj), true
	}
	i := strings.LastIndexByte(id, '_')
	if i < 0 {
		return nil, false
	}
	master, ok := c.events[id[:i]]
	if !ok || isDeleted(master.obj) || master.obj["recurrence"] == nil {
		return nil, false
	}
	return instanceOf(master.obj, id, id[i+1:])
}

// instanceOf makes up the occurrence of master that starts at rid, as
// Google shows an unmodified instance.
func instanceOf(master map[string]any, id, rid string) (map[string]any, bool) {
	start, _ := master["start"].(map[string]any)
	end, _ := master["end"].(map[string]any)
	if start == nil || end == nil {
		return nil, false
	}
	obj := deepCopy(master)
	delete(obj, "recurrence")
	obj["id"] = id
	obj["recurringEventId"] = master["id"]
	obj["etag"] = strings.TrimSuffix(master["etag"].(string), `"`) + "-" + rid + `"`
	if d, ok := start["date"].(string); ok {
		at, err := time.Parse("20060102", rid)
		if err != nil {
			return nil, false
		}
		s0, err1 := time.Parse(time.DateOnly, d)
		e0, err2 := time.Parse(time.DateOnly, end["date"].(string))
		if err1 != nil || err2 != nil {
			return nil, false
		}
		obj["originalStartTime"] = map[string]any{"date": at.Format(time.DateOnly)}
		obj["start"] = map[string]any{"date": at.Format(time.DateOnly)}
		obj["end"] = map[string]any{"date": at.Add(e0.Sub(s0)).Format(time.DateOnly)}
		return obj, true
	}
	at, err := time.Parse("20060102T150405Z", rid)
	if err != nil {
		return nil, false
	}
	s0, err1 := time.Parse(time.RFC3339, fmt.Sprint(start["dateTime"]))
	e0, err2 := time.Parse(time.RFC3339, fmt.Sprint(end["dateTime"]))
	if err1 != nil || err2 != nil {
		return nil, false
	}
	loc := time.UTC
	tz, _ := start["timeZone"].(string)
	if l, err := time.LoadLocation(tz); err == nil && tz != "" {
		loc = l
	}
	when := func(t time.Time) map[string]any {
		m := map[string]any{"dateTime": t.In(loc).Format(time.RFC3339)}
		if tz != "" {
			m["timeZone"] = tz
		}
		return m
	}
	obj["originalStartTime"] = when(at)
	obj["start"] = when(at)
	obj["end"] = when(at.Add(e0.Sub(s0)))
	return obj, true
}

func (s *Server) freeBusy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TimeMin string `json:"timeMin"`
		TimeMax string `json:"timeMax"`
		Items   []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "parseError", "Parse Error")
		return
	}
	from, err1 := time.Parse(time.RFC3339, req.TimeMin)
	to, err2 := time.Parse(time.RFC3339, req.TimeMax)
	if err1 != nil || err2 != nil || !to.After(from) {
		writeError(w, http.StatusBadRequest, "timeRangeEmpty", "The specified time range is empty.")
		return
	}
	if len(req.Items) > MaxFreeBusyItems {
		writeError(w, http.StatusBadRequest, "tooManyCalendarsRequested", "Too many calendars requested.")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cals := map[string]any{}
	for _, it := range req.Items {
		busy, ok := s.busy[strings.ToLower(it.ID)]
		if !ok {
			cals[it.ID] = map[string]any{
				"errors": []map[string]any{{"domain": "global", "reason": "notFound"}},
				"busy":   []any{},
			}
			continue
		}
		blocks := []map[string]any{}
		for _, b := range busy {
			if !b.Start.Before(to) || !b.End.After(from) {
				continue
			}
			blocks = append(blocks, map[string]any{
				"start": maxTime(b.Start, from).UTC().Format(time.RFC3339),
				"end":   minTime(b.End, to).UTC().Format(time.RFC3339),
			})
		}
		cals[it.ID] = map[string]any{"busy": blocks}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": "calendar#freeBusy", "timeMin": req.TimeMin, "timeMax": req.TimeMax, "calendars": cals,
	})
}

// validID is Google's rule for client-chosen event ids.
var validIDChars = regexp.MustCompile(`^[0-9a-v]+$`)

func validID(id string) bool { return len(id) >= 5 && len(id) <= 1024 && validIDChars.MatchString(id) }

// isDeleted reports whether obj is a deleted event rather than a cancelled
// instance of a series.
func isDeleted(obj map[string]any) bool {
	_, instance := obj["recurringEventId"]
	return obj["status"] == "cancelled" && !instance
}

func parseSyncToken(tok string) (gen, seq int, ok bool) {
	parts := strings.Split(tok, ":")
	if len(parts) != 3 || parts[0] != "sync" {
		return 0, 0, false
	}
	gen, err1 := strconv.Atoi(parts[1])
	seq, err2 := strconv.Atoi(parts[2])
	return gen, seq, err1 == nil && err2 == nil && seq >= 0
}

func pageToken(p listPage) string {
	return fmt.Sprintf("pg:%d:%d:%d:%d:%t:%s", p.offset, p.snapshot, p.since, p.gen, p.showDeleted, url.QueryEscape(p.uid))
}

func parsePageToken(tok string) (listPage, bool) {
	parts := strings.SplitN(tok, ":", 7)
	if len(parts) != 7 || parts[0] != "pg" {
		return listPage{}, false
	}
	var p listPage
	var err error
	nums := []*int{&p.offset, &p.snapshot, &p.since, &p.gen}
	for i, n := range nums {
		if *n, err = strconv.Atoi(parts[i+1]); err != nil {
			return listPage{}, false
		}
	}
	if p.showDeleted, err = strconv.ParseBool(parts[5]); err != nil {
		return listPage{}, false
	}
	if p.uid, err = url.QueryUnescape(parts[6]); err != nil {
		return listPage{}, false
	}
	return p, p.offset >= 0
}

// mergePatch applies a patch's top-level fields to obj; null removes one.
func mergePatch(obj, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(obj, k)
			continue
		}
		obj[k] = v
	}
}

func deepCopy(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		panic("fakegcal: event is not JSON: " + err.Error())
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		panic("fakegcal: event is not JSON: " + err.Error())
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a failed write shows up as a client-side parse error
}

// writeError answers in Google's error format.
func writeError(w http.ResponseWriter, status int, reason, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code":    status,
		"message": message,
		"errors":  []map[string]any{{"domain": "global", "reason": reason, "message": message}},
	}})
}
