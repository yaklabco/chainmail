package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
)

// The layouts calendar output shows times in.
const (
	calDayLayout    = "Mon 2006-01-02"
	calMinuteLayout = "Mon 2006-01-02 15:04"
	calClockLayout  = "15:04"
	calShortLayout  = "Mon 01-02 15:04"
	calShortDay     = "Mon 01-02"
)

// summaryWidth is how much of an event's title a table shows.
const summaryWidth = 50

// allDayDates returns the first and last dates of an all-day event. The
// daemon gives an all-day event 00:00 UTC of its first date and of the day
// after its last, so the dates are read in UTC, never in a local zone.
func allDayDates(start, end *timestamppb.Timestamp) (first, last time.Time) {
	first = start.AsTime().UTC()
	last = end.AsTime().UTC().AddDate(0, 0, -1)
	if last.Before(first) {
		last = first
	}
	return first, last
}

// timedRange is a span in loc: "Tue 2026-11-10 09:00-09:30 EST", or with
// both dates when it ends on another day, and with the zone of each end
// when a change of daylight saving time falls inside it.
func timedRange(start, end time.Time, loc *time.Location) string {
	return span(start.In(loc), end.In(loc), true)
}

// span is s to e in their zone: the start's date when dated, both dates
// when they fall on different days, and each end's zone abbreviation when
// the two differ, else the end's alone.
func span(s, e time.Time, dated bool) string {
	sameDay := s.Format(time.DateOnly) == e.Format(time.DateOnly)
	from, sep, to := s.Format(calClockLayout), "-", e.Format(calClockLayout)
	if dated || !sameDay {
		from = s.Format(calMinuteLayout)
	}
	if !sameDay {
		sep, to = " to ", e.Format(calMinuteLayout)
	}
	if sz := s.Format("MST"); sz != e.Format("MST") {
		from += " " + sz
	}
	return from + sep + to + " " + e.Format("MST")
}

// whenText is when an event takes place, in loc unless it is all day.
func whenText(e *chainmailv1.CalendarEvent, loc *time.Location) string {
	if e.GetAllDay() {
		first, last := allDayDates(e.GetStart(), e.GetEnd())
		if first.Equal(last) {
			return first.Format(calDayLayout) + ", all day"
		}
		return first.Format(calDayLayout) + " to " + last.Format(calDayLayout) + ", all day"
	}
	return timedRange(e.GetStart().AsTime(), e.GetEnd().AsTime(), loc)
}

// whenShort is an event's start in a few characters, for completion.
func whenShort(e *chainmailv1.CalendarEvent, loc *time.Location) string {
	if e.GetAllDay() {
		first, _ := allDayDates(e.GetStart(), e.GetEnd())
		return first.Format(calShortDay) + " all day"
	}
	return e.GetStart().AsTime().In(loc).Format(calShortLayout)
}

// ownZone is a timed event's own zone, when its offset differs from loc's
// at the event's start, else nil.
func ownZone(e *chainmailv1.CalendarEvent, loc *time.Location) *time.Location {
	if e.GetAllDay() || e.GetTimeZone() == "" {
		return nil
	}
	own, err := time.LoadLocation(e.GetTimeZone())
	if err != nil {
		return nil
	}
	start := e.GetStart().AsTime()
	_, ownOffset := start.In(own).Zone()
	_, locOffset := start.In(loc).Zone()
	if ownOffset == locOffset {
		return nil
	}
	return own
}

// ownZoneText is when a timed event takes place in its own zone, when that
// zone's offset differs from loc's at its start, else "".
func ownZoneText(e *chainmailv1.CalendarEvent, loc *time.Location) string {
	own := ownZone(e, loc)
	if own == nil {
		return ""
	}
	return timedRange(e.GetStart().AsTime(), e.GetEnd().AsTime(), own) + " (" + termText(e.GetTimeZone()) + ")"
}

