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
