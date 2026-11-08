package caldav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

func providerIDs(evs []calendar.Event) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range evs {
		if !seen[e.ProviderID] {
			seen[e.ProviderID] = true
			out = append(out, e.ProviderID)
		}
	}
	sort.Strings(out)
	return out
}

func TestSyncByETags(t *testing.T) {
	f := newFakeServer(t)
	f.backend.put(t, testCal+"series.ics", seriesICS)
	f.backend.put(t, testCal+"single.ics", singleICS)
	s := f.store(t)
	ctx := context.Background()

	ch, err := s.Sync(ctx, testCal, "")
	if err != nil {
		t.Fatalf("full Sync: %v", err)
	}
	if !ch.Full || !ch.ObjectReplace || len(ch.Deletes) != 0 {
		t.Errorf("full sync: Full %v ObjectReplace %v Deletes %v", ch.Full, ch.ObjectReplace, ch.Deletes)
	}
	if got := providerIDs(ch.Upserts); strings.Join(got, " ") != testCal+"series.ics "+testCal+"single.ics" {
		t.Errorf("upserts = %v", got)
	}
	if len(ch.Upserts) != 3 {
		t.Errorf("got %d upserts, want master, override and single", len(ch.Upserts))
	}
	for _, e := range ch.Upserts {
		if e.ETag != f.backend.etag(e.ProviderID) || len(e.Raw) == 0 {
			t.Errorf("upsert %s: etag %q raw %d, want %q", e.ProviderID, e.ETag, len(e.Raw), f.backend.etag(e.ProviderID))
		}
	}
	if !strings.HasPrefix(ch.Token, tokenETag) {
		t.Fatalf("token %q, want the etag form", ch.Token)
	}

	// Nothing changed: no upserts, no deletes.
	ch2, err := s.Sync(ctx, testCal, ch.Token)
	if err != nil {
		t.Fatal(err)
	}
	if ch2.Full || len(ch2.Upserts) != 0 || len(ch2.Deletes) != 0 {
		t.Errorf("unchanged sync = %+v", ch2)
	}

	// One changed, one deleted, one added.
	f.backend.put(t, testCal+"single.ics", strings.Replace(singleICS, "Dentist", "Dentist (moved)", 1))
	if err := f.backend.DeleteCalendarObject(ctx, testCal+"series.ics"); err != nil {
		t.Fatal(err)
	}
	f.backend.put(t, testCal+"new.ics", strings.Replace(singleICS, "single-1@", "new-1@", 1))
	f.reset()
	ch3, err := s.Sync(ctx, testCal, ch2.Token)
	if err != nil {
		t.Fatal(err)
	}
	if ch3.Full {
		t.Error("incremental sync reported Full")
	}
	if got := providerIDs(ch3.Upserts); strings.Join(got, " ") != testCal+"new.ics "+testCal+"single.ics" {
		t.Errorf("upserts = %v", got)
	}
	if strings.Join(ch3.Deletes, " ") != testCal+"series.ics" {
		t.Errorf("deletes = %v", ch3.Deletes)
	}
	// Only the changed objects were fetched.
	for _, r := range f.seen("REPORT") {
		if strings.Contains(r.body, "calendar-multiget") && strings.Contains(r.body, "series.ics") {
			t.Error("multiget asked for a deleted object")
		}
	}
}

func TestSyncByETagsSkipsUnchangedCTag(t *testing.T) {
	d := newDAVServer(t)
	d.multistatus("PROPFIND", "/cal/", ms(resp("/cal/", `<cs:getctag>ctag-1</cs:getctag>`)))
	d.multistatus("REPORT", "/cal/", ms(resp("/cal/a.ics", `<d:getetag>"a1"</d:getetag>`)))
	s := d.store(t, "/")
	tok, err := encodeETagState(etagState{CTag: "ctag-1", ETags: map[string]string{"/cal/a.ics": `"a1"`}})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.Sync(context.Background(), "/cal/", tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Upserts) != 0 || len(ch.Deletes) != 0 || ch.Token != tok {
		t.Errorf("sync with an unchanged ctag = %+v", ch)
	}
	for _, c := range d.calls {
		if strings.HasPrefix(c, "REPORT") {
			t.Errorf("an unchanged ctag still listed etags: %v", d.calls)
		}
	}
}

