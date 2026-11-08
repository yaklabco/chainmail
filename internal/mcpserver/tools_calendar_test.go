package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calendar"
)

var (
	// testEventRef is a single event's ref and testInstanceRef one instance
	// of a series.
	testEventRef    = calendar.EventRef("work", "primary", "evt1")
	testInstanceRef = calendar.InstanceRef(calendar.EventRef("work", "primary", "series1"), time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC))
	// calNow is the clock of every calendar test env.
	calNow = time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	calT1  = time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	calT2  = time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
)

// calEnv is a test env whose clock is calNow.
func calEnv(t *testing.T, f *fakeDaemon) *testEnv {
	t.Helper()
	return newTestEnv(t, f, latest, func(o *Options) { o.Now = func() time.Time { return calNow } })
}

// calNoCall fails the test if any CalendarService RPC reached the daemon.
func calNoCall(t *testing.T, f *fakeDaemon) {
	t.Helper()
	for _, c := range f.Calls() {
		if strings.HasPrefix(c.Procedure, "/chainmail.v1.CalendarService/") {
			t.Errorf("the request reached the daemon: %s %v", c.Procedure, c.Msg)
		}
	}
}

// TestCalendarToolValuesMatchTheModel ties the tools' lists to the proto and
// internal/calendar, so neither can drift from the tools.
func TestCalendarToolValuesMatchTheModel(t *testing.T) {
	t.Parallel()
	fields := (&chainmailv1.UpdateEventRequest{}).ProtoReflect().Descriptor().Fields()
	var names []string
	for i := range fields.Len() {
		switch n := string(fields.Get(i).Name()); n {
		case "ref", "etag", "update_mask":
		default:
			names = append(names, n)
		}
	}
	if !slices.Equal(names, eventUpdatePaths) {
		t.Errorf("eventUpdatePaths = %v, want the UpdateEventRequest value fields %v", eventUpdatePaths, names)
	}
	for _, p := range eventClearablePaths {
		if !slices.Contains(eventUpdatePaths, p) {
			t.Errorf("clearable path %q is not an update path", p)
		}
	}
	if want := []string{"accept", "decline", "tentative"}; !slices.Equal(rsvpResponses, want) {
		t.Errorf("rsvpResponses = %v, want %v", rsvpResponses, want)
	}
	if calendarMaxRange != 400*24*time.Hour {
		t.Errorf("calendarMaxRange = %v, want 400 days", calendarMaxRange)
	}
	for _, r := range []string{testEventRef, testInstanceRef, calendar.MailInviteRef("work", "uid@x.test")} {
		if _, _, err := calendar.ParseEventRef(r); err != nil {
			t.Fatalf("ParseEventRef(%q): %v", r, err)
		}
		if !eventRefPattern.MatchString(r) {
			t.Errorf("eventRefPattern refuses the ref %q", r)
		}
	}
	for _, r := range []string{"", "ce_", "CE_" + testEventRef[3:], strings.ToUpper(testEventRef), testEventRef + "@20261110", testEventRef[:28] + "i", testRef} {
		if _, _, err := calendar.ParseEventRef(r); err == nil {
			t.Fatalf("ParseEventRef(%q) accepts it", r)
		}
		if eventRefPattern.MatchString(r) {
			t.Errorf("eventRefPattern accepts %q, which ParseEventRef refuses", r)
		}
	}
}

