package calendar

import (
	"errors"
	"strings"
	"testing"
)

func inviteICS(method, extra string) string {
	m := ""
	if method != "" {
		m = "METHOD:" + method + "\r\n"
	}
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\n" + m +
		"BEGIN:VEVENT\r\nUID:inv-1@example.com\r\nDTSTAMP:20261101T120000Z\r\nSEQUENCE:2\r\nDTSTART:20261110T090000Z\r\nDTEND:20261110T100000Z\r\n" +
		"ORGANIZER:mailto:org@example.com\r\nATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:me@example.org\r\n" + extra + "END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
}

func TestParseInviteMethods(t *testing.T) {
	for _, method := range []string{MethodRequest, MethodCancel, MethodUpdate, "request"} {
		t.Run(method, func(t *testing.T) {
			inv, err := ParseInvite([]byte(inviteICS(method, "")), "utf-8")
			if err != nil {
				t.Fatalf("ParseInvite: %v", err)
			}
			if inv.Method != strings.ToUpper(method) || inv.UID != "inv-1@example.com" || inv.Sequence != 2 {
				t.Fatalf("invite %+v", inv)
			}
			if !inv.DTStamp.Equal(utc(2026, 11, 1, 12, 0, 0)) || len(inv.Events) != 1 {
				t.Fatalf("dtstamp %v events %d", inv.DTStamp, len(inv.Events))
			}
		})
	}
}

func TestParseInviteFixtures(t *testing.T) {
	for _, name := range []string{"google_invite.ics", "outlook_invite.ics"} {
		inv, err := ParseInvite(readFixture(t, name), "")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if inv.Method != MethodRequest || inv.UID == "" {
			t.Fatalf("%s: %+v", name, inv)
		}
	}
}

func TestParseInviteRejects(t *testing.T) {
	for _, method := range []string{"PUBLISH", "REPLY", "COUNTER", ""} {
		if _, err := ParseInvite([]byte(inviteICS(method, "")), "utf-8"); !errors.Is(err, ErrNotInvite) {
			t.Errorf("method %q: err = %v, want ErrNotInvite", method, err)
		}
	}

	twoUIDs := strings.Replace(inviteICS(MethodRequest, ""), "END:VCALENDAR",
		"BEGIN:VEVENT\r\nUID:other@example.com\r\nDTSTAMP:20261101T120000Z\r\nDTSTART:20261111T090000Z\r\nEND:VEVENT\r\nEND:VCALENDAR", 1)
	if _, err := ParseInvite([]byte(twoUIDs), "utf-8"); !errors.Is(err, ErrInvalidICS) {
		t.Errorf("two UIDs: err = %v, want ErrInvalidICS", err)
	}

	noUID := strings.Replace(inviteICS(MethodRequest, ""), "UID:inv-1@example.com\r\n", "", 1)
	if _, err := ParseInvite([]byte(noUID), "utf-8"); !errors.Is(err, ErrInvalidICS) {
		t.Errorf("no UID: err = %v, want ErrInvalidICS", err)
	}

	todo := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nMETHOD:REQUEST\r\nBEGIN:VTODO\r\nUID:t\r\nDTSTAMP:20261101T120000Z\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
	if _, err := ParseInvite([]byte(todo), "utf-8"); !errors.Is(err, ErrNotInvite) {
		t.Errorf("no VEVENT: err = %v, want ErrNotInvite", err)
	}

	if _, err := ParseInvite([]byte(inviteICS(MethodRequest, "")), "koi8-r"); !errors.Is(err, ErrInvalidICS) {
		t.Errorf("unsupported charset: err = %v, want ErrInvalidICS", err)
	}

	big := []byte(inviteICS(MethodRequest, "DESCRIPTION:"+strings.Repeat("a", MaxICSBytes)+"\r\n"))
	if _, err := ParseInvite(big, "utf-8"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("too large: err = %v, want ErrTooLarge", err)
	}
}

func TestParseInviteCharsets(t *testing.T) {
	// "Café" and a euro sign: é is 0xE9 in both Latin-1 and windows-1252;
	// the euro sign is 0x80 in windows-1252 only.
	latin1 := []byte(inviteICS(MethodRequest, "SUMMARY:Caf\xe9 \xbd\r\n"))
	inv, err := ParseInvite(latin1, "ISO-8859-1")
	if err != nil {
		t.Fatalf("iso-8859-1: %v", err)
	}
	if got := inv.Events[0].Summary; got != "Café ½" {
		t.Fatalf("iso-8859-1 summary = %q", got)
	}

	cp := []byte(inviteICS(MethodRequest, "SUMMARY:\x80 5 \x93quoted\x94 caf\xe9\r\n"))
	inv, err = ParseInvite(cp, "windows-1252")
	if err != nil {
		t.Fatalf("windows-1252: %v", err)
	}
	if got := inv.Events[0].Summary; got != "€ 5 “quoted” café" {
		t.Fatalf("windows-1252 summary = %q", got)
	}

	for _, cs := range []string{"", "us-ascii", "UTF-8", "utf8"} {
		inv, err := ParseInvite([]byte(inviteICS(MethodRequest, "SUMMARY:Café\r\n")), cs)
		if err != nil || inv.Events[0].Summary != "Café" {
			t.Fatalf("charset %q: %v %q", cs, err, inv.Events[0].Summary)
		}
	}

	bom := append([]byte("\xef\xbb\xbf"), inviteICS(MethodRequest, "")...)
	if _, err := ParseInvite(bom, "utf-8"); err != nil {
		t.Fatalf("UTF-8 BOM: %v", err)
	}
}
