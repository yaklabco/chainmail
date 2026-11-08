package compose

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// replyICS is an iTIP REPLY with CRLF line endings, a folded line and a
// non-ASCII name, as a calendar library writes one.
const replyICS = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//chainmail//EN\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\n" +
	"UID:abc@example.org\r\nDTSTAMP:20261108T120000Z\r\nSEQUENCE:0\r\nORGANIZER:mailto:ann@example.org\r\n" +
	"ATTENDEE;PARTSTAT=ACCEPTED;CN=Jérôme Example and a name long enough to fold onto a se\r\n cond line:mailto:james@example.com\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

func calendarReply() *CalendarAlternative {
	return &CalendarAlternative{Method: "REPLY", Data: []byte(replyICS)}
}

func TestBuildCalendarShape(t *testing.T) {
	t.Parallel()
	img := File{ContentType: "image/png", Data: pngData}
	att := File{Filename: "a.txt", ContentType: "text/plain", Data: []byte("a")}
	tests := []struct {
		name string
		edit func(*Spec)
		want string
	}{
		{"markdown", func(*Spec) {}, "multipart/alternative[text/plain,text/html,text/calendar]"},
		{"text", func(s *Spec) { s.Body = Body{Kind: Text, Source: "Accepted."} }, "multipart/alternative[text/plain,text/calendar]"},
		{"html", func(s *Spec) { s.Body = Body{Kind: HTML, Source: "<p>Accepted.</p>"} }, "multipart/alternative[text/plain,text/html,text/calendar]"},
		{"text with attachment", func(s *Spec) {
			s.Body = Body{Kind: Text, Source: "Accepted."}
			s.Attachments = []File{att}
		}, "multipart/mixed[multipart/alternative[text/plain,text/calendar],text/plain]"},
		{"inline", func(s *Spec) {
			s.Body.Source = "![x](x.png)"
			s.Inline = map[string]File{"x.png": img}
		}, "multipart/related[multipart/alternative[text/plain,text/html,text/calendar],image/png]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := baseSpec()
			s.Calendar = calendarReply()
			tt.edit(&s)
			if got := parse(t, mustBuild(t, s).Raw).shape(); got != tt.want {
				t.Errorf("shape = %s, want %s", got, tt.want)
			}
		})
	}
}

// The calendar part comes last in multipart/alternative, as RFC 6047 puts
// it, with the method and charset in its media type, no disposition, and
// its data back byte for byte. The other parts are what they are without
// it.
func TestBuildCalendarPart(t *testing.T) {
	t.Parallel()
	for _, body := range []Body{
		{Kind: Text, Source: "Accepted.\n"},
		{Kind: Markdown, Source: "Accepted **gladly**.\n"},
		{Kind: HTML, Source: "<p>Accepted.</p>", HTMLText: "Accepted.\n"},
	} {
		s := baseSpec()
		s.Body = body
		plain := parse(t, mustBuild(t, s).Raw)
		s.Calendar = calendarReply()
		m := mustBuild(t, s)
		root := parse(t, m.Raw)
		if root.mediaType != "multipart/alternative" || len(root.parts) < 2 {
			t.Fatalf("kind %d: shape = %s", body.Kind, root.shape())
		}
		cal := root.parts[len(root.parts)-1]
		if cal.mediaType != "text/calendar" || cal.params["method"] != "REPLY" || cal.params["charset"] != "utf-8" || len(cal.params) != 2 {
			t.Errorf("kind %d: calendar part = %s %v, want text/calendar with method=REPLY and charset=utf-8", body.Kind, cal.mediaType, cal.params)
		}
		if got := cal.header.Get("Content-Type"); got != "text/calendar; charset=utf-8; method=REPLY" {
			t.Errorf("kind %d: Content-Type = %q", body.Kind, got)
		}
		if cal.header.Has("Content-Disposition") {
			t.Errorf("kind %d: calendar part has a disposition: %q", body.Kind, cal.header.Get("Content-Disposition"))
		}
		if enc := cal.header.Get("Content-Transfer-Encoding"); enc != quotedPrintable {
			t.Errorf("kind %d: calendar part encoding = %q, want quoted-printable", body.Kind, enc)
		}
		if cal.raw != replyICS {
			t.Errorf("kind %d: calendar data = %q, want %q", body.Kind, cal.raw, replyICS)
		}
		text := root.find("text/plain")
		if want := plain.find("text/plain"); text == nil || want == nil || text.body != want.body || text.params["format"] != want.params["format"] {
			t.Errorf("kind %d: text part changed with a calendar part", body.Kind)
		}
		if htm, want := root.find("text/html"), plain.find("text/html"); (htm == nil) != (want == nil) || htm != nil && htm.body != want.body {
			t.Errorf("kind %d: HTML part changed with a calendar part", body.Kind)
		}
		for _, line := range bytes.Split(m.Raw, []byte("\r\n")) {
			if len(line) > 78 {
				t.Errorf("kind %d: line of %d bytes: %q", body.Kind, len(line), line)
			}
		}
	}
}

// The method is written upper-cased, as iTIP names methods.
func TestBuildCalendarMethodCase(t *testing.T) {
	t.Parallel()
	s := baseSpec()
	s.Calendar = &CalendarAlternative{Method: "request", Data: []byte(replyICS)}
	cal := parse(t, mustBuild(t, s).Raw).find("text/calendar")
	if cal == nil || cal.params["method"] != "REQUEST" {
		t.Errorf("calendar part = %+v, want method=REQUEST", cal)
	}
}

func TestBuildCalendarInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cal  CalendarAlternative
	}{
		{"no method", CalendarAlternative{Data: []byte(replyICS)}},
		{"method with a space", CalendarAlternative{Method: "RE PLY", Data: []byte(replyICS)}},
		{"method with a parameter", CalendarAlternative{Method: "REPLY; x=y", Data: []byte(replyICS)}},
		{"method with a line break", CalendarAlternative{Method: "REPLY\r\nBcc: x@example.org", Data: []byte(replyICS)}},
		{"method too long", CalendarAlternative{Method: strings.Repeat("X", 65), Data: []byte(replyICS)}},
		{"no data", CalendarAlternative{Method: "REPLY"}},
		{"data not UTF-8", CalendarAlternative{Method: "REPLY", Data: []byte("BEGIN:VCALENDAR\r\nX:\xff\r\nEND:VCALENDAR\r\n")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := baseSpec()
			s.Calendar = &tt.cal
			_, err := build(s, &seqReader{})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if strings.Contains(err.Error(), "VCALENDAR") || strings.Contains(err.Error(), "Bcc") {
				t.Errorf("err = %q, want it without the calendar data or method", err)
			}
		})
	}
}
