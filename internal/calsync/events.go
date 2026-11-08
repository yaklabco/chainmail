package calsync

import (
	"slices"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/calcache"
	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/watch"
)

// Changed field names of an event_changed event, in this order.
const (
	fieldStart      = "start"
	fieldEnd        = "end"
	fieldLocation   = "location"
	fieldRecurrence = "recurrence"
	fieldAttendees  = "attendees"
)

// seriesHorizon is how far ahead a series is searched for an instance when
// deciding whether it still goes on.
const seriesHorizon = 3650 * 24 * time.Hour

// eventInput is what calendarEvents needs to turn one calendar's diff into
// watch events.
type eventInput struct {
	account, calID string
	changes        []calcache.Change
	self           func(string) bool
	now            time.Time
	windowPast     time.Duration
	// master returns the stored row with no recurrence ID of a ProviderID
	// (the master or single event), or nil when there is none. It is asked
	// only about ProviderIDs whose base row is not in changes, at most once
	// each. Its error fails calendarEvents.
	master func(providerID string) (*calcache.EventRow, error)
}

// calendarEvents turns one calendar's diff into watch events:
//
//   - invite: a new row (or a row James was just added to) where James's
//     PARTSTAT is NEEDS-ACTION, he does not organize it, it is not
//     cancelled, and it ends after now. A new series gives one invite, not
//     one per override, when any instance of it as its overrides leave it
//     (moved or cancelled) asks for an answer and ends after now.
//   - event_changed: start, end, location, recurrence or the attendee set
//     changed on a row James attends or organizes, including a new override
//     that moves one instance of a stored series.
//   - event_cancelled: a row James attends, not organizes, became
//     CANCELLED, or its object was deleted, including one cancelled
//     instance of a series.
//   - invite_reply: an attendee other than James changed PARTSTAT on a row
//     James organizes.
//
// Rows that ended before now - windowPast give no events, so a sync that
// sends old rows again (after a prune or a full sync) stays quiet.
//
// A stored master that cannot be read fails the whole diff: without it a
// sparse override has no role, so its events would be lost.
func calendarEvents(in eventInput) ([]watch.Event, error) {
	b := eventBuilder{
		in: in, past: in.now.Add(-in.windowPast),
		base: map[string]*calcache.Change{}, overrides: map[string][]*calcache.EventRow{},
		stored: map[string]*calcache.EventRow{},
	}
	for i := range in.changes {
		c := &in.changes[i]
		row := changeRow(c)
		switch {
		case row.Event.RecurrenceID.IsZero():
			b.base[row.Event.ProviderID] = c
		case c.After != nil:
			b.overrides[row.Event.ProviderID] = append(b.overrides[row.Event.ProviderID], c.After)
		}
	}
	for i := range in.changes {
		c := &in.changes[i]
		switch c.Kind {
		case calcache.ChangeCreated:
			b.created(c.After)
		case calcache.ChangeUpdated:
			b.updated(c.Before, c.After)
		case calcache.ChangeDeleted:
			b.deleted(c.Before)
		}
		if b.err != nil {
			return nil, b.err
		}
	}
	return b.out, nil
}

type eventBuilder struct {
	in   eventInput
	past time.Time
	// base holds the change of each ProviderID's base row (the master or
	// the single event), when the diff has one.
	base map[string]*calcache.Change
	// overrides holds the overrides of each ProviderID as the diff leaves
	// them, deleted ones left out.
	overrides map[string][]*calcache.EventRow
	// stored holds the stored masters read so far, nil for none.
	stored map[string]*calcache.EventRow
	// err is the first failed read of a stored master.
	err error
	out []watch.Event
}

func changeRow(c *calcache.Change) *calcache.EventRow {
	if c.After != nil {
		return c.After
	}
	return c.Before
}

