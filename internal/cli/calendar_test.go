package cli

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
)

// calNow is the clock the calendar tests run at: Monday 9 November 2026,
// 08:00 in New York.
var calNow = time.Date(2026, 11, 9, 13, 0, 0, 0, time.UTC)

// The refs the calendar fixtures use.
const (
	calRef     = "ce_01jcb7m2k9r8q3v4w5x6y7z8a9"
	calRef2    = "ce_01jcb7m2k9r8q3v4w5x6y7z8b0"
	calRef3    = "ce_01jcb7m2k9r8q3v4w5x6y7z8c1"
	calInstRef = calRef + "@20261110T140000Z"
)

// mustZone loads an IANA zone or fails the test.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// fakeCalendar is a CalendarService for CLI tests. Each func answers its
// RPC; a nil func answers with an empty response. It records every request.
type fakeCalendar struct {
	chainmailv1connect.UnimplementedCalendarServiceHandler

	calendars func(*chainmailv1.ListCalendarsRequest) (*chainmailv1.ListCalendarsResponse, error)
	agenda    func(*chainmailv1.AgendaRequest) (*chainmailv1.AgendaResponse, error)
	event     func(*chainmailv1.GetEventRequest) (*chainmailv1.GetEventResponse, error)
	invites   func(*chainmailv1.ListInvitesRequest) (*chainmailv1.ListInvitesResponse, error)
	invite    func(*chainmailv1.GetInviteRequest) (*chainmailv1.GetInviteResponse, error)
	freeBusy  func(*chainmailv1.FreeBusyRequest) (*chainmailv1.FreeBusyResponse, error)
	respond   func(*chainmailv1.RespondRequest) (*chainmailv1.RespondResponse, error)
	create    func(*chainmailv1.CreateEventRequest) (*chainmailv1.CreateEventResponse, error)
	update    func(*chainmailv1.UpdateEventRequest) (*chainmailv1.UpdateEventResponse, error)
	cancel    func(*chainmailv1.CancelEventRequest) (*chainmailv1.CancelEventResponse, error)

	mu   sync.Mutex
	reqs []proto.Message
}

func (f *fakeCalendar) record(m proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, m)
}

// requests returns every request so far, in order.
func (f *fakeCalendar) requests() []proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// answerCalendar records req and answers it with fn, or with an empty
// response when fn is nil.
func answerCalendar[Req proto.Message, Resp any](f *fakeCalendar, req Req, fn func(Req) (*Resp, error)) (*connect.Response[Resp], error) {
	f.record(req)
	if fn == nil {
		return connect.NewResponse(new(Resp)), nil
	}
	resp, err := fn(req)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeCalendar) ListCalendars(_ context.Context, req *connect.Request[chainmailv1.ListCalendarsRequest]) (*connect.Response[chainmailv1.ListCalendarsResponse], error) {
	return answerCalendar(f, req.Msg, f.calendars)
}

func (f *fakeCalendar) Agenda(_ context.Context, req *connect.Request[chainmailv1.AgendaRequest]) (*connect.Response[chainmailv1.AgendaResponse], error) {
	return answerCalendar(f, req.Msg, f.agenda)
}

func (f *fakeCalendar) GetEvent(_ context.Context, req *connect.Request[chainmailv1.GetEventRequest]) (*connect.Response[chainmailv1.GetEventResponse], error) {
	return answerCalendar(f, req.Msg, f.event)
}

func (f *fakeCalendar) ListInvites(_ context.Context, req *connect.Request[chainmailv1.ListInvitesRequest]) (*connect.Response[chainmailv1.ListInvitesResponse], error) {
	return answerCalendar(f, req.Msg, f.invites)
}

func (f *fakeCalendar) GetInvite(_ context.Context, req *connect.Request[chainmailv1.GetInviteRequest]) (*connect.Response[chainmailv1.GetInviteResponse], error) {
	return answerCalendar(f, req.Msg, f.invite)
}

func (f *fakeCalendar) FreeBusy(_ context.Context, req *connect.Request[chainmailv1.FreeBusyRequest]) (*connect.Response[chainmailv1.FreeBusyResponse], error) {
	return answerCalendar(f, req.Msg, f.freeBusy)
}

func (f *fakeCalendar) Respond(_ context.Context, req *connect.Request[chainmailv1.RespondRequest]) (*connect.Response[chainmailv1.RespondResponse], error) {
	return answerCalendar(f, req.Msg, f.respond)
}

