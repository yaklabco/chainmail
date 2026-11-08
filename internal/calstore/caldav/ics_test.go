package caldav

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
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

// hostileObject is a stored object with an unknown property holding a long
// run of bytes that are not valid UTF-8.
func hostileObject() []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\n" +
		"BEGIN:VEVENT\r\nUID:hostile@example.com\r\nDTSTAMP:20261101T000000Z\r\n" +
		"DTSTART:20261120T100000Z\r\nDTEND:20261120T110000Z\r\nSUMMARY:Planning\r\n" +
		"X-HOSTILE:" + strings.Repeat("\x80", 4096) + "\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n")
}

func TestDecodeObjectRefusesInvalidUTF8(t *testing.T) {
	if _, err := decodeObject(hostileObject()); !errors.Is(err, calendar.ErrInvalidICS) {
		t.Fatalf("decodeObject = %v, want ErrInvalidICS", err)
	}
}

// TestEncodeObjectWithInvalidUTF8Finishes encodes a go-ical tree holding
// octets that are not UTF-8, which decodeObject no longer lets through, to
// check that folding still ends.
func TestEncodeObjectWithInvalidUTF8Finishes(t *testing.T) {
	cal, err := decodeAccepted(hostileObject())
	if err != nil {
		t.Fatalf("decodeAccepted: %v", err)
	}
	var out []byte
	finishes(t, "encodeObject", func() { out, err = encodeObject(cal) })
	if err != nil {
		t.Fatalf("encodeObject: %v", err)
	}
	for i, line := range strings.Split(string(out), "\r\n") {
		if len(line) > 75 {
			t.Errorf("line %d is %d octets", i, len(line))
		}
	}
}
