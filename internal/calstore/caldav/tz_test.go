package caldav

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// testZoneID is a TZID no zone database knows, so calendar.ParseICS has to
// read the generated VTIMEZONE rules instead of loading the zone by name.
const testZoneID = "X-Chainmail-Test-Zone"

// evaluate encodes vtz under testZoneID with one event at the wall time
// local and returns the instant calendar.ParseICS reads for it.
func evaluate(t *testing.T, vtz *ical.Component, local time.Time) time.Time {
	t.Helper()
	vtz.Props.Get(ical.PropTimezoneID).Value = testZoneID
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, prodID)
	cal.Props.SetText(ical.PropVersion, "2.0")
	ev := ical.NewComponent(ical.CompEvent)
	setRaw(ev.Props, ical.PropUID, "tz-test")
	setRaw(ev.Props, ical.PropDateTimeStamp, "20260101T000000Z")
	start := ical.NewProp(ical.PropDateTimeStart)
	start.Params.Set(ical.ParamTimezoneID, testZoneID)
	start.Value = local.Format("20060102T150405")
	ev.Props.Set(start)
	cal.Children = []*ical.Component{vtz, ev}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		t.Fatal(err)
	}
	_, evs, err := calendar.ParseICS(buf.Bytes())
	if err != nil {
		t.Fatalf("ParseICS: %v\n%s", err, buf.String())
	}
	return evs[0].Start
}

func TestVTimezoneMatchesGoZoneData(t *testing.T) {
	tests := []struct {
		zone        string
		first, last int
		open        bool
		wantRule    bool
	}{
		{"America/Los_Angeles", 2026, 2026, true, true},
		{"America/New_York", 2026, 2028, false, true},
		{"Europe/London", 2026, 2026, true, true},
		{"Europe/Berlin", 2025, 2030, false, true},
		{"Australia/Sydney", 2026, 2027, true, true},
		{"Asia/Kolkata", 2026, 2026, true, false},
		{"America/Sao_Paulo", 2017, 2021, false, false},
		{"America/Sao_Paulo", 2018, 2018, true, false},
		{"America/Chicago", 2005, 2005, true, true},
		{"Pacific/Auckland", 2026, 2026, true, true},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s %d-%d open=%v", tc.zone, tc.first, tc.last, tc.open), func(t *testing.T) {
			loc, err := time.LoadLocation(tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			vtz := vtimezone(tc.zone, loc, tc.first, tc.last, tc.open)
			hasRule := false
			for _, c := range vtz.Children {
				if c.Props.Get(ical.PropRecurrenceRule) != nil {
					hasRule = true
				}
			}
			if hasRule != tc.wantRule {
				t.Errorf("RRULE written = %v, want %v", hasRule, tc.wantRule)
			}
			last := tc.last
			if tc.open {
				last = tc.last + 12
			}
			// Every month at 10:00 and 03:30 local time over the span,
			// and the days around each transition.
			var checks []time.Time
			for y := tc.first; y <= last; y++ {
				for m := time.January; m <= time.December; m++ {
					checks = append(checks, time.Date(y, m, 15, 10, 0, 0, 0, loc), time.Date(y, m, 1, 3, 30, 0, 0, loc))
				}
			}
			for _, tr := range transitions(loc, time.Date(tc.first, 1, 1, 0, 0, 0, 0, loc), time.Date(last+1, 1, 1, 0, 0, 0, 0, loc)) {
				checks = append(checks, tr.at.Add(-26*time.Hour).In(loc), tr.at.Add(26*time.Hour).In(loc))
			}
			for _, want := range checks {
				local := time.Date(want.Year(), want.Month(), want.Day(), want.Hour(), want.Minute(), 0, 0, time.UTC)
				if got := evaluate(t, vtz, local); !got.Equal(want) {
					t.Errorf("%s local %s: VTIMEZONE gives %s, Go gives %s", tc.zone,
						local.Format("2006-01-02 15:04"), got.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339))
				}
			}
		})
	}
}

func TestVTimezoneFixedZoneAndBounds(t *testing.T) {
	vtz := vtimezone("UTC-ish", time.FixedZone("X", 3*3600+1800), 2026, 2026, true)
	if len(vtz.Children) != 1 {
		t.Fatalf("children %d, want one fixed observance", len(vtz.Children))
	}
	c := vtz.Children[0]
	if c.Props.Get(ical.PropTimezoneOffsetFrom).Value != "+0330" || c.Props.Get(ical.PropTimezoneOffsetTo).Value != "+0330" {
		t.Errorf("offsets %v", c.Props)
	}
	// A century-long series is bounded.
	ny, _ := time.LoadLocation("America/New_York")
	vtz = vtimezone("America/New_York", ny, 1900, 9999, false)
	if n := len(vtz.Children); n == 0 || n > tzMaxTransitions {
		t.Errorf("children %d", n)
	}
	if offsetString(-(4*3600+56*60+2)) != "-045602" || offsetString(0) != "+0000" {
		t.Error("offsetString")
	}
}

func TestAddTimezonesKeepsDefinedZones(t *testing.T) {
	raw := strings.Replace(seriesICS, "BEGIN:VEVENT", "BEGIN:VTIMEZONE\r\nTZID:America/New_York\r\nX-ORIGINAL:1\r\n"+
		"BEGIN:STANDARD\r\nDTSTART:19700101T000000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:-0500\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT", 1)
	o, err := decodeObject([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	cal := o.cal
	addTimezones(cal, fixedNow)
	n := 0
	for _, c := range cal.Children {
		if c.Name == ical.CompTimezone {
			n++
			if c.Props.Get("X-ORIGINAL") == nil {
				t.Error("the stored VTIMEZONE was replaced")
			}
		}
	}
	if n != 1 {
		t.Errorf("%d VTIMEZONEs, want the stored one only", n)
	}

	o, err = decodeObject([]byte(seriesICS))
	if err != nil {
		t.Fatal(err)
	}
	cal = o.cal
	addTimezones(cal, fixedNow)
	if cal.Children[0].Name != ical.CompTimezone {
		t.Fatalf("no VTIMEZONE added first: %s", cal.Children[0].Name)
	}
	if first, last, open := span(cal); first != 2026 || last != 2026 || !open {
		t.Errorf("span = %d %d %v, want 2026 2026 open (COUNT has no UNTIL)", first, last, open)
	}
}
