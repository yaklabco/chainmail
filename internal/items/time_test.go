package items

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFormatTime(t *testing.T) {
	t.Parallel()
	plusTwo := time.FixedZone("plus two", 2*60*60)
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{name: "zero is empty", in: time.Time{}, want: ""},
		{name: "whole second", in: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), want: "2026-11-06T12:00:00.000000000Z"},
		{name: "nanoseconds", in: time.Date(2026, 11, 6, 12, 0, 0, 5, time.UTC), want: "2026-11-06T12:00:00.000000005Z"},
		{name: "another zone renders in UTC", in: time.Date(2026, 11, 6, 14, 30, 0, 123456789, plusTwo), want: "2026-11-06T12:30:00.123456789Z"},
	}
	for _, tt := range tests {
		if got := formatTime(tt.in); got != tt.want {
			t.Errorf("%s: formatTime = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// Every non-zero rendering has one width, so the text columns sort in time
// order.
func TestFormatTimeSortsAsText(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var texts []string
	for _, d := range []time.Duration{0, time.Nanosecond, time.Microsecond, time.Millisecond, time.Second, time.Hour, 24 * time.Hour} {
		s := formatTime(base.Add(d))
		if len(s) != 30 {
			t.Errorf("formatTime(base+%v) = %q, %d bytes; want 30", d, s, len(s))
		}
		texts = append(texts, s)
	}
	if !slices.IsSorted(texts) {
		t.Errorf("renderings are not in time order: %q", texts)
	}
}

func TestParseTime(t *testing.T) {
	t.Parallel()
	plusTwo := time.FixedZone("plus two", 2*60*60)
	for _, in := range []time.Time{
		time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 6, 12, 0, 0, 999999999, time.UTC),
		time.Date(2026, 11, 6, 14, 30, 0, 7, plusTwo),
	} {
		got, err := parseTime(formatTime(in))
		if err != nil || !got.Equal(in) || got.Location() != time.UTC {
			t.Errorf("parseTime(formatTime(%v)) = %v, %v; want the same instant in UTC", in, got, err)
		}
	}
	if got, err := parseTime(""); err != nil || !got.IsZero() {
		t.Errorf("parseTime(\"\") = %v, %v; want the zero time", got, err)
	}
	for _, bad := range []string{"yesterday", "2026-11-06", "2026-11-06T12:00:00Z", "2026-11-06 12:00:00.000000000Z"} {
		if _, err := parseTime(bad); err == nil {
			t.Errorf("parseTime(%q) succeeded, want an error", bad)
		}
	}
}

// A stored time that does not parse gives a fixed error, so the column value
// never travels in error text.
func TestParseTimeErrorHoldsNoInput(t *testing.T) {
	t.Parallel()
	const marker = "marker-7f3a"
	_, err := parseTime(marker)
	if err == nil {
		t.Fatal("parseTime succeeded, want an error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("parseTime error %q repeats its input", err)
	}
	if !errors.Is(err, errStoredTime) {
		t.Errorf("parseTime error = %v, want errStoredTime", err)
	}
}