// syncServer scripts an RFC 6578 collection: each REPORT body's token
// selects a reply.
func syncServer(t *testing.T, replies map[string]func(w http.ResponseWriter)) *davServer {
	t.Helper()
	d := newDAVServer(t)
	d.multistatus("PROPFIND", "/cal/", ms(resp("/cal/", `<d:sync-token>t0</d:sync-token>`)))
	d.handle("REPORT", "/cal/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b := string(body)
		switch {
		case strings.Contains(b, "sync-collection"):
			// RFC 6578 section 3.2: the report is sent with Depth: 0, and
			// an absent Depth means the same. Mailwright refuses others.
			if dep := r.Header.Get("Depth"); dep != "" && dep != "0" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if !strings.Contains(b, "<d:sync-level>1</d:sync-level>") || !strings.Contains(b, "getetag") {
				t.Errorf("sync-collection body %s", b)
			}
			start := strings.Index(b, "<d:sync-token>") + len("<d:sync-token>")
			end := strings.Index(b, "</d:sync-token>")
			reply, ok := replies[b[start:end]]
			if !ok {
				t.Errorf("unexpected token %q", b[start:end])
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			reply(w)
		case strings.Contains(b, "calendar-multiget"):
			var out []string
			for _, name := range []string{"a", "b", "c"} {
				href := "/cal/" + name + ".ics"
				if strings.Contains(b, "<d:href>"+href+"</d:href>") {
					data := strings.Replace(singleICS, "single-1@", name+"@", 1)
					out = append(out, resp(href, `<d:getetag>"`+name+`2"</d:getetag><c:calendar-data>`+xmlText(data)+`</c:calendar-data>`))
				}
			}
			if strings.Contains(b, "/cal/gone.ics") {
				out = append(out, `<d:response><d:href>/cal/gone.ics</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response>`)
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = fmt.Fprint(w, ms(out...))
		default:
			t.Errorf("unexpected REPORT %s", b)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	return d
}

func syncReply(token string, responses ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = fmt.Fprint(w, strings.Replace(ms(responses...), "</d:multistatus>", "<d:sync-token>"+token+"</d:sync-token></d:multistatus>", 1))
	}
}

func TestSyncCollection(t *testing.T) {
	gone := `<d:response><d:href>/cal/b.ics</d:href><d:status>HTTP/1.1 404 Not Found</d:status></d:response>`
	truncated := `<d:response><d:href>/cal/</d:href><d:status>HTTP/1.1 507 Insufficient Storage</d:status></d:response>`
	d := syncServer(t, map[string]func(http.ResponseWriter){
		"": syncReply("t1",
			resp("/cal/a.ics", `<d:getetag>"a1"</d:getetag>`),
			resp("/cal/b.ics", `<d:getetag>"b1"</d:getetag>`)),
		"t1": syncReply("t2", resp("/cal/a.ics", `<d:getetag>"a2"</d:getetag>`), truncated),
		"t2": syncReply("t3", gone, resp("/cal/c.ics", `<d:getetag>"c2"</d:getetag>`),
			resp("/cal/gone.ics", `<d:getetag>"g"</d:getetag>`)),
	})
	s := d.store(t, "/")
	ctx := context.Background()

	ch, err := s.Sync(ctx, "/cal/", "")
	if err != nil {
		t.Fatalf("initial Sync: %v", err)
	}
	if !ch.Full || !ch.ObjectReplace || ch.Token != tokenSync+"t1" {
		t.Errorf("initial sync: Full %v ObjectReplace %v Token %q", ch.Full, ch.ObjectReplace, ch.Token)
	}
	if got := providerIDs(ch.Upserts); strings.Join(got, " ") != "/cal/a.ics /cal/b.ics" {
		t.Errorf("initial upserts = %v", got)
	}

	ch, err = s.Sync(ctx, "/cal/", ch.Token)
	if err != nil {
		t.Fatalf("incremental Sync: %v", err)
	}
	if ch.Full || ch.Token != tokenSync+"t3" {
		t.Errorf("incremental sync: Full %v Token %q", ch.Full, ch.Token)
	}
	if got := providerIDs(ch.Upserts); strings.Join(got, " ") != "/cal/a.ics /cal/c.ics" {
		t.Errorf("incremental upserts = %v", got)
	}
	// gone.ics vanished between the sync report and the multiget.
	if strings.Join(ch.Deletes, " ") != "/cal/b.ics /cal/gone.ics" {
		t.Errorf("incremental deletes = %v", ch.Deletes)
	}
	for _, e := range ch.Upserts {
		if e.ProviderID == "/cal/a.ics" && e.ETag != `"a2"` {
			t.Errorf("a.ics etag %q, want the multiget's", e.ETag)
		}
	}
}

func TestSyncInvalidToken(t *testing.T) {
	for name, reply := range map[string]func(http.ResponseWriter){
		"403 valid-sync-token": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `<d:error xmlns:d="DAV:"><d:valid-sync-token/></d:error>`)
		},
		"409": func(w http.ResponseWriter) { w.WriteHeader(http.StatusConflict) },
		"410": func(w http.ResponseWriter) { w.WriteHeader(http.StatusGone) },
	} {
		d := syncServer(t, map[string]func(http.ResponseWriter){"old": reply})
		_, err := d.store(t, "/").Sync(context.Background(), "/cal/", tokenSync+"old")
		if !errors.Is(err, calstore.ErrSyncTokenInvalid) {
			t.Errorf("%s: err = %v, want ErrSyncTokenInvalid", name, err)
		}
	}
	d := newDAVServer(t)
	for _, tok := range []string{"garbage", tokenETag + "!!!", tokenETag + "AAAA"} {
		if _, err := d.store(t, "/").Sync(context.Background(), "/cal/", tok); !errors.Is(err, calstore.ErrSyncTokenInvalid) {
			t.Errorf("token %q: err = %v, want ErrSyncTokenInvalid", tok, err)
		}
	}
}

func TestSyncReportsBrokenObjectsDeleted(t *testing.T) {
	d := newDAVServer(t)
	d.multistatus("PROPFIND", "/cal/", ms(resp("/cal/", `<cs:getctag>2</cs:getctag>`)))
	d.handle("REPORT", "/cal/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusMultiStatus)
		if strings.Contains(string(body), "calendar-query") {
			_, _ = fmt.Fprint(w, ms(resp("/cal/bad.ics", `<d:getetag>"x2"</d:getetag>`), resp("/cal/todo.ics", `<d:getetag>"t2"</d:getetag>`)))
			return
		}
		todo := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VTODO\r\nUID:t\r\nDTSTAMP:20261101T000000Z\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
		_, _ = fmt.Fprint(w, ms(
			resp("/cal/bad.ics", `<d:getetag>"x2"</d:getetag><c:calendar-data>not a calendar</c:calendar-data>`),
			resp("/cal/todo.ics", `<d:getetag>"t2"</d:getetag><c:calendar-data>`+xmlText(todo)+`</c:calendar-data>`)))
	})
	tok, err := encodeETagState(etagState{CTag: "1", ETags: map[string]string{"/cal/bad.ics": `"x1"`, "/cal/todo.ics": `"t1"`}})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := d.store(t, "/").Sync(context.Background(), "/cal/", tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Upserts) != 0 || strings.Join(ch.Deletes, " ") != "/cal/bad.ics /cal/todo.ics" {
		t.Errorf("changes = %+v", ch)
	}
	st, err := decodeETagState(strings.TrimPrefix(ch.Token, tokenETag))
	if err != nil {
		t.Fatal(err)
	}
	if st.ETags["/cal/bad.ics"] != `"x2"` {
		t.Errorf("a broken object's etag is not kept, so it would be fetched every sync: %v", st.ETags)
	}
}

