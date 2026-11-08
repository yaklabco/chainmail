package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
)

// createReq returns the one CreateEventRequest f received, with its
// derived idempotency key checked and cleared.
func createReq(t *testing.T, f *fakeCalendar) *chainmailv1.CreateEventRequest {
	t.Helper()
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %v, want one CreateEvent", reqs)
	}
	req, ok := proto.Clone(reqs[0]).(*chainmailv1.CreateEventRequest)
	if !ok {
		t.Fatalf("request = %T, want CreateEventRequest", reqs[0])
	}
	return req
}

func TestEventCreateRequests(t *testing.T) {
	t.Parallel()
	ny := func(day, hour, minute int) time.Time {
		return time.Date(2026, 11, day, hour, minute, 0, 0, mustZone(t, "America/New_York"))
	}
	// An all-day event goes on the wire as 00:00 UTC of its dates.
	utcDay := func(day int) time.Time { return time.Date(2026, 11, day, 0, 0, 0, 0, time.UTC) }
	tests := []struct {
		name string
		args []string
		want *chainmailv1.CreateEventRequest
	}{
		{
			name: "a meeting with attendees, for half an hour",
			args: []string{
				"--account", "work", "--title", "Planning", "--start", "2026-11-10 09:00", "--duration", "30m",
				"--attendee", "alice@example.com", "--attendee", "bob@example.com", "--location", "Room 4",
				"--description", "Agenda to follow.",
			},
			want: &chainmailv1.CreateEventRequest{
				Account: "work", Title: "Planning", Start: ts(ny(10, 9, 0)), End: ts(ny(10, 9, 30)),
				Attendees: []string{"alice@example.com", "bob@example.com"}, Location: "Room 4", Description: "Agenda to follow.",
			},
		},
		{
			name: "an end, a calendar, a zone and a weekly rule",
			args: []string{
				"--title", "Gym", "--start", "2026-11-10 07:00", "--end", "2026-11-10 08:00", "--tz", "Europe/London",
				"--calendar", "/dav/calendars/me/personal/", "--rrule", "FREQ=WEEKLY;BYDAY=TU",
			},
			want: &chainmailv1.CreateEventRequest{
				CalendarId: "/dav/calendars/me/personal/", Title: "Gym", TimeZone: "Europe/London",
				Start: ts(time.Date(2026, 11, 10, 7, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)),
				RecurrenceRule: "FREQ=WEEKLY;BYDAY=TU",
			},
		},
		{
			name: "all day, the end date included",
			args: []string{"--title", "Offsite", "--all-day", "--start", "2026-11-11", "--end", "2026-11-13"},
			want: &chainmailv1.CreateEventRequest{Title: "Offsite", AllDay: true, Start: ts(utcDay(11)), End: ts(utcDay(14))},
		},
		{
			name: "all day for one day by default",
			args: []string{"--title", "Holiday", "--all-day", "--start", "2026-11-26"},
			want: &chainmailv1.CreateEventRequest{Title: "Holiday", AllDay: true, Start: ts(utcDay(26)), End: ts(utcDay(27))},
		},
		{
			name: "all day for two days by duration",
			args: []string{"--title", "Trip", "--all-day", "--start", "2026-11-26", "--duration", "2d"},
			want: &chainmailv1.CreateEventRequest{Title: "Trip", AllDay: true, Start: ts(utcDay(26)), End: ts(utcDay(28))},
		},
		{
			name: "rfc 3339 times",
			args: []string{"--title", "Call", "--start", "2026-11-10T15:00:00Z", "--end", "2026-11-10T15:45:00Z"},
			want: &chainmailv1.CreateEventRequest{
				Title: "Call", Start: ts(time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 10, 15, 45, 0, 0, time.UTC)),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCalendar{}
			if got := runCal(t, f, append([]string{"event", "create"}, tt.args...)...); got.code != 0 {
				t.Fatalf("event create: %+v", got)
			}
			req := createReq(t, f)
			if key := req.GetIdempotencyKey(); !strings.HasPrefix(key, "cli-") || len(key) > 128 {
				t.Errorf("idempotency key = %q, want a derived cli- key", key)
			}
			req.IdempotencyKey = ""
			if !proto.Equal(req, tt.want) {
				t.Errorf("request = %v, want %v", req, tt.want)
			}
		})
	}
}

