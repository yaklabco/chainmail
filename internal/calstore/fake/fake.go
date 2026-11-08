// Package fake is an in-memory calstore.Store for tests. It keeps
// calendars and their objects (a single event, or a series master with its
// overrides), hands out change tokens, logs every call, and fails calls on
// request. Its helpers change events as an organizer or another attendee
// would, so a syncer sees the changes on its next Sync.
//
// The helpers panic when told to change a calendar or object the fake does
// not have, as such a call is a bug in the test.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

// Method names, for Fail, FailNext, CallsTo and Count.
const (
	MethodListCalendars = "ListCalendars"
	MethodSync          = "Sync"
	MethodGetEvent      = "GetEvent"
	MethodFreeBusy      = "FreeBusy"
	MethodPut           = "Put"
	MethodRespond       = "Respond"
	MethodDelete        = "Delete"
)

// Options configure New.
type Options struct {
	// ObjectReplace makes Sync results set calendar.Changes.ObjectReplace,
	// as CalDAV does. Either way an incremental Sync carries every row of
	// each changed object.
	ObjectReplace bool
	// Now stamps Event.Updated on every change. Nil means time.Now.
	Now func() time.Time
}

// Call is one Store method call, with the arguments that method takes.
type Call struct {
	Method     string
	CalendarID string
	ProviderID string
	Token      string            // Sync
	ETag       string            // Put, Delete
	Notify     calendar.Notify   // Put, Delete
	Event      calendar.Event    // Put
	Response   calendar.Response // Respond
	People     []string          // FreeBusy
	From, To   time.Time         // FreeBusy
}

// object is one stored object: rows[0] is the single event or the master
// (or the first override when there is no master), then overrides.
type object struct {
	providerID string
	rows       []calendar.Event
}

// change is one entry of a calendar's change log.
type change struct {
	version    int
	providerID string
}

type cal struct {
	info calendar.Calendar
	// incarnation tells this calendar from an earlier one with the same ID,
	// whose change log is gone, so its tokens are not valid here.
	incarnation int
	objects     []*object // in the order they were added
	log         []change
	version     int
}

func (c *cal) find(providerID string) (int, *object) {
	for i, o := range c.objects {
		if o.providerID == providerID {
			return i, o
		}
	}
	return -1, nil
}

// Store is an in-memory calstore.Store, safe for concurrent use.
type Store struct {
	mu            sync.Mutex
	objectReplace bool
	now           func() time.Time
	cals          []*cal
	epoch         int // bumped by InvalidateTokens
	incarnations  int // the last incarnation given to a new calendar
	nextETag      int
	nextObject    int
	freeBusy      map[string]calendar.FreeBusy
	fail          map[string]error
	failNext      map[string][]error
	calls         []Call
}

var _ calstore.Store = (*Store)(nil)

// errNoUID refuses a create without a UID, which names the new object.
var errNoUID = errors.New("fake: create without a UID")

// New returns an empty Store.
func New(o Options) *Store {
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Store{
		objectReplace: o.ObjectReplace,
		now:           o.Now,
		freeBusy:      map[string]calendar.FreeBusy{},
		fail:          map[string]error{},
		failNext:      map[string][]error{},
	}
}

// ObjectName is the ProviderID the fake gives an object created with uid.
func ObjectName(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return "obj-" + hex.EncodeToString(sum[:12])
}

// Fail makes every call of method fail with err, after any FailNext
// errors. A nil err clears it.
func (s *Store) Fail(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.fail, method)
		return
	}
	s.fail[method] = err
}

// FailNext makes the next len(errs) calls of method fail, one error each,
// in order.
func (s *Store) FailNext(method string, errs ...error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method] = append(s.failNext[method], errs...)
}

// Calls returns every call so far, failed ones included, in order.
func (s *Store) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, len(s.calls))
	for i, c := range s.calls {
		out[i] = cloneCall(c)
	}
	return out
}

// CallsTo returns the calls of method, in order.
func (s *Store) CallsTo(method string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, cloneCall(c))
		}
	}
	return out
}

// Count returns how many times method was called.
func (s *Store) Count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// ResetCalls forgets the calls so far.
func (s *Store) ResetCalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

// AddCalendar adds c, or replaces the calendar with c.ID keeping its events.
func (s *Store) AddCalendar(c calendar.Calendar) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.calLocked(c.ID); existing != nil {
		existing.info = c
		return
	}
	s.incarnations++
	s.cals = append(s.cals, &cal{info: c, incarnation: s.incarnations})
}

