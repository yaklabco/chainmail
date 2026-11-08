package caldav

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/emersion/go-ical"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

const icsContentType = "text/calendar; charset=utf-8"

// errNoETag is a replace or delete without the etag the caller read.
var errNoETag = fmt.Errorf("%w: etag required", calstore.ErrConflict)

// Put creates ev at <calendar>/<PathEscape(UID)>.ics with If-None-Match: *
// when etag is "", or replaces the object ev.ProviderID with If-Match etag.
// A replace patches the stored VCALENDAR (ev.Raw, or the object read again
// when Raw is empty): only the properties of ev's VEVENT that changed are
// rewritten, and other components and properties are kept. A VEVENT for a
// recurrence ID the object lacks is added as an override. NotifyNone sets
// SCHEDULE-AGENT=CLIENT on every attendee, so the server sends nothing.
// A VTIMEZONE is added for every IANA TZID the object uses without one.
// It returns the stored event, read back from the server.
func (s *Store) Put(ctx context.Context, calID string, ev calendar.Event, etag string, mode calendar.Notify) (calendar.Event, error) {
	coll, ok := collectionPath(calID)
	if !ok {
		return calendar.Event{}, errBadID
	}
	if etag == "" {
		return s.create(ctx, coll, ev, mode)
	}
	_, href, err := objectPath(coll, ev.ProviderID)
	if err != nil {
		return calendar.Event{}, err
	}
	return s.replace(ctx, href, ev, etag, mode)
}

func (s *Store) create(ctx context.Context, coll string, ev calendar.Event, mode calendar.Notify) (calendar.Event, error) {
	if ev.UID == "" {
		return calendar.Event{}, fmt.Errorf("%w: missing UID", calendar.ErrInvalidICS)
	}
	href := coll + url.PathEscape(ev.UID) + ".ics"
	if !childOf(coll, href) {
		return calendar.Event{}, fmt.Errorf("%w: bad UID", calendar.ErrInvalidICS)
	}
	ev.Updated = s.now()
	o, err := newObject(ev)
	if err != nil {
		return calendar.Event{}, err
	}
	addTimezones(o.cal, s.now())
	if mode == calendar.NotifyNone {
		scheduleAgentClient(o.cal)
	}
	body, err := encodeObject(o.cal)
	if err != nil {
		return calendar.Event{}, err
	}
	etag, err := s.putObject(ctx, href, body, map[string]string{"If-None-Match": "*"}, opCreate)
	if err != nil {
		return calendar.Event{}, err
	}
	return s.stored(ctx, href, etag, body, ev.RecurrenceID)
}

func (s *Store) replace(ctx context.Context, href string, ev calendar.Event, etag string, mode calendar.Notify) (calendar.Event, error) {
	raw := ev.Raw
	if len(raw) == 0 {
		obj, err := s.getObject(ctx, href)
		if err != nil {
			return calendar.Event{}, err
		}
		if obj.etag != etag {
			return calendar.Event{}, fmt.Errorf("%w: etag differs", calstore.ErrConflict)
		}
		raw = obj.raw
	}
	o, err := decodeObject(raw)
	if err != nil {
		return calendar.Event{}, err
	}
	now := s.now()
	if i := o.find(ev.RecurrenceID); i >= 0 {
		if o.events[i].UID != ev.UID {
			return calendar.Event{}, fmt.Errorf("%w: UID differs from the stored object", calendar.ErrInvalidICS)
		}
		if err := patch(o.comps[i], o.events[i], ev, now); err != nil {
			return calendar.Event{}, err
		}
	} else {
		if ev.RecurrenceID.IsZero() || len(o.events) == 0 || o.events[0].UID != ev.UID {
			return calendar.Event{}, fmt.Errorf("%w: no such event in the object", calstore.ErrNotFound)
		}
		ev.Updated = now
		comp, err := eventComponent(ev)
		if err != nil {
			return calendar.Event{}, err
		}
		o.cal.Children = append(o.cal.Children, comp)
	}
	addTimezones(o.cal, now)
	if mode == calendar.NotifyNone {
		scheduleAgentClient(o.cal)
	}
	body, err := encodeObject(o.cal)
	if err != nil {
		return calendar.Event{}, err
	}
	newTag, err := s.putObject(ctx, href, body, map[string]string{"If-Match": etag}, opWrite)
	if err != nil {
		return calendar.Event{}, err
	}
	return s.stored(ctx, href, newTag, body, ev.RecurrenceID)
}

