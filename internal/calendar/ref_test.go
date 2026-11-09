package calendar

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

var refPattern = regexp.MustCompile(`^ce_[0-9a-hjkmnp-tv-z]{26}(@[0-9]{8}T[0-9]{6}Z)?$`)

func TestEventRefStableAndPatternValid(t *testing.T) {
	a := EventRef("work", "primary", "evt-1")
	b := EventRef("work", "primary", "evt-1")
	if a != b {
		t.Fatalf("EventRef not stable: %q then %q", a, b)
	}
	if !refPattern.MatchString(a) {
		t.Fatalf("EventRef %q does not match the ref pattern", a)
	}
	// A fixed vector pins the encoding, so a refactor cannot silently change
	// every stored ref. It was computed outside Go:
	//   printf 'a\0b\0c' | sha256sum | cut -c1-32 | xxd -r -p | base32 |
	//   tr -d = | tr A-Z2-7 0123456789abcdefghjkmnpqrstvwxyz
	if got, want := EventRef("a", "b", "c"), "ce_hepxw467c3mve0pyzd5nw8jxww"; got != want {
		t.Fatalf("EventRef(a, b, c) = %q, want %q", got, want)
	}
}

func TestEventRefDistinctPerInput(t *testing.T) {
	inputs := [][3]string{
		{"work", "primary", "evt-1"},
		{"work", "primary", "evt-2"},
		{"work", "other", "evt-1"},
		{"home", "primary", "evt-1"},
		{"work", "primaryevt-1", ""},
		{"workprimary", "", "evt-1"},
		{"", "", ""},
	}
	seen := map[string][3]string{}
	for _, in := range inputs {
		r := EventRef(in[0], in[1], in[2])
		if prev, ok := seen[r]; ok {
			t.Fatalf("EventRef%q and EventRef%q collide: %q", prev, in, r)
		}
		seen[r] = in
	}
}

func TestInstanceRefRoundTrip(t *testing.T) {
	base := EventRef("work", "primary", "series-1")
	rid := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	inst := InstanceRef(base, rid)
	if want := base + "@20261110T150000Z"; inst != want {
		t.Fatalf("InstanceRef = %q, want %q", inst, want)
	}
	if !refPattern.MatchString(inst) {
		t.Fatalf("InstanceRef %q does not match the ref pattern", inst)
	}
	gotBase, gotRID, err := ParseEventRef(inst)
	if err != nil {
		t.Fatalf("ParseEventRef(%q): %v", inst, err)
	}
	if gotBase != base || !gotRID.Equal(rid) || gotRID.Location() != time.UTC {
		t.Fatalf("ParseEventRef = %q, %v; want %q, %v UTC", gotBase, gotRID, base, rid)
	}

	// A recurrence ID in another zone renders as the same UTC instant.
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	if got := InstanceRef(base, rid.In(la)); got != inst {
		t.Fatalf("InstanceRef in LA = %q, want %q", got, inst)
	}

	gotBase, gotRID, err = ParseEventRef(base)
	if err != nil || gotBase != base || !gotRID.IsZero() {
		t.Fatalf("ParseEventRef(base) = %q, %v, %v; want %q, zero, nil", gotBase, gotRID, err, base)
	}
}

func TestParseEventRefRejects(t *testing.T) {
	good := EventRef("work", "primary", "x")
	// The last character of a canonical ref carries three data bits and two
	// zero bits, so 1 and 2 never end one, though both are in the alphabet.
	tail := "1"
	if good[len(good)-1] == '1' {
		tail = "2"
	}
	nonCanonical := good[:len(good)-1] + tail
	cases := map[string]string{
		"empty":              "",
		"no prefix":          strings.TrimPrefix(good, "ce_"),
		"wrong prefix":       "cm_" + good[3:],
		"short":              good[:len(good)-1],
		"long":               good + "0",
		"uppercase":          strings.ToUpper(good),
		"excluded letter i":  good[:5] + "i" + good[6:],
		"excluded letter u":  good[:5] + "u" + good[6:],
		"non-canonical tail": nonCanonical,
		"bad rid":            good + "@20261310T150000Z",
		"rid without Z":      good + "@20261110T150000",
		"rid empty":          good + "@",
		"two at signs":       good + "@20261110T150000Z@20261110T150000Z",
		"huge":               good + strings.Repeat("@", 5000),
		"secret-ish":         "ce_<script>alert(1)</script>",
	}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseEventRef(s)
			if !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("ParseEventRef(%q) err = %v, want ErrInvalidRef", s, err)
			}
			if s != "" && len(s) > 3 && strings.Contains(err.Error(), s[3:]) {
				t.Fatalf("error %q repeats the input", err)
			}
		})
	}
}

func TestMailInviteRef(t *testing.T) {
	r := MailInviteRef("work", "abc@example.com")
	if !refPattern.MatchString(r) {
		t.Fatalf("MailInviteRef %q does not match the ref pattern", r)
	}
	if r != MailInviteRef("work", "abc@example.com") {
		t.Fatal("MailInviteRef not stable")
	}
	if r == MailInviteRef("home", "abc@example.com") || r == MailInviteRef("work", "abd@example.com") {
		t.Fatal("MailInviteRef not distinct per input")
	}
	for _, cal := range []string{"primary", "mail", "work@group.calendar.google.com", "/dav/calendars/james/default/"} {
		if r == EventRef("work", cal, "abc@example.com") {
			t.Fatalf("MailInviteRef equals EventRef with calendar %q", cal)
		}
	}
	if r != EventRef("work", "\x00mail", "abc@example.com") {
		t.Fatal(`MailInviteRef is not EventRef(account, "\x00mail", uid)`)
	}
}