func (f *fakeCalendar) CreateEvent(_ context.Context, req *connect.Request[chainmailv1.CreateEventRequest]) (*connect.Response[chainmailv1.CreateEventResponse], error) {
	return answerCalendar(f, req.Msg, f.create)
}

func (f *fakeCalendar) UpdateEvent(_ context.Context, req *connect.Request[chainmailv1.UpdateEventRequest]) (*connect.Response[chainmailv1.UpdateEventResponse], error) {
	return answerCalendar(f, req.Msg, f.update)
}

func (f *fakeCalendar) CancelEvent(_ context.Context, req *connect.Request[chainmailv1.CancelEventRequest]) (*connect.Response[chainmailv1.CancelEventResponse], error) {
	return answerCalendar(f, req.Msg, f.cancel)
}

// startFakeCalendar serves f on a Unix socket until the test ends and returns the socket.
func startFakeCalendar(t *testing.T, f *fakeCalendar) string {
	t.Helper()
	path, handler := chainmailv1connect.NewCalendarServiceHandler(f)
	return startFakeServer(t, path, handler)
}

// calEnv is an Env at calNow in New York.
func calEnv(t *testing.T) Env {
	t.Helper()
	return Env{TZ: mustZone(t, "America/New_York"), Now: func() time.Time { return calNow }}
}

// runCal runs chainmail against f at calNow in New York.
func runCal(t *testing.T, f *fakeCalendar, args ...string) cliResult {
	t.Helper()
	return runCLI(t, calEnv(t), append([]string{"--socket", startFakeCalendar(t, f)}, args...)...)
}

func untrusted(s string) *chainmailv1.Untrusted { return &chainmailv1.Untrusted{Untrusted: s} }

func addr(name, email string) *chainmailv1.Address {
	a := &chainmailv1.Address{Email: untrusted(email)}
	if name != "" {
		a.Name = untrusted(name)
	}
	return a
}

func ts(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

// standupFixture is a weekly instance James accepted, organized in Paris,
// with a summary that tries to clear the terminal.
func standupFixture() *chainmailv1.CalendarEvent {
	return &chainmailv1.CalendarEvent{
		Ref: calInstRef, Account: "work", CalendarId: "primary", Uid: "standup@example.com",
		Start: ts(time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 10, 14, 30, 0, 0, time.UTC)),
		TimeZone: "Europe/Paris", Status: "CONFIRMED", Sequence: 2,
		Summary: untrusted("Stand\x1b[2Jup"), Location: untrusted("Room 4"), Description: untrusted("Daily sync.\n\nBring notes."),
		Organizer: addr("Alice", "alice@example.com"),
		Attendees: []*chainmailv1.Attendee{
			{Address: addr("Alice", "alice@example.com"), Partstat: "ACCEPTED", Role: "CHAIR"},
			{Address: addr("", "me@example.com"), Partstat: "ACCEPTED", Role: "REQ-PARTICIPANT", IsSelf: true},
			{Address: addr("Bob", "bob@example.com"), Partstat: "NEEDS-ACTION"},
		},
		MyPartstat: "ACCEPTED", Etag: `"e1"`, RecurrenceRule: "FREQ=WEEKLY;BYDAY=TU", Recurring: true,
		RecurrenceId: ts(time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)),
	}
}

// offsiteFixture is an all-day event over three days that James organizes.
func offsiteFixture() *chainmailv1.CalendarEvent {
	return &chainmailv1.CalendarEvent{
		Ref: calRef2, Account: "home", CalendarId: "/dav/calendars/me/personal/", Uid: "offsite@chainmail",
		Start: ts(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)),
		AllDay: true, Status: "CONFIRMED", Summary: untrusted("Offsite"), IsOrganizer: true, Etag: `"e2"`,
		Organizer: addr("", "me@icloud.com"), Transparent: true,
	}
}

// lateFixture is a cancelled evening event that runs past midnight in New York.
func lateFixture() *chainmailv1.CalendarEvent {
	return &chainmailv1.CalendarEvent{
		Ref: calRef3, Account: "work", CalendarId: "primary",
		Start: ts(time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 13, 6, 0, 0, 0, time.UTC)),
		TimeZone: "America/New_York", Status: "CANCELLED", Summary: untrusted("Late call"), MyPartstat: "NEEDS-ACTION",
	}
}

