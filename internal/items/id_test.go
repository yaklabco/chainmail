package items

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// failingReader fails every read, as a broken random source would.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestNewID(t *testing.T) {
	t.Parallel()
	// The ULID spec's example time, whose time part is 01ARYZ6S41.
	specTime := time.UnixMilli(1469918176385)
	tests := []struct {
		name   string
		prefix string
		now    time.Time
		random []byte
		want   string
	}{
		{
			name: "counting random bytes", prefix: "it_", now: specTime,
			random: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, want: "it_01aryz6s41000g40r40m30e209",
		},
		{
			name: "every random bit set", prefix: "it_", now: specTime,
			random: bytes.Repeat([]byte{0xff}, 10), want: "it_01aryz6s41zzzzzzzzzzzzzzzz",
		},
		{
			name: "criterion at the epoch", prefix: "cr_", now: time.UnixMilli(0),
			random: make([]byte, 10), want: "cr_00000000000000000000000000",
		},
		{
			name: "last 48-bit millisecond", prefix: "it_", now: time.UnixMilli(1<<48 - 1),
			random: bytes.Repeat([]byte{0xff}, 10), want: "it_7zzzzzzzzzzzzzzzzzzzzzzzzz",
		},
		// Only the low 48 bits of the millisecond count are kept, so a time
		// past the 48-bit range wraps around to the start of it.
		{
			name: "first millisecond past 48 bits", prefix: "it_", now: time.UnixMilli(1 << 48),
			random: make([]byte, 10), want: "it_00000000000000000000000000",
		},
		{
			name: "past 48 bits keeps the low bits", prefix: "it_", now: time.UnixMilli(1<<48 + 33),
			random: make([]byte, 10), want: "it_00000000110000000000000000",
		},
		// A time before 1970 is a negative count, which the mask turns into
		// its 48-bit two's complement.
		{
			name: "one millisecond before the epoch", prefix: "it_", now: time.UnixMilli(-1),
			random: make([]byte, 10), want: "it_7zzzzzzzzz0000000000000000",
		},
		{
			name: "years before the epoch", prefix: "cr_", now: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC),
			random: make([]byte, 10), want: "cr_" + wrappedTime(-2208988800000) + "0000000000000000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := NewID(tt.prefix, tt.now, bytes.NewReader(tt.random)); got != tt.want {
				t.Errorf("NewID = %q, want %q", got, tt.want)
			}
		})
	}
}

// NewID reads exactly the ten random bytes it needs.
func TestNewIDReadsTenBytes(t *testing.T) {
	t.Parallel()
	r := bytes.NewReader(make([]byte, 25))
	NewID("it_", time.UnixMilli(0), r)
	if r.Len() != 15 {
		t.Errorf("%d bytes left unread, want 15", r.Len())
	}
}

// An ID from a later millisecond sorts after one from an earlier
// millisecond as text, whatever their random parts.
func TestNewIDSortsByTime(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 100 {
		random := bytes.Repeat([]byte{0xff}, 10) // the highest random part
		if i%2 == 1 {
			random = make([]byte, 10) // the lowest
		}
		ids = append(ids, NewID("it_", base.Add(time.Duration(i)*time.Millisecond), bytes.NewReader(random)))
	}
	if !slices.IsSorted(ids) {
		t.Errorf("IDs from successive milliseconds are not sorted: %q", ids)
	}
}

// A nil source reads crypto/rand, and a source that fails or runs short
// falls back to it. Either way the ID is well formed and two IDs differ.
func TestNewIDRandomSources(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		rnd  func() io.Reader
	}{
		{name: "nil", rnd: func() io.Reader { return nil }},
		{name: "failing", rnd: func() io.Reader { return failingReader{} }},
		{name: "short", rnd: func() io.Reader { return bytes.NewReader([]byte{1, 2, 3}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, b := NewID("it_", now, tt.rnd()), NewID("it_", now, tt.rnd())
			for _, id := range []string{a, b} {
				if len(id) != 29 || !ValidID(id) {
					t.Errorf("NewID = %q, want it_ and 26 characters", id)
				}
				for _, c := range id[3:] {
					if !strings.ContainsRune(idCrockford, c) {
						t.Errorf("NewID = %q holds %q, outside lowercase Crockford base32", id, c)
					}
				}
			}
			if a[:13] != b[:13] {
				t.Errorf("IDs %q and %q from one millisecond have different time parts", a, b)
			}
			if a == b {
				t.Errorf("two IDs are both %q, want different random parts", a)
			}
		})
	}
}

func TestValidID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		id   string
		want bool
	}{
		{AdminWorkID, true},
		{AdminPersonalID, true},
		{NewID("it_", time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), nil), true},
		{"it_a", true},
		{"it_0", true},
		{"it_" + strings.Repeat("a", 60), true},
		{"it_" + strings.Repeat("a", 61), false},
		{"it_", false},
		{"", false},
		{"IT_abc", false},
		{"it_ABC", false},
		{"cr_abc", false},
		{NewID("cr_", time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), nil), false},
		{"it-abc", false},
		{"it_a-b", false},
		{"it_a b", false},
		{"it_é", false},
		{" it_abc", false},
		{"it_abc\n", false},
	}
	for _, tt := range tests {
		if got := ValidID(tt.id); got != tt.want {
			t.Errorf("ValidID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
	if AdminWorkID != "it_admin_work" || AdminPersonalID != "it_admin_personal" {
		t.Errorf("Admin IDs = %q, %q", AdminWorkID, AdminPersonalID)
	}
}

func TestValidCriterionID(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		id   string
		want bool
	}{
		{NewID("cr_", now, nil), true},
		{"cr_a", true},
		{"cr_" + strings.Repeat("a", 60), true},
		{"cr_" + strings.Repeat("a", 61), false},
		{"cr_", false},
		{"", false},
		{"CR_abc", false},
		{"cr_ABC", false},
		{NewID("it_", now, nil), false},
		{AdminWorkID, false},
		{"cr-abc", false},
		{"cr_a b", false},
		{"cr_abc\n", false},
	}
	for _, tt := range tests {
		if got := ValidCriterionID(tt.id); got != tt.want {
			t.Errorf("ValidCriterionID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

// wrappedTime renders ms as NewID's ten time characters, worked out by hand
// from the 48-bit two's complement, independently of NewID's loop.
func wrappedTime(ms int64) string {
	v := uint64(ms) % (1 << 48)
	out := make([]byte, 10)
	for i := range out {
		shift := uint(45 - 5*i)
		out[i] = idCrockford[(v>>shift)&31]
	}
	return string(out)
}