// masterOf returns the series context of row: the row itself for a base
// row, else its master as the diff leaves it, or as stored.
func (b *eventBuilder) masterOf(row *calcache.EventRow) *calcache.EventRow {
	if row.Event.RecurrenceID.IsZero() {
		return row
	}
	if c, ok := b.base[row.Event.ProviderID]; ok {
		return changeRow(c)
	}
	if b.in.master == nil || b.err != nil {
		return nil
	}
	pid := row.Event.ProviderID
	if m, ok := b.stored[pid]; ok {
		return m
	}
	m, err := b.in.master(pid)
	if err != nil {
		b.err = err
		return nil
	}
	b.stored[pid] = m
	return m
}

// baseCreated reports whether the diff creates row's base row too.
func (b *eventBuilder) baseCreated(row *calcache.EventRow) bool {
	c, ok := b.base[row.Event.ProviderID]
	return ok && c.Kind == calcache.ChangeCreated
}

// role is James's part in an event.
type role struct {
	organizer bool
	partStat  calendar.PartStat // "" when James is not an attendee
}

func (r role) involved() bool { return r.organizer || r.partStat != "" }

// attends reports whether James attends and does not organize.
func (r role) attends() bool { return !r.organizer && r.partStat != "" }

// roleOf is James's part in row. An override that names no organizer and
// no attendees, such as a cancelled instance, takes its series' part.
func (b *eventBuilder) roleOf(row *calcache.EventRow) role {
	r := role{organizer: row.IsOrganizer, partStat: row.MyPartStat}
	if !row.Event.RecurrenceID.IsZero() && row.Event.Organizer == nil && len(row.Event.Attendees) == 0 {
		if m := b.masterOf(row); m != nil {
			r = role{organizer: m.IsOrganizer, partStat: m.MyPartStat}
		}
	}
	return r
}

func (b *eventBuilder) created(row *calcache.EventRow) {
	override := !row.Event.RecurrenceID.IsZero()
	if override && b.baseCreated(row) {
		// A new series is decided once, by its base row.
		return
	}
	if !override && b.isSeries(row) {
		b.newSeries(row)
		return
	}
	r := b.roleOf(row)
	if row.Event.Status == calendar.StatusCancelled {
		// A new cancelled override is one cancelled instance.
		if override && r.attends() && b.endsAfter(row, b.past) {
			b.cancelled(row)
		}
		return
	}
	if row.MyPartStat == calendar.PartStatNeedsAction && !r.organizer && b.endsAfter(row, b.in.now) {
		b.invite(row)
		return
	}
	if !override || !r.involved() || !b.endsAfter(row, b.past) {
		return
	}
	// A new override of a stored series: what it changed from the
	// instance the master gives.
	m := b.masterOf(row)
	if m == nil {
		return
	}
	if fields := overrideFields(row, m); len(fields) > 0 {
		b.changed(row, fields)
	}
	if r.organizer {
		b.replies(row, m.Event.Attendees, row.Event.Attendees)
	}
}

// isSeries reports whether the base row m has more than one instance: a
// rule, RDATEs, or overrides in the diff.
func (b *eventBuilder) isSeries(m *calcache.EventRow) bool {
	return m.Event.RRule != "" || len(m.Event.RDates) > 0 || len(b.overrides[m.Event.ProviderID]) > 0
}

// needsReply reports whether row asks James for an answer: it is not
// cancelled, he does not organize it, and his PARTSTAT is NEEDS-ACTION.
func (b *eventBuilder) needsReply(row *calcache.EventRow) bool {
	r := b.roleOf(row)
	return row.Event.Status != calendar.StatusCancelled && !r.organizer && r.partStat == calendar.PartStatNeedsAction
}

