package caldav

import (
	"context"
	"fmt"
	"strings"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

// Respond sets r.Self's PARTSTAT, and adds r.Comment as a COMMENT, on the
// object's master or single event, or on the override of one instance,
// adding the override when the object has none. It reads the object
// first and PUTs it back with If-Match, so the server's scheduling sends
// the organizer a REPLY. A change on the server in between is ErrConflict.
func (s *Store) Respond(ctx context.Context, calID, providerID string, r calendar.Response) (calendar.Event, error) {
	_, href, err := objectPath(calID, providerID)
	if err != nil {
		return calendar.Event{}, err
	}
	switch r.PartStat {
	case calendar.PartStatAccepted, calendar.PartStatDeclined, calendar.PartStatTentative:
	default:
		return calendar.Event{}, fmt.Errorf("%w: bad PARTSTAT", calendar.ErrInvalidICS)
	}
	if strings.TrimSpace(r.Self) == "" {
		return calendar.Event{}, fmt.Errorf("%w: no address", ErrNotAttendee)
	}
	obj, err := s.getObject(ctx, href)
	if err != nil {
		return calendar.Event{}, err
	}
	o, err := decodeObject(obj.raw)
	if err != nil {
		return calendar.Event{}, err
	}
	i := o.find(r.RecurrenceID)
	if i < 0 {
		if r.RecurrenceID.IsZero() {
			return calendar.Event{}, fmt.Errorf("%w: no event in object", calstore.ErrNotFound)
		}
		m := -1
		for j, ev := range o.events {
			if ev.RecurrenceID.IsZero() {
				m = j
				break
			}
		}
		if m < 0 || !isInstance(o.events, r.RecurrenceID) {
			return calendar.Event{}, fmt.Errorf("%w: no such instance", calstore.ErrNotFound)
		}
		o.cal.Children = append(o.cal.Children, overrideFrom(o.comps[m], o.events[m], r.RecurrenceID))
		o.comps = append(o.comps, o.cal.Children[len(o.cal.Children)-1])
		i = len(o.comps) - 1
	}
	comp := o.comps[i]
	if !setPartStat(comp, r.Self, r.PartStat) {
		return calendar.Event{}, ErrNotAttendee
	}
	addComment(comp, r.Comment)
	stamp(comp, s.now())
	addTimezones(o.cal, s.now())
	body, err := encodeObject(o.cal)
	if err != nil {
		return calendar.Event{}, err
	}
	newTag, err := s.putObject(ctx, href, body, map[string]string{"If-Match": obj.etag}, opWrite)
	if err != nil {
		return calendar.Event{}, err
	}
	return s.stored(ctx, href, newTag, body, r.RecurrenceID)
}
