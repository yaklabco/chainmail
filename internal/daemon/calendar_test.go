package daemon_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
	calfake "github.com/yaklabco/chainmail/internal/calstore/fake"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

// calConfig has a store none account (host) that answers invites by
// email, and two CalDAV accounts (mw, work) backed by calstore fakes. Every
// account submits through one SMTP proxy port. The policy allows
// example.org, so blocked.invalid is never allowed.
const calConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
display_name = "Me Myself"
smtp = { host = "127.0.0.1", port = %[1]d, security = "starttls" }

[accounts.mw]
provider = "hostinger"
address = "james@mw.example"
smtp = { host = "127.0.0.1", port = %[1]d, security = "starttls" }

[accounts.mw.calendar]
store = "caldav"
url = "https://mw.example/"

[accounts.work]
provider = "hostinger"
address = "james@work.example"
smtp = { host = "127.0.0.1", port = %[1]d, security = "starttls" }

[accounts.work.calendar]
store = "caldav"
url = "https://work.example/"

[policy]
send_allow = ["*@example.org"]
`

// The calendars of the fakes.
const (
	mwCal       = "/cal/mw/"
	mwShared    = "/cal/mw-shared/"
	workCal     = "/cal/work/"
	secretText  = "SECRET-EVENT-TEXT"
	allowedAddr = "boss@example.org"
	blockedAddr = "stranger@blocked.invalid"
)

// calEnv is a daemon with calendar fakes for mw and work, a fake mail store
// per account and an SMTP proxy.
type calEnv struct {
	*daemontest.Harness
	ca         *tlstest.CA
	proxy      *smtpProxy
	mw, work   *calfake.Store
	mail       map[string]*fake.Store
	cache      *calcache.Store
	start      time.Time // a whole hour, two days from now
	mwFailSync bool
}

// startCal starts the daemon and waits for both calendar accounts' first
// sync, unless fail is set, which keeps mw's syncs failing.
func startCal(t *testing.T, seed func(e *calEnv)) *calEnv {
	t.Helper()
	e := &calEnv{
		ca:    tlstest.New(t),
		proxy: startProxy(t),
		mw:    calfake.New(calfake.Options{ObjectReplace: true}),
		work:  calfake.New(calfake.Options{ObjectReplace: true}),
		mail: map[string]*fake.Store{
			"host": fake.New("host", false), "mw": fake.New("mw", false), "work": fake.New("work", false),
		},
		start: time.Now().Add(48 * time.Hour).Truncate(time.Hour).UTC(),
	}
	e.mw.AddCalendar(calendar.Calendar{ID: mwCal, Name: "Work " + secretText, Access: calendar.AccessWrite, Primary: true, TimeZone: "Europe/London"})
	e.mw.AddCalendar(calendar.Calendar{ID: mwShared, Name: "Shared", Access: calendar.AccessRead})
	e.work.AddCalendar(calendar.Calendar{ID: workCal, Name: "Office", Access: calendar.AccessWrite, Primary: true})
	if seed != nil {
		seed(e)
	}
	if e.mwFailSync {
		e.mw.Fail(calfake.MethodListCalendars, calstore.ErrUnavailable)
	}
	e.Harness = daemontest.Start(t, fmt.Sprintf(calConfig, e.proxy.port()), daemontest.Options{
		RootCAs:               e.ca.Pool,
		NewStore:              func(a config.ResolvedAccount) mailstore.Store { return e.mail[a.Name] },
		OutcomeSearchInterval: 10 * time.Millisecond,
		Calendar:              true,
		CalendarStores:        map[string]calstore.Store{"mw": e.mw, "work": e.work},
		CalendarPoll:          time.Hour,
	})
	for _, name := range []string{"host", "mw", "work"} {
		setSecret(t, e.Harness, "accounts/"+name, "pw")
	}
	if !e.mwFailSync {
		waitCalendarReady(t, e.Harness, "mw")
	}
	waitCalendarReady(t, e.Harness, "work")
	cc, err := calcache.Open(t.Context(), filepath.Join(e.StateDir(), calcache.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	e.cache = cc
	return e
}

// person is an attendee with a PARTSTAT.
func person(email string, ps calendar.PartStat) calendar.Attendee {
	return calendar.Attendee{Person: calendar.Person{Email: email, Name: "Name " + secretText}, PartStat: ps, Role: "REQ-PARTICIPANT"}
}

// invitation is an event boss@example.org organizes with self invited.
func invitation(uid, self string, start time.Time) calendar.Event {
	return calendar.Event{
		UID: uid, Summary: "Summary " + secretText, Location: "Room " + secretText, Description: "Body " + secretText,
		Start: start, End: start.Add(time.Hour), Status: calendar.StatusConfirmed,
		Organizer: &calendar.Person{Email: allowedAddr, Name: "Boss " + secretText},
		Attendees: []calendar.Attendee{person(allowedAddr, calendar.PartStatAccepted), person(self, calendar.PartStatNeedsAction)},
	}
}

// own is an event self organizes with attendees.
func own(uid, self string, start time.Time, others ...string) calendar.Event {
	ev := calendar.Event{
		UID: uid, Summary: "Mine " + secretText, Start: start, End: start.Add(time.Hour), Status: calendar.StatusConfirmed,
		Organizer: &calendar.Person{Email: self},
	}
	if len(others) > 0 {
		ev.Attendees = append(ev.Attendees, person(self, calendar.PartStatAccepted))
		for _, o := range others {
			ev.Attendees = append(ev.Attendees, person(o, calendar.PartStatNeedsAction))
		}
	}
	return ev
}

// ts is t as a protobuf timestamp.
func ts(t time.Time) *timestamppb.Timestamp { return timestamppb.New(t) }

// wantReason fails the test unless err carries reason.
func wantReason(t *testing.T, what string, err error, reason string) {
	t.Helper()
	if got := api.Detail(err).GetReason(); got != reason {
		t.Fatalf("%s: err = %v (reason %q), want %s", what, err, got, reason)
	}
}

// noSecretInAudit fails the test if any audit row holds event text.
func noSecretInAudit(t *testing.T, h *daemontest.Harness) {
	t.Helper()
	for _, r := range h.AuditRows(t) {
		if strings.Contains(fmt.Sprint(r), "SECRET") || strings.Contains(fmt.Sprint(r), "Boss") {
			t.Errorf("audit row holds event text: %+v", r)
		}
	}
}

// auditRow returns the last audit row of action.
func auditRow(t *testing.T, h *daemontest.Harness, action string) daemontest.AuditRow {
	t.Helper()
	rows := h.AuditRows(t)
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Action == action {
			return rows[i]
		}
	}
	t.Fatalf("no audit row for %s in %+v", action, rows)
	return daemontest.AuditRow{}
}

// checkUntrusted fails the test when event text appears in any plain string
// of m: event text must only come back inside Untrusted messages.
func checkUntrusted(t *testing.T, m proto.Message) {
	t.Helper()
	var walk func(protoMsg proto.Message)
	walk = func(protoMsg proto.Message) {
		r := protoMsg.ProtoReflect()
		if _, ok := protoMsg.(*chainmailv1.Untrusted); ok {
			return
		}
		r.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			switch {
			case fd.IsList() && fd.Message() != nil:
				for i := range v.List().Len() {
					walk(v.List().Get(i).Message().Interface())
				}
			case fd.IsList():
				for i := range v.List().Len() {
					if strings.Contains(v.List().Get(i).String(), "SECRET") {
						t.Errorf("event text in plain field %s", fd.FullName())
					}
				}
			case fd.Message() != nil:
				walk(v.Message().Interface())
			default:
				if strings.Contains(v.String(), "SECRET") {
					t.Errorf("event text in plain field %s", fd.FullName())
				}
			}
			return true
		})
	}
	walk(m)
}

func TestCalendarServiceIsRegistered(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, "", daemontest.Options{})
	_, err := h.Client.Calendar.ListCalendars(t.Context(), connect.NewRequest(&chainmailv1.ListCalendarsRequest{}))
	if err != nil && strings.Contains(err.Error(), "404") {
		t.Fatalf("ListCalendars: %v, want the route served", err)
	}
	if connect.CodeOf(err) == connect.CodeUnimplemented {
		t.Fatalf("ListCalendars: %v, want it implemented", err)
	}
}

func TestCalendarReadsBeforeTheFirstSync(t *testing.T) {
	t.Parallel()
	e := startCal(t, func(e *calEnv) { e.mwFailSync = true })
	ctx := t.Context()
	_, err := e.Client.Calendar.ListCalendars(ctx, connect.NewRequest(&chainmailv1.ListCalendarsRequest{Accounts: []string{"mw"}}))
	wantReason(t, "ListCalendars", err, api.ReasonCacheNotReady)
	_, err = e.Client.Calendar.Agenda(ctx, connect.NewRequest(&chainmailv1.AgendaRequest{}))
	wantReason(t, "Agenda of every account", err, api.ReasonCacheNotReady)
	// work synced, so it answers on its own.
	resp, err := e.Client.Calendar.Agenda(ctx, connect.NewRequest(&chainmailv1.AgendaRequest{Accounts: []string{"work"}}))
	if err != nil {
		t.Fatalf("Agenda of work: %v", err)
	}
	if len(resp.Msg.GetEvents()) != 0 {
		t.Errorf("events = %v, want none", resp.Msg.GetEvents())
	}
}

func TestListCalendars(t *testing.T) {
	t.Parallel()
	e := startCal(t, nil)
	ctx := t.Context()
	resp, err := e.Client.Calendar.ListCalendars(ctx, connect.NewRequest(&chainmailv1.ListCalendarsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range resp.Msg.GetCalendars() {
		got = append(got, fmt.Sprintf("%s %s %s default=%v primary=%v tz=%s", c.GetAccount(), c.GetId(), c.GetAccess(), c.GetIsDefault(), c.GetPrimary(), c.GetTimeZone()))
	}
	want := []string{
		"mw /cal/mw/ write default=true primary=true tz=Europe/London",
		"mw /cal/mw-shared/ read default=false primary=false tz=",
		"work /cal/work/ write default=true primary=true tz=",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calendars:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if n := resp.Msg.GetCalendars()[0].GetName().GetUntrusted(); !strings.Contains(n, secretText) {
		t.Errorf("name = %q, want the calendar's name as untrusted text", n)
	}
	checkUntrusted(t, resp.Msg)

	_, err = e.Client.Calendar.ListCalendars(ctx, connect.NewRequest(&chainmailv1.ListCalendarsRequest{Accounts: []string{"host"}}))
	wantReason(t, "ListCalendars of a store none account", err, api.ReasonNoCalendarStore)
	_, err = e.Client.Calendar.ListCalendars(ctx, connect.NewRequest(&chainmailv1.ListCalendarsRequest{Accounts: []string{"nope"}}))
	wantReason(t, "ListCalendars of an unknown account", err, api.ReasonNotFound)
	if r := auditRow(t, e.Harness, "calendar.list_calendars"); r.Result != "error" || r.Reason != api.ReasonNotFound {
		t.Errorf("audit row %+v", r)
	}
	noSecretInAudit(t, e.Harness)
}

func TestAgenda(t *testing.T) {
	t.Parallel()
	var weekly, single, declined string
	e := startCal(t, func(e *calEnv) {
		s := e.start
		master := own("weekly@x", "james@mw.example", s.Add(-14*24*time.Hour))
		master.RRule = "FREQ=WEEKLY"
		weekly = e.mw.Add(mwCal, master)
		// This week's instance moved half an hour later.
		e.mw.Change(mwCal, weekly, func(evs []calendar.Event) []calendar.Event {
			moved := evs[0]
			moved.RRule, moved.RecurrenceID = "", s
			moved.Start, moved.End = s.Add(30*time.Minute), s.Add(90*time.Minute)
			return append(evs, moved)
		})
		single = e.mw.Add(mwCal, invitation("single@x", "james@mw.example", s.Add(2*time.Hour)))
		dec := invitation("declined@x", "james@mw.example", s.Add(3*time.Hour))
		dec.Attendees[1].PartStat = calendar.PartStatDeclined
		declined = e.mw.Add(mwCal, dec)
		cancelled := invitation("cancelled@x", "james@mw.example", s.Add(4*time.Hour))
		cancelled.Status = calendar.StatusCancelled
		e.mw.Add(mwCal, cancelled)
		e.work.Add(workCal, own("work@x", "james@work.example", s.Add(time.Hour)))
	})
	ctx := t.Context()
	from, to := e.start.Add(-time.Hour), e.start.Add(5*time.Hour)
	resp, err := e.Client.Calendar.Agenda(ctx, connect.NewRequest(&chainmailv1.AgendaRequest{From: ts(from), To: ts(to)}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, ev := range resp.Msg.GetEvents() {
		got = append(got, ev.GetAccount()+" "+ev.GetRef()+" "+ev.GetStart().AsTime().Format(time.RFC3339))
	}
	base := calendar.EventRef
	want := []string{
		"mw " + calendar.InstanceRef(base("mw", mwCal, weekly), e.start) + " " + e.start.Add(30*time.Minute).Format(time.RFC3339),
		"work " + base("work", workCal, calfake.ObjectName("work@x")) + " " + e.start.Add(time.Hour).Format(time.RFC3339),
		"mw " + base("mw", mwCal, single) + " " + e.start.Add(2*time.Hour).Format(time.RFC3339),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("agenda:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if resp.Msg.GetTruncated() {
		t.Error("truncated, want not")
	}
	inst := resp.Msg.GetEvents()[0]
	if !inst.GetRecurring() || inst.GetRecurrenceRule() != "FREQ=WEEKLY" || !inst.GetRecurrenceId().AsTime().Equal(e.start) || !inst.GetIsOrganizer() {
		t.Errorf("instance = %v", inst)
	}
	if s := resp.Msg.GetEvents()[2]; s.GetMyPartstat() != "NEEDS-ACTION" || s.GetIsOrganizer() || s.GetSummary().GetUntrusted() != "Summary "+secretText {
		t.Errorf("single = %v", s)
	}
	checkUntrusted(t, resp.Msg)

	resp, err = e.Client.Calendar.Agenda(ctx, connect.NewRequest(&chainmailv1.AgendaRequest{
		Accounts: []string{"mw"}, Calendars: []string{mwCal}, From: ts(from), To: ts(to), IncludeDeclined: true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(resp.Msg.GetEvents()); n != 3 || resp.Msg.GetEvents()[2].GetRef() != base("mw", mwCal, declined) {
		t.Errorf("with declined: %d events %v, want the declined one last", n, resp.Msg.GetEvents())
	}

	for name, req := range map[string]*chainmailv1.AgendaRequest{
		"backwards":   {From: ts(to), To: ts(from)},
		"401 days":    {From: ts(from), To: ts(from.Add(401 * 24 * time.Hour))},
		"store none":  {Accounts: []string{"host"}},
		"bad account": {Accounts: []string{"Bad Name"}},
	} {
		_, err := e.Client.Calendar.Agenda(ctx, connect.NewRequest(req))
		want := api.ReasonInvalidArgument
		if name == "store none" {
			want = api.ReasonNoCalendarStore
		}
		wantReason(t, name, err, want)
	}
	if r := auditRow(t, e.Harness, "calendar.agenda"); r.Result != "error" {
		t.Errorf("audit row %+v", r)
	}
	noSecretInAudit(t, e.Harness)
}

func TestGetEvent(t *testing.T) {
	t.Parallel()
	var weekly, single string
	e := startCal(t, func(e *calEnv) {
		master := invitation("weekly@x", "james@mw.example", e.start)
		master.RRule = "FREQ=WEEKLY;COUNT=4"
		weekly = e.mw.Add(mwCal, master)
		single = e.mw.Add(mwCal, invitation("single@x", "james@mw.example", e.start))
	})
	ctx := t.Context()
	get := func(ref string) (*chainmailv1.CalendarEvent, error) {
		resp, err := e.Client.Calendar.GetEvent(ctx, connect.NewRequest(&chainmailv1.GetEventRequest{Ref: ref}))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetEvent(), nil
	}
	ref := calendar.EventRef("mw", mwCal, single)
	ev, err := get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if ev.GetRef() != ref || ev.GetUid() != "single@x" || ev.GetEtag() == "" || len(ev.GetAttendees()) != 2 ||
		ev.GetOrganizer().GetEmail().GetUntrusted() != allowedAddr || !ev.GetAttendees()[1].GetIsSelf() {
		t.Errorf("single = %v", ev)
	}
	checkUntrusted(t, ev)

	// The third instance of the series, with no override of its own.
	third := e.start.Add(14 * 24 * time.Hour)
	iref := calendar.InstanceRef(calendar.EventRef("mw", mwCal, weekly), third)
	ev, err = get(iref)
	if err != nil {
		t.Fatal(err)
	}
	if ev.GetRef() != iref || !ev.GetStart().AsTime().Equal(third) || !ev.GetRecurrenceId().AsTime().Equal(third) || !ev.GetRecurring() {
		t.Errorf("instance = %v", ev)
	}
	_, err = get(calendar.InstanceRef(calendar.EventRef("mw", mwCal, weekly), third.Add(time.Hour)))
	wantReason(t, "no such instance", err, api.ReasonNotFound)
	_, err = get(calendar.EventRef("mw", mwCal, "nothing"))
	wantReason(t, "unknown ref", err, api.ReasonNotFound)
	_, err = get("ce_bad " + secretText)
	wantReason(t, "bad ref", err, api.ReasonInvalidArgument)
	if err == nil || strings.Contains(err.Error(), secretText) {
		t.Errorf("bad ref error %v repeats the ref", err)
	}
	if r := auditRow(t, e.Harness, "calendar.get_event"); !strings.Contains(r.Targets, "(invalid)") {
		t.Errorf("audit row %+v, want the bad ref recorded as invalid", r)
	}
	noSecretInAudit(t, e.Harness)
}

// putMailInvite stores a mail invite for account as the invite hook would.
func (e *calEnv) putMailInvite(t *testing.T, account, msgRef, method string, ev calendar.Event) string {
	t.Helper()
	raw, err := calendar.EncodeICSAt(method, []calendar.Event{ev}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	inv, err := calendar.ParseInvite(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	row := calcache.MailInviteRow{
		Account: account, MessageRef: msgRef, UID: inv.UID, Method: inv.Method, Sequence: inv.Sequence,
		DTStamp: inv.DTStamp, Raw: raw, FoundAt: time.Now(),
	}
	if err := e.cache.PutMailInvite(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	return calendar.MailInviteRef(account, inv.UID)
}

func TestListAndGetInvites(t *testing.T) {
	t.Parallel()
	var calInvite string
	e := startCal(t, func(e *calEnv) {
		calInvite = e.mw.Add(mwCal, invitation("cal@x", "james@mw.example", e.start))
		accepted := invitation("accepted@x", "james@mw.example", e.start)
		accepted.Attendees[1].PartStat = calendar.PartStatAccepted
		e.mw.Add(mwCal, accepted)
		e.mw.Add(mwCal, own("mine@x", "james@mw.example", e.start, allowedAddr))
		e.mw.Add(mwCal, invitation("past@x", "james@mw.example", time.Now().Add(-3*time.Hour)))
	})
	ctx := t.Context()
	mailRef := e.putMailInvite(t, "host", "host:INBOX:1:1", "REQUEST", invitation("mail@x", "me@example.com", e.start.Add(time.Hour)))
	// A mail copy of the calendar invite: the calendar invite wins and
	// carries its message ref.
	e.putMailInvite(t, "mw", "mw:INBOX:1:2", "REQUEST", invitation("cal@x", "james@mw.example", e.start))
	// Withdrawn, organized by James, and over.
	e.putMailInvite(t, "host", "host:INBOX:1:3", "REQUEST", invitation("withdrawn@x", "me@example.com", e.start))
	cancel := invitation("withdrawn@x", "me@example.com", e.start)
	cancel.Sequence = 1
	cancel.Status = calendar.StatusCancelled
	e.putMailInvite(t, "host", "host:INBOX:1:4", "CANCEL", cancel)
	e.putMailInvite(t, "host", "host:Sent:1:5", "REQUEST", own("organized@x", "me@example.com", e.start, allowedAddr))
	e.putMailInvite(t, "host", "host:INBOX:1:6", "REQUEST", invitation("over@x", "me@example.com", time.Now().Add(-3*time.Hour)))

	resp, err := e.Client.Calendar.ListInvites(ctx, connect.NewRequest(&chainmailv1.ListInvitesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, inv := range resp.Msg.GetInvites() {
		ev := inv.GetEvent()
		got = append(got, fmt.Sprintf("%s %s %s %s %s msg=%s", inv.GetSource(), inv.GetMethod(), ev.GetAccount(), ev.GetRef(), ev.GetMyPartstat(), ev.GetMessageRef()))
	}
	want := []string{
		"calendar REQUEST mw " + calendar.EventRef("mw", mwCal, calInvite) + " NEEDS-ACTION msg=mw:INBOX:1:2",
		"mail REQUEST host " + mailRef + " NEEDS-ACTION msg=host:INBOX:1:1",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("invites:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	checkUntrusted(t, resp.Msg)

	inv, err := e.Client.Calendar.GetInvite(ctx, connect.NewRequest(&chainmailv1.GetInviteRequest{Ref: mailRef}))
	if err != nil {
		t.Fatal(err)
	}
	if g := inv.Msg.GetInvite(); g.GetSource() != "mail" || g.GetEvent().GetSummary().GetUntrusted() != "Summary "+secretText || g.GetEvent().GetUid() != "mail@x" {
		t.Errorf("mail invite = %v", g)
	}
	inv, err = e.Client.Calendar.GetInvite(ctx, connect.NewRequest(&chainmailv1.GetInviteRequest{Ref: calendar.EventRef("mw", mwCal, calInvite)}))
	if err != nil {
		t.Fatal(err)
	}
	if g := inv.Msg.GetInvite(); g.GetSource() != "calendar" || g.GetEvent().GetMessageRef() != "mw:INBOX:1:2" {
		t.Errorf("calendar invite = %v", g)
	}
	_, err = e.Client.Calendar.GetInvite(ctx, connect.NewRequest(&chainmailv1.GetInviteRequest{Ref: calendar.MailInviteRef("host", "nothing")}))
	wantReason(t, "unknown invite", err, api.ReasonNotFound)

	resp, err = e.Client.Calendar.ListInvites(ctx, connect.NewRequest(&chainmailv1.ListInvitesRequest{Accounts: []string{"host"}}))
	if err != nil || len(resp.Msg.GetInvites()) != 1 {
		t.Errorf("host invites = %v, %v; want the mail invite", resp.Msg.GetInvites(), err)
	}
	noSecretInAudit(t, e.Harness)
}

func TestFreeBusy(t *testing.T) {
	t.Parallel()
	e := startCal(t, func(e *calEnv) {
		s := e.start
		e.mw.Add(mwCal, own("a@x", "james@mw.example", s))
		e.work.Add(workCal, own("b@x", "james@work.example", s.Add(30*time.Minute)))
		dec := invitation("dec@x", "james@mw.example", s.Add(3*time.Hour))
		dec.Attendees[1].PartStat = calendar.PartStatDeclined
		e.mw.Add(mwCal, dec)
		free := own("free@x", "james@mw.example", s.Add(4*time.Hour))
		free.Transparent = true
		e.mw.Add(mwCal, free)
		e.work.SetFreeBusy(allowedAddr, calendar.FreeBusy{Known: true, Busy: []calendar.BusyBlock{{Start: s, End: s.Add(time.Hour)}}})
	})
	ctx := t.Context()
	from, to := e.start.Add(-time.Hour), e.start.Add(6*time.Hour)
	resp, err := e.Client.Calendar.FreeBusy(ctx, connect.NewRequest(&chainmailv1.FreeBusyRequest{
		Account: "work", People: []string{allowedAddr, "nobody@example.org"}, From: ts(from), To: ts(to),
	}))
	if err != nil {
		t.Fatal(err)
	}
	self := resp.Msg.GetSelfBusy()
	if len(self) != 1 || !self[0].GetStart().AsTime().Equal(e.start) || !self[0].GetEnd().AsTime().Equal(e.start.Add(90*time.Minute)) {
		t.Errorf("self busy = %v, want one merged block of the two accepted events", self)
	}
	ppl := resp.Msg.GetPeople()
	if len(ppl) != 2 || ppl[0].GetPerson() != allowedAddr || !ppl[0].GetKnown() || len(ppl[0].GetBusy()) != 1 ||
		ppl[1].GetPerson() != "nobody@example.org" || ppl[1].GetKnown() {
		t.Errorf("people = %v", ppl)
	}

	_, err = e.Client.Calendar.FreeBusy(ctx, connect.NewRequest(&chainmailv1.FreeBusyRequest{People: []string{allowedAddr}}))
	wantReason(t, "people without an account", err, api.ReasonInvalidArgument)
	_, err = e.Client.Calendar.FreeBusy(ctx, connect.NewRequest(&chainmailv1.FreeBusyRequest{Account: "host", People: []string{allowedAddr}}))
	wantReason(t, "people on a store none account", err, api.ReasonNoCalendarStore)
	_, err = e.Client.Calendar.FreeBusy(ctx, connect.NewRequest(&chainmailv1.FreeBusyRequest{Account: "work", People: []string{"not an address " + secretText}}))
	wantReason(t, "bad person", err, api.ReasonInvalidArgument)
	if err == nil || strings.Contains(err.Error(), secretText) {
		t.Errorf("bad person error %v repeats the address", err)
	}
	e.work.FailNext(calfake.MethodFreeBusy, fmt.Errorf("%w: status 401", calstore.ErrAuth))
	_, err = e.Client.Calendar.FreeBusy(ctx, connect.NewRequest(&chainmailv1.FreeBusyRequest{Account: "work", People: []string{allowedAddr}}))
	wantReason(t, "store refuses credentials", err, api.ReasonAuthRequired)
	if h := api.Detail(err).GetHint(); h != "run: chainmail auth login work" {
		t.Errorf("hint = %q, want the account's login", h)
	}
	noSecretInAudit(t, e.Harness)
}