// newSeries gives the invite of a series the diff creates, decided on the
// series as its master and overrides make it: one invite when an instance
// James must answer ends after now, on the master when it asks for an
// answer, else on that override. A cancelled override removes its instance
// and a moved one counts where it now is. Nothing else is reported for a
// new series.
func (b *eventBuilder) newSeries(m *calcache.EventRow) {
	if m.Event.Status == calendar.StatusCancelled {
		return
	}
	now := b.in.now
	overrides := b.overrides[m.Event.ProviderID]
	series := make([]calendar.Event, 0, len(overrides)+1)
	series = append(series, m.Event)
	for _, o := range overrides {
		series = append(series, placed(o.Event, m.Event))
	}
	masterAsks := b.needsReply(m)
	// Every override in the range is an instance, so the first
	// len(overrides)+1 instances hold one the master gives, when there is
	// one.
	insts, _, err := calendar.Expand(series, now, now.Add(seriesHorizon), len(overrides)+1)
	if err != nil {
		// A rule chainmail does not expand still counts as going on.
		if masterAsks {
			b.invite(m)
		}
		return
	}
	if masterAsks && slices.ContainsFunc(insts, func(in calendar.Instance) bool { return in.Event.RecurrenceID.IsZero() }) {
		b.invite(m)
		return
	}
	for i, o := range overrides {
		if !b.needsReply(o) || !series[i+1].End.After(now) {
			continue
		}
		if masterAsks {
			b.invite(m)
		} else {
			b.invite(o)
		}
		return
	}
}

// placed returns the override o of master m with a sparse override's start
// and end filled in from its recurrence ID and the master's length.
func placed(o, m calendar.Event) calendar.Event {
	if o.Start.IsZero() {
		o.Start = o.RecurrenceID
		o.End = o.RecurrenceID.Add(max(m.End.Sub(m.Start), 0))
	}
	return o
}

func (b *eventBuilder) updated(before, after *calcache.EventRow) {
	r := b.roleOf(after)
	if !r.involved() {
		r = b.roleOf(before)
	}
	wasCancelled := before.Event.Status == calendar.StatusCancelled
	if after.Event.Status == calendar.StatusCancelled {
		if !wasCancelled && r.attends() && b.endsAfter(after, b.past) {
			b.cancelled(after)
		}
		return
	}
	if !after.IsOrganizer && after.MyPartStat == calendar.PartStatNeedsAction && before.MyPartStat == "" && b.endsAfter(after, b.in.now) {
		// James was added to an event he could already see.
		b.invite(after)
		return
	}
	if !r.involved() || !b.endsAfter(after, b.past) {
		return
	}
	if fields := changedFields(before.Event, after.Event); len(fields) > 0 {
		b.changed(after, fields)
	}
	if r.organizer {
		b.replies(after, before.Event.Attendees, after.Event.Attendees)
	}
}

func (b *eventBuilder) deleted(row *calcache.EventRow) {
	// An override removed from a series that stays is an exception
	// undone, not a cancellation; a deleted series is reported once, by
	// its base row.
	if !row.Event.RecurrenceID.IsZero() {
		return
	}
	if row.Event.Status != calendar.StatusCancelled && b.roleOf(row).attends() && b.endsAfter(row, b.past) {
		b.cancelled(row)
	}
}

func (b *eventBuilder) emit(kind watch.Kind, row *calcache.EventRow, c watch.CalendarChange) {
	c.Ref, c.CalendarID = row.Ref, b.in.calID
	b.out = append(b.out, watch.Event{Kind: kind, Account: b.in.account, Ref: row.Ref, Calendar: &c})
}

func (b *eventBuilder) invite(row *calcache.EventRow) {
	c := watch.CalendarChange{Start: rfc3339(row.Event.Start), End: rfc3339(row.Event.End)}
	if o := row.Event.Organizer; o != nil {
		c.Organizer = o.Email
	}
	b.emit(watch.KindInvite, row, c)
}

func (b *eventBuilder) changed(row *calcache.EventRow, fields []string) {
	b.emit(watch.KindEventChanged, row, watch.CalendarChange{
		Fields: fields, Start: rfc3339(row.Event.Start), End: rfc3339(row.Event.End),
	})
}

