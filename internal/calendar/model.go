package calendar

import (
	"errors"
	"time"
)

// Parsing and expansion limits.
const (
	// MaxICSBytes bounds one iCalendar object, in mail or from a server.
	MaxICSBytes = 1 << 20
	// MaxAttendees bounds the attendees kept per event. The rest are
	// dropped and the event is flagged AttendeesTruncated.
	MaxAttendees = 500
	// MaxInstances bounds the instances one series expands to per query.
	MaxInstances = 1000
)

// Errors. Their texts are fixed; wrapped forms add only field names.
var (
	ErrTooLarge         = errors.New("calendar object too large")
	ErrInvalidICS       = errors.New("invalid calendar data")
	ErrInvalidRef       = errors.New("invalid event reference")
	ErrUnsafeRecurrence = errors.New("recurrence rule not expanded")
	ErrNotInvite        = errors.New("not a calendar invite")
)

// Access is what chainmail may do on a calendar.
type Access string

// Access values.
const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
)

// PartStat is an attendee's participation status (RFC 5545 PARTSTAT).
type PartStat string

// PartStat values for events.
const (
	PartStatNeedsAction PartStat = "NEEDS-ACTION"
	PartStatAccepted    PartStat = "ACCEPTED"
	PartStatDeclined    PartStat = "DECLINED"
	PartStatTentative   PartStat = "TENTATIVE"
	PartStatDelegated   PartStat = "DELEGATED"
)

// Status is an event's STATUS. An event without one has Status "".
type Status string

// Status values.
const (
	StatusConfirmed Status = "CONFIRMED"
	StatusTentative Status = "TENTATIVE"
	StatusCancelled Status = "CANCELLED"
)

// iTIP methods chainmail reads or writes.
const (
	MethodRequest = "REQUEST"
	MethodUpdate  = "UPDATE"
	MethodCancel  = "CANCEL"
	MethodReply   = "REPLY"
)

// Person is a calendar user. Name is untrusted.
type Person struct{ Email, Name string }

// Attendee is one ATTENDEE of an event.
type Attendee struct {
	Person
	PartStat PartStat
	Role     string // REQ-PARTICIPANT, OPT-PARTICIPANT, CHAIR, NON-PARTICIPANT; "" when absent
	RSVP     bool
}

// Calendar is one calendar of an account.
type Calendar struct {
	ID       string
	Name     string // untrusted
	Access   Access
	Primary  bool
	TimeZone string // IANA, "" when unknown
}

// Event is one VEVENT: a single event, a series master (RRule set,
// RecurrenceID zero) or an override (RecurrenceID set). Overrides share the
// master's ProviderID and UID.
type Event struct {
	ProviderID         string // Google: the series or event id; CalDAV: the object href path
	ETag               string
	UID                string
	RecurrenceID       time.Time // UTC, zero for singles and masters
	Summary            string    // untrusted
	Location           string    // untrusted
	Description        string    // untrusted
	Start, End         time.Time // UTC; all-day: 00:00 UTC of the dates, End exclusive
	TZID               string    // IANA zone of Start, "" for UTC or floating
	AllDay             bool
	Status             Status
	Sequence           int
	Transparent        bool
	Organizer          *Person
	Attendees          []Attendee
	AttendeesTruncated bool
	RRule              string      // RRULE value without the "RRULE:" prefix
	RDates, ExDates    []time.Time // UTC
	Updated            time.Time
	Raw                []byte // provider object: the whole VCALENDAR for CalDAV, the event JSON for Google
}

// IsMaster reports whether e is the master of a recurring series.
func (e Event) IsMaster() bool { return e.RRule != "" && e.RecurrenceID.IsZero() }

// Changes is one Sync result. When ObjectReplace is true (CalDAV), the
// upserts of each ProviderID are the complete set of rows for that object and
// replace every stored row with that ProviderID. When false (Google), each
// upsert replaces only the row with the same ProviderID and RecurrenceID.
// Deletes are ProviderIDs; deleting one removes all its rows. Full means the
// token was empty or invalid and Upserts is the whole calendar.
type Changes struct {
	Upserts       []Event
	Deletes       []string
	Token         string
	Full          bool
	ObjectReplace bool
}

// BusyBlock is one busy interval, [Start, End).
type BusyBlock struct{ Start, End time.Time }

// FreeBusy is one person's busy time. Known is false when the server could
// not tell, such as for a person outside the account's domain.
type FreeBusy struct {
	Known bool
	Busy  []BusyBlock
}

// Notify says whether a write sends invitations or cancellations.
type Notify int

// Notify values.
const (
	NotifySend Notify = iota + 1
	NotifyNone
)

// Response is an RSVP by Self (an address of the account) to a series
// (RecurrenceID zero) or one instance.
type Response struct {
	Self         string
	PartStat     PartStat
	Comment      string // untrusted once stored
	RecurrenceID time.Time
	// ETag, when set, is the etag of the version the caller checked (its
	// organizer, its access and the policy), sent as If-Match: a store
	// whose current version has another etag answers
	// calstore.ErrConflict and writes nothing. For an instance with no
	// override of its own it may be the series master's etag, which the
	// store checks against the master before writing the instance.
	ETag string
}

// Instance is one occurrence. Event is the master or the override it comes from.
type Instance struct {
	Event        Event
	Start, End   time.Time
	RecurrenceID time.Time // zero for a single event
}