// ownZoneRow is ownZoneText for a table row, to follow whenText on the same
// line: " (15:00-15:30 CET Europe/Paris)", with dates only when they are
// not the dates loc shows. It is "" when ownZoneText is.
func ownZoneRow(e *chainmailv1.CalendarEvent, loc *time.Location) string {
	own := ownZone(e, loc)
	if own == nil {
		return ""
	}
	start, end := e.GetStart().AsTime(), e.GetEnd().AsTime()
	s, en := start.In(own), end.In(own)
	dated := s.Format(time.DateOnly) != start.In(loc).Format(time.DateOnly) || en.Format(time.DateOnly) != end.In(loc).Format(time.DateOnly)
	return " (" + span(s, en, dated) + " " + termText(e.GetTimeZone()) + ")"
}

// partstatText is how James stands with an event: organizer, his PARTSTAT
// in lower case, or "-".
func partstatText(e *chainmailv1.CalendarEvent) string {
	if e.GetIsOrganizer() {
		return "organizer"
	}
	return orDash(strings.ToLower(termText(e.GetMyPartstat())))
}

// addressText is an address as "Name <email>", or the email alone.
func addressText(a *chainmailv1.Address) string {
	if a == nil {
		return ""
	}
	return termText(addressList([]*chainmailv1.Address{a}))
}

// calendarsTable prints one line per calendar.
func calendarsTable(w io.Writer, r *chainmailv1.ListCalendarsResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tID\tACCESS\tFLAGS\tZONE\tNAME")
	for _, c := range r.GetCalendars() {
		var flags []string
		if c.GetIsDefault() {
			flags = append(flags, "default")
		}
		if c.GetPrimary() {
			flags = append(flags, "primary")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			termText(c.GetAccount()), termText(c.GetId()), orDash(termText(c.GetAccess())), orDash(strings.Join(flags, ",")),
			orDash(termText(c.GetTimeZone())), orDash(clip(termText(c.GetName().GetUntrusted()), summaryWidth)))
	}
	return tw.Flush()
}

// agendaTable prints one line per event, in loc, with the time in the
// event's own zone when that differs, and says when instances were left
// out.
func agendaTable(w io.Writer, loc *time.Location, r *chainmailv1.AgendaResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "WHEN\tACCOUNT\tSTATUS\tME\tREF\tSUMMARY")
	for _, e := range r.GetEvents() {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			whenText(e, loc)+ownZoneRow(e, loc), termText(e.GetAccount()), orDash(strings.ToLower(termText(e.GetStatus()))), partstatText(e),
			termText(e.GetRef()), orDash(clip(termText(e.GetSummary().GetUntrusted()), summaryWidth)))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if r.GetTruncated() {
		return writeString(w, "some instances are left out: a series repeats more than 1000 times in the range, or its rule is not expanded\n")
	}
	return nil
}

// invitesTable prints one line per invite, in loc, with the time in the
// event's own zone when that differs.
func invitesTable(w io.Writer, loc *time.Location, r *chainmailv1.ListInvitesResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "WHEN\tACCOUNT\tSOURCE\tMETHOD\tORGANIZER\tREF\tSUMMARY")
	for _, inv := range r.GetInvites() {
		e := inv.GetEvent()
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			whenText(e, loc)+ownZoneRow(e, loc), termText(e.GetAccount()), orDash(termText(inv.GetSource())), orDash(termText(inv.GetMethod())),
			orDash(clip(termText(e.GetOrganizer().GetEmail().GetUntrusted()), summaryWidth)), termText(e.GetRef()),
			orDash(clip(termText(e.GetSummary().GetUntrusted()), summaryWidth)))
	}
	return tw.Flush()
}

// inviteViewText prints an invite: where it came from, then its event.
func inviteViewText(w io.Writer, loc *time.Location, inv *chainmailv1.Invite) error {
	return eventViewText(w, loc, inv.GetEvent(),
		[2]string{"source", termText(inv.GetSource())}, [2]string{"method", termText(inv.GetMethod())})
}