func (b *eventBuilder) cancelled(row *calcache.EventRow) {
	start := row.Event.Start
	if start.IsZero() {
		start = row.Event.RecurrenceID
	}
	b.emit(watch.KindEventCancelled, row, watch.CalendarChange{Start: rfc3339(start)})
}

// replies emits an invite_reply for each attendee other than James whose
// PARTSTAT differs between before and after.
func (b *eventBuilder) replies(row *calcache.EventRow, before, after []calendar.Attendee) {
	was := make(map[string]calendar.PartStat, len(before))
	for _, a := range before {
		was[strings.ToLower(a.Email)] = a.PartStat
	}
	for _, a := range after {
		key := strings.ToLower(a.Email)
		old, ok := was[key]
		if !ok || old == a.PartStat || a.Email == "" || b.in.self(a.Email) {
			continue
		}
		b.emit(watch.KindInviteReply, row, watch.CalendarChange{Attendee: a.Email, PartStat: string(a.PartStat)})
	}
}

// endsAfter reports whether row ends after t: for a series master, whether
// any instance does.
func (b *eventBuilder) endsAfter(row *calcache.EventRow, t time.Time) bool {
	ev := row.Event
	if ev.RecurrenceID.IsZero() && (ev.RRule != "" || len(ev.RDates) > 0) {
		insts, _, err := calendar.Expand([]calendar.Event{ev}, t, t.Add(seriesHorizon), 1)
		// A rule chainmail does not expand still counts as going on.
		return err != nil || len(insts) > 0
	}
	end := ev.End
	if ev.Start.IsZero() && !ev.RecurrenceID.IsZero() {
		// A sparse override: its instance starts at its recurrence ID.
		end = ev.RecurrenceID
		if m := b.masterOf(row); m != nil {
			end = end.Add(m.Event.End.Sub(m.Event.Start))
		}
	}
	return end.After(t)
}

// changedFields names what changed between two rows of the same event.
func changedFields(before, after calendar.Event) []string {
	var out []string
	if !before.Start.Equal(after.Start) {
		out = append(out, fieldStart)
	}
	if !before.End.Equal(after.End) {
		out = append(out, fieldEnd)
	}
	if before.Location != after.Location {
		out = append(out, fieldLocation)
	}
	if before.RRule != after.RRule || !sameTimes(before.RDates, after.RDates) || !sameTimes(before.ExDates, after.ExDates) {
		out = append(out, fieldRecurrence)
	}
	if !sameAttendees(before.Attendees, after.Attendees) {
		out = append(out, fieldAttendees)
	}
	return out
}

// overrideFields names what a new override changes from the instance of
// its master it replaces. An override that lists no attendees keeps the
// master's.
func overrideFields(o, m *calcache.EventRow) []string {
	rid := o.Event.RecurrenceID
	var out []string
	if !o.Event.Start.IsZero() && !o.Event.Start.Equal(rid) {
		out = append(out, fieldStart)
	}
	if end := rid.Add(m.Event.End.Sub(m.Event.Start)); !o.Event.End.IsZero() && !o.Event.End.Equal(end) {
		out = append(out, fieldEnd)
	}
	if o.Event.Location != m.Event.Location {
		out = append(out, fieldLocation)
	}
	if len(o.Event.Attendees) > 0 && !sameAttendees(o.Event.Attendees, m.Event.Attendees) {
		out = append(out, fieldAttendees)
	}
	return out
}

func sameTimes(a, b []time.Time) bool {
	return slices.EqualFunc(a, b, time.Time.Equal)
}

// sameAttendees reports whether two lists name the same addresses,
// whatever their order, case or PARTSTATs.
func sameAttendees(a, b []calendar.Attendee) bool {
	set := func(list []calendar.Attendee) []string {
		out := make([]string, 0, len(list))
		for _, x := range list {
			out = append(out, strings.ToLower(x.Email))
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(set(a), set(b))
}

// rfc3339 formats t in UTC, "" for the zero time.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
