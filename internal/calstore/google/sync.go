package google

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"

	"github.com/yaklabco/chainmail/internal/calendar"
	"github.com/yaklabco/chainmail/internal/calstore"
)

// pageSize is the maxResults of every listing.
const pageSize = 250

type calendarListPage struct {
	Items []struct {
		ID              string `json:"id"`
		Summary         string `json:"summary"`
		SummaryOverride string `json:"summaryOverride"`
		TimeZone        string `json:"timeZone"`
		AccessRole      string `json:"accessRole"`
		Primary         bool   `json:"primary"`
		Deleted         bool   `json:"deleted"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

// ListCalendars returns the calendars on the account's calendar list.
func (s *Store) ListCalendars(ctx context.Context) ([]calendar.Calendar, error) {
	q := url.Values{"maxResults": {strconv.Itoa(pageSize)}}
	var out []calendar.Calendar
	seen := map[string]bool{}
	for range maxPages {
		data, err := s.do(ctx, call{method: "GET", path: "/users/me/calendarList", query: q, limit: maxPageBytes})
		if err != nil {
			return nil, err
		}
		var page calendarListPage
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, errBadResponse
		}
		for _, it := range page.Items {
			if it.Deleted || it.ID == "" {
				continue
			}
			c := calendar.Calendar{ID: it.ID, Name: it.Summary, Access: calendar.AccessRead, Primary: it.Primary}
			if it.SummaryOverride != "" {
				c.Name = it.SummaryOverride
			}
			if it.AccessRole == "owner" || it.AccessRole == "writer" {
				c.Access = calendar.AccessWrite
			}
			c.TimeZone = calendarZone(it.TimeZone)
			out = append(out, c)
		}
		if page.NextPageToken == "" {
			return out, nil
		}
		if seen[page.NextPageToken] {
			return nil, errPageRepeated
		}
		seen[page.NextPageToken] = true
		q.Set("pageToken", page.NextPageToken)
	}
	return nil, fmt.Errorf("%w: more than %d pages", calstore.ErrUnavailable, maxPages)
}

// eventsPage is one page of events.list. Items stay raw so each event keeps
// its JSON.
type eventsPage struct {
	Items         []json.RawMessage `json:"items"`
	NextPageToken string            `json:"nextPageToken"`
	NextSyncToken string            `json:"nextSyncToken"`
}

// maxSeriesLookups bounds the series one Sync reads to learn the UID of
// cancelled instances listed without their master. An incremental Sync
// with more such series syncs in full instead, since a full listing carries
// every master.
const maxSeriesLookups = 50

// errTooManySeries is a Sync with more series to read than maxSeriesLookups.
var errTooManySeries = fmt.Errorf("%w: too many series without a UID", calstore.ErrUnavailable)

// Sync lists the calendar's events changed since token, or all of them when
// token is "". Each upsert replaces the row with its ProviderID and
// RecurrenceID (Changes.ObjectReplace is false). A token Google no longer
// accepts gives calstore.ErrSyncTokenInvalid; the caller syncs again from
// "". Every override carries its series's UID, which Google leaves out of
// cancelled instances: Sync takes it from the master in the same listing,
// or reads the master.
func (s *Store) Sync(ctx context.Context, calID, token string) (calendar.Changes, error) {
	ch, err := s.sync(ctx, calID, token)
	if errors.Is(err, errTooManySeries) && token != "" {
		return s.sync(ctx, calID, "")
	}
	return ch, err
}

func (s *Store) sync(ctx context.Context, calID, token string) (calendar.Changes, error) {
	ch := calendar.Changes{Full: token == ""}
	q := listQuery()
	if token != "" {
		q.Set("syncToken", token)
	}
	uids := map[string]string{} // series id to iCalUID, from the masters listed
	err := s.listEvents(ctx, calID, q, true, func(ev calendar.Event, a apiEvent) {
		if a.RecurringEventID == "" && a.ICalUID != "" {
			uids[a.ID] = a.ICalUID
		}
		if a.deleted() {
			ch.Deletes = append(ch.Deletes, a.ID)
			return
		}
		ch.Upserts = append(ch.Upserts, ev)
	}, func(next string) { ch.Token = next })
	if err != nil {
		return calendar.Changes{}, err
	}
	if ch.Token == "" {
		return calendar.Changes{}, fmt.Errorf("%w: no sync token", errBadResponse)
	}
	if err := s.resolveUIDs(ctx, calID, &ch, uids); err != nil {
		return calendar.Changes{}, err
	}
	return ch, nil
}

// resolveUIDs drops the upserts of deleted series and gives every override
// without a UID its series's UID: from uids, or from the master, read once
// per series. A series that is gone joins ch.Deletes, so its cancelled
// instances do not outlive it in the cache.
func (s *Store) resolveUIDs(ctx context.Context, calID string, ch *calendar.Changes, uids map[string]string) error {
	gone := make(map[string]bool, len(ch.Deletes))
	for _, id := range ch.Deletes {
		gone[id] = true
	}
	ch.Upserts = slices.DeleteFunc(ch.Upserts, func(ev calendar.Event) bool { return gone[ev.ProviderID] })
	var missing []string
	for _, ev := range ch.Upserts {
		if ev.UID == "" && !ev.RecurrenceID.IsZero() && uids[ev.ProviderID] == "" && !slices.Contains(missing, ev.ProviderID) {
			missing = append(missing, ev.ProviderID)
		}
	}
	if len(missing) > maxSeriesLookups {
		return errTooManySeries
	}
	for _, id := range missing {
		uid, err := s.seriesUID(ctx, calID, id)
		switch {
		case errors.Is(err, calstore.ErrNotFound):
			ch.Deletes = append(ch.Deletes, id)
			gone[id] = true
		case err != nil:
			return err
		default:
			uids[id] = uid
		}
	}
	ch.Upserts = slices.DeleteFunc(ch.Upserts, func(ev calendar.Event) bool { return gone[ev.ProviderID] })
	for i := range ch.Upserts {
		if ev := &ch.Upserts[i]; ev.UID == "" && !ev.RecurrenceID.IsZero() {
			ev.UID = uids[ev.ProviderID]
		}
	}
	return nil
}

// seriesUID reads the master id for its iCalUID. A master that is gone or
// deleted gives calstore.ErrNotFound.
func (s *Store) seriesUID(ctx context.Context, calID, id string) (string, error) {
	data, err := s.do(ctx, call{method: "GET", path: eventPath(calID, id), limit: maxEventBytes})
	if err != nil {
		return "", err
	}
	var a apiEvent
	if err := json.Unmarshal(data, &a); err != nil {
		return "", errBadResponse
	}
	if a.deleted() {
		return "", fmt.Errorf("%w: series %s deleted", calstore.ErrNotFound, id)
	}
	if a.ICalUID == "" {
		return "", fmt.Errorf("%w: series %s has no UID", errBadResponse, id)
	}
	return a.ICalUID, nil
}

// listQuery is the query of every events.list: every change, including
// deletions, with series as masters and exceptions.
func listQuery() url.Values {
	return url.Values{
		"showDeleted":  {"true"},
		"singleEvents": {"false"},
		"maxResults":   {strconv.Itoa(pageSize)},
	}
}

// listEvents reads every page of an events.list, calling each for each
// event and done with the final page's sync token. syncing says the query
// is a Sync, so a 410 means its sync or page token expired. A listing that
// reads more than calstore.MaxSyncBytes of pages or more than
// calstore.MaxSyncEvents events gives calstore.ErrSyncTooLarge, and one
// whose server repeats a page token gives errPageRepeated, so a server
// cannot make a sync keep data without bound.
func (s *Store) listEvents(ctx context.Context, calID string, q url.Values, syncing bool, each func(calendar.Event, apiEvent), done func(syncToken string)) error {
	maxBytes, maxEvents := s.syncLimits()
	var read, events int
	seen := map[string]bool{}
	for range maxPages {
		data, err := s.do(ctx, call{method: "GET", path: eventsPath(calID), query: q, limit: maxPageBytes, sync: syncing})
		if err != nil {
			return err
		}
		if read += len(data); read > maxBytes {
			return calstore.ErrSyncTooLarge
		}
		var page eventsPage
		if err := json.Unmarshal(data, &page); err != nil {
			return errBadResponse
		}
		if events += len(page.Items); events > maxEvents {
			return calstore.ErrSyncTooLarge
		}
		for _, raw := range page.Items {
			ev, a, err := decodeEvent(raw)
			if err != nil {
				return err
			}
			each(ev, a)
		}
		if page.NextPageToken == "" {
			done(page.NextSyncToken)
			return nil
		}
		if seen[page.NextPageToken] {
			return errPageRepeated
		}
		seen[page.NextPageToken] = true
		q.Set("pageToken", page.NextPageToken)
	}
	return fmt.Errorf("%w: more than %d pages", calstore.ErrUnavailable, maxPages)
}

// errPageRepeated is a listing whose server handed out a page token it had
// already given, which would page for ever.
var errPageRepeated = fmt.Errorf("%w: page token repeated", calstore.ErrUnavailable)

// syncLimits returns the bytes and events one listing may read.
func (s *Store) syncLimits() (bytes, events int) {
	bytes, events = s.syncBytes, s.syncEvents
	if bytes <= 0 {
		bytes = calstore.MaxSyncBytes
	}
	if events <= 0 {
		events = calstore.MaxSyncEvents
	}
	return bytes, events
}