func agendaFixture() *chainmailv1.AgendaResponse {
	return &chainmailv1.AgendaResponse{
		Events:    []*chainmailv1.CalendarEvent{standupFixture(), offsiteFixture(), lateFixture()},
		Truncated: true,
	}
}

func calendarsFixture() *chainmailv1.ListCalendarsResponse {
	return &chainmailv1.ListCalendarsResponse{Calendars: []*chainmailv1.CalendarInfo{
		{Account: "work", Id: "primary", Name: untrusted("me@example.com"), Access: "write", IsDefault: true, Primary: true, TimeZone: "America/New_York"},
		{Account: "work", Id: "team@group.calendar.google.com", Name: untrusted("Team\x1b[31m"), Access: "read"},
		{Account: "home", Id: "/dav/calendars/me/personal/", Name: untrusted("Personal"), Access: "write", IsDefault: true},
	}}
}

func invitesFixture() *chainmailv1.ListInvitesResponse {
	mailOnly := &chainmailv1.CalendarEvent{
		Ref: calRef3, Account: "host", Uid: "dinner@example.org",
		Start: ts(time.Date(2026, 11, 20, 23, 30, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 21, 1, 30, 0, 0, time.UTC)),
		Summary: untrusted("Dinner"), Organizer: addr("Carol", "carol@example.org"), MyPartstat: "NEEDS-ACTION",
		MessageRef: "cm1:host:INBOX:7:42",
	}
	ev := standupFixture()
	ev.MyPartstat = "NEEDS-ACTION"
	return &chainmailv1.ListInvitesResponse{Invites: []*chainmailv1.Invite{
		{Event: ev, Source: "calendar", Method: "REQUEST"},
		{Event: mailOnly, Source: "mail", Method: "REQUEST"},
	}}
}

// TestCalendarCommandsGolden pins the table and json output of the read commands.
func TestCalendarCommandsGolden(t *testing.T) {
	t.Parallel()
	f := &fakeCalendar{
		calendars: func(*chainmailv1.ListCalendarsRequest) (*chainmailv1.ListCalendarsResponse, error) {
			return calendarsFixture(), nil
		},
		agenda: func(*chainmailv1.AgendaRequest) (*chainmailv1.AgendaResponse, error) { return agendaFixture(), nil },
		event: func(*chainmailv1.GetEventRequest) (*chainmailv1.GetEventResponse, error) {
			return &chainmailv1.GetEventResponse{Event: standupFixture()}, nil
		},
		invites: func(*chainmailv1.ListInvitesRequest) (*chainmailv1.ListInvitesResponse, error) {
			return invitesFixture(), nil
		},
		invite: func(*chainmailv1.GetInviteRequest) (*chainmailv1.GetInviteResponse, error) {
			return &chainmailv1.GetInviteResponse{Invite: invitesFixture().GetInvites()[1]}, nil
		},
		freeBusy: func(*chainmailv1.FreeBusyRequest) (*chainmailv1.FreeBusyResponse, error) {
			return &chainmailv1.FreeBusyResponse{
				SelfBusy: []*chainmailv1.BusyBlock{
					{Start: ts(time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC))},
					{Start: ts(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC))},
				},
				People: []*chainmailv1.PersonFreeBusy{
					{Person: "alice@example.com", Known: true, Busy: []*chainmailv1.BusyBlock{
						{Start: ts(time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)), End: ts(time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC))},
					}},
					{Person: "bob@example.com", Known: true},
					{Person: "stranger@example.net"},
				},
			}, nil
		},
	}
	socket := startFakeCalendar(t, f)
	tests := []struct {
		name string
		args []string
	}{
		{name: "calendars", args: []string{"calendars"}},
		{name: "agenda", args: []string{"agenda"}},
		{name: "event", args: []string{"event", calInstRef}},
		{name: "invites", args: []string{"invites"}},
		{name: "invite", args: []string{"invite", calRef3}},
		{name: "freebusy", args: []string{"freebusy", "--account", "work", "alice@example.com", "bob@example.com", "stranger@example.net"}},
	}
	for _, tt := range tests {
		for _, output := range []string{"table", "json"} {
			t.Run(tt.name+"_"+output, func(t *testing.T) {
				t.Parallel()
				got := runCLI(t, calEnv(t), append([]string{"--socket", socket, "--output", output}, tt.args...)...)
				if got.code != 0 {
					t.Fatalf("%v: %+v", tt.args, got)
				}
				golden(t, tt.name+"_"+output, got.stdout)
			})
		}
	}
}