func TestMultigetSplitsLargeAnswers(t *testing.T) {
	d := newDAVServer(t)
	var batches []int
	d.handle("REPORT", "/cal/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := strings.Count(string(body), "<d:href>")
		batches = append(batches, n)
		w.WriteHeader(http.StatusMultiStatus)
		if n > 2 {
			_, _ = fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:">`+strings.Repeat(" ", maxMultistatus)+`</d:multistatus>`)
			return
		}
		var out []string
		for _, h := range strings.Split(string(body), "<d:href>")[1:] {
			href, _, _ := strings.Cut(h, "<")
			out = append(out, resp(href, `<d:getetag>"1"</d:getetag><c:calendar-data>`+xmlText(singleICS)+`</c:calendar-data>`))
		}
		_, _ = fmt.Fprint(w, ms(out...))
	})
	s := d.store(t, "/")
	hrefs := []string{"/cal/1.ics", "/cal/2.ics", "/cal/3.ics", "/cal/4.ics", "/cal/5.ics"}
	var objs []*storedObject
	missing, err := s.multiget(context.Background(), "/cal/", hrefs, func(o *storedObject) error {
		objs = append(objs, o)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 5 || len(missing) != 0 {
		t.Errorf("got %d objects, %v missing; batches %v", len(objs), missing, batches)
	}
}

// failedResponse is a multistatus response whose resource failed with
// status line code, with no propstat.
func failedResponse(href, code string) string {
	return `<d:response><d:href>` + href + `</d:href><d:status>HTTP/1.1 ` + code + `</d:status></d:response>`
}

// failedPropstat is a multistatus response whose properties failed with
// status line code.
func failedPropstat(href, props, code string) string {
	return `<d:response><d:href>` + href + `</d:href><d:propstat><d:prop>` + props +
		`</d:prop><d:status>HTTP/1.1 ` + code + `</d:status></d:propstat></d:response>`
}

// reportServer answers sync-collection with syncResponses (and token t2),
// calendar-query with queryResponses and calendar-multiget with
// multigetResponses.
func reportServer(t *testing.T, syncResponses, queryResponses, multigetResponses []string) *davServer {
	t.Helper()
	d := newDAVServer(t)
	d.multistatus("PROPFIND", "/cal/", ms(resp("/cal/", `<cs:getctag>2</cs:getctag>`)))
	d.handle("REPORT", "/cal/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		b := string(body)
		w.WriteHeader(http.StatusMultiStatus)
		switch {
		case strings.Contains(b, "sync-collection"):
			_, _ = fmt.Fprint(w, strings.Replace(ms(syncResponses...), "</d:multistatus>", "<d:sync-token>t2</d:sync-token></d:multistatus>", 1))
		case strings.Contains(b, "calendar-query"):
			_, _ = fmt.Fprint(w, ms(queryResponses...))
		default:
			_, _ = fmt.Fprint(w, ms(multigetResponses...))
		}
	})
	return d
}

