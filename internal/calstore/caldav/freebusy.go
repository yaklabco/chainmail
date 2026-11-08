package caldav

import (
	"context"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

// freeBusyBatch is how many people one free/busy POST asks about. RFC
// 6638 servers cap recipients (Mailwright at 100 by default).
const freeBusyBatch = 50

// FreeBusy POSTs a VFREEBUSY REQUEST for people to the principal's
// schedule outbox and reads the schedule-response (RFC 6638 section 5).
// A person the server answers for with a 2.x request status and a complete
// VFREEBUSY is Known. The rest, and everyone when the server has no outbox or the principal no
// address, are Known false, never free.
func (s *Store) FreeBusy(ctx context.Context, people []string, from, to time.Time) (map[string]calendar.FreeBusy, error) {
	out := make(map[string]calendar.FreeBusy, len(people))
	for _, p := range people {
		out[p] = calendar.FreeBusy{}
	}
	if len(people) == 0 || !to.After(from) {
		return out, nil
	}
	d, err := s.discover(ctx)
	if err != nil {
		return nil, err
	}
	if d.outbox == "" || len(d.addresses) == 0 {
		return out, nil
	}
	for start := 0; start < len(people); start += freeBusyBatch {
		batch := people[start:min(start+freeBusyBatch, len(people))]
		if err := s.freeBusyBatch(ctx, d, batch, from, to, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) freeBusyBatch(ctx context.Context, d *discovery, people []string, from, to time.Time, out map[string]calendar.FreeBusy) error {
	body, err := freeBusyRequest(d.addresses[0], people, from, to, s.now())
	if err != nil {
		return err
	}
	resp, err := s.do(ctx, request{
		method:      http.MethodPost,
		path:        d.outbox,
		body:        body,
		contentType: icsContentType,
		limit:       maxMultistatus,
	})
	if err != nil {
		return err
	}
	if resp.status/100 != 2 {
		return statusError(opRead, resp)
	}
	var sr scheduleResponse
	if err := xml.Unmarshal(resp.body, &sr); err != nil {
		return fmt.Errorf("%w: malformed schedule-response", calstore.ErrUnavailable)
	}
	byAddr := map[string]string{}
	for _, p := range people {
		byAddr[strings.ToLower(strings.TrimSpace(p))] = p
	}
	for _, r := range sr.Responses {
		recipient := strings.TrimSpace(r.Recipient.Href)
		if recipient == "" {
			recipient = strings.TrimSpace(r.Recipient.Text)
		}
		if len(recipient) >= 7 && strings.EqualFold(recipient[:7], "mailto:") {
			recipient = recipient[7:]
		}
		person, ok := byAddr[strings.ToLower(recipient)]
		if !ok || !strings.HasPrefix(strings.TrimSpace(r.RequestStatus), "2.") {
			continue
		}
		// Success without a whole VFREEBUSY tells nothing, so the person
		// stays unknown rather than free.
		busy, err := parseFreeBusy(r.CalendarData, from, to)
		if err != nil {
			continue
		}
		out[person] = calendar.FreeBusy{Known: true, Busy: busy}
	}
	return nil
}

// freeBusyRequest builds the iTIP VFREEBUSY REQUEST. Addresses with a
// line break or control character are refused, so none can add a property.
func freeBusyRequest(organizer string, people []string, from, to, now time.Time) ([]byte, error) {
	uid := rand.Text()
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\r\n") }
	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:" + prodID)
	line("METHOD:REQUEST")
	line("BEGIN:VFREEBUSY")
	line("UID:" + uid)
	line("DTSTAMP:" + now.UTC().Format("20060102T150405Z"))
	line("DTSTART:" + from.UTC().Format("20060102T150405Z"))
	line("DTEND:" + to.UTC().Format("20060102T150405Z"))
	if !safeAddress(organizer) {
		return nil, fmt.Errorf("%w: bad organizer address", calendar.ErrInvalidICS)
	}
	line("ORGANIZER:mailto:" + organizer)
	for _, p := range people {
		p = strings.TrimSpace(p)
		if !safeAddress(p) {
			return nil, fmt.Errorf("%w: bad attendee address", calendar.ErrInvalidICS)
		}
		line("ATTENDEE:mailto:" + p)
	}
	line("END:VFREEBUSY")
	line("END:VCALENDAR")
	return fold([]byte(b.String())), nil
}

func safeAddress(s string) bool {
	if s == "" || !strings.Contains(s, "@") {
		return false
	}
	for _, r := range s {
		if r < 0x21 || r == 0x7f || r == ';' || r == ',' || r == '"' || r == ':' {
			return false
		}
	}
	return true
}

// maxFreeBusyDepth bounds the nesting of components in a free/busy answer.
const maxFreeBusyDepth = 8

var errFreeBusyData = fmt.Errorf("%w: not a complete VFREEBUSY", calendar.ErrInvalidICS)

// parseFreeBusy reads a free/busy answer, which must be exactly one
// VCALENDAR with paired BEGIN and END lines, nested at most
// maxFreeBusyDepth deep, holding at least one complete VFREEBUSY directly.
// Anything else, such as truncated data or another component, is an error,
// so the person stays unknown. The FREEBUSY properties of those VFREEBUSY
// components are read: busy, busy-tentative and busy-unavailable periods
// (FREE ones are skipped), clipped to [from, to) and merged. Periods are
// start/end or start/duration, in UTC as RFC 5545 requires for FREEBUSY.
func parseFreeBusy(data string, from, to time.Time) ([]calendar.BusyBlock, error) {
	if len(data) > calendar.MaxICSBytes {
		return nil, calendar.ErrTooLarge
	}
	var (
		stack  []string
		done   bool // END:VCALENDAR was read
		found  bool // a VFREEBUSY of the VCALENDAR was closed
		blocks []calendar.BusyBlock
	)
	for _, l := range unfold(data) {
		l = strings.TrimSuffix(l, "\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		if done {
			return nil, errFreeBusyData
		}
		name, value, ok := strings.Cut(l, ":")
		if !ok {
			return nil, errFreeBusyData
		}
		params := strings.Split(name, ";")
		switch prop := strings.ToUpper(strings.TrimSpace(params[0])); {
		case prop == "BEGIN":
			comp := strings.ToUpper(strings.TrimSpace(value))
			// VFREEBUSY holds properties only (RFC 5545 3.6.4): a component
			// inside it would hide busy periods from the depth check below.
			inFreeBusy := len(stack) > 0 && stack[len(stack)-1] == "VFREEBUSY"
			if (len(stack) == 0) != (comp == "VCALENDAR") || len(stack) >= maxFreeBusyDepth || inFreeBusy {
				return nil, errFreeBusyData
			}
			stack = append(stack, comp)
		case prop == "END":
			comp := strings.ToUpper(strings.TrimSpace(value))
			if len(stack) == 0 || stack[len(stack)-1] != comp {
				return nil, errFreeBusyData
			}
			stack = stack[:len(stack)-1]
			found = found || (comp == "VFREEBUSY" && len(stack) == 1)
			done = len(stack) == 0
		case len(stack) == 0:
			return nil, errFreeBusyData
		case prop == "FREEBUSY" && len(stack) == 2 && stack[1] == "VFREEBUSY":
			bs, err := busyPeriods(params[1:], value, from, to)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, bs...)
		}
	}
	if !done || !found {
		return nil, errFreeBusyData
	}
	return calendar.MergeBusy(blocks), nil
}

// busyPeriods reads the periods of one FREEBUSY property with the given
// parameters, skipping FBTYPE=FREE, clipped to [from, to).
func busyPeriods(params []string, value string, from, to time.Time) ([]calendar.BusyBlock, error) {
	fbtype := "BUSY"
	for _, p := range params {
		k, v, _ := strings.Cut(p, "=")
		if strings.EqualFold(strings.TrimSpace(k), "FBTYPE") {
			fbtype = strings.ToUpper(strings.Trim(strings.TrimSpace(v), `"`))
		}
	}
	if fbtype == "FREE" {
		return nil, nil
	}
	var blocks []calendar.BusyBlock
	for _, period := range strings.Split(value, ",") {
		b, ok := parsePeriod(strings.TrimSpace(period))
		if !ok {
			return nil, fmt.Errorf("%w: bad FREEBUSY period", calendar.ErrInvalidICS)
		}
		if b.Start.Before(from) {
			b.Start = from
		}
		if b.End.After(to) {
			b.End = to
		}
		if b.End.After(b.Start) {
			blocks = append(blocks, b)
		}
	}
	return blocks, nil
}

func parsePeriod(s string) (calendar.BusyBlock, bool) {
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		return calendar.BusyBlock{}, false
	}
	start, err := time.Parse("20060102T150405Z", a)
	if err != nil {
		return calendar.BusyBlock{}, false
	}
	if end, err := time.Parse("20060102T150405Z", b); err == nil {
		return calendar.BusyBlock{Start: start, End: end}, true
	}
	d, ok := parseDuration(b)
	if !ok {
		return calendar.BusyBlock{}, false
	}
	return calendar.BusyBlock{Start: start, End: start.Add(d)}, true
}

// parseDuration reads an RFC 5545 positive DURATION such as PT1H30M or P1D.
func parseDuration(s string) (time.Duration, bool) {
	s = strings.TrimPrefix(s, "+")
	if !strings.HasPrefix(s, "P") || len(s) < 3 {
		return 0, false
	}
	var d time.Duration
	inTime := false
	n := 0
	digits := false
	for _, r := range s[1:] {
		switch {
		case r >= '0' && r <= '9':
			n = n*10 + int(r-'0')
			digits = true
			if n > 1_000_000 {
				return 0, false
			}
		case r == 'T':
			inTime = true
		case digits && r == 'W' && !inTime:
			d += time.Duration(n) * 7 * 24 * time.Hour
		case digits && r == 'D' && !inTime:
			d += time.Duration(n) * 24 * time.Hour
		case digits && r == 'H' && inTime:
			d += time.Duration(n) * time.Hour
		case digits && r == 'M' && inTime:
			d += time.Duration(n) * time.Minute
		case digits && r == 'S' && inTime:
			d += time.Duration(n) * time.Second
		default:
			return 0, false
		}
		if r < '0' || r > '9' {
			if r != 'T' {
				n, digits = 0, false
			}
		}
	}
	return d, d > 0 && !digits
}

// unfold splits iCalendar text into logical lines, in linear time.
func unfold(s string) []string {
	s = unfolder.Replace(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(s, "\n")
}

var unfolder = strings.NewReplacer("\r\n ", "", "\r\n\t", "", "\n ", "", "\n\t", "")
