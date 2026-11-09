package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
)

// stubEventStart is the start stubCalendar stamps on what it returns.
var stubEventStart = time.Date(2026, 11, 9, 17, 0, 0, 0, time.UTC)

// stubCalendar answers the CalendarService calls the client tests make
// from fixed values, and sends each call's client kind to headers.
type stubCalendar struct {
	chainmailv1connect.UnimplementedCalendarServiceHandler
	headers chan<- string
	// nameBytes pads the calendar name ListCalendars returns, to test the
	// client's read limit.
	nameBytes int
}

func (s stubCalendar) ListCalendars(_ context.Context, req *connect.Request[chainmailv1.ListCalendarsRequest]) (*connect.Response[chainmailv1.ListCalendarsResponse], error) {
	s.headers <- req.Header().Get(api.HeaderClient)
	name := "Work"
	if s.nameBytes > 0 {
		name = strings.Repeat("x", s.nameBytes)
	}
	cals := make([]*chainmailv1.CalendarInfo, 0, len(req.Msg.GetAccounts()))
	for _, a := range req.Msg.GetAccounts() {
		cals = append(cals, &chainmailv1.CalendarInfo{Account: a, Id: "primary", Name: &chainmailv1.Untrusted{Untrusted: name}, Access: "write", IsDefault: true, Primary: true})
	}
	return connect.NewResponse(&chainmailv1.ListCalendarsResponse{Calendars: cals}), nil
}

// UpdateEvent echoes the ref, the etag and the mask, and reports a hold
// whose summary names the mask paths.
func (s stubCalendar) UpdateEvent(_ context.Context, req *connect.Request[chainmailv1.UpdateEventRequest]) (*connect.Response[chainmailv1.UpdateEventResponse], error) {
	s.headers <- req.Header().Get(api.HeaderClient)
	return connect.NewResponse(&chainmailv1.UpdateEventResponse{
		Event: &chainmailv1.CalendarEvent{Ref: req.Msg.GetRef(), Etag: req.Msg.GetEtag(), Start: req.Msg.GetStart()},
		Held: &chainmailv1.CalendarHeld{
			Hint: "held by policy", Summary: strings.Join(req.Msg.GetUpdateMask().GetPaths(), ","), TooMany: true,
			Blocked: []*chainmailv1.Untrusted{{Untrusted: "stranger@example.com"}},
		},
	}), nil
}

// serveCalendar serves h as the CalendarService on a new Unix socket and
// returns the socket path.
func serveCalendar(t *testing.T, h chainmailv1connect.CalendarServiceHandler) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewCalendarServiceHandler(h))
	return serveUnix(t, &http.Server{Handler: mux})
}

// TestCalendarReachesTheDaemon checks that the Calendar client calls
// CalendarService methods over the socket, names the client kind, and
// carries untrusted text, the field mask, timestamps and a hold both ways.
func TestCalendarReachesTheDaemon(t *testing.T) {
	t.Parallel()
	headers := make(chan string, 2)
	c := New(serveCalendar(t, stubCalendar{headers: headers}), "test")
	defer c.Close()

	listed, err := c.Calendar.ListCalendars(t.Context(), connect.NewRequest(&chainmailv1.ListCalendarsRequest{Accounts: []string{"work"}}))
	if err != nil {
		t.Fatalf("ListCalendars: %v", err)
	}
	cals := listed.Msg.GetCalendars()
	if len(cals) != 1 || cals[0].GetAccount() != "work" || cals[0].GetId() != "primary" || cals[0].GetName().GetUntrusted() != "Work" ||
		cals[0].GetAccess() != "write" || !cals[0].GetIsDefault() || !cals[0].GetPrimary() {
		t.Errorf("calendars = %v, want work's primary calendar Work, writable and default", cals)
	}

	const ref = "ce_0123456789abcdefghjkmnpqrs@20261109T170000Z"
	updated, err := c.Calendar.UpdateEvent(t.Context(), connect.NewRequest(&chainmailv1.UpdateEventRequest{
		Ref: ref, Etag: `"e1"`, Start: timestamppb.New(stubEventStart),
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"start", "end"}},
	}))
	if err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	ev, held := updated.Msg.GetEvent(), updated.Msg.GetHeld()
	if ev.GetRef() != ref || ev.GetEtag() != `"e1"` || !ev.GetStart().AsTime().Equal(stubEventStart) {
		t.Errorf("event = %v, want ref %s, etag \"e1\" and start %v", ev, ref, stubEventStart)
	}
	if held.GetSummary() != "start,end" || !held.GetTooMany() || len(held.GetBlocked()) != 1 ||
		held.GetBlocked()[0].GetUntrusted() != "stranger@example.com" {
		t.Errorf("held = %v, want summary start,end, too many and one blocked address", held)
	}

	for range 2 {
		if got := <-headers; got != "test" {
			t.Errorf("%s = %q, want test", api.HeaderClient, got)
		}
	}

	// The stub leaves CancelEvent to the generated handler, which proves
	// the call reached CalendarService and not some other route.
	if _, err := c.Calendar.CancelEvent(t.Context(), connect.NewRequest(&chainmailv1.CancelEventRequest{Ref: ref})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("CancelEvent: code %v (%v), want %v", connect.CodeOf(err), err, connect.CodeUnimplemented)
	}
}

// TestCalendarReadLimit checks that the Calendar client reads a response as
// large as the daemon's 16 MiB cap, as a long agenda can be, and refuses
// one past maxMessageBytes.
func TestCalendarReadLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		bytes   int
		refused bool
	}{
		{name: "16 MiB is read", bytes: 16 << 20},
		{name: "18 MiB is refused", bytes: 18 << 20, refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := make(chan string, 1)
			c := New(serveCalendar(t, stubCalendar{headers: headers, nameBytes: tt.bytes}), "test")
			defer c.Close()
			_, err := c.Calendar.ListCalendars(t.Context(), connect.NewRequest(&chainmailv1.ListCalendarsRequest{Accounts: []string{"work"}}))
			switch {
			case !tt.refused && err != nil:
				t.Errorf("ListCalendars: %v, want success", err)
			case tt.refused && connect.CodeOf(err) != connect.CodeResourceExhausted:
				t.Errorf("ListCalendars: code %v (%v), want %v", connect.CodeOf(err), err, connect.CodeResourceExhausted)
			}
		})
	}
}
