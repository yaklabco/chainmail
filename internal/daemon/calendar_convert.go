package daemon

import (
	"math"
	"regexp"
	"slices"
	"strings"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
)

// otherToken stands for a value from event data that is not one of its
// documented values. Such values are written by whoever sent the event,
// so only documented ones are passed on as plain strings.
const otherToken = "OTHER"

// The documented values of the event fields CalendarService returns as
// plain strings.
var (
	eventStatuses = []string{string(calendar.StatusConfirmed), string(calendar.StatusTentative), string(calendar.StatusCancelled)}
	partStats     = []string{
		string(calendar.PartStatNeedsAction), string(calendar.PartStatAccepted), string(calendar.PartStatDeclined),
		string(calendar.PartStatTentative), string(calendar.PartStatDelegated),
	}
	attendeeRoles = []string{"REQ-PARTICIPANT", "OPT-PARTICIPANT", "CHAIR", "NON-PARTICIPANT"}
	inviteMethods = []string{calendar.MethodRequest, calendar.MethodUpdate, calendar.MethodCancel}
)

// zoneNameRE matches the shape of an IANA zone name, such as
// America/Los_Angeles, Etc/GMT+5 or UTC.
var zoneNameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+)*$`)

// knownToken returns v when it is empty or one of allowed, else otherToken.
func knownToken(v string, allowed []string) string {
	if v == "" || slices.Contains(allowed, v) {
		return v
	}
	return otherToken
}

// zoneName returns tz when it has the shape of an IANA zone name, else "".
func zoneName(tz string) string {
	if len(tz) <= 64 && zoneNameRE.MatchString(tz) {
		return tz
	}
	return ""
}

// plain makes a value from event data that the API returns as a plain
// string, such as a UID, an etag or an RRULE, safe to carry: valid UTF-8
// with no control characters.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}

// personProto is an organizer or attendee address, every part untrusted.
func personProto(p calendar.Person) *chainmailv1.Address {
	return &chainmailv1.Address{Name: untrusted(p.Name), Email: untrusted(p.Email)}
}

// eventView is one event as CalendarService returns it, before conversion.
type eventView struct {
	ref, account, calendarID string
	// ev is the event: a single event, a series master, an override, or an
	// instance with the instance's times and recurrence ID.
	ev calendar.Event
	// myPartStat and isOrganizer are James's role, as calcache computed it.
	myPartStat  calendar.PartStat
	isOrganizer bool
	// rrule is the series rule, set on a series and on its instances.
	rrule string
	// recurring marks a series or one of its instances.
	recurring  bool
	messageRef string
}

// viewOfRow is the view of a stored row as it is.
func viewOfRow(r calcache.EventRow) eventView {
	return eventView{
		ref: r.Ref, account: r.Account, calendarID: r.CalendarID, ev: r.Event,
		myPartStat: r.MyPartStat, isOrganizer: r.IsOrganizer, rrule: r.Event.RRule,
		recurring: isSeries(r.Event) || !r.Event.RecurrenceID.IsZero(),
	}
}

// viewOfResolved is the view of the event a ref names.
func viewOfResolved(r resolvedEvent) eventView {
	v := viewOfRow(r.row)
	v.ref, v.ev, v.recurring = r.ref, r.ev, r.recurring()
	v.myPartStat, v.isOrganizer = r.role()
	if r.master != nil {
		v.rrule = r.master.Event.RRule
	}
	return v
}

// eventProto converts a view to the API form. Text written by whoever made
// the event (summary, location, description, names and addresses) is
// wrapped as untrusted. Fields the API documents as fixed values carry only
// those values, and anything else as OTHER. self reports James's own
// addresses.
func eventProto(v eventView, self func(string) bool) *chainmailv1.CalendarEvent {
	ev := v.ev
	out := &chainmailv1.CalendarEvent{
		Ref: v.ref, Account: v.account, CalendarId: plain(v.calendarID), Uid: plain(ev.UID),
		Start: timestamp(ev.Start), End: timestamp(ev.End), TimeZone: zoneName(ev.TZID), AllDay: ev.AllDay,
		Status: knownToken(string(ev.Status), eventStatuses), Sequence: int32(min(max(ev.Sequence, 0), math.MaxInt32)),
		Summary: untrusted(ev.Summary), Location: untrusted(ev.Location), Description: untrustedBlock(ev.Description),
		AttendeesTruncated: ev.AttendeesTruncated, MyPartstat: knownToken(string(v.myPartStat), partStats),
		IsOrganizer: v.isOrganizer || !hasOtherAttendees(ev, self), Etag: plain(ev.ETag),
		RecurrenceRule: plain(v.rrule), Recurring: v.recurring, RecurrenceId: timestamp(ev.RecurrenceID),
		Transparent: ev.Transparent, MessageRef: v.messageRef,
	}
	if ev.Organizer != nil {
		out.Organizer = personProto(*ev.Organizer)
	}
	out.Attendees = make([]*chainmailv1.Attendee, len(ev.Attendees))
	for i, a := range ev.Attendees {
		out.Attendees[i] = &chainmailv1.Attendee{
			Address: personProto(a.Person), Partstat: knownToken(string(a.PartStat), partStats),
			Role: knownToken(a.Role, attendeeRoles), IsSelf: self(a.Email),
		}
	}
	return out
}

// hasOtherAttendees reports whether ev has an attendee that is not one of
// James's own addresses.
func hasOtherAttendees(ev calendar.Event, self func(string) bool) bool {
	return slices.ContainsFunc(ev.Attendees, func(a calendar.Attendee) bool { return !self(a.Email) })
}

// calendarInfoProto converts a stored calendar.
func calendarInfoProto(c calcache.CalendarRow) *chainmailv1.CalendarInfo {
	return &chainmailv1.CalendarInfo{
		Account: c.Account, Id: plain(c.ID), Name: untrusted(c.Name), Access: knownToken(string(c.Access), []string{string(calendar.AccessRead), string(calendar.AccessWrite)}),
		IsDefault: c.IsDefault, Primary: c.Primary, TimeZone: zoneName(c.TimeZone),
	}
}

// busyProto converts busy blocks.
func busyProto(blocks []calendar.BusyBlock) []*chainmailv1.BusyBlock {
	out := make([]*chainmailv1.BusyBlock, len(blocks))
	for i, b := range blocks {
		out[i] = &chainmailv1.BusyBlock{Start: timestamp(b.Start), End: timestamp(b.End)}
	}
	return out
}