// runCalIn runs chainmail against f at calNow with the CLI in loc.
func runCalIn(t *testing.T, f *fakeCalendar, loc *time.Location, args ...string) cliResult {
	t.Helper()
	env := Env{TZ: loc, Now: func() time.Time { return calNow }}
	return runCLI(t, env, append([]string{"--socket", startFakeCalendar(t, f)}, args...)...)
}

// An all-day event carries 00:00 UTC of the dates typed, end exclusive,
// whatever zone the CLI runs in or --tz names (Amendment 17). The zones
// here are far from UTC, so local midnight falls on another UTC date.
func TestEventCreateAllDayDatesAreUTCMidnights(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, time.UTC) }
	tests := []struct {
		name       string
		zone       string
		args       []string
		start, end time.Time
		tz         string
	}{
		{name: "fourteen hours ahead", zone: "Pacific/Kiritimati", args: []string{"--start", "2026-11-11", "--end", "2026-11-13"}, start: day(11), end: day(14)},
		{name: "eleven hours behind", zone: "Pacific/Pago_Pago", args: []string{"--start", "2026-11-11", "--duration", "2d"}, start: day(11), end: day(13)},
		{name: "one day by default", zone: "Asia/Tokyo", args: []string{"--start", "2026-11-30"}, start: day(30), end: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)},
		{
			name: "--tz names the zone the dates are read in", zone: "Pacific/Kiritimati",
			args: []string{"--start", "2026-11-11", "--tz", "America/Los_Angeles"}, start: day(11), end: day(12), tz: "America/Los_Angeles",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCalendar{}
			args := append([]string{"event", "create", "--title", "Away", "--all-day"}, tt.args...)
			if got := runCalIn(t, f, mustZone(t, tt.zone), args...); got.code != 0 {
				t.Fatalf("event create: %+v", got)
			}
			req := createReq(t, f)
			if got := req.GetStart().AsTime(); !got.Equal(tt.start) {
				t.Errorf("start = %v, want %v", got, tt.start)
			}
			if got := req.GetEnd().AsTime(); !got.Equal(tt.end) {
				t.Errorf("end = %v, want %v", got, tt.end)
			}
			if !req.GetAllDay() || req.GetTimeZone() != tt.tz {
				t.Errorf("all_day = %v, time_zone = %q, want true and %q", req.GetAllDay(), req.GetTimeZone(), tt.tz)
			}
		})
	}
}

// The same create derives the same key, so a repeat does not create
// twice; a change to any field derives another; --idempotency-key wins.
func TestEventCreateDerivesItsKey(t *testing.T) {
	t.Parallel()
	key := func(args ...string) string {
		t.Helper()
		f := &fakeCalendar{}
		base := []string{"event", "create", "--account", "work", "--title", "Planning", "--start", "2026-11-10 09:00", "--duration", "1h"}
		if got := runCal(t, f, append(base, args...)...); got.code != 0 {
			t.Fatalf("event create: %+v", got)
		}
		return createReq(t, f).GetIdempotencyKey()
	}
	first := key("--attendee", "a@example.com", "--attendee", "b@example.com")
	if again := key("--attendee", "b@example.com", "--attendee", "a@example.com"); again != first {
		t.Errorf("attendee order changed the key: %q, then %q", first, again)
	}
	for _, change := range [][]string{
		{"--attendee", "a@example.com"},
		{"--attendee", "a@example.com", "--attendee", "b@example.com", "--location", "Room 4"},
		{"--attendee", "a@example.com", "--attendee", "b@example.com", "--calendar", "team"},
		{"--attendee", "a@example.com", "--attendee", "b@example.com", "--rrule", "FREQ=DAILY"},
	} {
		if other := key(change...); other == first {
			t.Errorf("%v derived the same key as the first create", change)
		}
	}
	if got := key("--idempotency-key", "plan-2026-11-10"); got != "plan-2026-11-10" {
		t.Errorf("key = %q, want the given key", got)
	}
}

func TestEventCreateUsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		msg  string
	}{
		{name: "no title", args: []string{"--start", "2026-11-10 09:00", "--duration", "1h"}, msg: "--title is required"},
		{name: "no start", args: []string{"--title", "x", "--duration", "1h"}, msg: "--start is required"},
		{name: "no end", args: []string{"--title", "x", "--start", "2026-11-10 09:00"}, msg: "give --end or --duration"},
		{name: "end and duration", args: []string{"--title", "x", "--start", "2026-11-10 09:00", "--end", "2026-11-10 10:00", "--duration", "1h"}, msg: "--end and --duration cannot be used together"},
		{name: "bad duration", args: []string{"--title", "x", "--start", "2026-11-10 09:00", "--duration", "soon"}, msg: "--duration"},
		{name: "negative duration", args: []string{"--title", "x", "--start", "2026-11-10 09:00", "--duration", "-1h"}, msg: "--duration must be positive"},
		{name: "end before start", args: []string{"--title", "x", "--start", "2026-11-10 09:00", "--end", "2026-11-10 08:00"}, msg: "--end must be after --start"},
		{name: "all day with a time", args: []string{"--title", "x", "--all-day", "--start", "2026-11-10 09:00"}, msg: "--all-day takes dates"},
		{name: "all day with part days", args: []string{"--title", "x", "--all-day", "--start", "2026-11-10", "--duration", "36h"}, msg: "whole days"},
		{name: "bad zone", args: []string{"--title", "x", "--start", "2026-11-10 09:00", "--duration", "1h", "--tz", "Nowhere"}, msg: "unknown --tz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCalendar{}
			got := runCal(t, f, append([]string{"event", "create"}, tt.args...)...)
			if got.code != api.ExitUsage || !strings.Contains(errorMessage(t, got.stderr), tt.msg) {
				t.Errorf("%v: %+v, want exit 2 with %q", tt.args, got, tt.msg)
			}
			if reqs := f.requests(); len(reqs) != 0 {
				t.Errorf("requests = %v, want none", reqs)
			}
		})
	}
}

