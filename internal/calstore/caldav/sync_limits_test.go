package caldav

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// overrideObject is a master with n overrides, all one object.
func overrideObject(n int) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\n" +
		"BEGIN:VEVENT\r\nUID:big\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261102T090000Z\r\nRRULE:FREQ=DAILY\r\nEND:VEVENT\r\n")
	for i := range n {
		fmt.Fprintf(&b, "BEGIN:VEVENT\r\nUID:big\r\nRECURRENCE-ID:2027%04dT090000Z\r\nDTSTART:20261102T100000Z\r\nEND:VEVENT\r\n", i)
	}
	b.WriteString("END:VCALENDAR\r\n")
	return b.String()
}

// TestSyncSkipsObjectsOverTheEventLimit covers an object with more VEVENTs
// than calendar.MaxObjectEvents: it is reported skipped, not deleted, and
// its etag is kept so it is read again only once it changes.
func TestSyncSkipsObjectsOverTheEventLimit(t *testing.T) {
	big := overrideObject(calendar.MaxObjectEvents)
	d := newDAVServer(t)
	d.multistatus("PROPFIND", "/cal/", ms(resp("/cal/", `<cs:getctag>2</cs:getctag>`)))
	d.handle("REPORT", "/cal/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusMultiStatus)
		if strings.Contains(string(body), "calendar-query") {
			_, _ = fmt.Fprint(w, ms(resp("/cal/big.ics", `<d:getetag>"b2"</d:getetag>`)))
			return
		}
		_, _ = fmt.Fprint(w, ms(resp("/cal/big.ics", `<d:getetag>"b2"</d:getetag><c:calendar-data>`+xmlText(big)+`</c:calendar-data>`)))
	})
	tok, err := encodeETagState(etagState{CTag: "1", ETags: map[string]string{"/cal/big.ics": `"b1"`}})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := d.store(t, "/").Sync(context.Background(), "/cal/", tok)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Upserts) != 0 || len(ch.Deletes) != 0 || strings.Join(ch.Skipped, " ") != "/cal/big.ics" {
		t.Errorf("changes: %d upserts, deletes %v, skipped %v; want only /cal/big.ics skipped", len(ch.Upserts), ch.Deletes, ch.Skipped)
	}
	st, err := decodeETagState(strings.TrimPrefix(ch.Token, tokenETag))
	if err != nil {
		t.Fatal(err)
	}
	if st.ETags["/cal/big.ics"] != `"b2"` {
		t.Errorf("a skipped object's etag is not kept: %v", st.ETags)
	}
}
