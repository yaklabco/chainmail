package calendar

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// finishes runs f and fails the test when it has not returned within a
// generous bound, so a folding loop that never ends fails instead of
// hanging the suite.
func finishes(t *testing.T, name string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", name)
	}
}

func TestEncodeICSInvalidUTF8Finishes(t *testing.T) {
	ev := Event{
		UID:   "x" + strings.Repeat("\x80", 4096),
		Start: utc(2026, 11, 20, 10, 0, 0),
		End:   utc(2026, 11, 20, 11, 0, 0),
	}
	var out []byte
	var err error
	finishes(t, "EncodeICS", func() { out, err = EncodeICS("", []Event{ev}) })
	if err != nil {
		t.Fatalf("EncodeICS: %v", err)
	}
	for i, line := range bytes.Split(out, []byte("\r\n")) {
		if len(line) > foldWidth {
			t.Errorf("line %d is %d octets", i, len(line))
		}
	}
}

func TestFoldAlwaysAdvances(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"continuation bytes", bytes.Repeat([]byte{0x80}, 1000)},
		{"after a name", append([]byte("X-A:"), bytes.Repeat([]byte{0xbf}, 500)...)},
		{"multibyte", []byte(strings.Repeat("é", 200))},
		{"lead bytes", bytes.Repeat([]byte{0xf0}, 300)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := append(bytes.Clone(tc.in), "\r\n"...)
			var out []byte
			var err error
			finishes(t, "Fold", func() { out, err = Fold(in, MaxICSBytes) })
			if err != nil {
				t.Fatalf("Fold: %v", err)
			}
			if got := bytes.ReplaceAll(out, []byte("\r\n "), nil); !bytes.Equal(got, in) {
				t.Errorf("unfolded output differs from the input")
			}
			for i, line := range bytes.Split(out, []byte("\r\n")) {
				if len(line) > foldWidth {
					t.Errorf("line %d is %d octets", i, len(line))
				}
			}
		})
	}
}

func TestFoldKeepsRunesWhole(t *testing.T) {
	in := []byte("X-A:" + strings.Repeat("é", 200) + "\r\n")
	out, err := Fold(in, MaxICSBytes)
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(out), "\r\n") {
		if !utf8.ValidString(line) {
			t.Errorf("line %d splits a rune", i)
		}
	}
}

func TestFoldRefusesOutputOverTheLimit(t *testing.T) {
	in := []byte(strings.Repeat("a", 1000) + "\r\n")
	if _, err := Fold(in, 900); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Fold over the limit: err = %v, want ErrTooLarge", err)
	}
	if _, err := Fold(in, 2000); err != nil {
		t.Errorf("Fold under the limit: %v", err)
	}
}

func manyEvents(n int) []byte {
	var b strings.Builder
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\n")
	for i := range n {
		b.WriteString("BEGIN:VEVENT\r\nUID:u" + strconv.Itoa(i) + "\r\nDTSTART:20261120T100000Z\r\nEND:VEVENT\r\n")
	}
	b.WriteString("END:VCALENDAR\r\n")
	return []byte(b.String())
}

func TestParseICSEventLimit(t *testing.T) {
	if _, evs, err := ParseICS(manyEvents(MaxObjectEvents)); err != nil || len(evs) != MaxObjectEvents {
		t.Fatalf("ParseICS at the limit = %d events, %v", len(evs), err)
	}
	if _, _, err := ParseICS(manyEvents(MaxObjectEvents + 1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ParseICS over the limit: err = %v, want ErrTooLarge", err)
	}
}

func TestZonesWall(t *testing.T) {
	data := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Europe/Berlin\r\nBEGIN:STANDARD\r\nDTSTART:19700101T000000\r\n" +
		"TZOFFSETFROM:+0300\r\nTZOFFSETTO:+0300\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nUID:u\r\nDTSTART;TZID=Europe/Berlin:20261110T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	cal, err := decodeCalendar(data)
	if err != nil {
		t.Fatal(err)
	}
	z := ZonesOf(cal)
	at := utc(2026, 11, 10, 6, 0, 0)
	for _, tc := range []struct {
		tzid string
		want time.Time
		ok   bool
	}{
		{"Europe/Berlin", utc(2026, 11, 10, 9, 0, 0), true},           // the object's +03:00
		{"Europe/Paris", utc(2026, 11, 10, 7, 0, 0), true},            // tzdata
		{"W. Europe Standard Time", utc(2026, 11, 10, 7, 0, 0), true}, // a Windows name
		{"Nowhere/Special", time.Time{}, false},
	} {
		got, ok := z.Wall(tc.tzid, at)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("Wall(%q) = %v, %v; want %v, %v", tc.tzid, got, ok, tc.want, tc.ok)
		}
	}
	_, evs, err := ParseICS(data)
	if err != nil || !evs[0].Start.Equal(at) {
		t.Fatalf("ParseICS start %v, %v; want %v", evs[0].Start, err, at)
	}
}