// A create, an edit, a cancel or an RSVP the policy holds prints its
// result on stdout and exits 4 with HELD and the daemon's hint.
func TestCalendarHeldExitsFour(t *testing.T) {
	t.Parallel()
	heldCreate := &chainmailv1.CalendarHeld{
		Hint:     "created without sending invitations; send them from the calendar app",
		Blocked:  []*chainmailv1.Untrusted{untrusted("stranger@example.net")},
		EventRef: calRef,
	}
	heldChange := &chainmailv1.CalendarHeld{Hint: "not applied; make the change in the calendar app", Summary: "start, end: 2026-11-10T15:00:00Z to 2026-11-10T16:00:00Z; 3 attendees", TooMany: true}
	heldReply := &chainmailv1.CalendarHeld{Hint: "the reply is in Drafts; send it from there", DraftRef: "cm1:work:Drafts:3:9", Blocked: []*chainmailv1.Untrusted{untrusted("al\x1b[2Jice@example.com")}}
	f := &fakeCalendar{
		create: func(*chainmailv1.CreateEventRequest) (*chainmailv1.CreateEventResponse, error) {
			ev := standupFixture()
			ev.Ref = calRef
			return &chainmailv1.CreateEventResponse{Event: ev, Held: heldCreate}, nil
		},
		update: func(*chainmailv1.UpdateEventRequest) (*chainmailv1.UpdateEventResponse, error) {
			return &chainmailv1.UpdateEventResponse{Held: heldChange}, nil
		},
		cancel: func(*chainmailv1.CancelEventRequest) (*chainmailv1.CancelEventResponse, error) {
			return &chainmailv1.CancelEventResponse{Held: heldChange}, nil
		},
		respond: func(*chainmailv1.RespondRequest) (*chainmailv1.RespondResponse, error) {
			return &chainmailv1.RespondResponse{Held: heldReply, Via: "email"}, nil
		},
	}
	tests := []struct {
		name   string
		args   []string
		hint   string
		stdout string
	}{
		{
			name: "create",
			args: []string{"event", "create", "--title", "x", "--start", "2026-11-10 09:00", "--duration", "1h", "--attendee", "stranger@example.net"},
			hint: heldCreate.GetHint(),
			stdout: "held by policy: created " + calRef + " as a draft event; no invitations were sent\n" +
				"blocked: stranger@example.net\n" + "hint: " + heldCreate.GetHint() + "\n",
		},
		{
			name: "edit",
			args: []string{"event", "edit", calRef, "--etag", `"e1"`, "--title", "y"},
			hint: heldChange.GetHint(),
			stdout: "held by policy, not applied: " + calRef + "\n" + "change: " + heldChange.GetSummary() + "\n" +
				"too many attendees\n" + "hint: " + heldChange.GetHint() + "\n",
		},
		{
			name: "cancel",
			args: []string{"event", "cancel", calRef, "--etag", `"e1"`},
			hint: heldChange.GetHint(),
			stdout: "held by policy, not applied: " + calRef + "\n" + "change: " + heldChange.GetSummary() + "\n" +
				"too many attendees\n" + "hint: " + heldChange.GetHint() + "\n",
		},
		{
			name: "rsvp",
			args: []string{"rsvp", calRef, "accept"},
			hint: heldReply.GetHint(),
			stdout: "held by policy, not answered: " + calRef + "\n" + "reply saved to Drafts: cm1:work:Drafts:3:9\n" +
				"blocked: al[2Jice@example.com\n" + "hint: " + heldReply.GetHint() + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := runCal(t, f, append([]string{"--output", "table"}, tt.args...)...)
			if got.code != api.ExitHeld || errorReason(t, got.stderr) != api.ReasonHeld || !strings.Contains(got.stderr, tt.hint) {
				t.Errorf("%v: %+v, want exit 4 with HELD and the hint", tt.args, got)
			}
			if got.stdout != tt.stdout {
				t.Errorf("stdout = %q, want %q", got.stdout, tt.stdout)
			}
		})
	}
	// json output is the response, then the same exit.
	got := runCal(t, f, "--output", "json", "rsvp", calRef, "decline")
	if got.code != api.ExitHeld || !strings.Contains(got.stdout, `"draft_ref":"cm1:work:Drafts:3:9"`) {
		t.Errorf("rsvp json: %+v", got)
	}
}

func TestEventCreateTable(t *testing.T) {
	t.Parallel()
	f := &fakeCalendar{create: func(*chainmailv1.CreateEventRequest) (*chainmailv1.CreateEventResponse, error) {
		return &chainmailv1.CreateEventResponse{Event: offsiteFixture()}, nil
	}}
	got := runCal(t, f, "--output", "table", "event", "create", "--title", "Offsite", "--all-day", "--start", "2026-11-11", "--end", "2026-11-13")
	if want := "created " + calRef2 + " on home /dav/calendars/me/personal/: Wed 2026-11-11 to Fri 2026-11-13, all day\n"; got.code != 0 || got.stdout != want {
		t.Errorf("event create: %+v, want stdout %q", got, want)
	}
}