// Event text never reaches the terminal raw: every summary, location,
// description and name is cleaned.
func TestCalendarTablesCleanEventText(t *testing.T) {
	t.Parallel()
	f := &fakeCalendar{
		agenda: func(*chainmailv1.AgendaRequest) (*chainmailv1.AgendaResponse, error) { return agendaFixture(), nil },
		event: func(*chainmailv1.GetEventRequest) (*chainmailv1.GetEventResponse, error) {
			ev := standupFixture()
			ev.Location = untrusted("Room\x1b[31m 4")
			ev.Description = untrusted("line\x1b]0;title\x07 one")
			ev.Organizer = addr("Al\x1b[2Jice", "alice@example.com")
			return &chainmailv1.GetEventResponse{Event: ev}, nil
		},
		calendars: func(*chainmailv1.ListCalendarsRequest) (*chainmailv1.ListCalendarsResponse, error) {
			return calendarsFixture(), nil
		},
	}
	for _, args := range [][]string{{"agenda"}, {"event", calInstRef}, {"calendars"}} {
		got := runCal(t, f, append([]string{"--output", "table"}, args...)...)
		if got.code != 0 {
			t.Fatalf("%v: %+v", args, got)
		}
		if strings.ContainsAny(got.stdout, "\x1b\x07") {
			t.Errorf("%v printed a control character: %q", args, got.stdout)
		}
	}
}

func TestCalendarsRequest(t *testing.T) {
	t.Parallel()
	f := &fakeCalendar{}
	if got := runCal(t, f, "calendars", "--account", "work", "--account", "home"); got.code != 0 {
		t.Fatalf("calendars: %+v", got)
	}
	want := []proto.Message{&chainmailv1.ListCalendarsRequest{Accounts: []string{"work", "home"}}}
	if reqs := f.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
		t.Errorf("requests = %v, want %v", reqs, want)
	}
}