// RemoveCalendar removes a calendar and its events. An unknown ID is a
// no-op.
func (s *Store) RemoveCalendar(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cals = slices.DeleteFunc(s.cals, func(c *cal) bool { return c.info.ID == id })
}

// Add stores evs as one new object in calendar calID and returns its
// ProviderID: evs[0].ProviderID when set, else ObjectName(evs[0].UID), else
// a generated name. Every row gets the ProviderID and a new etag.
func (s *Store) Add(calID string, evs ...calendar.Event) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.mustCalLocked(calID)
	if len(evs) == 0 {
		panic("fake: Add with no events")
	}
	p := evs[0].ProviderID
	switch {
	case p != "":
	case evs[0].UID != "":
		p = ObjectName(evs[0].UID)
	default:
		s.nextObject++
		p = "obj-" + strconv.Itoa(s.nextObject)
	}
	if _, o := c.find(p); o != nil {
		panic("fake: Add of an object that exists: " + p)
	}
	o := &object{providerID: p, rows: cloneEvents(evs)}
	c.objects = append(c.objects, o)
	s.touchLocked(c, o)
	return p
}

// Change replaces the rows of an object with what fn returns; fn gets a
// copy. The rows keep the ProviderID and get a new etag. Sequence is not
// bumped: fn does that when it means to.
func (s *Store) Change(calID, providerID string, fn func([]calendar.Event) []calendar.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, o := s.mustObjectLocked(calID, providerID)
	rows := fn(cloneEvents(o.rows))
	if len(rows) == 0 {
		panic("fake: Change left no rows")
	}
	o.rows = cloneEvents(rows)
	s.touchLocked(c, o)
}

// Cancel cancels an object as its organizer would: every row becomes
// CANCELLED and its sequence goes up by one.
func (s *Store) Cancel(calID, providerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, o := s.mustObjectLocked(calID, providerID)
	for i := range o.rows {
		o.rows[i].Status = calendar.StatusCancelled
		o.rows[i].Sequence++
	}
	s.touchLocked(c, o)
}

// CancelInstance cancels one instance of a series as its organizer would:
// the override at rid, made from the master when there is none, becomes
// CANCELLED.
func (s *Store) CancelInstance(calID, providerID string, rid time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, o := s.mustObjectLocked(calID, providerID)
	row, i, ok := instance(o, rid)
	if !ok || rid.IsZero() {
		panic("fake: CancelInstance needs a recurrence ID and a master")
	}
	row.Status = calendar.StatusCancelled
	row.Sequence++
	s.putRowLocked(c, o, i, row)
}

// Remove deletes an object, as an organizer deleting the event would.
func (s *Store) Remove(calID, providerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, o := s.mustObjectLocked(calID, providerID)
	c.objects = slices.DeleteFunc(c.objects, func(x *object) bool { return x == o })
	s.logLocked(c, providerID)
}

// SetPartStat sets the PARTSTAT of attendee email on the master or single
// event (rid zero) or on the instance at rid, adding an override from the
// master when there is none, as an attendee answering would.
func (s *Store) SetPartStat(calID, providerID string, rid time.Time, email string, ps calendar.PartStat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, o := s.mustObjectLocked(calID, providerID)
	if _, err := s.setPartStatLocked(c, o, rid, email, ps); err != nil {
		panic("fake: SetPartStat: " + err.Error())
	}
}

// SetFreeBusy sets what FreeBusy reports for person. People without one
// are reported as not known.
func (s *Store) SetFreeBusy(person string, fb calendar.FreeBusy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fb.Busy = slices.Clone(fb.Busy)
	s.freeBusy[person] = fb
}

// Object returns a copy of an object's rows and whether it exists.
func (s *Store) Object(calID, providerID string) ([]calendar.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.calLocked(calID)
	if c == nil {
		return nil, false
	}
	_, o := c.find(providerID)
	if o == nil {
		return nil, false
	}
	return cloneEvents(o.rows), true
}

// InvalidateTokens makes every token handed out so far invalid, so the
// next Sync with one returns calstore.ErrSyncTokenInvalid.
func (s *Store) InvalidateTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch++
}

// ListCalendars implements calstore.Store.
func (s *Store) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginLocked(ctx, Call{Method: MethodListCalendars}); err != nil {
		return nil, err
	}
	out := make([]calendar.Calendar, 0, len(s.cals))
	for _, c := range s.cals {
		out = append(out, c.info)
	}
	return out, nil
}

