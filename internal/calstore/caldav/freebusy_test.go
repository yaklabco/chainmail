package caldav

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
)

func TestFreeBusy(t *testing.T) {
	d := newDAVServer(t)
	principalRoutes(d)
	from := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	var posted string
	d.handle(http.MethodPost, "/dav/itip/jane/outbox/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		posted = string(body)
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/calendar") {
			t.Errorf("Content-Type %q", ct)
		}
		reply := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nMETHOD:REPLY\r\nBEGIN:VFREEBUSY\r\n" +
			"FREEBUSY;FBTYPE=BUSY:20261110T090000Z/20261110T100000Z,20261110T093000Z/PT1H\r\n" +
			"FREEBUSY;FBTYPE=FREE:20261110T120000Z/20261110T130000Z\r\n" +
			"FREEBUSY;FBTYPE=BUSY-TENTATIVE:20261109T230000Z/20261110T003000Z\r\n" +
			"FREEBUSY:20261110T150000Z/2026111\r\n 0T160000Z\r\n" +
			"END:VFREEBUSY\r\nEND:VCALENDAR\r\n"
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprintf(w, `<?xml version="1.0"?><c:schedule-response xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`+
			`<c:response><c:recipient><d:href>mailto:BOB@example.com</d:href></c:recipient><c:request-status>2.0;Success</c:request-status><c:calendar-data>%s</c:calendar-data></c:response>`+
			`<c:response><c:recipient><d:href>mailto:stranger@elsewhere.example</d:href></c:recipient><c:request-status>3.7;Invalid calendar user</c:request-status></c:response>`+
			`<c:response><c:recipient>mailto:nodata@example.com</c:recipient><c:request-status>2.0;Success</c:request-status></c:response>`+
			`</c:schedule-response>`, xmlText(reply))
	})
	s := d.store(t, "/")
	people := []string{"bob@example.com", "stranger@elsewhere.example", "silent@example.com", "nodata@example.com"}
	got, err := s.FreeBusy(context.Background(), people, from, to)
	if err != nil {
		t.Fatalf("FreeBusy: %v", err)
	}
	bob := got["bob@example.com"]
	want := []calendar.BusyBlock{
		{Start: from, End: from.Add(30 * time.Minute)},
		{Start: from.Add(9 * time.Hour), End: from.Add(10*time.Hour + 30*time.Minute)},
		{Start: from.Add(15 * time.Hour), End: from.Add(16 * time.Hour)},
	}
	if !bob.Known || len(bob.Busy) != len(want) {
		t.Fatalf("bob = %+v, want %v", bob, want)
	}
	for i := range want {
		if !bob.Busy[i].Start.Equal(want[i].Start) || !bob.Busy[i].End.Equal(want[i].End) {
			t.Errorf("busy %d = %v, want %v", i, bob.Busy[i], want[i])
		}
	}
	for _, p := range people[1:] {
		if fb, ok := got[p]; !ok || fb.Known {
			t.Errorf("%s = %+v, %v; want unknown", p, fb, ok)
		}
	}
	for _, want := range []string{
		"METHOD:REQUEST", "BEGIN:VFREEBUSY", "ORGANIZER:mailto:jane@example.com",
		"ATTENDEE:mailto:bob@example.com", "DTSTART:20261110T000000Z", "DTEND:20261111T000000Z",
	} {
		if !strings.Contains(posted, want+"\r\n") {
			t.Errorf("request lacks %q:\n%s", want, posted)
		}
	}
}

