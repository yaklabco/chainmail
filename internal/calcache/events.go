package calcache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/watch"
)

// CalendarRow is one stored calendar with its sync state.
type CalendarRow struct {
	Account string
	calendar.Calendar
	IsDefault bool
	SyncToken string
	LastSync  time.Time
	LastError string // fixed text only
}

// EventRow is one stored VEVENT: a single event, a series master or an
// override.
type EventRow struct {
	Ref         string // base ref, or InstanceRef for an override
	Account     string
	CalendarID  string
	Event       calendar.Event
	MyPartStat  calendar.PartStat // "" when James is not an attendee
	IsOrganizer bool
}

// errStoredRow reports a stored row this version cannot read. The text is
// fixed: the row holds untrusted event text.
var errStoredRow = errors.New("calendar.db holds an unreadable event row")

// formatTime and parseTime are the stored time encoding, shared with
// state.db: fixed-width UTC text that sorts and range-compares as text,
// with "" for the zero time.
func formatTime(t time.Time) string { return watch.FormatTime(t) }

func parseTime(s string) (time.Time, error) {
	t, err := watch.ParseTime(s)
	if err != nil {
		// ParseTime's error repeats the input; this one does not.
		return time.Time{}, errStoredRow
	}
	return t, nil
}

// Calendars returns the account's calendars, the default first, then the
// primary, then by ID.
func (s *Store) Calendars(ctx context.Context, account string) ([]CalendarRow, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+calendarColumns+" FROM calendars WHERE account = ? ORDER BY is_default DESC, is_primary DESC, calendar_id",
		account)
	if err != nil {
		return nil, s.fail("reading calendars", err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports read errors
	var out []CalendarRow
	for rows.Next() {
		c, err := scanCalendar(rows)
		if err != nil {
			return nil, s.fail("reading calendars", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, s.fail("reading calendars", err)
	}
	return out, nil
}

// Calendar returns one stored calendar, or ErrNotFound.
func (s *Store) Calendar(ctx context.Context, account, calID string) (CalendarRow, error) {
	row := s.db.QueryRowContext(ctx,
		"SELECT "+calendarColumns+" FROM calendars WHERE account = ? AND calendar_id = ?", account, calID)
	c, err := scanCalendar(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CalendarRow{}, ErrNotFound
	}
	if err != nil {
		return CalendarRow{}, s.fail("reading a calendar", err)
	}
	return c, nil
}

const calendarColumns = "account, calendar_id, name, access, is_primary, time_zone, is_default, sync_token, last_sync, last_error"

// scanner is a *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanCalendar(sc scanner) (CalendarRow, error) {
	var (
		c                  CalendarRow
		access, lastSync   string
		primary, isDefault bool
	)
	if err := sc.Scan(&c.Account, &c.ID, &c.Name, &access, &primary, &c.TimeZone, &isDefault,
		&c.SyncToken, &lastSync, &c.LastError); err != nil {
		return CalendarRow{}, err
	}
	c.Access = calendar.Access(access)
	c.Primary = primary
	c.IsDefault = isDefault
	t, err := parseTime(lastSync)
	if err != nil {
		return CalendarRow{}, err
	}
	c.LastSync = t
	return c, nil
}

// SetCalendars upserts cals and deletes calendars (and their events) of the
// account not in cals. defaultID names the default calendar; no calendar is
// the default when it matches none. An existing calendar keeps its sync
// token, last sync and last error. An access other than write is stored as
// read, so chainmail never writes to a calendar it cannot tell is writable.
func (s *Store) SetCalendars(ctx context.Context, account string, cals []calendar.Calendar, defaultID string) error {
	return s.inTx(ctx, "storing calendars", func(tx *sql.Tx) error {
		keep := make(map[string]bool, len(cals))
		for _, c := range cals {
			keep[c.ID] = true
			access := calendar.AccessRead
			if c.Access == calendar.AccessWrite {
				access = calendar.AccessWrite
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO calendars (account, calendar_id, name, access, is_primary, time_zone, is_default)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (account, calendar_id) DO UPDATE SET
					name = excluded.name, access = excluded.access, is_primary = excluded.is_primary,
					time_zone = excluded.time_zone, is_default = excluded.is_default`,
				account, c.ID, c.Name, string(access), boolInt(c.Primary), c.TimeZone, boolInt(c.ID == defaultID)); err != nil {
				return err
			}
		}
		rows, err := tx.QueryContext(ctx, "SELECT calendar_id FROM calendars WHERE account = ?", account)
		if err != nil {
			return err
		}
		var gone []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close() // the scan error is the one to report
				return err
			}
			if !keep[id] {
				gone = append(gone, id)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range gone {
			// cal_events rows go with it (ON DELETE CASCADE).
			if _, err := tx.ExecContext(ctx, "DELETE FROM calendars WHERE account = ? AND calendar_id = ?", account, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetError records msg, which must be fixed text, as the calendar's last
// error. ErrNotFound when the calendar is not stored.
func (s *Store) SetError(ctx context.Context, account, calID, msg string) error {
	res, err := s.db.ExecContext(ctx,
		"UPDATE calendars SET last_error = ? WHERE account = ? AND calendar_id = ?", msg, account, calID)
	if err != nil {
		return s.fail("recording a calendar error", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return s.fail("recording a calendar error", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkReady records that the account's first complete sync finished at at.
func (s *Store) MarkReady(ctx context.Context, account string, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		"INSERT INTO accounts (account, ready_at) VALUES (?, ?) ON CONFLICT (account) DO UPDATE SET ready_at = excluded.ready_at",
		account, formatTime(at)); err != nil {
		return s.fail("marking the calendar cache ready", err)
	}
	return nil
}

// Ready reports whether MarkReady was called for the account since the
// file was created.
func (s *Store) Ready(ctx context.Context, account string) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE account = ?", account).Scan(&n); err != nil {
		return false, s.fail("reading calendar cache readiness", err)
	}
	return n > 0, nil
}

// Stats counts the account's stored calendars and event rows.
func (s *Store) Stats(ctx context.Context, account string) (calendars, events int64, err error) {
	if err := s.db.QueryRowContext(ctx,
		"SELECT (SELECT count(*) FROM calendars WHERE account = ?), (SELECT count(*) FROM cal_events WHERE account = ?)",
		account, account).Scan(&calendars, &events); err != nil {
		return 0, 0, s.fail("counting calendar rows", err)
	}
	return calendars, events, nil
}

// Event returns the stored row with ref: a base ref names a single event
// or a series master, an instance ref names a stored override. An instance
// of a series with no override of its own is not a stored row, so its ref
// gives ErrNotFound; callers expand the master instead.
func (s *Store) Event(ctx context.Context, ref string) (EventRow, error) {
	rows, err := s.queryEvents(ctx, "reading an event",
		"WHERE ref = ? ORDER BY account, calendar_id, provider_id, recurrence_id LIMIT 1", ref)
	if err != nil {
		return EventRow{}, err
	}
	if len(rows) == 0 {
		return EventRow{}, ErrNotFound
	}
	return rows[0], nil
}

// Series returns every stored row of one object: the master first, then
// its overrides by recurrence ID, or the one single event.
func (s *Store) Series(ctx context.Context, account, calID, providerID string) ([]EventRow, error) {
	return s.queryEvents(ctx, "reading a series",
		"WHERE account = ? AND calendar_id = ? AND provider_id = ? ORDER BY recurrence_id",
		account, calID, providerID)
}

// ByUID returns the account's rows with the iCalendar UID, in every
// calendar, by calendar, ProviderID and recurrence ID.
func (s *Store) ByUID(ctx context.Context, account, uid string) ([]EventRow, error) {
	return s.queryEvents(ctx, "reading events by UID",
		"WHERE account = ? AND uid = ? ORDER BY calendar_id, provider_id, recurrence_id", account, uid)
}

// Range returns the rows needed to expand [from, to): singles and overrides
// overlapping it, and every row of each series whose master starts before to.
// A series is a master with an RRULE or RDATEs. Empty accounts or calIDs
// mean every account or calendar. A zero-length event overlaps the range
// holding its instant, as calendar.Expand counts it. Rows come by account,
// calendar, ProviderID and recurrence ID.
func (s *Store) Range(ctx context.Context, accounts, calIDs []string, from, to time.Time) ([]EventRow, error) {
	if !from.Before(to) {
		return nil, nil
	}
	f, t := formatTime(from), formatTime(to)
	var (
		where []string
		args  []any
	)
	if len(accounts) > 0 {
		where = append(where, "e.account IN ("+placeholders(len(accounts))+")")
		for _, a := range accounts {
			args = append(args, a)
		}
	}
	if len(calIDs) > 0 {
		where = append(where, "e.calendar_id IN ("+placeholders(len(calIDs))+")")
		for _, c := range calIDs {
			args = append(args, c)
		}
	}
	where = append(where, `(
		(e.is_master = 0 AND e.start_utc < ? AND (e.end_utc > ? OR (e.end_utc <= e.start_utc AND e.start_utc >= ?)))
		OR EXISTS (SELECT 1 FROM cal_events m
			WHERE m.account = e.account AND m.calendar_id = e.calendar_id AND m.provider_id = e.provider_id
			AND m.recurrence_id = '' AND m.is_master = 1 AND m.start_utc < ?))`)
	args = append(args, t, f, f, t)
	return s.queryEvents(ctx, "reading events in a range",
		"WHERE "+strings.Join(where, " AND ")+" ORDER BY e.account, e.calendar_id, e.provider_id, e.recurrence_id", args...)
}

// Prune deletes singles that ended before before, and overrides whose own
// end and RECURRENCE-ID are both before it, and returns how many it
// deleted. An override that moved an occurrence from after before to a
// time before it is kept: deleting it would let its master put the
// original occurrence back (Amendment 18). Series masters are kept
// whatever their dates.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	cutoff := formatTime(before)
	// recurrence_id is "" for a single, which sorts before every stored time.
	var n int64
	err := s.inTx(ctx, "pruning old events", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"DELETE FROM cal_events WHERE is_master = 0 AND end_utc < ? AND recurrence_id < ?", cutoff, cutoff)
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM cal_raws WHERE "+unreferencedRaw)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// queryEvents reads event rows; clause follows "FROM cal_events e". The
// rows are read first and their raw objects after, once per object and
// content, since the one connection serves one query at a time. Both
// reads run in one read transaction, so they see the file as of one
// moment: an Apply that replaces the rows and deletes their old raw
// objects lands before both reads or after both.
func (s *Store) queryEvents(ctx context.Context, op, clause string, args ...any) ([]EventRow, error) {
	var out []EventRow
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		recs, err := func() ([]storedRecord, error) {
			rows, err := tx.QueryContext(ctx,
				"SELECT e.account, e.calendar_id, "+prefixed("e.", eventColumns)+" FROM cal_events e "+clause, args...)
			if err != nil {
				return nil, err
			}
			defer func() { _ = rows.Close() }() // rows.Err below reports read errors
			var recs []storedRecord
			for rows.Next() {
				var sr storedRecord
				if sr.r, err = scanRecord(rows, &sr.account, &sr.calID); err != nil {
					return nil, err
				}
				recs = append(recs, sr)
			}
			return recs, rows.Err()
		}()
		if err != nil {
			return err
		}
		if s.rowsRead != nil {
			s.rowsRead()
		}
		out, err = decodeRecords(ctx, tx, recs, nil)
		return err
	})
	if err != nil && !errors.Is(err, errStoredRow) {
		return nil, s.fail(op, err)
	}
	return out, err
}

// readTx runs fn in a read transaction, which sees the file as of its
// first read whatever other connections commit meanwhile (WAL), and holds
// the store's one connection, so none of the store's own writes runs in
// between. The transaction is rolled back: fn only reads. fn's error is
// returned as it is, for the caller to classify.
func (s *Store) readTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // fn only reads; nothing to commit
	return fn(tx)
}

// storedRecord is a record with the account and calendar it belongs to.
type storedRecord struct {
	account, calID string
	r              record
}

// decodeRecords decodes recs, reading each raw object they name from
// cal_raws once, unless have, by sum, already holds it. Rows with the same
// object and content share one Raw slice.
func decodeRecords(ctx context.Context, q querier, recs []storedRecord, have map[string][]byte) ([]EventRow, error) {
	var need []rawKey
	wanted := map[rawKey]bool{}
	for _, sr := range recs {
		if sr.r.rawSum == "" {
			continue
		}
		if _, ok := have[sr.r.rawSum]; ok {
			continue
		}
		k := rawKey{account: sr.account, calID: sr.calID, provider: sr.r.providerID, sum: sr.r.rawSum}
		if !wanted[k] {
			wanted[k] = true
			need = append(need, k)
		}
	}
	loaded, err := loadRaws(ctx, q, need)
	if err != nil {
		return nil, err
	}
	out := make([]EventRow, 0, len(recs))
	for _, sr := range recs {
		raw, ok := have[sr.r.rawSum]
		if !ok && sr.r.rawSum != "" {
			if raw, ok = loaded[rawKey{account: sr.account, calID: sr.calID, provider: sr.r.providerID, sum: sr.r.rawSum}]; !ok {
				// The row names a raw object calendar.db does not hold:
				// decoding it without one would drop its VTIMEZONE.
				return nil, errStoredRow
			}
		}
		row, err := sr.r.decode(sr.account, sr.calID, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// prefixed qualifies each column of a comma-separated list.
func prefixed(prefix, columns string) string {
	cols := strings.Split(columns, ", ")
	for i, c := range cols {
		cols[i] = prefix + c
	}
	return strings.Join(cols, ", ")
}

// eventColumns are the cal_events columns after account and calendar_id,
// in record.args and scanRecord order.
const eventColumns = "provider_id, recurrence_id, ref, is_master, etag, uid, start_utc, end_utc, tzid, all_day, " +
	"status, sequence, transparent, organizer_json, attendees_json, attendees_truncated, " +
	"rrule, rdates_json, exdates_json, summary, location, description, updated, raw_sum, my_partstat, is_organizer"

const eventColumnCount = 26

// record is one cal_events row in stored form. Its fields are comparable,
// so two records are the same row exactly when they are ==.
type record struct {
	providerID, recurrenceID, ref string
	isMaster                      bool
	etag, uid, start, end, tzid   string
	allDay                        bool
	status                        string
	sequence                      int
	transparent                   bool
	organizer, attendees          string
	attendeesTruncated            bool
	rrule, rdates, exdates        string
	summary, location, desc       string
	updated                       string
	rawSum                        string // the cal_raws sum of the raw object, "" for none
	myPartStat                    string
	isOrganizer                   bool
}

func (r record) key() rowKey { return rowKey{provider: r.providerID, rid: r.recurrenceID} }

// args are r's values in eventColumns order.
func (r record) args() []any {
	return []any{
		r.providerID, r.recurrenceID, r.ref, boolInt(r.isMaster), r.etag, r.uid, r.start, r.end, r.tzid, boolInt(r.allDay),
		r.status, r.sequence, boolInt(r.transparent), r.organizer, r.attendees, boolInt(r.attendeesTruncated),
		r.rrule, r.rdates, r.exdates, r.summary, r.location, r.desc, r.updated, r.rawSum, r.myPartStat, boolInt(r.isOrganizer),
	}
}

// scanRecord reads account, calendar_id and then eventColumns.
func scanRecord(sc scanner, account, calID *string) (record, error) {
	var r record
	if err := sc.Scan(account, calID,
		&r.providerID, &r.recurrenceID, &r.ref, &r.isMaster, &r.etag, &r.uid, &r.start, &r.end, &r.tzid, &r.allDay,
		&r.status, &r.sequence, &r.transparent, &r.organizer, &r.attendees, &r.attendeesTruncated,
		&r.rrule, &r.rdates, &r.exdates, &r.summary, &r.location, &r.desc, &r.updated, &r.rawSum, &r.myPartStat, &r.isOrganizer); err != nil {
		return record{}, err
	}
	return r, nil
}

// storedPerson and storedAttendee are the JSON forms of organizer_json and
// attendees_json.
type storedPerson struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type storedAttendee struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	PartStat string `json:"partstat,omitempty"`
	Role     string `json:"role,omitempty"`
	RSVP     bool   `json:"rsvp,omitempty"`
}

// encode turns ev into its stored row in the account's calendar calID,
// computing MyPartStat and IsOrganizer with self (nil knows no address).
// Times are stored in UTC; an event without an end ends at its start. The
// raw object goes into raws, once per backing array, and the row keeps its
// sum.
func encode(account, calID string, ev calendar.Event, self func(string) bool, raws *rawSet) (record, error) {
	if self == nil {
		self = func(string) bool { return false }
	}
	end := ev.End
	if end.IsZero() {
		end = ev.Start
	}
	r := record{
		providerID:         ev.ProviderID,
		recurrenceID:       formatTime(ev.RecurrenceID),
		isMaster:           ev.RecurrenceID.IsZero() && (ev.RRule != "" || len(ev.RDates) > 0),
		etag:               ev.ETag,
		uid:                ev.UID,
		start:              formatTime(ev.Start),
		end:                formatTime(end),
		tzid:               ev.TZID,
		allDay:             ev.AllDay,
		status:             string(ev.Status),
		sequence:           ev.Sequence,
		transparent:        ev.Transparent,
		attendeesTruncated: ev.AttendeesTruncated,
		rrule:              ev.RRule,
		summary:            ev.Summary,
		location:           ev.Location,
		desc:               ev.Description,
		updated:            formatTime(ev.Updated),
		rawSum:             raws.add(ev.Raw),
	}
	base := calendar.EventRef(account, calID, ev.ProviderID)
	r.ref = base
	if !ev.RecurrenceID.IsZero() {
		r.ref = calendar.InstanceRef(base, ev.RecurrenceID)
	}
	if o := ev.Organizer; o != nil && (o.Email != "" || o.Name != "") {
		b, err := json.Marshal(storedPerson{Email: o.Email, Name: o.Name})
		if err != nil {
			return record{}, fmt.Errorf("encoding an organizer: %w", errStoredRow)
		}
		r.organizer = string(b)
		r.isOrganizer = o.Email != "" && self(o.Email)
	}
	if len(ev.Attendees) > 0 {
		atts := make([]storedAttendee, 0, len(ev.Attendees))
		for _, a := range ev.Attendees {
			atts = append(atts, storedAttendee{Email: a.Email, Name: a.Name, PartStat: string(a.PartStat), Role: a.Role, RSVP: a.RSVP})
			if r.myPartStat == "" && a.Email != "" && self(a.Email) {
				r.myPartStat = string(a.PartStat)
				if a.PartStat == "" {
					r.myPartStat = string(calendar.PartStatNeedsAction) // the RFC 5545 default
				}
			}
		}
		b, err := json.Marshal(atts)
		if err != nil {
			return record{}, fmt.Errorf("encoding attendees: %w", errStoredRow)
		}
		r.attendees = string(b)
	}
	var err error
	if r.rdates, err = encodeTimes(ev.RDates); err != nil {
		return record{}, err
	}
	if r.exdates, err = encodeTimes(ev.ExDates); err != nil {
		return record{}, err
	}
	return r, nil
}

func encodeTimes(ts []time.Time) (string, error) {
	if len(ts) == 0 {
		return "", nil
	}
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = formatTime(t)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encoding dates: %w", errStoredRow)
	}
	return string(b), nil
}

func decodeTimes(s string) ([]time.Time, error) {
	if s == "" {
		return nil, nil
	}
	var texts []string
	if err := json.Unmarshal([]byte(s), &texts); err != nil {
		return nil, errStoredRow
	}
	out := make([]time.Time, len(texts))
	for i, text := range texts {
		t, err := parseTime(text)
		if err != nil {
			return nil, err
		}
		out[i] = t
	}
	return out, nil
}

// decode turns a stored row back into an EventRow with raw, the object its
// raw_sum names, as Event.Raw. Errors are fixed text.
func (r record) decode(account, calID string, raw []byte) (EventRow, error) {
	ev := calendar.Event{
		ProviderID:         r.providerID,
		ETag:               r.etag,
		UID:                r.uid,
		Summary:            r.summary,
		Location:           r.location,
		Description:        r.desc,
		TZID:               r.tzid,
		AllDay:             r.allDay,
		Status:             calendar.Status(r.status),
		Sequence:           r.sequence,
		Transparent:        r.transparent,
		AttendeesTruncated: r.attendeesTruncated,
		RRule:              r.rrule,
		Raw:                raw,
	}
	var err error
	for _, f := range []struct {
		dst  *time.Time
		text string
	}{{&ev.RecurrenceID, r.recurrenceID}, {&ev.Start, r.start}, {&ev.End, r.end}, {&ev.Updated, r.updated}} {
		if *f.dst, err = parseTime(f.text); err != nil {
			return EventRow{}, err
		}
	}
	if r.organizer != "" {
		var p storedPerson
		if err := json.Unmarshal([]byte(r.organizer), &p); err != nil {
			return EventRow{}, errStoredRow
		}
		ev.Organizer = &calendar.Person{Email: p.Email, Name: p.Name}
	}
	if r.attendees != "" {
		var atts []storedAttendee
		if err := json.Unmarshal([]byte(r.attendees), &atts); err != nil {
			return EventRow{}, errStoredRow
		}
		ev.Attendees = make([]calendar.Attendee, 0, len(atts))
		for _, a := range atts {
			ev.Attendees = append(ev.Attendees, calendar.Attendee{
				Person:   calendar.Person{Email: a.Email, Name: a.Name},
				PartStat: calendar.PartStat(a.PartStat),
				Role:     a.Role,
				RSVP:     a.RSVP,
			})
		}
	}
	if ev.RDates, err = decodeTimes(r.rdates); err != nil {
		return EventRow{}, err
	}
	if ev.ExDates, err = decodeTimes(r.exdates); err != nil {
		return EventRow{}, err
	}
	return EventRow{
		Ref:         r.ref,
		Account:     account,
		CalendarID:  calID,
		Event:       ev,
		MyPartStat:  calendar.PartStat(r.myPartStat),
		IsOrganizer: r.isOrganizer,
	}, nil
}