// Sync implements calstore.Store. An empty token gives every object; a
// token from an earlier Sync of the same calendar gives the objects
// changed since, with every row of each, and the ProviderIDs removed. A
// token from another calendar, from a removed calendar with the same ID,
// from before InvalidateTokens, or not made by the fake gives
// calstore.ErrSyncTokenInvalid.
func (s *Store) Sync(ctx context.Context, calID, token string) (calendar.Changes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginLocked(ctx, Call{Method: MethodSync, CalendarID: calID, Token: token}); err != nil {
		return calendar.Changes{}, err
	}
	c := s.calLocked(calID)
	if c == nil {
		return calendar.Changes{}, calstore.ErrNotFound
	}
	out := calendar.Changes{Token: s.tokenLocked(c), ObjectReplace: s.objectReplace}
	if token == "" {
		out.Full = true
		for _, o := range c.objects {
			out.Upserts = append(out.Upserts, cloneEvents(o.rows)...)
		}
		return out, nil
	}
	since, ok := s.parseTokenLocked(c, token)
	if !ok {
		return calendar.Changes{}, calstore.ErrSyncTokenInvalid
	}
	changed := map[string]bool{}
	for _, ch := range c.log {
		if ch.version > since {
			changed[ch.providerID] = true
		}
	}
	for _, o := range c.objects {
		if changed[o.providerID] {
			out.Upserts = append(out.Upserts, cloneEvents(o.rows)...)
			delete(changed, o.providerID)
		}
	}
	for _, ch := range c.log {
		if changed[ch.providerID] {
			out.Deletes = append(out.Deletes, ch.providerID)
			delete(changed, ch.providerID)
		}
	}
	return out, nil
}

// GetEvent implements calstore.Store.
func (s *Store) GetEvent(ctx context.Context, calID, providerID string) ([]calendar.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginLocked(ctx, Call{Method: MethodGetEvent, CalendarID: calID, ProviderID: providerID}); err != nil {
		return nil, err
	}
	_, o, err := s.objectLocked(calID, providerID)
	if err != nil {
		return nil, err
	}
	return cloneEvents(o.rows), nil
}

// FreeBusy implements calstore.Store with what SetFreeBusy set, clipped to
// [from, to). People without one are not known.
func (s *Store) FreeBusy(ctx context.Context, people []string, from, to time.Time) (map[string]calendar.FreeBusy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginLocked(ctx, Call{Method: MethodFreeBusy, People: slices.Clone(people), From: from, To: to}); err != nil {
		return nil, err
	}
	out := make(map[string]calendar.FreeBusy, len(people))
	for _, p := range people {
		fb, ok := s.freeBusy[p]
		if !ok {
			out[p] = calendar.FreeBusy{}
			continue
		}
		clipped := calendar.FreeBusy{Known: fb.Known}
		for _, b := range fb.Busy {
			if !b.End.After(from) || !b.Start.Before(to) {
				continue
			}
			if b.Start.Before(from) {
				b.Start = from
			}
			if b.End.After(to) {
				b.End = to
			}
			clipped.Busy = append(clipped.Busy, b)
		}
		out[p] = clipped
	}
	return out, nil
}

// Put implements calstore.Store. A create names the object ObjectName(ev.UID)
// and stores ev alone; a replace puts ev in place of the row with its
// RecurrenceID (the master when zero), or adds it as an override.
func (s *Store) Put(ctx context.Context, calID string, ev calendar.Event, etag string, mode calendar.Notify) (calendar.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := Call{Method: MethodPut, CalendarID: calID, ProviderID: ev.ProviderID, ETag: etag, Notify: mode, Event: cloneEvent(ev)}
	if err := s.beginLocked(ctx, call); err != nil {
		return calendar.Event{}, err
	}
	c, err := s.writableLocked(calID)
	if err != nil {
		return calendar.Event{}, err
	}
	if etag == "" {
		if ev.UID == "" {
			return calendar.Event{}, errNoUID
		}
		p := ObjectName(ev.UID)
		if _, o := c.find(p); o != nil {
			return calendar.Event{}, calstore.ErrExists
		}
		ev.ProviderID = p
		o := &object{providerID: p, rows: []calendar.Event{cloneEvent(ev)}}
		c.objects = append(c.objects, o)
		s.touchLocked(c, o)
		return cloneEvent(o.rows[0]), nil
	}
	_, o := c.find(ev.ProviderID)
	if o == nil {
		return calendar.Event{}, calstore.ErrNotFound
	}
	if o.rows[0].ETag != etag {
		return calendar.Event{}, calstore.ErrConflict
	}
	i := slices.IndexFunc(o.rows, func(r calendar.Event) bool { return r.RecurrenceID.Equal(ev.RecurrenceID) })
	if i < 0 {
		i = len(o.rows)
	}
	return s.putRowLocked(c, o, i, cloneEvent(ev)), nil
}

