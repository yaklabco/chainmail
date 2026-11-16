package daemon

import (
	"testing"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// selfAttendee finds the account's own entry the way selfOf knows the
// account: without regard to case or stray spaces around the address, and
// returns the address as listed so the answer names the same entry.
func TestSelfAttendeeMatchesLikeSelfOf(t *testing.T) {
	t.Parallel()
	self := ownAddressMatcher("me@example.com")
	attendees := func(emails ...string) calendar.Event {
		var ev calendar.Event
		for _, e := range emails {
			ev.Attendees = append(ev.Attendees, calendar.Attendee{Person: calendar.Person{Email: e}})
		}
		return ev
	}
	for _, tt := range []struct {
		name string
		ev   calendar.Event
		want string
	}{
		{"exact", attendees("boss@example.com", "me@example.com"), "me@example.com"},
		{"case", attendees("Me@Example.com"), "Me@Example.com"},
		{"stray spaces", attendees(" me@example.com\t"), " me@example.com\t"},
		{"another own account", attendees("me@other.example"), ""},
		{"none", attendees(), ""},
	} {
		if got := selfAttendee(tt.ev, self); got != tt.want {
			t.Errorf("%s: selfAttendee = %q, want %q", tt.name, got, tt.want)
		}
	}
	if ownAddressMatcher("")("") {
		t.Error("an account with no address knows the empty address")
	}
}
