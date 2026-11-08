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
	"github.com/yaklabco/chainmail/internal/calstore"
)

// GetEvent returns one event: a single event, or a series master followed
// by its exceptions (cancelled instances among them). Google keeps a
// deleted event for a while; GetEvent returns it with Status CANCELLED.
// Every override carries its series's UID, which Google leaves out of
// cancelled instances.
func (s *Store) GetEvent(ctx context.Context, calID, providerID string) ([]calendar.Event, error) {
	data, err := s.do(ctx, call{method: "GET", path: eventPath(calID, providerID), limit: maxEventBytes})
	if err != nil {
		return nil, err
	}
	ev, a, err := decodeEvent(data)
	if err != nil {
		return nil, err
	}
	if ev.UID == "" && !ev.RecurrenceID.IsZero() {
		// One instance, asked for by its instance id.
		if ev.UID, err = s.seriesUID(ctx, calID, ev.ProviderID); err != nil {
			return nil, err
		}
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
			if ex.UID == "" {
				ex.UID = a.ICalUID
			}
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
// with If-Match etag. For an instance, etag may be the series master's
// current etag, as an instance with no exception of its own is known by
// it: the instance is then written with its own etag. A patch sends only the fields that differ from ev.Raw,
// or every field chainmail models when there is no Raw. It returns the event
// as Google stored it.
func (s *Store) Put(ctx context.Context, calID string, ev calendar.Event, etag string, mode calendar.Notify) (calendar.Event, error) {
	if etag == "" {
		return s.create(ctx, calID, ev, mode)
	}
	if ev.ProviderID == "" {
		return calendar.Event{}, fmt.Errorf("%w: event has no provider id", ErrRequest)
	}
	target, ifMatch := ev.ProviderID, etag
	if !ev.RecurrenceID.IsZero() {
		id, masterETag, err := s.instanceID(ctx, calID, ev.ProviderID, ev.RecurrenceID, ev.Raw)
		if err != nil {
			return calendar.Event{}, err
		}
		target = id
		if masterETag != "" && etag == masterETag {
			// The caller knows the instance only by its series's etag,
			// which still matches: write the instance with its own.
			if ifMatch, err = s.etagOf(ctx, calID, id); err != nil {
				return calendar.Event{}, err
			}
		}
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
		method: "PATCH", path: eventPath(calID, target), body: body, ifMatch: ifMatch,
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
		b["start"] = patchTime(ev.Start, ev.TZID, ev.AllDay)
	}
	if differs(sameZone && ev.End.Equal(old.End)) {
		b["end"] = patchTime(ev.End, ev.TZID, ev.AllDay)
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
// sendUpdates=all so Google tells the organizer. It sends a participant-only
// update (attendeesOmitted true with only the self attendee), so attendees
// Google left out of the event it read are kept. It reads the event first
// and patches with If-Match on the etag it read, so a change in between
// gives calstore.ErrConflict. When r.ETag is set, an event whose etag
// differs from it is calstore.ErrConflict before anything is written; for
// an instance, r.ETag may instead be the series master's current etag,
// as an instance with no exception of its own is known by that etag.
func (s *Store) Respond(ctx context.Context, calID, providerID string, r calendar.Response) (calendar.Event, error) {
	status := responseStatus(r.PartStat)
	if status == "" || r.PartStat == calendar.PartStatNeedsAction {
		return calendar.Event{}, fmt.Errorf("%w: unsupported participation status", ErrRequest)
	}
	if r.Self == "" {
		return calendar.Event{}, fmt.Errorf("%w: no attendee address", ErrRequest)
	}
	target, masterETag := providerID, ""
	if !r.RecurrenceID.IsZero() {
		id, mtag, err := s.instanceID(ctx, calID, providerID, r.RecurrenceID, nil)
		if err != nil {
			return calendar.Event{}, err
		}
		target, masterETag = id, mtag
	}
	data, err := s.do(ctx, call{method: "GET", path: eventPath(calID, target), limit: maxEventBytes})
	if err != nil {
		return calendar.Event{}, err
	}
	var cur struct {
		ETag             string           `json:"etag"`
		Attendees        []map[string]any `json:"attendees"`
		AttendeesOmitted bool             `json:"attendeesOmitted"`
	}
	if err := json.Unmarshal(data, &cur); err != nil {
		return calendar.Event{}, errBadResponse
	}
	if r.ETag != "" && cur.ETag != r.ETag && (masterETag == "" || r.ETag != masterETag) {
		// Not the version the caller checked, nor an instance of the
		// series version it checked.
		return calendar.Event{}, fmt.Errorf("%w: etag differs", calstore.ErrConflict)
	}
	idx := slices.IndexFunc(cur.Attendees, func(a map[string]any) bool {
		email, _ := a["email"].(string)
		return strings.EqualFold(email, r.Self)
	})
	var self map[string]any
	switch {
	case idx >= 0:
		self = cur.Attendees[idx] // keeps the fields chainmail does not model
	case cur.AttendeesOmitted:
		// Google may not show the participant of a large event; it matches
		// the update to the attendee by address.
		self = map[string]any{"email": r.Self}
	default:
		return calendar.Event{}, fmt.Errorf("%w: the account is not an attendee", ErrRequest)
	}
	self["responseStatus"] = status
	if r.Comment != "" {
		self["comment"] = r.Comment
	}
	// A participant-only update: PATCH replaces arrays, so sending the list
	// as read would drop every attendee Google left out of it.
	body := map[string]any{"attendees": []map[string]any{self}, "attendeesOmitted": true}
	return s.write(ctx, call{
		method: "PATCH", path: eventPath(calID, target), body: body,
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

// instanceID is the id of the instance rid of the series id. Google names an
// instance by the kind of its original start (originalStartTime), which is
// the series's kind, not by its current one: an exception moved from timed
// to all-day keeps its timed id. raw, the instance's JSON when the caller has
// it, gives the id directly; otherwise the master is read for its kind, and
// masterETag is its current etag (else "").
func (s *Store) instanceID(ctx context.Context, calID, id string, rid time.Time, raw []byte) (instID, masterETag string, err error) {
	if len(raw) > 0 {
		var a apiEvent
		if json.Unmarshal(raw, &a) == nil && a.ID != "" && a.RecurringEventID == id {
			if orig, _, _, err := parseTime(a.OriginalStartTime); err == nil && orig.Equal(rid) {
				return a.ID, "", nil
			}
		}
	}
	data, err := s.do(ctx, call{method: "GET", path: eventPath(calID, id), limit: maxEventBytes})
	if err != nil {
		return "", "", err
	}
	master, _, err := decodeEvent(data)
	if err != nil {
		return "", "", err
	}
	return InstanceID(id, rid, master.AllDay), master.ETag, nil
}

// etagOf reads the current etag of the event or instance id.
func (s *Store) etagOf(ctx context.Context, calID, id string) (string, error) {
	data, err := s.do(ctx, call{method: "GET", path: eventPath(calID, id), limit: maxEventBytes})
	if err != nil {
		return "", err
	}
	var cur struct {
		ETag string `json:"etag"`
	}
	if err := json.Unmarshal(data, &cur); err != nil || cur.ETag == "" {
		return "", errBadResponse
	}
	return cur.ETag, nil
}
