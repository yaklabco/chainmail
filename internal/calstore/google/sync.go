package google

import (
	"context"
	"encoding/json"
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
			c.TimeZone = zoneName(it.TimeZone)
			out = append(out, c)
		}
		if page.NextPageToken == "" {
			return out, nil
		}
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

// Sync lists the calendar's events changed since token, or all of them when
// token is "". Each upsert replaces the row with its ProviderID and
// RecurrenceID (Changes.ObjectReplace is false). A token Google no longer
// accepts gives calstore.ErrSyncTokenInvalid; the caller syncs again from
// "".
func (s *Store) Sync(ctx context.Context, calID, token string) (calendar.Changes, error) {
	ch := calendar.Changes{Full: token == ""}
	q := listQuery()
	if token != "" {
		q.Set("syncToken", token)
	}
	err := s.listEvents(ctx, calID, q, true, func(ev calendar.Event, a apiEvent) {
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
	// A deleted series has no rows: drop the cancelled instances Google
	// lists with it, so they do not outlive their master in the cache.
	if len(ch.Deletes) > 0 {
		gone := make(map[string]bool, len(ch.Deletes))
		for _, id := range ch.Deletes {
			gone[id] = true
		}
		ch.Upserts = slices.DeleteFunc(ch.Upserts, func(ev calendar.Event) bool { return gone[ev.ProviderID] })
	}
	return ch, nil
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
// is a Sync, so a 410 means its sync or page token expired.
func (s *Store) listEvents(ctx context.Context, calID string, q url.Values, syncing bool, each func(calendar.Event, apiEvent), done func(syncToken string)) error {
	for range maxPages {
		data, err := s.do(ctx, call{method: "GET", path: eventsPath(calID), query: q, limit: maxPageBytes, sync: syncing})
		if err != nil {
			return err
		}
		var page eventsPage
		if err := json.Unmarshal(data, &page); err != nil {
			return errBadResponse
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
		q.Set("pageToken", page.NextPageToken)
	}
	return fmt.Errorf("%w: more than %d pages", calstore.ErrUnavailable, maxPages)
}
