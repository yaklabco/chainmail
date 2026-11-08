package calcache

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/yaklabco/chainmail/internal/calendar"
)

// ChangeKind is what a sync did to one stored event row.
type ChangeKind string

// ChangeKind values.
const (
	ChangeCreated ChangeKind = "created"
	ChangeUpdated ChangeKind = "updated"
	ChangeDeleted ChangeKind = "deleted"
)

// Change is one row a sync creates, updates or deletes. Before is nil for
// a create and After is nil for a delete.
type Change struct {
	Kind          ChangeKind
	Before, After *EventRow
}

// errNoProviderID refuses an upsert a backend sent without a ProviderID.
var errNoProviderID = errors.New("calendar event without a provider ID")

// rowKey is a stored row's key inside one calendar.
type rowKey struct {
	provider string
	rid      string // watch.FormatTime of the recurrence ID, "" for singles and masters
}

func compareKeys(a, b rowKey) int {
	if c := cmp.Compare(a.provider, b.provider); c != 0 {
		return c
	}
	return cmp.Compare(a.rid, b.rid)
}

// rowChange is one planned change in stored form.
type rowChange struct {
	kind          ChangeKind
	before, after *record
}

// querier is what loading the stored rows needs: the database or a
// transaction.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Diff compares ch with the stored rows of the calendar, without writing.
// Changes come sorted by ProviderID and then recurrence ID. A row whose
// stored form, including the etag, raw object, MyPartStat and IsOrganizer,
// is unchanged is not a change.
func (s *Store) Diff(ctx context.Context, account, calID string, ch calendar.Changes, self func(string) bool) ([]Change, error) {
	changes, err := planChanges(ctx, s.db, account, calID, ch, self)
	if err != nil {
		if errors.Is(err, errNoProviderID) {
			return nil, err
		}
		return nil, s.fail("diffing calendar changes", err)
	}
	out := make([]Change, 0, len(changes))
	for _, c := range changes {
		change := Change{Kind: c.kind}
		if c.before != nil {
			row, err := c.before.decode(account, calID)
			if err != nil {
				return nil, err
			}
			change.Before = &row
		}
		if c.after != nil {
			row, err := c.after.decode(account, calID)
			if err != nil {
				return nil, err
			}
			change.After = &row
		}
		out = append(out, change)
	}
	return out, nil
}

// Apply writes ch in one transaction with the new token and LastSync,
// clearing LastError. The calendar must be stored (SetCalendars), else
// ErrNotFound. Only the rows Diff would report are written.
func (s *Store) Apply(ctx context.Context, account, calID string, ch calendar.Changes, self func(string) bool, syncedAt time.Time) error {
	return s.inTx(ctx, "applying calendar changes", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"UPDATE calendars SET sync_token = ?, last_sync = ?, last_error = '' WHERE account = ? AND calendar_id = ?",
			ch.Token, formatTime(syncedAt), account, calID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return ErrNotFound
		}
		changes, err := planChanges(ctx, tx, account, calID, ch, self)
		if err != nil {
			return err
		}
		for _, c := range changes {
			if c.kind == ChangeDeleted {
				if _, err := tx.ExecContext(ctx,
					"DELETE FROM cal_events WHERE account = ? AND calendar_id = ? AND provider_id = ? AND recurrence_id = ?",
					account, calID, c.before.providerID, c.before.recurrenceID); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT OR REPLACE INTO cal_events (account, calendar_id, "+eventColumns+") VALUES (?, ?, "+placeholders(eventColumnCount)+")",
				append([]any{account, calID}, c.after.args()...)...); err != nil {
				return err
			}
		}
		return nil
	})
}

// planChanges works out the rows ch creates, updates and deletes in the
// calendar. The new state is the stored rows, less every row of a deleted
// ProviderID, every row of an upserted ProviderID when ch.ObjectReplace,
// and every row when ch.Full, with each upsert then put in its row's place.
func planChanges(ctx context.Context, q querier, account, calID string, ch calendar.Changes, self func(string) bool) ([]rowChange, error) {
	upserts := make([]record, 0, len(ch.Upserts))
	upserted := map[string]bool{}
	for _, ev := range ch.Upserts {
		if ev.ProviderID == "" {
			return nil, errNoProviderID
		}
		r, err := encode(account, calID, ev, self)
		if err != nil {
			return nil, err
		}
		upserts = append(upserts, r)
		upserted[ev.ProviderID] = true
	}
	deleted := map[string]bool{}
	for _, p := range ch.Deletes {
		deleted[p] = true
	}

	var old map[rowKey]record
	var err error
	if ch.Full {
		old, err = loadRecords(ctx, q, account, calID, nil)
	} else {
		providers := slices.Sorted(maps.Keys(upserted))
		for p := range deleted {
			if !upserted[p] {
				providers = append(providers, p)
			}
		}
		old, err = loadRecords(ctx, q, account, calID, providers)
	}
	if err != nil {
		return nil, err
	}

	next := make(map[rowKey]record, len(old)+len(upserts))
	for k, r := range old {
		if ch.Full || deleted[k.provider] || (ch.ObjectReplace && upserted[k.provider]) {
			continue
		}
		next[k] = r
	}
	for _, r := range upserts {
		next[r.key()] = r
	}

	all := map[rowKey]bool{}
	for k := range old {
		all[k] = true
	}
	for k := range next {
		all[k] = true
	}
	var changes []rowChange
	for _, k := range slices.SortedFunc(maps.Keys(all), compareKeys) {
		before, hadBefore := old[k]
		after, hasAfter := next[k]
		switch {
		case hadBefore && !hasAfter:
			changes = append(changes, rowChange{kind: ChangeDeleted, before: &before})
		case !hadBefore && hasAfter:
			changes = append(changes, rowChange{kind: ChangeCreated, after: &after})
		case before != after:
			changes = append(changes, rowChange{kind: ChangeUpdated, before: &before, after: &after})
		}
	}
	return changes, nil
}

// providerChunk bounds the ProviderIDs in one IN list, well under SQLite's
// variable limit.
const providerChunk = 500

// loadRecords reads the stored rows of the calendar, all of them when
// providers is nil, else those of the given ProviderIDs.
func loadRecords(ctx context.Context, q querier, account, calID string, providers []string) (map[rowKey]record, error) {
	out := map[rowKey]record{}
	read := func(query string, args ...any) error {
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }() // rows.Err below reports read errors
		for rows.Next() {
			var acct, cal string
			r, err := scanRecord(rows, &acct, &cal)
			if err != nil {
				return err
			}
			out[r.key()] = r
		}
		return rows.Err()
	}
	base := "SELECT account, calendar_id, " + eventColumns + " FROM cal_events WHERE account = ? AND calendar_id = ?"
	if providers == nil {
		if err := read(base, account, calID); err != nil {
			return nil, err
		}
		return out, nil
	}
	for chunk := range slices.Chunk(providers, providerChunk) {
		args := []any{account, calID}
		for _, p := range chunk {
			args = append(args, p)
		}
		if err := read(base+" AND provider_id IN ("+placeholders(len(chunk))+")", args...); err != nil {
			return nil, fmt.Errorf("reading stored events: %w", err)
		}
	}
	return out, nil
}