// TestCalendarToolRequests checks each tool sends the request its input
// means, over the wire, with client kind mcp.
func TestCalendarToolRequests(t *testing.T) {
	t.Parallel()
	ts := timestamppb.New
	tests := []struct {
		name string
		tool string
		args map[string]any
		proc string
		want proto.Message
	}{
		{
			name: "list calendars of every account", tool: "list_calendars", args: map[string]any{},
			proc: chainmailv1connect.CalendarServiceListCalendarsProcedure, want: &chainmailv1.ListCalendarsRequest{},
		},
		{
			name: "list calendars of named accounts", tool: "list_calendars", args: map[string]any{"accounts": []string{"work", "home"}},
			proc: chainmailv1connect.CalendarServiceListCalendarsProcedure, want: &chainmailv1.ListCalendarsRequest{Accounts: []string{"work", "home"}},
		},
		{
			name: "agenda defaults to the next 7 days", tool: "agenda", args: map[string]any{},
			proc: chainmailv1connect.CalendarServiceAgendaProcedure,
			want: &chainmailv1.AgendaRequest{From: ts(calNow), To: ts(calNow.Add(7 * 24 * time.Hour))},
		},
		{
			name: "agenda for days from a time", tool: "agenda", args: map[string]any{"from": "2026-11-10T09:00:00+01:00", "days": 2},
			proc: chainmailv1connect.CalendarServiceAgendaProcedure,
			want: &chainmailv1.AgendaRequest{From: ts(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)), To: ts(time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC))},
		},
		{
			name: "agenda with every field", tool: "agenda",
			args: map[string]any{
				"accounts": []string{"work"}, "calendars": []string{"primary", "team@group.calendar.google.com"},
				"from": "2026-11-10T00:00:00Z", "to": "2027-12-15T00:00:00Z", "include_declined": true,
			},
			proc: chainmailv1connect.CalendarServiceAgendaProcedure,
			want: &chainmailv1.AgendaRequest{
				Accounts: []string{"work"}, Calendars: []string{"primary", "team@group.calendar.google.com"},
				From: ts(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)), To: ts(time.Date(2027, 12, 15, 0, 0, 0, 0, time.UTC)), IncludeDeclined: true,
			},
		},
		{
			name: "get an event", tool: "get_event", args: map[string]any{"ref": testEventRef},
			proc: chainmailv1connect.CalendarServiceGetEventProcedure, want: &chainmailv1.GetEventRequest{Ref: testEventRef},
		},
		{
			name: "get an instance", tool: "get_event", args: map[string]any{"ref": testInstanceRef},
			proc: chainmailv1connect.CalendarServiceGetEventProcedure, want: &chainmailv1.GetEventRequest{Ref: testInstanceRef},
		},
		{
			name: "list invites", tool: "list_invites", args: map[string]any{"accounts": []string{"home"}},
			proc: chainmailv1connect.CalendarServiceListInvitesProcedure, want: &chainmailv1.ListInvitesRequest{Accounts: []string{"home"}},
		},
		{
			name: "get an invite", tool: "get_invite", args: map[string]any{"ref": testEventRef},
			proc: chainmailv1connect.CalendarServiceGetInviteProcedure, want: &chainmailv1.GetInviteRequest{Ref: testEventRef},
		},
		{
			name: "free/busy for James alone", tool: "free_busy", args: map[string]any{},
			proc: chainmailv1connect.CalendarServiceFreeBusyProcedure,
			want: &chainmailv1.FreeBusyRequest{From: ts(calNow), To: ts(calNow.Add(7 * 24 * time.Hour))},
		},
		{
			name: "free/busy for people", tool: "free_busy",
			args: map[string]any{"account": "work", "people": []string{"ann@x.test", "bob@x.test"}, "from": "2026-11-10T09:00:00Z", "to": "2026-11-10T17:00:00Z"},
			proc: chainmailv1connect.CalendarServiceFreeBusyProcedure,
			want: &chainmailv1.FreeBusyRequest{Account: "work", People: []string{"ann@x.test", "bob@x.test"}, From: ts(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)), To: ts(time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC))},
		},
		{
			name: "rsvp", tool: "rsvp", args: map[string]any{"ref": testInstanceRef, "response": "decline"},
			proc: chainmailv1connect.CalendarServiceRespondProcedure, want: &chainmailv1.RespondRequest{Ref: testInstanceRef, Response: "decline"},
		},
		{
			name: "rsvp with every field", tool: "rsvp", args: map[string]any{"ref": testEventRef, "response": "tentative", "comment": "Might be late", "idempotency_key": "rsvp-1"},
			proc: chainmailv1connect.CalendarServiceRespondProcedure,
			want: &chainmailv1.RespondRequest{Ref: testEventRef, Response: "tentative", Comment: "Might be late", IdempotencyKey: "rsvp-1"},
		},
		{
			name: "create with every field", tool: "create_event",
			args: map[string]any{
				"account": "work", "calendar_id": "primary", "title": "Planning", "start": "2026-11-10T15:00:00Z", "end": "2026-11-10T16:00:00Z",
				"time_zone": "Europe/London", "location": "Room 1", "description": "Agenda first", "attendees": []string{"ann@x.test"},
				"recurrence_rule": "FREQ=WEEKLY;BYDAY=TU", "idempotency_key": "plan-1",
			},
			proc: chainmailv1connect.CalendarServiceCreateEventProcedure,
			want: &chainmailv1.CreateEventRequest{
				Account: "work", CalendarId: "primary", Title: "Planning", Start: ts(calT1), End: ts(calT2), TimeZone: "Europe/London",
				Location: "Room 1", Description: "Agenda first", Attendees: []string{"ann@x.test"}, RecurrenceRule: "FREQ=WEEKLY;BYDAY=TU",
				IdempotencyKey: "plan-1",
			},
		},
		{
			name: "create with a duration", tool: "create_event",
			args: map[string]any{"account": "work", "title": "Focus", "start": "2026-11-10T15:00:00Z", "duration": "1h", "idempotency_key": "focus-1"},
			proc: chainmailv1connect.CalendarServiceCreateEventProcedure,
			want: &chainmailv1.CreateEventRequest{Account: "work", Title: "Focus", Start: ts(calT1), End: ts(calT2), IdempotencyKey: "focus-1"},
		},
		{
			name: "create an all-day event", tool: "create_event",
			args: map[string]any{"account": "home", "title": "Away", "start": "2026-11-10T00:00:00Z", "end": "2026-11-12T00:00:00Z", "all_day": true, "idempotency_key": "away"},
			proc: chainmailv1connect.CalendarServiceCreateEventProcedure,
			want: &chainmailv1.CreateEventRequest{
				Account: "home", Title: "Away", Start: ts(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)),
				AllDay: true, IdempotencyKey: "away",
			},
		},
		{
			name: "update moves an event", tool: "update_event",
			args: map[string]any{"ref": testEventRef, "etag": `"e1"`, "start": "2026-11-10T15:00:00Z", "end": "2026-11-10T16:00:00Z"},
			proc: chainmailv1connect.CalendarServiceUpdateEventProcedure,
			want: &chainmailv1.UpdateEventRequest{
				Ref: testEventRef, Etag: `"e1"`, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"start", "end"}}, Start: ts(calT1), End: ts(calT2),
			},
		},
		{
			name: "update with every field", tool: "update_event",
			args: map[string]any{
				"ref": testInstanceRef, "etag": "e2", "title": "Planning", "start": "2026-11-10T15:00:00Z", "end": "2026-11-10T16:00:00Z",
				"time_zone": "Europe/London", "all_day": false, "location": "Room 2", "description": "New agenda",
				"attendees": []string{"ann@x.test", "bob@x.test"}, "recurrence_rule": "FREQ=DAILY",
			},
			proc: chainmailv1connect.CalendarServiceUpdateEventProcedure,
			want: &chainmailv1.UpdateEventRequest{
				Ref: testInstanceRef, Etag: "e2",
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title", "start", "end", "time_zone", "all_day", "location", "description", "attendees", "recurrence_rule"}},
				Title:      "Planning", Start: ts(calT1), End: ts(calT2), TimeZone: "Europe/London", Location: "Room 2", Description: "New agenda",
				Attendees: []string{"ann@x.test", "bob@x.test"}, RecurrenceRule: "FREQ=DAILY",
			},
		},
		{
			name: "update clears fields", tool: "update_event",
			args: map[string]any{"ref": testEventRef, "etag": "e3", "clear": []string{"recurrence_rule", "attendees", "location"}},
			proc: chainmailv1connect.CalendarServiceUpdateEventProcedure,
			want: &chainmailv1.UpdateEventRequest{
				Ref: testEventRef, Etag: "e3", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"location", "attendees", "recurrence_rule"}},
			},
		},
		{
			name: "cancel", tool: "cancel_event", args: map[string]any{"ref": testEventRef, "etag": "e4"},
			proc: chainmailv1connect.CalendarServiceCancelEventProcedure, want: &chainmailv1.CancelEventRequest{Ref: testEventRef, Etag: "e4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := calEnv(t, &fakeDaemon{}) // unfaked RPCs answer Unimplemented; the request is still recorded
			e.call(t, tt.tool, tt.args)
			c := e.fake.lastCall(t, tt.proc)
			if !proto.Equal(c.Msg, tt.want) {
				t.Errorf("request = %v, want %v", c.Msg, tt.want)
			}
			if c.Client != "mcp" {
				t.Errorf("client kind = %q, want mcp", c.Client)
			}
		})
	}
}