// Respond implements calstore.Store: it sets r.Self's PARTSTAT, matched
// without regard to case, on the master or single event, or on the
// instance at r.RecurrenceID, adding an override from the master when
// there is none. Self not among the attendees gives calstore.ErrNotFound.
// It works on a read-only calendar too, as answering an invite does.
// The comment is logged but not stored.
func (s *Store) Respond(ctx context.Context, calID, providerID string, r calendar.Response) (calendar.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginLocked(ctx, Call{Method: MethodRespond, CalendarID: calID, ProviderID: providerID, Response: r}); err != nil {
		return calendar.Event{}, err
	}
	c, o, err := s.objectLocked(calID, providerID)
	if err != nil {
		return calendar.Event{}, err
	}
	return s.setPartStatLocked(c, o, r.RecurrenceID, r.Self, r.PartStat)
}

// Delete implements calstore.Store. An empty etag deletes whatever the
// object's etag.
func (s *Store) Delete(ctx context.Context, calID, providerID, etag string, mode calendar.Notify) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beginLocked(ctx, Call{Method: MethodDelete, CalendarID: calID, ProviderID: providerID, ETag: etag, Notify: mode}); err != nil {
		return err
	}
	c, err := s.writableLocked(calID)
	if err != nil {
		return err
	}
	_, o := c.find(providerID)
	if o == nil {
		return calstore.ErrNotFound
	}
	if etag != "" && o.rows[0].ETag != etag {
		return calstore.ErrConflict
	}
	c.objects = slices.DeleteFunc(c.objects, func(x *object) bool { return x == o })
	s.logLocked(c, providerID)
	return nil
}

// beginLocked logs call and returns the error the call must fail with:
// ctx's, then a FailNext error, then a Fail error.
func (s *Store) beginLocked(ctx context.Context, call Call) error {
	s.calls = append(s.calls, call)
	if err := ctx.Err(); err != nil {
		return err
	}
	if q := s.failNext[call.Method]; len(q) > 0 {
		s.failNext[call.Method] = q[1:]
		return q[0]
	}
	return s.fail[call.Method]
}

func (s *Store) calLocked(id string) *cal {
	for _, c := range s.cals {
		if c.info.ID == id {
			return c
		}
	}
	return nil
}

func (s *Store) mustCalLocked(id string) *cal {
	c := s.calLocked(id)
	if c == nil {
		panic("fake: no calendar " + id)
	}
	return c
}

func (s *Store) objectLocked(calID, providerID string) (*cal, *object, error) {
	c := s.calLocked(calID)
	if c == nil {
		return nil, nil, calstore.ErrNotFound
	}
	_, o := c.find(providerID)
	if o == nil {
		return nil, nil, calstore.ErrNotFound
	}
	return c, o, nil
}

func (s *Store) mustObjectLocked(calID, providerID string) (*cal, *object) {
	c := s.mustCalLocked(calID)
	_, o := c.find(providerID)
	if o == nil {
		panic("fake: no object " + providerID + " in calendar " + calID)
	}
	return c, o
}

// writableLocked returns the calendar for a write: calstore.ErrNotFound
// when there is none, calstore.ErrReadOnly when its access is not write.
func (s *Store) writableLocked(calID string) (*cal, error) {
	c := s.calLocked(calID)
	if c == nil {
		return nil, calstore.ErrNotFound
	}
	if c.info.Access != calendar.AccessWrite {
		return nil, calstore.ErrReadOnly
	}
	return c, nil
}

// touchLocked stamps every row of o with its ProviderID, a new etag and
// now, and logs the change.
func (s *Store) touchLocked(c *cal, o *object) {
	s.nextETag++
	etag := `"` + strconv.Itoa(s.nextETag) + `"`
	now := s.now().UTC()
	for i := range o.rows {
		o.rows[i].ProviderID = o.providerID
		o.rows[i].ETag = etag
		o.rows[i].Updated = now
	}
	s.logLocked(c, o.providerID)
}