func TestFreeBusyWithoutOutboxIsUnknown(t *testing.T) {
	f := newFakeServer(t) // go-webdav serves no scheduling collections
	got, err := f.store(t).FreeBusy(context.Background(), []string{"bob@example.com"}, fixedNow, fixedNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if fb, ok := got["bob@example.com"]; !ok || fb.Known {
		t.Errorf("bob = %+v, %v; want unknown", fb, ok)
	}
}

func TestFreeBusyRefusesInjection(t *testing.T) {
	if _, err := freeBusyRequest("jane@example.com", []string{"bob@example.com\r\nATTENDEE:mailto:x@y"}, fixedNow, fixedNow, fixedNow); err == nil {
		t.Error("an address with a line break was accepted")
	}
}

func TestParseDuration(t *testing.T) {
	tests := map[string]time.Duration{
		"PT1H": time.Hour, "PT1H30M": 90 * time.Minute, "P1D": 24 * time.Hour, "P1W": 7 * 24 * time.Hour,
		"P1DT2H": 26 * time.Hour, "PT45S": 45 * time.Second,
	}
	for in, want := range tests {
		if got, ok := parseDuration(in); !ok || got != want {
			t.Errorf("parseDuration(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "P", "PT", "1H", "PT1", "PTH", "P1H", "PT1D", "-PT1H"} {
		if _, ok := parseDuration(bad); ok {
			t.Errorf("parseDuration(%q) accepted", bad)
		}
	}
}

// TestFreeBusyNeedsACompleteVFreeBusy: a person is Known only when the
// answer is a whole VCALENDAR holding a whole VFREEBUSY.
func TestFreeBusyNeedsACompleteVFreeBusy(t *testing.T) {
	const (
		head = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nMETHOD:REPLY\r\n"
		fb   = "BEGIN:VFREEBUSY\r\nFREEBUSY:20261110T090000Z/20261110T100000Z\r\nEND:VFREEBUSY\r\n"
		tail = "END:VCALENDAR\r\n"
	)
	answers := map[string]string{
		"complete@example.com":  head + fb + tail,
		"empty@example.com":     head + "BEGIN:VFREEBUSY\r\nEND:VFREEBUSY\r\n" + tail,
		"bare@example.com":      "BEGIN:VFREEBUSY",
		"truncated@example.com": head + "BEGIN:VFREEBUSY\r\nFREEBUSY:20261110T090000Z/20261110T100000Z\r\n",
		"noend@example.com":     head + fb,
		"wrong@example.com":     head + "BEGIN:VEVENT\r\nUID:x\r\nFREEBUSY:20261110T090000Z/20261110T100000Z\r\nEND:VEVENT\r\n" + tail,
		"nested@example.com":    head + "BEGIN:VEVENT\r\nUID:x\r\n" + fb + "END:VEVENT\r\n" + tail,
		"mismatch@example.com":  head + "BEGIN:VFREEBUSY\r\nFREEBUSY:20261110T090000Z/20261110T100000Z\r\nEND:VEVENT\r\n" + tail,
		"trailing@example.com":  head + fb + tail + "BEGIN:VFREEBUSY\r\n",
		"outside@example.com":   "BEGIN:VFREEBUSY\r\nEND:VFREEBUSY\r\n",
		"nocolon@example.com":   head + "BEGIN:VFREEBUSY\r\ngarbage\r\nEND:VFREEBUSY\r\n" + tail,
		"twocals@example.com":   head + fb + tail + head + fb + tail,
		"deep@example.com":      head + strings.Repeat("BEGIN:X-A\r\n", 100) + strings.Repeat("END:X-A\r\n", 100) + fb + tail,
	}
	d := newDAVServer(t)
	principalRoutes(d)
	d.handle(http.MethodPost, "/dav/itip/jane/outbox/", func(w http.ResponseWriter, _ *http.Request) {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><c:schedule-response xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav">`)
		for who, data := range answers {
			fmt.Fprintf(&b, `<c:response><c:recipient><d:href>mailto:%s</d:href></c:recipient><c:request-status>2.0;Success</c:request-status><c:calendar-data>%s</c:calendar-data></c:response>`, who, xmlText(data))
		}
		b.WriteString(`</c:schedule-response>`)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, b.String())
	})
	people := make([]string, 0, len(answers))
	for who := range answers {
		people = append(people, who)
	}
	from := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	got, err := d.store(t, "/").FreeBusy(context.Background(), people, from, from.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for who := range answers {
		want := who == "complete@example.com" || who == "empty@example.com"
		if got[who].Known != want {
			t.Errorf("%s: Known %v, want %v", who, got[who].Known, want)
		}
	}
	if c := got["complete@example.com"]; len(c.Busy) != 1 || !c.Busy[0].Start.Equal(from.Add(9*time.Hour)) {
		t.Errorf("complete answer busy = %v", c.Busy)
	}
	if e := got["empty@example.com"]; len(e.Busy) != 0 {
		t.Errorf("empty answer busy = %v", e.Busy)
	}
}