// TestCreateEventDerivesAKey: with no idempotency_key, create_event derives
// one from its input, so a repeat of the same call finds the event the first
// made. Attendee order does not change the key; any other field does.
func TestCreateEventDerivesAKey(t *testing.T) {
	t.Parallel()
	e := calEnv(t, &fakeDaemon{})
	key := func(args map[string]any) string {
		t.Helper()
		e.call(t, "create_event", args)
		return e.fake.lastCall(t, chainmailv1connect.CalendarServiceCreateEventProcedure).Msg.(*chainmailv1.CreateEventRequest).GetIdempotencyKey()
	}
	base := map[string]any{"account": "work", "title": "Planning", "start": "2026-11-10T15:00:00Z", "end": "2026-11-10T16:00:00Z", "attendees": []string{"a@x.test", "b@x.test"}}
	k1 := key(base)
	if !strings.HasPrefix(k1, keyPrefix) || len(k1) > maxKeyLen {
		t.Errorf("derived key %q, want the %s prefix and at most %d characters", k1, keyPrefix, maxKeyLen)
	}
	if k2 := key(base); k2 != k1 {
		t.Errorf("a repeat derived %q, want %q", k2, k1)
	}
	reordered := map[string]any{"account": "work", "title": "Planning", "start": "2026-11-10T15:00:00Z", "end": "2026-11-10T16:00:00Z", "attendees": []string{"b@x.test", "a@x.test"}}
	if k := key(reordered); k != k1 {
		t.Errorf("reordered attendees derived %q, want %q", k, k1)
	}
	moved := map[string]any{"account": "work", "title": "Planning", "start": "2026-11-10T16:00:00Z", "end": "2026-11-10T17:00:00Z", "attendees": []string{"a@x.test", "b@x.test"}}
	if k := key(moved); k == k1 {
		t.Errorf("a different start derived the same key %q", k)
	}
	if k := key(map[string]any{"account": "work", "title": "Planning", "start": "2026-11-10T15:00:00Z", "end": "2026-11-10T16:00:00Z", "idempotency_key": "mine"}); k != "mine" {
		t.Errorf("a given key became %q", k)
	}
}

// TestCalendarToolsRefuseLocally: input the schema accepts but the tools
// refuse never reaches the daemon.
func TestCalendarToolsRefuseLocally(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, tool string
		args       map[string]any
		inMessage  string
	}{
		{name: "a mail ref", tool: "get_event", args: map[string]any{"ref": testRef}, inMessage: "ref is not an event ref"},
		{name: "an invite ref", tool: "get_invite", args: map[string]any{"ref": "it_a"}, inMessage: "ref is not an event ref"},
		{name: "an rsvp ref", tool: "rsvp", args: map[string]any{"ref": "ce_x", "response": "accept"}, inMessage: "ref is not an event ref"},
		{name: "an update ref", tool: "update_event", args: map[string]any{"ref": "x", "etag": "e", "title": "t"}, inMessage: "ref is not an event ref"},
		{name: "a cancel ref", tool: "cancel_event", args: map[string]any{"ref": "x", "etag": "e"}, inMessage: "ref is not an event ref"},
		{name: "a bad from", tool: "agenda", args: map[string]any{"from": "tomorrow"}, inMessage: "from must be an RFC 3339 time"},
		{name: "a date for to", tool: "agenda", args: map[string]any{"to": "2026-11-12"}, inMessage: "to must be an RFC 3339 time"},
		{name: "to and days", tool: "agenda", args: map[string]any{"to": "2026-11-12T00:00:00Z", "days": 2}, inMessage: "give to or days, not both"},
		{name: "days above", tool: "agenda", args: map[string]any{"days": 401}, inMessage: "days must be 1 to 400"},
		{name: "days below", tool: "free_busy", args: map[string]any{"days": -1}, inMessage: "days must be 1 to 400"},
		{name: "to before from", tool: "agenda", args: map[string]any{"from": "2026-11-12T00:00:00Z", "to": "2026-11-10T00:00:00Z"}, inMessage: "to must be after from"},
		{name: "to equal to from", tool: "free_busy", args: map[string]any{"from": "2026-11-12T00:00:00Z", "to": "2026-11-12T00:00:00Z"}, inMessage: "to must be after from"},
		{name: "a range too long", tool: "agenda", args: map[string]any{"from": "2026-01-01T00:00:00Z", "to": "2027-02-06T00:00:00Z"}, inMessage: "at most 400 days apart"},
		{name: "people without an account", tool: "free_busy", args: map[string]any{"people": []string{"ann@x.test"}}, inMessage: "account is required with people"},
		{name: "an unknown response", tool: "rsvp", args: map[string]any{"ref": testEventRef, "response": "maybe"}, inMessage: "response must be accept, decline or tentative"},
		{name: "a response in capitals", tool: "rsvp", args: map[string]any{"ref": testEventRef, "response": "Accept"}, inMessage: "response must be accept, decline or tentative"},
		{name: "an empty start", tool: "create_event", args: map[string]any{"account": "work", "title": "t", "start": "", "end": "2026-11-10T16:00:00Z"}, inMessage: "start must be an RFC 3339 time"},
		{name: "no end", tool: "create_event", args: map[string]any{"account": "work", "title": "t", "start": "2026-11-10T15:00:00Z"}, inMessage: "give end or duration"},
		{name: "end and duration", tool: "create_event", args: map[string]any{"account": "work", "title": "t", "start": "2026-11-10T15:00:00Z", "end": "2026-11-10T16:00:00Z", "duration": "1h"}, inMessage: "give end or duration, not both"},
		{name: "a bad duration", tool: "create_event", args: map[string]any{"account": "work", "title": "t", "start": "2026-11-10T15:00:00Z", "duration": "an hour"}, inMessage: "duration must be a positive duration"},
		{name: "a negative duration", tool: "create_event", args: map[string]any{"account": "work", "title": "t", "start": "2026-11-10T15:00:00Z", "duration": "-1h"}, inMessage: "duration must be a positive duration"},
		{name: "end before start", tool: "create_event", args: map[string]any{"account": "work", "title": "t", "start": "2026-11-10T16:00:00Z", "end": "2026-11-10T15:00:00Z"}, inMessage: "end must be after start"},
		{name: "nothing to update", tool: "update_event", args: map[string]any{"ref": testEventRef, "etag": "e"}, inMessage: "give at least one field to change"},
		{name: "no etag to update", tool: "update_event", args: map[string]any{"ref": testEventRef, "etag": "", "title": "t"}, inMessage: "etag is required"},
		{name: "an unclearable field", tool: "update_event", args: map[string]any{"ref": testEventRef, "etag": "e", "clear": []string{"title"}}, inMessage: "clear names a field it cannot clear"},
		{name: "given and cleared", tool: "update_event", args: map[string]any{"ref": testEventRef, "etag": "e", "location": "x", "clear": []string{"location"}}, inMessage: "location is both given and cleared"},
		{name: "a bad update start", tool: "update_event", args: map[string]any{"ref": testEventRef, "etag": "e", "start": "noon"}, inMessage: "start must be an RFC 3339 time"},
		{name: "no etag to cancel", tool: "cancel_event", args: map[string]any{"ref": testEventRef, "etag": ""}, inMessage: "etag is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := calEnv(t, &fakeDaemon{})
			body := wantError(t, e.call(t, tt.tool, tt.args), api.ReasonInvalidArgument)
			if !strings.Contains(body.Message, tt.inMessage) {
				t.Errorf("message %q lacks %q", body.Message, tt.inMessage)
			}
			calNoCall(t, e.fake)
		})
	}
}