func TestRsvp(t *testing.T) {
	t.Parallel()
	f := &fakeCalendar{respond: func(*chainmailv1.RespondRequest) (*chainmailv1.RespondResponse, error) {
		ev := standupFixture()
		ev.MyPartstat = "TENTATIVE"
		return &chainmailv1.RespondResponse{Event: ev, Via: "store"}, nil
	}}
	got := runCal(t, f, "--output", "table", "rsvp", calInstRef, "tentative", "--comment", "may be late", "--idempotency-key", "k1")
	if want := "answered " + calInstRef + ": tentative, through the calendar\n"; got.code != 0 || got.stdout != want {
		t.Errorf("rsvp: %+v, want stdout %q", got, want)
	}
	want := []proto.Message{&chainmailv1.RespondRequest{Ref: calInstRef, Response: "tentative", Comment: "may be late", IdempotencyKey: "k1"}}
	if reqs := f.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
		t.Errorf("requests = %v, want %v", reqs, want)
	}

	email := &fakeCalendar{respond: func(*chainmailv1.RespondRequest) (*chainmailv1.RespondResponse, error) {
		return &chainmailv1.RespondResponse{Via: "email", SentRef: "cm1:host:Sent:2:5"}, nil
	}}
	got = runCal(t, email, "--output", "table", "rsvp", calRef3, "decline")
	if want := "answered " + calRef3 + ": decline, by email to the organizer\nsaved to Sent: cm1:host:Sent:2:5\n"; got.code != 0 || got.stdout != want {
		t.Errorf("rsvp by email: %+v, want stdout %q", got, want)
	}

	bad := &fakeCalendar{}
	got = runCal(t, bad, "rsvp", calRef, "maybe")
	if got.code != api.ExitUsage || !strings.Contains(got.stderr, "accept, decline or tentative") || len(bad.requests()) != 0 {
		t.Errorf("rsvp maybe: %+v, want a usage error", got)
	}
}

// edit sends only the fields its flags name, in the mask's order, with the
// etag given, or else the one the event has now.
func TestEventEditRequests(t *testing.T) {
	t.Parallel()
	current := standupFixture() // 14:00 to 14:30 UTC, etag "e1"
	tests := []struct {
		name    string
		args    []string
		want    *chainmailv1.UpdateEventRequest
		readsIt bool
	}{
		{
			name: "title with an etag",
			args: []string{"--etag", `"e0"`, "--title", "Standup v2"},
			want: &chainmailv1.UpdateEventRequest{Ref: calInstRef, Etag: `"e0"`, Title: "Standup v2", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}}},
		},
		{
			name:    "the etag read first",
			args:    []string{"--location", ""},
			readsIt: true,
			want:    &chainmailv1.UpdateEventRequest{Ref: calInstRef, Etag: `"e1"`, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"location"}}},
		},
		{
			name:    "a new start keeps the length",
			args:    []string{"--start", "2026-11-10 10:00"},
			readsIt: true,
			want: &chainmailv1.UpdateEventRequest{
				Ref: calInstRef, Etag: `"e1"`, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"start", "end"}},
				Start: ts(time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 10, 15, 30, 0, 0, time.UTC)),
			},
		},
		{
			name:    "a duration from the current start",
			args:    []string{"--etag", `"e1"`, "--duration", "1h"},
			readsIt: true,
			want: &chainmailv1.UpdateEventRequest{
				Ref: calInstRef, Etag: `"e1"`, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"end"}},
				End: ts(time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)),
			},
		},
		{
			name: "every field",
			args: []string{
				"--etag", "x", "--title", "T", "--start", "2026-11-10 10:00", "--end", "2026-11-10 11:00", "--tz", "UTC",
				"--location", "L", "--description", "D", "--attendee", "a@example.com", "--rrule", "",
			},
			want: &chainmailv1.UpdateEventRequest{
				Ref: calInstRef, Etag: "x", Title: "T", TimeZone: "UTC", Location: "L", Description: "D",
				Start: ts(time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)),
				Attendees:  []string{"a@example.com"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title", "start", "end", "time_zone", "location", "description", "attendees", "recurrence_rule"}},
			},
		},
		{
			name: "attendees cleared",
			args: []string{"--etag", "x", "--no-attendees"},
			want: &chainmailv1.UpdateEventRequest{Ref: calInstRef, Etag: "x", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"attendees"}}},
		},
		{
			name: "made all day",
			args: []string{"--etag", "x", "--all-day", "--start", "2026-11-10"},
			want: &chainmailv1.UpdateEventRequest{
				Ref: calInstRef, Etag: "x", AllDay: true, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"start", "end", "all_day"}},
				Start: ts(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCalendar{
				event: func(*chainmailv1.GetEventRequest) (*chainmailv1.GetEventResponse, error) {
					return &chainmailv1.GetEventResponse{Event: current}, nil
				},
				update: func(*chainmailv1.UpdateEventRequest) (*chainmailv1.UpdateEventResponse, error) {
					return &chainmailv1.UpdateEventResponse{Event: current}, nil
				},
			}
			got := runCal(t, f, append([]string{"--output", "table", "event", "edit", calInstRef}, tt.args...)...)
			if got.code != 0 || got.stdout != "updated "+calInstRef+"\n" {
				t.Fatalf("event edit: %+v", got)
			}
			want := []proto.Message{tt.want}
			if tt.readsIt {
				want = []proto.Message{&chainmailv1.GetEventRequest{Ref: calInstRef}, tt.want}
			}
			if reqs := f.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
				t.Errorf("requests = %v, want %v", reqs, want)
			}
		})
	}
}

func TestEventEditUsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		msg  string
	}{
		{name: "nothing to change", args: []string{"--etag", "x"}, msg: "nothing to change"},
		{name: "attendees and none", args: []string{"--attendee", "a@example.com", "--no-attendees"}, msg: "--attendee and --no-attendees cannot be used together"},
		{name: "end and duration", args: []string{"--end", "2026-11-10 10:00", "--duration", "1h"}, msg: "--end and --duration cannot be used together"},
		{name: "all day without dates", args: []string{"--all-day"}, msg: "--all-day needs --start"},
		{name: "no ref", args: nil, msg: "accepts 1 arg"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCalendar{}
			args := []string{"event", "edit"}
			if tt.args != nil {
				args = append(args, calRef)
			}
			got := runCal(t, f, append(args, tt.args...)...)
			if got.code != api.ExitUsage || !strings.Contains(errorMessage(t, got.stderr), tt.msg) {
				t.Errorf("%v: %+v, want exit 2 with %q", tt.args, got, tt.msg)
			}
			if reqs := f.requests(); len(reqs) != 0 {
				t.Errorf("requests = %v, want none", reqs)
			}
		})
	}
}

func TestEventCancel(t *testing.T) {
	t.Parallel()
	f := &fakeCalendar{
		event: func(*chainmailv1.GetEventRequest) (*chainmailv1.GetEventResponse, error) {
			return &chainmailv1.GetEventResponse{Event: offsiteFixture()}, nil
		},
		cancel: func(*chainmailv1.CancelEventRequest) (*chainmailv1.CancelEventResponse, error) {
			return &chainmailv1.CancelEventResponse{Cancelled: true}, nil
		},
	}
	got := runCal(t, f, "--output", "table", "event", "cancel", calRef2)
	if got.code != 0 || got.stdout != "cancelled "+calRef2+"\n" {
		t.Fatalf("event cancel: %+v", got)
	}
	want := []proto.Message{&chainmailv1.GetEventRequest{Ref: calRef2}, &chainmailv1.CancelEventRequest{Ref: calRef2, Etag: `"e2"`}}
	if reqs := f.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
		t.Errorf("requests = %v, want %v", reqs, want)
	}
	withTag := &fakeCalendar{}
	if got := runCal(t, withTag, "event", "cancel", calRef2, "--etag", "x"); got.code != 0 {
		t.Fatalf("event cancel --etag: %+v", got)
	}
	want = []proto.Message{&chainmailv1.CancelEventRequest{Ref: calRef2, Etag: "x"}}
	if reqs := withTag.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
		t.Errorf("requests = %v, want %v", reqs, want)
	}
}