// TestSyncResourceFailuresFailTheSync: a member the server could not
// report, in the change list or in the multiget, is not a deleted one. The
// whole Sync fails, so the caller keeps its token and tries again; only an
// explicit 404 or 410 is a delete.
func TestSyncResourceFailuresFailTheSync(t *testing.T) {
	okA := resp("/cal/a.ics", `<d:getetag>"a2"</d:getetag>`)
	dataA := resp("/cal/a.ics", `<d:getetag>"a2"</d:getetag><c:calendar-data>`+xmlText(singleICS)+`</c:calendar-data>`)
	okB := resp("/cal/b.ics", `<d:getetag>"b2"</d:getetag>`)
	tests := []struct {
		name               string
		sync, query, multi []string
		want               error
	}{
		{"sync member 403", []string{okA, failedResponse("/cal/b.ics", "403 Forbidden")}, nil, []string{dataA}, calstore.ErrAuth},
		{"sync member 500", []string{okA, failedResponse("/cal/b.ics", "500 Internal Server Error")}, nil, []string{dataA}, calstore.ErrUnavailable},
		{"sync propstat 500", []string{okA, failedPropstat("/cal/b.ics", `<d:getetag/>`, "500 Internal Server Error")}, nil, []string{dataA}, calstore.ErrUnavailable},
		{"sync propstat 403", []string{okA, failedPropstat("/cal/b.ics", `<d:getetag/>`, "403 Forbidden")}, nil, []string{dataA}, calstore.ErrAuth},
		{"sync member without etag", []string{okA, `<d:response><d:href>/cal/b.ics</d:href></d:response>`}, nil, []string{dataA}, calstore.ErrUnavailable},
		{"multiget 403", []string{okA, okB}, nil, []string{dataA, failedResponse("/cal/b.ics", "403 Forbidden")}, calstore.ErrAuth},
		{"multiget 500", []string{okA, okB}, nil, []string{dataA, failedResponse("/cal/b.ics", "500 Internal Server Error")}, calstore.ErrUnavailable},
		{"multiget data propstat 500", []string{okA, okB}, nil, []string{dataA, `<d:response><d:href>/cal/b.ics</d:href>` +
			`<d:propstat><d:prop><d:getetag>"b2"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>` +
			`<d:propstat><d:prop><c:calendar-data/></d:prop><d:status>HTTP/1.1 500 Internal Server Error</d:status></d:propstat></d:response>`}, calstore.ErrUnavailable},
		{"multiget member missing", []string{okA, okB}, nil, []string{dataA}, calstore.ErrUnavailable},
		{"multiget etag propstat 403", []string{okA, okB}, nil, []string{dataA, `<d:response><d:href>/cal/b.ics</d:href>` +
			`<d:propstat><d:prop><c:calendar-data>` + xmlText(singleICS) + `</c:calendar-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>` +
			`<d:propstat><d:prop><d:getetag/></d:prop><d:status>HTTP/1.1 403 Forbidden</d:status></d:propstat></d:response>`}, calstore.ErrAuth},
		{"multiget without etag", []string{okA, okB}, nil, []string{dataA, resp("/cal/b.ics", `<c:calendar-data>`+xmlText(singleICS)+`</c:calendar-data>`)}, calstore.ErrUnavailable},
		{"multiget empty etag", []string{okA, okB}, nil, []string{dataA, resp("/cal/b.ics", `<d:getetag></d:getetag><c:calendar-data>`+xmlText(singleICS)+`</c:calendar-data>`)}, calstore.ErrUnavailable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := reportServer(t, tc.sync, tc.query, tc.multi)
			ch, err := d.store(t, "/").Sync(context.Background(), "/cal/", tokenSync+"t1")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Sync = token %q deletes %v, %v; want %v", ch.Token, ch.Deletes, err, tc.want)
			}
			if ch.Token != "" || len(ch.Deletes) != 0 {
				t.Errorf("a failed sync returned token %q deletes %v", ch.Token, ch.Deletes)
			}
		})
	}

	// The same for an etag sync: a member the calendar-query could not
	// report stays, and the sync fails.
	tok, err := encodeETagState(etagState{CTag: "1", ETags: map[string]string{"/cal/a.ics": `"a1"`, "/cal/b.ics": `"b1"`}})
	if err != nil {
		t.Fatal(err)
	}
	for name, failed := range map[string]string{
		"query member 500":   failedResponse("/cal/b.ics", "500 Internal Server Error"),
		"query propstat 403": failedPropstat("/cal/b.ics", `<d:getetag/>`, "403 Forbidden"),
	} {
		d := reportServer(t, nil, []string{okA, failed}, []string{dataA})
		ch, err := d.store(t, "/").Sync(context.Background(), "/cal/", tok)
		if err == nil || ch.Token != "" || len(ch.Deletes) != 0 {
			t.Errorf("%s: Sync = token %q deletes %v, %v; want an error", name, ch.Token, ch.Deletes, err)
		}
	}

	// An explicit 404 or 410 is still a delete.
	for _, code := range []string{"404 Not Found", "410 Gone"} {
		d := reportServer(t, []string{okA, okB}, nil, []string{dataA, failedResponse("/cal/b.ics", code)})
		ch, err := d.store(t, "/").Sync(context.Background(), "/cal/", tokenSync+"t1")
		if err != nil {
			t.Fatalf("multiget %s: %v", code, err)
		}
		if strings.Join(ch.Deletes, " ") != "/cal/b.ics" || ch.Token != tokenSync+"t2" {
			t.Errorf("multiget %s: token %q deletes %v, want b.ics deleted", code, ch.Token, ch.Deletes)
		}
	}
}