// TestCalendarToolsDoNotEchoRefusedText: a refused value is never repeated,
// since an agent can paste event text or an address where a ref, time,
// response or field name goes.
func TestCalendarToolsDoNotEchoRefusedText(t *testing.T) {
	t.Parallel()
	const secret = "SECRET-boss@example.com"
	tests := []struct {
		name, tool string
		args       map[string]any
	}{
		{name: "a ref", tool: "get_event", args: map[string]any{"ref": secret}},
		{name: "a time", tool: "agenda", args: map[string]any{"from": secret}},
		{name: "a response", tool: "rsvp", args: map[string]any{"ref": testEventRef, "response": secret}},
		{name: "a duration", tool: "create_event", args: map[string]any{"account": "work", "title": "t", "start": "2026-11-10T15:00:00Z", "duration": secret}},
		{name: "a clear entry", tool: "update_event", args: map[string]any{"ref": testEventRef, "etag": "e", "clear": []string{secret}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := calEnv(t, &fakeDaemon{})
			res := e.call(t, tt.tool, tt.args)
			wantError(t, res, api.ReasonInvalidArgument)
			raw, err := json.Marshal(res)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(raw), "SECRET") {
				t.Errorf("the refusal repeats the refused text: %s", raw)
			}
			calNoCall(t, e.fake)
		})
	}
}

// calEventAnswer is a daemon event with text in every untrusted field.
func calEventAnswer(text string) *chainmailv1.CalendarEvent {
	return &chainmailv1.CalendarEvent{
		Ref: testEventRef, Account: "work", CalendarId: "primary", Uid: text,
		Start: timestamppb.New(calT1), End: timestamppb.New(calT2), TimeZone: "Europe/London", Status: "CONFIRMED", Sequence: 2,
		Summary: readU(text), Location: readU(text), Description: readU(text),
		Organizer: &chainmailv1.Address{Name: readU(text), Email: readU("org@x.test")},
		Attendees: []*chainmailv1.Attendee{
			{Address: &chainmailv1.Address{Name: readU(text), Email: readU("ann@x.test")}, Partstat: "ACCEPTED", Role: "REQ-PARTICIPANT"},
			{Address: &chainmailv1.Address{Email: readU("me@work.test")}, Partstat: "NEEDS-ACTION", IsSelf: true},
		},
		MyPartstat: "NEEDS-ACTION", Etag: `"e1"`, RecurrenceRule: text, Recurring: true,
		RecurrenceId: timestamppb.New(calT1), MessageRef: testRef,
	}
}

// calEventWant is calEventAnswer as agents see it.
func calEventWant(text string) calendarEventOut {
	return calendarEventOut{
		Ref: testEventRef, Account: "work", CalendarID: "primary", UID: &untrusted{Untrusted: text},
		Start: "2026-11-10T15:00:00Z", End: "2026-11-10T16:00:00Z", TimeZone: "Europe/London", Status: "CONFIRMED", Sequence: 2,
		Summary: &untrusted{Untrusted: text}, Location: &untrusted{Untrusted: text}, Description: &untrusted{Untrusted: text},
		Organizer: &untrusted{Untrusted: text + " <org@x.test>"},
		Attendees: []attendeeOut{
			{Address: untrusted{Untrusted: text + " <ann@x.test>"}, PartStat: "ACCEPTED", Role: "REQ-PARTICIPANT"},
			{Address: untrusted{Untrusted: "me@work.test"}, PartStat: "NEEDS-ACTION", IsSelf: true},
		},
		MyPartStat: "NEEDS-ACTION", Etag: `"e1"`, RecurrenceRule: &untrusted{Untrusted: text}, Recurring: true,
		RecurrenceID: "2026-11-10T15:00:00Z", MessageRef: testRef,
	}
}

// calHeldAnswer is a daemon hold with two blocked attendees.
func calHeldAnswer(text string) *chainmailv1.CalendarHeld {
	return &chainmailv1.CalendarHeld{
		Hint: "send the invitations from the calendar app", Blocked: []*chainmailv1.Untrusted{readU(text), readU("bob@x.test")},
		TooMany: true, EventRef: testEventRef, Summary: "start, end; 2 attendees",
	}
}

func calHeldWant(text string) *calendarHeldOut {
	return &calendarHeldOut{
		Hint: "send the invitations from the calendar app", Blocked: []untrusted{{Untrusted: text}, {Untrusted: "bob@x.test"}},
		TooMany: true, EventRef: testEventRef, Summary: "start, end; 2 attendees",
	}
}

