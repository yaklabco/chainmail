package calendar

import (
	"bytes"
	"errors"
	"testing"
)

func TestBuildReplySeries(t *testing.T) {
	_, events := mustParse(t, readFixture(t, "google_invite.ics"))
	ev := events[0]
	now := utc(2026, 11, 8, 12, 0, 0)
	data, err := BuildReply(ev, "James@Example.org", PartStatAccepted, "See you there; thanks", now)
	if err != nil {
		t.Fatalf("BuildReply: %v", err)
	}
	method, got := mustParse(t, data)
	if method != MethodReply || len(got) != 1 {
		t.Fatalf("method %q, %d events; want REPLY, 1", method, len(got))
	}
	r := got[0]
	if r.UID != ev.UID || r.Sequence != ev.Sequence || !r.RecurrenceID.IsZero() {
		t.Fatalf("reply UID %q seq %d rid %v", r.UID, r.Sequence, r.RecurrenceID)
	}
	if r.Organizer == nil || r.Organizer.Email != "alice@example.com" || r.Organizer.Name != "Alice Example" {
		t.Fatalf("organizer not kept: %+v", r.Organizer)
	}
	if len(r.Attendees) != 1 {
		t.Fatalf("reply carries %d attendees, want only self", len(r.Attendees))
	}
	if a := r.Attendees[0]; a.Email != "James@Example.org" || a.PartStat != PartStatAccepted {
		t.Fatalf("attendee %+v", a)
	}
	if !r.Updated.Equal(now) {
		t.Fatalf("DTSTAMP read back as %v, want %v", r.Updated, now)
	}
	if !bytes.Contains(data, []byte("COMMENT:See you there\\; thanks\r\n")) {
		t.Fatalf("COMMENT missing or unescaped:\n%s", data)
	}
	for _, leak := range []string{"Weekly sync", "Room 4", "RRULE", "EXDATE", "alice@example.com\r\nATTENDEE"} {
		if bytes.Contains(data, []byte(leak)) {
			t.Fatalf("reply carries %q:\n%s", leak, data)
		}
	}
}

func TestBuildReplyInstance(t *testing.T) {
	ev := Event{
		UID:          "series@example.com",
		RecurrenceID: utc(2026, 11, 17, 17, 0, 0),
		Start:        utc(2026, 11, 17, 17, 0, 0),
		End:          utc(2026, 11, 17, 18, 0, 0),
		TZID:         "America/Los_Angeles",
		Sequence:     5,
		Organizer:    &Person{Email: "org@example.com"},
	}
	data, err := BuildReply(ev, "me@example.org", PartStatTentative, "", utc(2026, 11, 8, 0, 0, 0))
	if err != nil {
		t.Fatalf("BuildReply: %v", err)
	}
	_, got := mustParse(t, data)
	r := got[0]
	if !r.RecurrenceID.Equal(ev.RecurrenceID) || r.Attendees[0].PartStat != PartStatTentative || r.Sequence != 5 {
		t.Fatalf("instance reply: rid %v partstat %q seq %d", r.RecurrenceID, r.Attendees[0].PartStat, r.Sequence)
	}
	if bytes.Contains(data, []byte("COMMENT")) {
		t.Fatal("COMMENT written without a comment")
	}
}

func TestBuildReplyDeclinedKeepsName(t *testing.T) {
	ev := Event{
		UID: "x@example.com", Start: utc(2026, 11, 17, 17, 0, 0), End: utc(2026, 11, 17, 18, 0, 0),
		Organizer: &Person{Email: "org@example.com", Name: "Org"},
		Attendees: []Attendee{{Person: Person{Email: "me@example.org", Name: "Me Myself"}, PartStat: PartStatNeedsAction, RSVP: true}},
	}
	data, err := BuildReply(ev, "ME@example.org", PartStatDeclined, "", utc(2026, 11, 8, 0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	_, got := mustParse(t, data)
	if a := got[0].Attendees[0]; a.PartStat != PartStatDeclined || a.Name != "Me Myself" {
		t.Fatalf("attendee %+v", a)
	}
}

func TestBuildReplyRejects(t *testing.T) {
	ev := Event{UID: "x@example.com", Start: utc(2026, 11, 17, 17, 0, 0), Organizer: &Person{Email: "org@example.com"}}
	now := utc(2026, 11, 8, 0, 0, 0)
	noOrg := ev
	noOrg.Organizer = nil
	noUID := ev
	noUID.UID = ""
	cases := map[string]func() error{
		"no organizer":     func() error { _, err := BuildReply(noOrg, "me@example.org", PartStatAccepted, "", now); return err },
		"no uid":           func() error { _, err := BuildReply(noUID, "me@example.org", PartStatAccepted, "", now); return err },
		"no self":          func() error { _, err := BuildReply(ev, "", PartStatAccepted, "", now); return err },
		"self newline":     func() error { _, err := BuildReply(ev, "me@x\r\nATTENDEE:y", PartStatAccepted, "", now); return err },
		"needs-action":     func() error { _, err := BuildReply(ev, "me@example.org", PartStatNeedsAction, "", now); return err },
		"empty partstat":   func() error { _, err := BuildReply(ev, "me@example.org", "", "", now); return err },
		"unknown partstat": func() error { _, err := BuildReply(ev, "me@example.org", "MAYBE", "", now); return err },
	}
	for name, f := range cases {
		if err := f(); !errors.Is(err, ErrInvalidICS) {
			t.Errorf("%s: err = %v, want ErrInvalidICS", name, err)
		}
	}
	// A comment is text: a line break in it is escaped, not refused.
	if _, err := BuildReply(ev, "me@example.org", PartStatAccepted, "a\r\nATTENDEE:mailto:x@evil", now); err != nil {
		t.Fatalf("comment with a line break: %v", err)
	}
}