// putObject PUTs a calendar object and returns the new etag, "" when the
// server sent none.
func (s *Store) putObject(ctx context.Context, href string, body []byte, cond map[string]string, op opKind) (string, error) {
	resp, err := s.do(ctx, request{
		method:      http.MethodPut,
		path:        href,
		body:        body,
		contentType: icsContentType,
		header:      cond,
	})
	if err != nil {
		return "", err
	}
	if resp.status/100 != 2 {
		return "", statusError(op, resp.status, resp.body)
	}
	return normalizeETag(resp.header.Get("ETag")), nil
}

// stored reads an object back after a write, since servers add properties
// such as SCHEDULE-STATUS, and returns its event with recurrence ID rid.
// When the read fails the event is taken from what was sent, with the etag
// the PUT returned.
func (s *Store) stored(ctx context.Context, href, etag string, sent []byte, rid time.Time) (calendar.Event, error) {
	obj, err := s.getObject(ctx, href)
	if err != nil {
		if ctx.Err() != nil {
			return calendar.Event{}, ctx.Err()
		}
		obj = &storedObject{href: href, etag: etag, raw: sent}
	}
	evs, err := obj.events()
	if err != nil {
		return calendar.Event{}, err
	}
	for _, e := range evs {
		if e.RecurrenceID.Equal(rid) {
			return e, nil
		}
	}
	return calendar.Event{}, fmt.Errorf("%w: event missing after write", calstore.ErrNotFound)
}

// Delete removes an object with If-Match etag. With NotifyNone it first
// rewrites the object with SCHEDULE-AGENT=CLIENT on every attendee, when
// any lacks it, and sends Schedule-Reply: F, so the server sends neither a
// CANCEL nor a REPLY (RFC 6638 sections 7.1 and 8.1).
func (s *Store) Delete(ctx context.Context, calID, providerID, etag string, mode calendar.Notify) error {
	_, href, err := objectPath(calID, providerID)
	if err != nil {
		return err
	}
	if etag == "" {
		return errNoETag
	}
	header := map[string]string{"If-Match": etag}
	if mode == calendar.NotifyNone {
		if etag, err = s.silence(ctx, href, etag); err != nil {
			return err
		}
		header = map[string]string{"If-Match": etag, "Schedule-Reply": "F"}
	}
	resp, err := s.do(ctx, request{method: http.MethodDelete, path: href, header: header})
	if err != nil {
		return err
	}
	if resp.status/100 != 2 {
		return statusError(opWrite, resp.status, resp.body)
	}
	return nil
}

// silence rewrites an object so that no attendee is scheduled by the
// server, and returns its new etag. An object already so is left alone.
func (s *Store) silence(ctx context.Context, href, etag string) (string, error) {
	obj, err := s.getObject(ctx, href)
	if err != nil {
		return "", err
	}
	if obj.etag != etag {
		return "", fmt.Errorf("%w: etag differs", calstore.ErrConflict)
	}
	o, err := decodeObject(obj.raw)
	if err != nil {
		return "", err
	}
	cal := o.cal
	if !hasServerScheduledAttendee(cal) {
		return etag, nil
	}
	scheduleAgentClient(cal)
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent {
			stamp(c, s.now())
		}
	}
	body, err := encodeObject(cal)
	if err != nil {
		return "", err
	}
	newTag, err := s.putObject(ctx, href, body, map[string]string{"If-Match": etag}, opWrite)
	if err != nil {
		return "", err
	}
	if newTag == "" {
		newTag, err = s.etagOf(ctx, href)
		if err != nil {
			return "", err
		}
	}
	return newTag, nil
}