// calendarAnswering fakes the ten CalendarService RPCs with text in every
// untrusted field. Writes answer done; heldDaemon answers held.
func calendarAnswering(text string) *fakeDaemon {
	ev := func() *chainmailv1.CalendarEvent { return calEventAnswer(text) }
	return &fakeDaemon{
		ListCalendars: func(context.Context, *chainmailv1.ListCalendarsRequest) (*chainmailv1.ListCalendarsResponse, error) {
			return &chainmailv1.ListCalendarsResponse{Calendars: []*chainmailv1.CalendarInfo{
				{Account: "work", Id: "primary", Name: readU(text), Access: "write", IsDefault: true, Primary: true, TimeZone: "Europe/London"},
				{Account: "work", Id: "holidays", Name: readU(text), Access: "read"},
			}}, nil
		},
		Agenda: func(context.Context, *chainmailv1.AgendaRequest) (*chainmailv1.AgendaResponse, error) {
			return &chainmailv1.AgendaResponse{Events: []*chainmailv1.CalendarEvent{ev(), ev()}, Truncated: true}, nil
		},
		GetEvent: func(context.Context, *chainmailv1.GetEventRequest) (*chainmailv1.GetEventResponse, error) {
			return &chainmailv1.GetEventResponse{Event: ev()}, nil
		},
		ListInvites: func(context.Context, *chainmailv1.ListInvitesRequest) (*chainmailv1.ListInvitesResponse, error) {
			return &chainmailv1.ListInvitesResponse{Invites: []*chainmailv1.Invite{{Event: ev(), Source: "calendar", Method: "REQUEST"}}}, nil
		},
		GetInvite: func(context.Context, *chainmailv1.GetInviteRequest) (*chainmailv1.GetInviteResponse, error) {
			return &chainmailv1.GetInviteResponse{Invite: &chainmailv1.Invite{Event: ev(), Source: "mail", Method: "UPDATE"}}, nil
		},
		FreeBusy: func(context.Context, *chainmailv1.FreeBusyRequest) (*chainmailv1.FreeBusyResponse, error) {
			return &chainmailv1.FreeBusyResponse{
				SelfBusy: []*chainmailv1.BusyBlock{{Start: timestamppb.New(calT1), End: timestamppb.New(calT2)}},
				People: []*chainmailv1.PersonFreeBusy{
					{Person: "ann@x.test", Known: true, Busy: []*chainmailv1.BusyBlock{{Start: timestamppb.New(calT1), End: timestamppb.New(calT2)}}},
					{Person: "bob@x.test"},
				},
			}, nil
		},
		Respond: func(context.Context, *chainmailv1.RespondRequest) (*chainmailv1.RespondResponse, error) {
			return &chainmailv1.RespondResponse{Event: ev(), Via: "email", SentRef: testRef2}, nil
		},
		CreateEvent: func(context.Context, *chainmailv1.CreateEventRequest) (*chainmailv1.CreateEventResponse, error) {
			return &chainmailv1.CreateEventResponse{Event: ev()}, nil
		},
		UpdateEvent: func(context.Context, *chainmailv1.UpdateEventRequest) (*chainmailv1.UpdateEventResponse, error) {
			return &chainmailv1.UpdateEventResponse{Event: ev()}, nil
		},
		CancelEvent: func(context.Context, *chainmailv1.CancelEventRequest) (*chainmailv1.CancelEventResponse, error) {
			return &chainmailv1.CancelEventResponse{Cancelled: true}, nil
		},
	}
}

// calendarHolding fakes the four writes as held by policy.
func calendarHolding(text, via string) *fakeDaemon {
	f := calendarAnswering(text)
	f.Respond = func(context.Context, *chainmailv1.RespondRequest) (*chainmailv1.RespondResponse, error) {
		h := &chainmailv1.CalendarHeld{Hint: "answer in the calendar app", Blocked: []*chainmailv1.Untrusted{readU(text)}}
		if via == "email" {
			h = &chainmailv1.CalendarHeld{Hint: "the reply is in Drafts", Blocked: []*chainmailv1.Untrusted{readU(text)}, DraftRef: testRef2}
		}
		return &chainmailv1.RespondResponse{Held: h, Via: via}, nil
	}
	f.CreateEvent = func(context.Context, *chainmailv1.CreateEventRequest) (*chainmailv1.CreateEventResponse, error) {
		return &chainmailv1.CreateEventResponse{Event: calEventAnswer(text), Held: calHeldAnswer(text)}, nil
	}
	f.UpdateEvent = func(context.Context, *chainmailv1.UpdateEventRequest) (*chainmailv1.UpdateEventResponse, error) {
		h := calHeldAnswer(text)
		h.EventRef = ""
		return &chainmailv1.UpdateEventResponse{Held: h}, nil
	}
	f.CancelEvent = func(context.Context, *chainmailv1.CancelEventRequest) (*chainmailv1.CancelEventResponse, error) {
		return &chainmailv1.CancelEventResponse{Held: &chainmailv1.CalendarHeld{Hint: "tell James", Blocked: []*chainmailv1.Untrusted{readU(text)}, Summary: "cancel; 1 attendee"}}, nil
	}
	return f
}

// calendarFailing fakes the ten CalendarService RPCs as failing with err.
func calendarFailing(err error) *fakeDaemon {
	return &fakeDaemon{
		ListCalendars: func(context.Context, *chainmailv1.ListCalendarsRequest) (*chainmailv1.ListCalendarsResponse, error) {
			return nil, err
		},
		Agenda: func(context.Context, *chainmailv1.AgendaRequest) (*chainmailv1.AgendaResponse, error) {
			return nil, err
		},
		GetEvent: func(context.Context, *chainmailv1.GetEventRequest) (*chainmailv1.GetEventResponse, error) {
			return nil, err
		},
		ListInvites: func(context.Context, *chainmailv1.ListInvitesRequest) (*chainmailv1.ListInvitesResponse, error) {
			return nil, err
		},
		GetInvite: func(context.Context, *chainmailv1.GetInviteRequest) (*chainmailv1.GetInviteResponse, error) {
			return nil, err
		},
		FreeBusy: func(context.Context, *chainmailv1.FreeBusyRequest) (*chainmailv1.FreeBusyResponse, error) {
			return nil, err
		},
		Respond: func(context.Context, *chainmailv1.RespondRequest) (*chainmailv1.RespondResponse, error) {
			return nil, err
		},
		CreateEvent: func(context.Context, *chainmailv1.CreateEventRequest) (*chainmailv1.CreateEventResponse, error) {
			return nil, err
		},
		UpdateEvent: func(context.Context, *chainmailv1.UpdateEventRequest) (*chainmailv1.UpdateEventResponse, error) {
			return nil, err
		},
		CancelEvent: func(context.Context, *chainmailv1.CancelEventRequest) (*chainmailv1.CancelEventResponse, error) {
			return nil, err
		},
	}
}

// calendarCalls is a valid call of every calendar tool.
var calendarCalls = map[string]map[string]any{
	"list_calendars": {},
	"agenda":         {"from": "2026-11-10T00:00:00Z", "to": "2026-11-17T00:00:00Z"},
	"get_event":      {"ref": testEventRef},
	"list_invites":   {},
	"get_invite":     {"ref": testEventRef},
	"free_busy":      {"account": "work", "people": []string{"ann@x.test", "bob@x.test"}, "from": "2026-11-10T00:00:00Z", "to": "2026-11-11T00:00:00Z"},
	"rsvp":           {"ref": testEventRef, "response": "accept"},
	"create_event":   {"account": "work", "title": "Planning", "start": "2026-11-10T15:00:00Z", "end": "2026-11-10T16:00:00Z", "attendees": []string{"ann@x.test"}, "idempotency_key": "plan-1"},
	"update_event":   {"ref": testEventRef, "etag": `"e1"`, "location": "Room 2"},
	"cancel_event":   {"ref": testEventRef, "etag": `"e1"`},
}

