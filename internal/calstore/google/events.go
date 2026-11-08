package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// GetEvent returns one event: a single event, or a series master followed
// by its exceptions (cancelled instances among them). Google keeps a
// deleted event for a while; GetEvent returns it with Status CANCELLED.
func (s *Store) GetEvent(ctx context.Context, calID, providerID string) ([]calendar.Event, error) {
	data, err := s.do(ctx, call{method: "GET", path: eventPath(calID, providerID), limit: maxEventBytes})
	if err != nil {
		return nil, err
	}
	ev, a, err := decodeEvent(data)
	if err != nil {
		return nil, err
	}
	out := []calendar.Event{ev}
	if !ev.IsMaster() || a.ICalUID == "" {
		return out, nil
	}
	// The exceptions of a series share its iCalUID.
	q := listQuery()
	q.Set("iCalUID", a.ICalUID)
	err = s.listEvents(ctx, calID, q, false, func(ex calendar.Event, xa apiEvent) {
		if xa.RecurringEventID == providerID {
			out = append(out, ex)
		}
	}, func(string) {})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Put creates ev when etag is "", with the id ObjectID(ev.UID), or patches
// the event ev.ProviderID (the instance ev.RecurrenceID names, when set)
// with If-Match etag. A patch sends only the fields that differ from ev.Raw,
// or every field chainmail models when there is no Raw. It returns the event
// as Google stored it.
func (s *Store) Put(ctx context.Context, calID string, ev calendar.Event, etag string, mode calendar.Notify) (calendar.Event, error) {
	if etag == "" {
		return s.create(ctx, calID, ev, mode)
	}
	if ev.ProviderID == "" {
		return calendar.Event{}, fmt.Errorf("%w: event has no provider id", ErrRequest)
	}
	target := ev.ProviderID
	if !ev.RecurrenceID.IsZero() {
		target = InstanceID(ev.ProviderID, ev.RecurrenceID, ev.AllDay)
	}
	var old *calendar.Event
	if len(ev.Raw) > 0 {
		if o, _, err := decodeEvent(ev.Raw); err == nil {
			old = &o
		}
	}
	body, err := patchBody(ev, old, rawAttendees(ev.Raw))
	if err != nil {
		return calendar.Event{}, err
	}
	return s.write(ctx, call{
		method: "PATCH", path: eventPath(calID, target), body: body, ifMatch: etag,
		query: sendQuery(mode), write: true,
	})
}

func (s *Store) create(ctx context.Context, calID string, ev calendar.Event, mode calendar.Notify) (calendar.Event, error) {
	if ev.UID == "" {
		return calendar.Event{}, fmt.Errorf("%w: event has no UID", ErrRequest)
	}
	if !ev.RecurrenceID.IsZero() {
		return calendar.Event{}, fmt.Errorf("%w: an override cannot be created", ErrRequest)
	}
	body := map[string]any{
		"id":    ObjectID(ev.UID),
		"start": eventTime(ev.Start, ev.TZID, ev.AllDay),
		"end":   eventTime(ev.End, ev.TZID, ev.AllDay),
	}
	for k, v := range map[string]string{"summary": ev.Summary, "description": ev.Description, "location": ev.Location} {
		if v != "" {
			body[k] = v
		}
	}
	if st := apiStatus(ev.Status); st != "" && ev.Status != calendar.StatusCancelled {
		body["status"] = st
	}
	if ev.Transparent {
		body["transparency"] = "transparent"
	}
	if len(ev.Attendees) > 0 {
		body["attendees"] = attendeesJSON(ev.Attendees, nil)
	}
	rec, err := recurrenceLines(ev)
	if err != nil {
		return calendar.Event{}, err
	}
	if len(rec) > 0 {
		body["recurrence"] = rec
	}
	return s.write(ctx, call{
		method: "POST", path: eventsPath(calID), body: body, query: sendQuery(mode), write: true, create: true,
	})
}

// patchBody is the PATCH of the fields of ev that differ from old, or of
// every field chainmail models when old is nil. known holds what Google
// knows of each attendee already on the event.
func patchBody(ev calendar.Event, old *calendar.Event, known map[string]map[string]any) (map[string]any, error) {
	b := map[string]any{}
	differs := func(same bool) bool { return old == nil || !same }
	if differs(old != nil && ev.Summary == old.Summary) {
		b["summary"] = ev.Summary
	}
	if differs(old != nil && ev.Description == old.Description) {
		b["description"] = ev.Description
	}
	if differs(old != nil && ev.Location == old.Location) {
		b["location"] = ev.Location
	}
	sameZone := old != nil && ev.AllDay == old.AllDay && ev.TZID == old.TZID
	if differs(sameZone && ev.Start.Equal(old.Start)) {
		b["start"] = eventTime(ev.Start, ev.TZID, ev.AllDay)
	}
	if differs(sameZone && ev.End.Equal(old.End)) {
		b["end"] = eventTime(ev.End, ev.TZID, ev.AllDay)
	}
	if differs(old != nil && ev.Transparent == old.Transparent) {
		b["transparency"] = "opaque"
		if ev.Transparent {
			b["transparency"] = "transparent"
		}
	}
	if st := apiStatus(ev.Status); st != "" && differs(old != nil && ev.Status == old.Status) {
		b["status"] = st
	}
	if differs(old != nil && sameAttendees(ev.Attendees, old.Attendees)) {
		if ev.AttendeesTruncated {
			return nil, fmt.Errorf("%w: the attendee list is incomplete", ErrRequest)
		}
		b["attendees"] = attendeesJSON(ev.Attendees, known)
	}
	if ev.RecurrenceID.IsZero() && differs(old != nil && sameRecurrence(ev, *old)) {
		rec, err := recurrenceLines(ev)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			b["recurrence"] = nil // JSON null removes the recurrence
		} else {
			b["recurrence"] = rec
		}
	}
	return b, nil
}