// eventViewText prints one event: first rows, then its fields, its
// attendees and its description. Every value is cleaned for the terminal.
func eventViewText(w io.Writer, loc *time.Location, e *chainmailv1.CalendarEvent, first ...[2]string) error {
	rows := slices.Concat(first, [][2]string{
		{"ref", termText(e.GetRef())},
		{"account", termText(e.GetAccount())},
		{"calendar", termText(e.GetCalendarId())},
		{"summary", termText(e.GetSummary().GetUntrusted())},
		{"when", whenText(e, loc)},
	})
	if own := ownZoneText(e, loc); own != "" {
		rows = append(rows, [2]string{"event zone", own})
	}
	rows = append(rows,
		[2]string{"status", strings.ToLower(termText(e.GetStatus()))},
		[2]string{"location", termText(e.GetLocation().GetUntrusted())},
		[2]string{"organizer", addressText(e.GetOrganizer())},
		[2]string{"me", partstatText(e)},
	)
	if e.GetRecurring() {
		rows = append(rows, [2]string{"recurrence", termText(e.GetRecurrenceRule())})
	}
	if id := e.GetRecurrenceId(); id != nil {
		rows = append(rows, [2]string{"instance of", id.AsTime().UTC().Format(time.RFC3339)})
	}
	busy := "yes"
	if e.GetTransparent() {
		busy = "no"
	}
	rows = append(rows,
		[2]string{"busy", busy},
		[2]string{"sequence", fmt.Sprint(e.GetSequence())},
		[2]string{"uid", termText(e.GetUid())},
		[2]string{"etag", termText(e.GetEtag())},
	)
	if m := e.GetMessageRef(); m != "" {
		rows = append(rows, [2]string{"message", termText(m)})
	}
	tw := newTable(w)
	for _, row := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\n", row[0], orDash(row[1]))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if err := attendeesText(w, e); err != nil {
		return err
	}
	if d := termBlock(e.GetDescription().GetUntrusted()); strings.TrimSpace(d) != "" {
		return writeString(w, "\ndescription:\n"+indent(d))
	}
	return nil
}

