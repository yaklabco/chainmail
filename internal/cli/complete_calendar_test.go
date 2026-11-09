package cli

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

// completionCalendar answers the lookups calendar completion makes.
func completionCalendar() *fakeCalendar {
	return &fakeCalendar{
		calendars: func(*chainmailv1.ListCalendarsRequest) (*chainmailv1.ListCalendarsResponse, error) {
			r := calendarsFixture()
			r.Calendars = append(r.Calendars, &chainmailv1.CalendarInfo{Account: "work", Id: "bad\nid", Name: untrusted("x")})
			return r, nil
		},
		agenda: func(*chainmailv1.AgendaRequest) (*chainmailv1.AgendaResponse, error) {
			r := agendaFixture()
			// A second instance of the same series is offered once per ref.
			r.Events = append(r.Events, standupFixture())
			return r, nil
		},
		invites: func(*chainmailv1.ListInvitesRequest) (*chainmailv1.ListInvitesResponse, error) {
			return invitesFixture(), nil
		},
	}
}

// calCompEnv points completion at f at calNow in New York.
func calCompEnv(t *testing.T, f *fakeCalendar) Env {
	t.Helper()
	env := sockEnv(startFakeCalendar(t, f))
	env.TZ = mustZone(t, "America/New_York")
	env.Now = func() time.Time { return calNow }
	return env
}

func TestCompleteCalendar(t *testing.T) {
	t.Parallel()
	agendaRefs := []string{
		calInstRef + "\tTue 11-10 09:00 Stand [2Jup",
		calRef2 + "\tWed 11-11 all day Offsite",
		calRef3 + "\tThu 11-12 22:00 Late call",
	}
	inviteRefs := []string{calInstRef + "\tTue 11-10 09:00 Stand [2Jup", calRef3 + "\tFri 11-20 18:30 Dinner"}
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "event", args: []string{"event", ""}, want: append([]string{
			"cancel\tCancel an event and tell its attendees, or delete one with no other attendees",
			"create\tCreate an event; attendees the policy does not allow make it a draft that invites no one",
			"edit\tChange an event's fields; a change the policy does not allow is not applied",
		}, agendaRefs...)},
		{name: "event takes one", args: []string{"event", calRef, ""}, want: []string{}},
		{name: "edit", args: []string{"event", "edit", "ce_01jcb7m2k9r8q3v4w5x6y7z8b"}, want: []string{agendaRefs[1]}},
		{name: "cancel", args: []string{"event", "cancel", ""}, want: agendaRefs},
		{name: "invite", args: []string{"invite", ""}, want: inviteRefs},
		{name: "rsvp ref", args: []string{"rsvp", ""}, want: inviteRefs},
		{name: "rsvp answer", args: []string{"rsvp", calRef, ""}, want: []string{"accept", "decline", "tentative"}},
		{name: "rsvp done", args: []string{"rsvp", calRef, "accept", ""}, want: []string{}},
		{
			name: "calendar ids",
			args: []string{"agenda", "--calendar", ""},
			want: []string{"primary\twork me@example.com", "team@group.calendar.google.com\twork Team [31m", "/dav/calendars/me/personal/\thome Personal"},
		},
		{name: "calendar id prefix", args: []string{"event", "create", "--calendar", "/"}, want: []string{"/dav/calendars/me/personal/\thome Personal"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, _ := completeArgs(t, calCompEnv(t, completionCalendar()), tt.args...)
			if !slices.Equal(got, tt.want) {
				t.Errorf("candidates %q, want %q", got, tt.want)
			}
		})
	}
}

// Event refs come from the next fortnight's agenda; calendar IDs for the
// accounts --account names.
func TestCompleteCalendarRequests(t *testing.T) {
	t.Parallel()
	f := completionCalendar()
	env := calCompEnv(t, f)
	completeArgs(t, env, "event", "")
	completeArgs(t, env, "agenda", "--account", "work", "--calendar", "")
	want := []proto.Message{
		&chainmailv1.AgendaRequest{From: ts(calNow), To: ts(calNow.AddDate(0, 0, 14))},
		&chainmailv1.ListCalendarsRequest{Accounts: []string{"work"}},
	}
	if reqs := f.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
		t.Errorf("requests = %v, want %v", reqs, want)
	}
}