func TestCalendarToolOutputs(t *testing.T) {
	t.Parallel()
	const text = "Planning"
	ev := calEventWant(text)
	busy := []busyOut{{Start: "2026-11-10T15:00:00Z", End: "2026-11-10T16:00:00Z"}}
	tests := []struct {
		tool string
		want any
	}{
		{tool: "list_calendars", want: listCalendarsOut{Calendars: []calendarInfoOut{
			{Account: "work", ID: "primary", Name: untrusted{Untrusted: text}, Access: "write", IsDefault: true, Primary: true, TimeZone: "Europe/London"},
			{Account: "work", ID: "holidays", Name: untrusted{Untrusted: text}, Access: "read"},
		}}},
		{tool: "agenda", want: agendaOut{Events: []calendarEventOut{ev, ev}, Truncated: true, From: "2026-11-10T00:00:00Z", To: "2026-11-17T00:00:00Z"}},
		{tool: "get_event", want: getEventOut{Event: ev}},
		{tool: "list_invites", want: listInvitesOut{Invites: []inviteOut{{Event: ev, Source: "calendar", Method: "REQUEST"}}}},
		{tool: "get_invite", want: getInviteOut{Invite: inviteOut{Event: ev, Source: "mail", Method: "UPDATE"}}},
		{tool: "free_busy", want: freeBusyOut{
			SelfBusy: busy, From: "2026-11-10T00:00:00Z", To: "2026-11-11T00:00:00Z",
			People: []personBusyOut{{Person: "ann@x.test", Known: true, Busy: busy}, {Person: "bob@x.test", Busy: []busyOut{}}},
		}},
		{tool: "rsvp", want: rsvpOut{Status: statusDone, Via: "email", Event: &ev, SentRef: testRef2}},
		{tool: "create_event", want: createEventOut{Status: statusDone, Event: &ev, IdempotencyKey: "plan-1"}},
		{tool: "update_event", want: updateEventOut{Status: statusDone, Event: &ev}},
		{tool: "cancel_event", want: cancelEventOut{Status: statusDone, Ref: testEventRef, Cancelled: true}},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			t.Parallel()
			e := calEnv(t, calendarAnswering(text))
			res := e.call(t, tt.tool, calendarCalls[tt.tool])
			wantOK(t, res)
			got := reflect.New(reflect.TypeOf(tt.want))
			decode(t, res, got.Interface())
			if !reflect.DeepEqual(got.Elem().Interface(), tt.want) {
				t.Errorf("%s =\n%+v\nwant\n%+v", tt.tool, got.Elem().Interface(), tt.want)
			}
		})
	}
}

// TestCalendarHeldResults checks each write's held result: status held,
// the hold's fields present, and a trusted first line that says what did
// and did not happen and never quotes a blocked address.
func TestCalendarHeldResults(t *testing.T) {
	t.Parallel()
	const text = "Planning"
	ev := calEventWant(text)
	updateHeld := calHeldWant(text)
	updateHeld.EventRef = ""
	tests := []struct {
		name, tool, via string
		want            any
		summary         []string
	}{
		{
			name: "create", tool: "create_event",
			want: createEventOut{Status: statusHeld, Event: &ev, Held: calHeldWant(text), IdempotencyKey: "plan-1"},
			summary: []string{
				"HELD BY POLICY", "draft event (event " + testEventRef + ")", "NO invitations were sent", "Do not retry",
				"2 attendees are not on the allowlist", "More attendees than policy allows",
			},
		},
		{
			name: "update", tool: "update_event",
			want:    updateEventOut{Status: statusHeld, Held: updateHeld},
			summary: []string{"HELD BY POLICY", "the change was NOT applied", "Do not retry", "Held change: start, end; 2 attendees", "2 attendees are not on the allowlist"},
		},
		{
			name: "cancel", tool: "cancel_event",
			want: cancelEventOut{Status: statusHeld, Ref: testEventRef, Held: &calendarHeldOut{
				Hint: "tell James", Blocked: []untrusted{{Untrusted: text}}, Summary: "cancel; 1 attendee",
			}},
			summary: []string{"HELD BY POLICY", "the cancel was NOT applied", "Do not retry", "1 attendee is not on the allowlist"},
		},
		{
			name: "rsvp through the store", tool: "rsvp", via: "store",
			want: rsvpOut{Status: statusHeld, Via: "store", Held: &calendarHeldOut{
				Hint: "answer in the calendar app", Blocked: []untrusted{{Untrusted: text}},
			}},
			summary: []string{"HELD BY POLICY", "the reply was NOT sent", "calendar app", "Do not retry", "organizer is not on the allowlist"},
		},
		{
			name: "rsvp by email", tool: "rsvp", via: "email",
			want: rsvpOut{Status: statusHeld, Via: "email", Held: &calendarHeldOut{
				Hint: "the reply is in Drafts", Blocked: []untrusted{{Untrusted: text}}, DraftRef: testRef2,
			}},
			summary: []string{"HELD BY POLICY", "the reply was NOT sent", "Drafts folder", "(draft " + testRef2 + ")", "Do not retry"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := calEnv(t, calendarHolding(text, tt.via))
			res := e.call(t, tt.tool, calendarCalls[tt.tool])
			wantOK(t, res)
			got := reflect.New(reflect.TypeOf(tt.want))
			decode(t, res, got.Interface())
			if !reflect.DeepEqual(got.Elem().Interface(), tt.want) {
				t.Errorf("%s =\n%+v\nwant\n%+v", tt.tool, got.Elem().Interface(), tt.want)
			}
			text := textOf(res)
			first, _, _ := strings.Cut(text, untrustedPreamble)
			for _, s := range tt.summary {
				if !strings.Contains(first, s) {
					t.Errorf("summary %q lacks %q", first, s)
				}
			}
			if strings.Contains(first, "Planning") {
				t.Errorf("summary quotes untrusted text: %q", first)
			}
			var raw map[string]any
			decode(t, res, &raw)
			held, _ := raw["held"].(map[string]any)
			for _, k := range []string{"hint", "blocked", "too_many"} {
				if _, ok := held[k]; !ok {
					t.Errorf("held result lacks %q: %v", k, raw)
				}
			}
		})
	}
}