func (s *Store) logLocked(c *cal, providerID string) {
	c.version++
	c.log = append(c.log, change{version: c.version, providerID: providerID})
}

// tokenLocked is c's current token: the epoch, c's incarnation, the version
// and the calendar ID, so a token is valid only for the calendar that
// made it, and not for a calendar later added again with the same ID.
func (s *Store) tokenLocked(c *cal) string {
	return fmt.Sprintf("fake:%d:%d:%d:%s", s.epoch, c.incarnation, c.version, c.info.ID)
}

func (s *Store) parseTokenLocked(c *cal, token string) (int, bool) {
	parts := strings.SplitN(token, ":", 5)
	if len(parts) != 5 || parts[0] != "fake" || parts[4] != c.info.ID {
		return 0, false
	}
	epoch, err := strconv.Atoi(parts[1])
	if err != nil || epoch != s.epoch {
		return 0, false
	}
	incarnation, err := strconv.Atoi(parts[2])
	if err != nil || incarnation != c.incarnation {
		return 0, false
	}
	version, err := strconv.Atoi(parts[3])
	if err != nil || version < 0 || version > c.version {
		return 0, false
	}
	return version, true
}

// instance returns o's row for rid and its index: the master or single
// event when rid is zero, else the override at rid. When there is no such
// override, it returns a new one made from the master with index
// len(o.rows), which the caller appends to keep it. It reports false when
// an override is needed and o has no master to make it from.
func instance(o *object, rid time.Time) (calendar.Event, int, bool) {
	if rid.IsZero() {
		return cloneEvent(o.rows[0]), 0, true
	}
	if i := slices.IndexFunc(o.rows, func(r calendar.Event) bool { return r.RecurrenceID.Equal(rid) }); i >= 0 {
		return cloneEvent(o.rows[i]), i, true
	}
	master := o.rows[0]
	if !master.RecurrenceID.IsZero() {
		return calendar.Event{}, 0, false
	}
	ov := cloneEvent(master)
	ov.RRule, ov.RDates, ov.ExDates = "", nil, nil
	ov.RecurrenceID = rid.UTC()
	ov.Start = rid.UTC()
	ov.End = ov.Start.Add(master.End.Sub(master.Start))
	return ov, len(o.rows), true
}

// putRowLocked stores row at index i of o, appending when i is past the
// end, and touches o.
func (s *Store) putRowLocked(c *cal, o *object, i int, row calendar.Event) calendar.Event {
	if i == len(o.rows) {
		o.rows = append(o.rows, row)
	} else {
		o.rows[i] = row
	}
	s.touchLocked(c, o)
	return cloneEvent(o.rows[i])
}

// setPartStatLocked sets email's PARTSTAT, matched without regard to
// case, on o's row for rid and returns a copy of the row. Nothing changes
// when email is not an attendee of that row.
func (s *Store) setPartStatLocked(c *cal, o *object, rid time.Time, email string, ps calendar.PartStat) (calendar.Event, error) {
	row, i, ok := instance(o, rid)
	if !ok {
		return calendar.Event{}, fmt.Errorf("no master for the instance: %w", calstore.ErrNotFound)
	}
	a := slices.IndexFunc(row.Attendees, func(a calendar.Attendee) bool { return strings.EqualFold(a.Email, email) })
	if a < 0 {
		return calendar.Event{}, fmt.Errorf("not an attendee: %w", calstore.ErrNotFound)
	}
	row.Attendees[a].PartStat = ps
	return s.putRowLocked(c, o, i, row), nil
}

func cloneEvent(ev calendar.Event) calendar.Event {
	if ev.Organizer != nil {
		o := *ev.Organizer
		ev.Organizer = &o
	}
	ev.Attendees = slices.Clone(ev.Attendees)
	ev.RDates = slices.Clone(ev.RDates)
	ev.ExDates = slices.Clone(ev.ExDates)
	ev.Raw = slices.Clone(ev.Raw)
	return ev
}

func cloneEvents(evs []calendar.Event) []calendar.Event {
	out := make([]calendar.Event, len(evs))
	for i, ev := range evs {
		out[i] = cloneEvent(ev)
	}
	return out
}

func cloneCall(c Call) Call {
	c.Event = cloneEvent(c.Event)
	c.People = slices.Clone(c.People)
	return c
}

// ObjectID is the ProviderID a create of an event with uid gives, in any
// calendar: ObjectName(uid).
func (*Store) ObjectID(_, uid string) string { return ObjectName(uid) }