// Times without a zone read in the CLI's zone, or --tz; a date alone in
// --to takes in that whole day; --days counts from --from, or from now.
func TestAgendaRequests(t *testing.T) {
	t.Parallel()
	at := func(s string) *timestamppb.Timestamp {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return timestamppb.New(v)
	}
	tests := []struct {
		name string
		args []string
		want *chainmailv1.AgendaRequest
	}{
		{name: "the next seven days", args: nil, want: &chainmailv1.AgendaRequest{From: at("2026-11-09T13:00:00Z"), To: at("2026-11-16T13:00:00Z")}},
		{name: "days", args: []string{"--days", "3"}, want: &chainmailv1.AgendaRequest{From: at("2026-11-09T13:00:00Z"), To: at("2026-11-12T13:00:00Z")}},
		{
			name: "dates in the local zone, the last day included",
			args: []string{"--from", "2026-11-10", "--to", "2026-11-12"},
			want: &chainmailv1.AgendaRequest{From: at("2026-11-10T05:00:00Z"), To: at("2026-11-13T05:00:00Z")},
		},
		{
			name: "a time in another zone",
			args: []string{"--from", "2026-11-10 09:00", "--tz", "Europe/London", "--days", "1"},
			want: &chainmailv1.AgendaRequest{From: at("2026-11-10T09:00:00Z"), To: at("2026-11-11T09:00:00Z")},
		},
		{
			name: "rfc 3339 keeps its own offset",
			args: []string{"--from", "2026-11-10T09:00:00+01:00", "--to", "2026-11-10T18:00:00+01:00"},
			want: &chainmailv1.AgendaRequest{From: at("2026-11-10T08:00:00Z"), To: at("2026-11-10T17:00:00Z")},
		},
		{name: "to alone starts now", args: []string{"--to", "2026-11-10"}, want: &chainmailv1.AgendaRequest{From: at("2026-11-09T13:00:00Z"), To: at("2026-11-11T05:00:00Z")}},
		{
			name: "accounts, calendars and declined events",
			args: []string{"--account", "work", "--account", "home", "--calendar", "primary", "--include-declined"},
			want: &chainmailv1.AgendaRequest{
				Accounts: []string{"work", "home"}, Calendars: []string{"primary"}, IncludeDeclined: true,
				From: at("2026-11-09T13:00:00Z"), To: at("2026-11-16T13:00:00Z"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCalendar{}
			if got := runCal(t, f, append([]string{"agenda"}, tt.args...)...); got.code != 0 {
				t.Fatalf("agenda: %+v", got)
			}
			if reqs := f.requests(); !slices.EqualFunc(reqs, []proto.Message{tt.want}, proto.Equal) {
				t.Errorf("requests = %v, want %v", reqs, tt.want)
			}
		})
	}
}

// Bad times and ranges are usage errors and reach no daemon.
func TestCalendarRangeUsageErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		msg  string
	}{
		{name: "days and to", args: []string{"agenda", "--days", "2", "--to", "2026-11-12"}, msg: "--days and --to cannot be used together"},
		{name: "bad time", args: []string{"agenda", "--from", "next tuesday"}, msg: `--from "next tuesday": want RFC 3339`},
		{name: "to before from", args: []string{"agenda", "--from", "2026-11-12", "--to", "2026-11-10"}, msg: "--to must be after --from"},
		{name: "no days", args: []string{"agenda", "--days", "0"}, msg: "--days must be 1 to 400"},
		{name: "too many days", args: []string{"agenda", "--days", "401"}, msg: "--days must be 1 to 400"},
		{name: "range too long", args: []string{"agenda", "--from", "2026-01-01", "--to", "2027-03-01"}, msg: "at most 400 days apart"},
		{name: "unknown zone", args: []string{"agenda", "--tz", "Mars/Base"}, msg: `unknown --tz "Mars/Base"`},
		{name: "local is no zone name", args: []string{"agenda", "--tz", "Local"}, msg: `unknown --tz "Local"`},
		{name: "freebusy people need an account", args: []string{"freebusy", "alice@example.com"}, msg: "--account is required"},
		{name: "freebusy bad time", args: []string{"freebusy", "--from", "soon"}, msg: `--from "soon"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCalendar{}
			got := runCal(t, f, tt.args...)
			if got.code != api.ExitUsage || !strings.Contains(errorMessage(t, got.stderr), tt.msg) {
				t.Errorf("%v: %+v, want exit 2 with %q", tt.args, got, tt.msg)
			}
			if reqs := f.requests(); len(reqs) != 0 {
				t.Errorf("requests = %v, want none", reqs)
			}
		})
	}
}

func TestFreeBusyRequest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want *chainmailv1.FreeBusyRequest
	}{
		{
			name: "only me, the next seven days",
			want: &chainmailv1.FreeBusyRequest{From: ts(calNow), To: ts(calNow.AddDate(0, 0, 7))},
		},
		{
			name: "people on one day",
			args: []string{"--account", "work", "--from", "2026-11-10", "--to", "2026-11-10", "alice@example.com", "bob@example.com"},
			want: &chainmailv1.FreeBusyRequest{
				Account: "work", People: []string{"alice@example.com", "bob@example.com"},
				From: ts(time.Date(2026, 11, 10, 5, 0, 0, 0, time.UTC)), To: ts(time.Date(2026, 11, 11, 5, 0, 0, 0, time.UTC)),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCalendar{}
			if got := runCal(t, f, append([]string{"freebusy"}, tt.args...)...); got.code != 0 {
				t.Fatalf("freebusy: %+v", got)
			}
			if reqs := f.requests(); !slices.EqualFunc(reqs, []proto.Message{tt.want}, proto.Equal) {
				t.Errorf("requests = %v, want %v", reqs, tt.want)
			}
		})
	}
}

// event, invite and invites pass the ref and accounts on unchanged.
func TestCalendarRefRequests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want proto.Message
	}{
		{args: []string{"event", calInstRef}, want: &chainmailv1.GetEventRequest{Ref: calInstRef}},
		{args: []string{"invite", calRef}, want: &chainmailv1.GetInviteRequest{Ref: calRef}},
		{args: []string{"invites", "--account", "host"}, want: &chainmailv1.ListInvitesRequest{Accounts: []string{"host"}}},
	}
	for _, tt := range tests {
		f := &fakeCalendar{}
		if got := runCal(t, f, tt.args...); got.code != 0 {
			t.Fatalf("%v: %+v", tt.args, got)
		}
		if reqs := f.requests(); !slices.EqualFunc(reqs, []proto.Message{tt.want}, proto.Equal) {
			t.Errorf("%v: requests = %v, want %v", tt.args, reqs, tt.want)
		}
	}
}

// A daemon error keeps its reason and exit code: NO_CALENDAR_STORE exits 2,
// CACHE_NOT_READY its own code, with the daemon's hint.
func TestCalendarErrorsKeepTheirReason(t *testing.T) {
	t.Parallel()
	f := &fakeCalendar{agenda: func(*chainmailv1.AgendaRequest) (*chainmailv1.AgendaResponse, error) {
		return nil, api.New(connect.CodeFailedPrecondition, api.ReasonNoCalendarStore, "set [accounts.host.calendar] store", errors.New("account host has no calendar store"))
	}}
	got := runCal(t, f, "agenda", "--account", "host")
	if got.code != api.ExitCode(api.ReasonNoCalendarStore) || errorReason(t, got.stderr) != api.ReasonNoCalendarStore {
		t.Errorf("agenda: %+v, want NO_CALENDAR_STORE", got)
	}
}

// errorMessage is the message of the error JSON line on stderr.
func errorMessage(t *testing.T, stderr string) string {
	t.Helper()
	var out errorOutput
	if err := json.Unmarshal([]byte(stderr), &out); err != nil {
		t.Fatalf("stderr is not an error JSON line: %q", stderr)
	}
	return out.Error.Message
}

// A span that crosses a change of daylight saving time names the zone of
// each end, so neither end is labelled with the other's offset.
func TestTimedRangeAcrossDaylightSaving(t *testing.T) {
	t.Parallel()
	ny := mustZone(t, "America/New_York")
	tests := []struct {
		start, end time.Time
		want       string
	}{
		{
			start: time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC), end: time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC),
			want: "Sun 2026-03-08 01:00 EST-04:00 EDT",
		},
		{
			start: time.Date(2026, 3, 8, 3, 0, 0, 0, time.UTC), end: time.Date(2026, 3, 8, 8, 0, 0, 0, time.UTC),
			want: "Sat 2026-03-07 22:00 EST to Sun 2026-03-08 04:00 EDT",
		},
		{
			start: time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC), end: time.Date(2026, 11, 10, 14, 30, 0, 0, time.UTC),
			want: "Tue 2026-11-10 09:00-09:30 EST",
		},
	}
	for _, tt := range tests {
		if got := timedRange(tt.start, tt.end, ny); got != tt.want {
			t.Errorf("timedRange(%v, %v) = %q, want %q", tt.start, tt.end, got, tt.want)
		}
	}
}

// An agenda or invites row for an event whose own zone shows another time
// than the display zone adds the time in the event's zone, on the same
// line, as event does.
func TestCalendarRowsShowTheEventZone(t *testing.T) {
	t.Parallel()
	f := &fakeCalendar{
		agenda: func(*chainmailv1.AgendaRequest) (*chainmailv1.AgendaResponse, error) { return agendaFixture(), nil },
		invites: func(*chainmailv1.ListInvitesRequest) (*chainmailv1.ListInvitesResponse, error) {
			return invitesFixture(), nil
		},
	}
	for _, cmd := range []string{"agenda", "invites"} {
		got := runCal(t, f, "--output", "table", cmd)
		if got.code != 0 {
			t.Fatalf("%s: %+v", cmd, got)
		}
		var row string
		for line := range strings.Lines(got.stdout) {
			if strings.Contains(line, calInstRef) {
				row = line
			}
		}
		if want := "Tue 2026-11-10 09:00-09:30 EST (15:00-15:30 CET Europe/Paris)"; !strings.HasPrefix(row, want) {
			t.Errorf("%s row = %q, want it to start %q", cmd, row, want)
		}
		// The late call is in New York, the display zone, so its row has no second time.
		if strings.Contains(got.stdout, "(America/New_York)") || strings.Contains(got.stdout, "New_York)") {
			t.Errorf("%s shows the display zone again:\n%s", cmd, got.stdout)
		}
	}
	// The event zone's dates are shown when they are not the display dates.
	late := lateFixture()
	late.TimeZone = "Asia/Tokyo"
	if got, want := ownZoneRow(late, mustZone(t, "America/New_York")), " (Fri 2026-11-13 12:00-15:00 JST Asia/Tokyo)"; got != want {
		t.Errorf("ownZoneRow = %q, want %q", got, want)
	}
	// A London event across the start of BST, shown in UTC: the offsets match
	// at the start but not at the end, so the event zone is shown.
	spring := lateFixture()
	spring.TimeZone = "Europe/London"
	spring.Start = timestamppb.New(time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC))
	spring.End = timestamppb.New(time.Date(2026, 3, 29, 2, 30, 0, 0, time.UTC))
	if got := ownZoneRow(spring, time.UTC); !strings.Contains(got, "Europe/London") {
		t.Errorf("ownZoneRow across BST = %q, want the event zone shown", got)
	}
}
