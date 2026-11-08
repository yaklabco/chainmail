package calendar

import (
	"fmt"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// BuildReply builds an iTIP METHOD:REPLY for ev (the master, or the
// override when ev.RecurrenceID is set) carrying only the ATTENDEE self with
// ps, the ORGANIZER, UID, SEQUENCE, RECURRENCE-ID when set, DTSTAMP now and
// COMMENT when non-empty.
//
// DTSTART and DTEND are kept too, since some clients match replies on
// them. No other event text is copied. The attendee keeps the CN ev gives
// self, matched without regard to case. ps must be ACCEPTED, DECLINED or
// TENTATIVE; other values, a missing organizer, UID or self, and a self
// address with a control character return ErrInvalidICS.
func BuildReply(ev Event, self string, ps PartStat, comment string, now time.Time) ([]byte, error) {
	switch ps {
	case PartStatAccepted, PartStatDeclined, PartStatTentative:
	default:
		return nil, fmt.Errorf("%w: reply PARTSTAT must be ACCEPTED, DECLINED or TENTATIVE", ErrInvalidICS)
	}
	if ev.Organizer == nil || ev.Organizer.Email == "" {
		return nil, fmt.Errorf("%w: no ORGANIZER", ErrInvalidICS)
	}
	if self == "" {
		return nil, fmt.Errorf("%w: no ATTENDEE", ErrInvalidICS)
	}
	att := Attendee{Person: Person{Email: self}, PartStat: ps}
	for _, a := range ev.Attendees {
		if strings.EqualFold(a.Email, self) {
			att.Name = a.Name
			break
		}
	}
	reply := Event{
		UID:          ev.UID,
		RecurrenceID: ev.RecurrenceID,
		Start:        ev.Start,
		End:          ev.End,
		TZID:         ev.TZID,
		AllDay:       ev.AllDay,
		Sequence:     ev.Sequence,
		Organizer:    ev.Organizer,
		Attendees:    []Attendee{att},
	}
	comp, err := encodeEvent(reply, now)
	if err != nil {
		return nil, err
	}
	setText(comp.Props, ical.PropComment, comment)
	cal, err := newCalendar(MethodReply)
	if err != nil {
		return nil, err
	}
	cal.Children = append(cal.Children, comp)
	return encodeCalendar(cal)
}