// attendeesText prints an event's attendees, one per line.
func attendeesText(w io.Writer, e *chainmailv1.CalendarEvent) error {
	if len(e.GetAttendees()) == 0 {
		return nil
	}
	if err := writeString(w, "\nattendees:\n"); err != nil {
		return err
	}
	tw := newTable(w)
	for _, a := range e.GetAttendees() {
		var notes []string
		if a.GetIsSelf() {
			notes = append(notes, "me")
		}
		if role := a.GetRole(); role != "" && role != "REQ-PARTICIPANT" {
			notes = append(notes, strings.ToLower(termText(role)))
		}
		line := "  " + orDash(termText(a.GetPartstat())) + "\t" + addressText(a.GetAddress())
		if len(notes) > 0 {
			line += "\t" + strings.Join(notes, ", ")
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if e.GetAttendeesTruncated() {
		return writeString(w, "  more than 500 attendees; the rest are left out\n")
	}
	return nil
}

// freeBusyTable prints James's busy blocks, then each person's, in loc.
func freeBusyTable(w io.Writer, loc *time.Location, r *chainmailv1.FreeBusyResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "WHO\tBUSY")
	blocks := func(who string, busy []*chainmailv1.BusyBlock) {
		if len(busy) == 0 {
			_, _ = fmt.Fprintf(tw, "%s\tfree\n", who)
		}
		for _, b := range busy {
			_, _ = fmt.Fprintf(tw, "%s\t%s\n", who, timedRange(b.GetStart().AsTime(), b.GetEnd().AsTime(), loc))
		}
	}
	blocks("me", r.GetSelfBusy())
	for _, p := range r.GetPeople() {
		who := orDash(termText(p.GetPerson()))
		if !p.GetKnown() {
			_, _ = fmt.Fprintf(tw, "%s\tunknown: the provider cannot see this calendar\n", who)
			continue
		}
		blocks(who, p.GetBusy())
	}
	return tw.Flush()
}

// What a held calendar operation did, for its first line and its error.
type heldKind int

const (
	heldCreate heldKind = iota
	heldChange
	heldRsvp
)

// calendarHeldError is the HELD error a held calendar operation exits
// with, or nil when h is nil.
func calendarHeldError(kind heldKind, h *chainmailv1.CalendarHeld) error {
	if h == nil {
		return nil
	}
	msg := "held by policy: not applied"
	switch {
	case kind == heldCreate:
		msg = "held by policy: created as a draft event; no invitations were sent"
	case kind == heldRsvp && h.GetDraftRef() != "":
		msg = "held by policy: the reply was saved to Drafts"
	case kind == heldRsvp:
		msg = "held by policy: not answered"
	}
	return &exitError{reason: api.ReasonHeld, msg: msg, hint: h.GetHint()}
}

// heldDetails are the lines after a held result's first: the Drafts
// copy, the change, the blocked attendees, too many and the hint.
func heldDetails(b *strings.Builder, h *chainmailv1.CalendarHeld) {
	if d := h.GetDraftRef(); d != "" {
		fmt.Fprintf(b, "reply saved to Drafts: %s\n", termText(d))
	}
	if s := h.GetSummary(); s != "" {
		fmt.Fprintf(b, "change: %s\n", termText(s))
	}
	if blocked := h.GetBlocked(); len(blocked) > 0 {
		clean := make([]string, len(blocked))
		for i, a := range blocked {
			clean[i] = termText(a.GetUntrusted())
		}
		fmt.Fprintf(b, "blocked: %s\n", strings.Join(clean, ", "))
	}
	if h.GetTooMany() {
		b.WriteString("too many attendees\n")
	}
	if hint := h.GetHint(); hint != "" {
		fmt.Fprintf(b, "hint: %s\n", termText(hint))
	}
}

// rsvpText says how an invite was answered, or that the answer was held.
func rsvpText(ref, answer string, r *chainmailv1.RespondResponse) string {
	var b strings.Builder
	if h := r.GetHeld(); h != nil {
		fmt.Fprintf(&b, "held by policy, not answered: %s\n", termText(ref))
		heldDetails(&b, h)
		return b.String()
	}
	via := "through the calendar"
	if r.GetVia() == "email" {
		via = "by email to the organizer"
	}
	fmt.Fprintf(&b, "answered %s: %s, %s\n", termText(ref), answer, via)
	if s := r.GetSentRef(); s != "" {
		fmt.Fprintf(&b, "saved to Sent: %s\n", termText(s))
	}
	return b.String()
}

// eventCreatedText says where an event was created and when it takes place, or
// that it was created as a held draft event.
func eventCreatedText(loc *time.Location, r *chainmailv1.CreateEventResponse) string {
	var b strings.Builder
	e := r.GetEvent()
	if h := r.GetHeld(); h != nil {
		ref := h.GetEventRef()
		if ref == "" {
			ref = e.GetRef()
		}
		fmt.Fprintf(&b, "held by policy: created %s as a draft event; no invitations were sent\n", termText(ref))
		heldDetails(&b, h)
		return b.String()
	}
	fmt.Fprintf(&b, "created %s on %s %s: %s\n", termText(e.GetRef()), termText(e.GetAccount()), termText(e.GetCalendarId()), whenText(e, loc))
	return b.String()
}

// changedText says an edit or cancel was done, as verb, or that it was held.
func changedText(verb, ref string, h *chainmailv1.CalendarHeld) string {
	if h == nil {
		return verb + " " + termText(ref) + "\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "held by policy, not applied: %s\n", termText(ref))
	heldDetails(&b, h)
	return b.String()
}