// TestCalendarToolsHandEmptyListsAsArrays: a list with nothing in it is
// still an array, so an agent never has to tell null from empty.
func TestCalendarToolsHandEmptyListsAsArrays(t *testing.T) {
	t.Parallel()
	bare := &chainmailv1.CalendarEvent{Ref: testEventRef, Account: "work"}
	f := &fakeDaemon{
		ListCalendars: func(context.Context, *chainmailv1.ListCalendarsRequest) (*chainmailv1.ListCalendarsResponse, error) {
			return &chainmailv1.ListCalendarsResponse{}, nil
		},
		Agenda: func(context.Context, *chainmailv1.AgendaRequest) (*chainmailv1.AgendaResponse, error) {
			return &chainmailv1.AgendaResponse{}, nil
		},
		GetEvent: func(context.Context, *chainmailv1.GetEventRequest) (*chainmailv1.GetEventResponse, error) {
			return &chainmailv1.GetEventResponse{Event: bare}, nil
		},
		ListInvites: func(context.Context, *chainmailv1.ListInvitesRequest) (*chainmailv1.ListInvitesResponse, error) {
			return &chainmailv1.ListInvitesResponse{}, nil
		},
		FreeBusy: func(context.Context, *chainmailv1.FreeBusyRequest) (*chainmailv1.FreeBusyResponse, error) {
			return &chainmailv1.FreeBusyResponse{}, nil
		},
		CreateEvent: func(context.Context, *chainmailv1.CreateEventRequest) (*chainmailv1.CreateEventResponse, error) {
			return &chainmailv1.CreateEventResponse{Event: bare, Held: &chainmailv1.CalendarHeld{Hint: "h"}}, nil
		},
	}
	e := calEnv(t, f)
	empty := func(what string, v any) {
		t.Helper()
		if list, ok := v.([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %v, want an empty array", what, v)
		}
	}
	for tool, key := range map[string]string{"list_calendars": "calendars", "agenda": "events", "list_invites": "invites", "free_busy": "self_busy"} {
		var got map[string]any
		res := e.call(t, tool, calendarCalls[tool])
		wantOK(t, res)
		decode(t, res, &got)
		empty(tool+" "+key, got[key])
		if tool == "free_busy" {
			empty("free_busy people", got["people"])
		}
	}
	var got map[string]any
	res := e.call(t, "get_event", calendarCalls["get_event"])
	wantOK(t, res)
	decode(t, res, &got)
	event, _ := got["event"].(map[string]any)
	empty("get_event attendees", event["attendees"])

	got = nil
	res = e.call(t, "create_event", calendarCalls["create_event"])
	wantOK(t, res)
	decode(t, res, &got)
	held, _ := got["held"].(map[string]any)
	empty("held blocked", held["blocked"])
}

// TestCalendarToolSummaries pins each tool's first line. free_busy names
// no event, and a cancel that was done holds no event text, so both answer
// in plain text, without untrusted markers.
func TestCalendarToolSummaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		tool, summary string
		plain         bool
	}{
		{tool: "list_calendars", summary: "calendars: 2"},
		{tool: "agenda", summary: "events: 2 from 2026-11-10T00:00:00Z to 2026-11-17T00:00:00Z; truncated: some instances are left out"},
		{tool: "get_event", summary: "event " + testEventRef + ", attendees 2"},
		{tool: "list_invites", summary: "invites: 1"},
		{tool: "get_invite", summary: "invite " + testEventRef + " from mail, method UPDATE"},
		{tool: "free_busy", summary: "busy blocks for James: 1; people: 2, unknown: 1", plain: true},
		{tool: "rsvp", summary: "Answered accept to " + testEventRef + " by email."},
		{tool: "create_event", summary: "Created event " + testEventRef + " with 2 attendees."},
		{tool: "update_event", summary: "Updated event " + testEventRef + "."},
		{tool: "cancel_event", summary: "Cancelled event " + testEventRef + ".", plain: true},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			t.Parallel()
			e := calEnv(t, calendarAnswering("Planning"))
			res := e.call(t, tt.tool, calendarCalls[tt.tool])
			wantOK(t, res)
			text := textOf(res)
			if !strings.HasPrefix(text, tt.summary+"\n") {
				t.Errorf("text = %q, want the summary %q first", text, tt.summary)
			}
			if marked := strings.Contains(text, "<<<chainmail-untrusted "); marked == tt.plain {
				t.Errorf("untrusted markers present = %t, want %t: %q", marked, !tt.plain, text)
			}
			if !tt.plain && strings.Count(text, "<<<end chainmail-untrusted ") != 1 {
				t.Errorf("text is not wrapped once: %q", text)
			}
		})
	}
}

// TestCalendarToolsWrapEventText: event text and calendar names reach the
// client only as untrusted data, in the structured content and between the
// markers.
func TestCalendarToolsWrapEventText(t *testing.T) {
	t.Parallel()
	// An event carries the hostile text in its UID, summary, location,
	// description, organizer, one attendee and recurrence rule: 7.
	tests := []struct {
		tool string
		held bool
		want int
	}{
		{tool: "list_calendars", want: 2},
		{tool: "agenda", want: 14},
		{tool: "get_event", want: 7},
		{tool: "list_invites", want: 7},
		{tool: "get_invite", want: 7},
		{tool: "rsvp", want: 7},
		{tool: "create_event", want: 7},
		{tool: "update_event", want: 7},
		{tool: "rsvp", held: true, want: 1},         // the blocked organizer
		{tool: "create_event", held: true, want: 8}, // the draft event and one blocked attendee
		{tool: "update_event", held: true, want: 1}, // one blocked attendee
		{tool: "cancel_event", held: true, want: 1}, // one blocked attendee
	}
	for _, tt := range tests {
		name := tt.tool
		if tt.held {
			name += " held"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := calendarAnswering(readHostile)
			if tt.held {
				f = calendarHolding(readHostile, "store")
			}
			e := calEnv(t, f)
			res := e.call(t, tt.tool, calendarCalls[tt.tool])
			wantOK(t, res)
			if n := readWrappedOnly(t, res.StructuredContent, readHostile); n != tt.want {
				t.Errorf("%d enveloped hostile strings, want %d", n, tt.want)
			}
			readInsideMarkers(t, textOf(res), readHostile)
		})
	}
}

func TestCalendarToolsPassDaemonErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                      string
		err                       error
		reason, inMessage, inHint string
	}{
		{
			name:   "a changed event",
			err:    api.New(connect.CodeAborted, api.ReasonEventConflict, "read the event again with get_event", errors.New("the event changed since it was read")),
			reason: api.ReasonEventConflict, inMessage: "changed since it was read", inHint: "get_event",
		},
		{
			name:   "not the organizer",
			err:    api.New(connect.CodeFailedPrecondition, api.ReasonNotOrganizer, "ask the organizer", errors.New("only the organizer can change this event")),
			reason: api.ReasonNotOrganizer, inMessage: "only the organizer", inHint: "organizer",
		},
		{
			name:   "a read-only calendar",
			err:    api.New(connect.CodePermissionDenied, api.ReasonCalendarReadOnly, "pick a calendar with write access", errors.New("the calendar is read-only")),
			reason: api.ReasonCalendarReadOnly, inMessage: "read-only", inHint: "write access",
		},
		{
			name:   "no calendar store",
			err:    api.New(connect.CodeFailedPrecondition, api.ReasonNoCalendarStore, "the account has no calendar", errors.New("the account has no calendar store")),
			reason: api.ReasonNoCalendarStore, inMessage: "no calendar store", inHint: "no calendar",
		},
		{
			name:   "a cache that is not ready",
			err:    api.New(connect.CodeFailedPrecondition, api.ReasonCacheNotReady, "run: chainmail cache status", errors.New("the calendar cache has not synced yet")),
			reason: api.ReasonCacheNotReady, inMessage: "not synced yet", inHint: "chainmail cache status",
		},
		{
			name:   "a bare error gets fixed text",
			err:    errors.New("disk I/O error at /var/lib/chainmail/calendar.db"),
			reason: api.ReasonInternal, inMessage: "internal error", inHint: "tell James",
		},
	}
	for tool, args := range calendarCalls {
		for _, tt := range tests {
			t.Run(tool+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				e := calEnv(t, calendarFailing(tt.err))
				res := e.call(t, tool, args)
				body := wantError(t, res, tt.reason)
				if !strings.Contains(body.Message, tt.inMessage) || !strings.Contains(body.Hint, tt.inHint) {
					t.Errorf("error = %+v, want a message with %q and a hint with %q", body, tt.inMessage, tt.inHint)
				}
				if strings.Contains(textOf(res), "/var/lib/chainmail") {
					t.Errorf("the daemon's own error text leaked: %q", textOf(res))
				}
			})
		}
	}
}

// TestCalendarToolAnnotations pins each calendar tool's title and hints,
// that a tool whose output holds event text says it is untrusted, and that
// the descriptions teach holds and conflicts.
func TestCalendarToolAnnotations(t *testing.T) {
	t.Parallel()
	type want struct {
		title     string
		h         hints
		untrusted bool
		mentions  []string
	}
	read := hints{readOnly: true, idempotent: true, openWorld: true}
	held := []string{"held", "draft event", "NOT applied", "Drafts", "Do not retry", "tell James"}
	table := map[string]want{
		"list_calendars": {"List calendars", read, true, []string{"access", "calendar_id"}},
		"agenda":         {"Show the agenda", read, true, []string{"RFC 3339", "400 days", "include_declined", "truncated", "CACHE_NOT_READY", "@<recurrence ID>"}},
		"get_event":      {"Show an event", read, true, []string{"etag", "@<recurrence ID>"}},
		"list_invites":   {"List invites", read, true, []string{"NEEDS-ACTION", "rsvp", "message_ref"}},
		"get_invite":     {"Show an invite", read, true, []string{"calendar or mail", "REQUEST"}},
		"free_busy":      {"Show free/busy", read, false, []string{"never free", "400 days"}},
		"rsvp":           {"Answer an invite", hints{idempotent: true, openWorld: true}, true, append([]string{"accept, decline or tentative", "organizer", "send policy"}, held...)},
		"create_event":   {"Create an event", hints{openWorld: true}, true, append([]string{"send policy", "check each attendee", "idempotency_key", "duration"}, held...)},
		"update_event":   {"Update an event", hints{openWorld: true}, true, append([]string{"etag", "EVENT_CONFLICT", "NOT_ORGANIZER", "clear"}, held...)},
		"cancel_event":   {"Cancel an event", hints{destructive: true, openWorld: true}, true, append([]string{"etag", "EVENT_CONFLICT"}, held...)},
	}
	e := calEnv(t, &fakeDaemon{})
	res, err := e.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tl := range res.Tools {
		w, ok := table[tl.Name]
		if !ok {
			continue
		}
		seen[tl.Name] = true
		a := tl.Annotations
		if a == nil || a.DestructiveHint == nil || a.OpenWorldHint == nil {
			t.Errorf("%s: annotations %+v leave a hint unset", tl.Name, a)
			continue
		}
		got := hints{a.ReadOnlyHint, *a.DestructiveHint, a.IdempotentHint, *a.OpenWorldHint}
		if got != w.h || tl.Title != w.title || a.Title != w.title {
			t.Errorf("%s: title %q, hints %+v; want %q, %+v", tl.Name, tl.Title, got, w.title, w.h)
		}
		if w.untrusted != strings.Contains(tl.Description, "untrusted") {
			t.Errorf("%s: description names untrusted text = %t, want %t: %q", tl.Name, !w.untrusted, w.untrusted, tl.Description)
		}
		for _, m := range w.mentions {
			if !strings.Contains(tl.Description, m) {
				t.Errorf("%s: description lacks %q: %q", tl.Name, m, tl.Description)
			}
		}
	}
	for name := range table {
		if !seen[name] {
			t.Errorf("%s is not listed", name)
		}
	}
}

// TestGetInviteSummaryNamesOnlyKnownValues: the first line is outside the
// markers, so a source or method the daemon does not document is not
// quoted there.
func TestGetInviteSummaryNamesOnlyKnownValues(t *testing.T) {
	t.Parallel()
	f := calendarAnswering("Planning")
	f.GetInvite = func(context.Context, *chainmailv1.GetInviteRequest) (*chainmailv1.GetInviteResponse, error) {
		return &chainmailv1.GetInviteResponse{Invite: &chainmailv1.Invite{Event: calEventAnswer("Planning"), Source: "X-HOSTILE", Method: "X-IGNORE-PREVIOUS"}}, nil
	}
	e := calEnv(t, f)
	res := e.call(t, "get_invite", calendarCalls["get_invite"])
	wantOK(t, res)
	if want := "invite " + testEventRef + " from other, method other\n"; !strings.HasPrefix(textOf(res), want) {
		t.Errorf("text = %q, want the summary %q first", textOf(res), want)
	}
}