func sameAttendees(a, b []calendar.Attendee) bool {
	return slices.EqualFunc(a, b, func(x, y calendar.Attendee) bool {
		return strings.EqualFold(x.Email, y.Email) && x.Name == y.Name && x.PartStat == y.PartStat && x.Role == y.Role
	})
}

func sameRecurrence(a, b calendar.Event) bool {
	return a.RRule == b.RRule &&
		slices.EqualFunc(a.RDates, b.RDates, time.Time.Equal) &&
		slices.EqualFunc(a.ExDates, b.ExDates, time.Time.Equal)
}

// Respond sets r.Self's responseStatus, and comment when r.Comment is set,
// on the event or on the instance r.RecurrenceID names, with
// sendUpdates=all so Google tells the organizer. It reads the event first
// and patches with If-Match on the etag it read, so a change in between
// gives calstore.ErrConflict.
func (s *Store) Respond(ctx context.Context, calID, providerID string, r calendar.Response) (calendar.Event, error) {
	status := responseStatus(r.PartStat)
	if status == "" || r.PartStat == calendar.PartStatNeedsAction {
		return calendar.Event{}, fmt.Errorf("%w: unsupported participation status", ErrRequest)
	}
	if r.Self == "" {
		return calendar.Event{}, fmt.Errorf("%w: no attendee address", ErrRequest)
	}
	target := providerID
	if !r.RecurrenceID.IsZero() {
		data, err := s.do(ctx, call{method: "GET", path: eventPath(calID, providerID), limit: maxEventBytes})
		if err != nil {
			return calendar.Event{}, err
		}
		master, _, err := decodeEvent(data)
		if err != nil {
			return calendar.Event{}, err
		}
		target = InstanceID(providerID, r.RecurrenceID, master.AllDay)
	}
	data, err := s.do(ctx, call{method: "GET", path: eventPath(calID, target), limit: maxEventBytes})
	if err != nil {
		return calendar.Event{}, err
	}
	var cur struct {
		ETag      string           `json:"etag"`
		Attendees []map[string]any `json:"attendees"`
	}
	if err := json.Unmarshal(data, &cur); err != nil {
		return calendar.Event{}, errBadResponse
	}
	self := slices.IndexFunc(cur.Attendees, func(a map[string]any) bool {
		email, _ := a["email"].(string)
		return strings.EqualFold(email, r.Self)
	})
	if self < 0 {
		return calendar.Event{}, fmt.Errorf("%w: the account is not an attendee", ErrRequest)
	}
	cur.Attendees[self]["responseStatus"] = status
	if r.Comment != "" {
		cur.Attendees[self]["comment"] = r.Comment
	}
	return s.write(ctx, call{
		method: "PATCH", path: eventPath(calID, target), body: map[string]any{"attendees": cur.Attendees},
		ifMatch: cur.ETag, query: sendQuery(calendar.NotifySend), write: true,
	})
}

// Delete deletes the event or instance providerID, with If-Match etag when
// etag is set.
func (s *Store) Delete(ctx context.Context, calID, providerID, etag string, mode calendar.Notify) error {
	_, err := s.do(ctx, call{
		method: "DELETE", path: eventPath(calID, providerID), ifMatch: etag,
		query: sendQuery(mode), write: true, limit: maxEventBytes,
	})
	return err
}

// write sends a create or patch and decodes the event Google returns.
func (s *Store) write(ctx context.Context, c call) (calendar.Event, error) {
	c.limit = maxEventBytes
	data, err := s.do(ctx, c)
	if err != nil {
		return calendar.Event{}, err
	}
	ev, _, err := decodeEvent(data)
	return ev, err
}

func sendQuery(mode calendar.Notify) url.Values {
	return url.Values{"sendUpdates": {sendUpdates(mode)}}
}
