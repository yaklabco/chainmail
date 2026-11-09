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
// is unchanged is not a change. The changes are admitted first (Admit):
// a skipped object's stored rows are not changes. The stored rows and
// their raw objects are read in one read transaction (readTx).
func (s *Store) Diff(ctx context.Context, account, calID string, ch calendar.Changes, self func(string) bool) ([]Change, error) {
	var (
		p    plan
		rows []EventRow
	)
	err := s.readTx(ctx, func(tx *sql.Tx) error {
		var err error
		if p, err = planChanges(ctx, tx, account, calID, ch, self); err != nil {
			return err
		}
		if s.rowsRead != nil {
			s.rowsRead()
		}
		var recs []storedRecord
		for _, c := range p.changes {
			if c.before != nil {
				recs = append(recs, storedRecord{account: account, calID: calID, r: *c.before})
			}
			if c.after != nil {
				recs = append(recs, storedRecord{account: account, calID: calID, r: *c.after})
			}
		}
		rows, err = decodeRecords(ctx, tx, recs, p.raws.bySum)
		return err
	})
	if err != nil {
		if errors.Is(err, errNoProviderID) || errors.Is(err, errStoredRow) {
			return nil, err
		}
		return nil, s.fail("diffing calendar changes", err)
	}
	out := make([]Change, 0, len(p.changes))
	for _, c := range p.changes {
		change := Change{Kind: c.kind}
		if c.before != nil {
			change.Before = &rows[0]
			rows = rows[1:]
		}
		if c.after != nil {
			change.After = &rows[0]
			rows = rows[1:]
		}
		out = append(out, change)
	}
	return out, nil
}

// Apply writes ch in one transaction with the new token and LastSync,
// clearing LastError. The calendar must be stored (SetCalendars), else
// ErrNotFound. Only the rows Diff would report are written, and each raw
// object once.
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
		p, err := planChanges(ctx, tx, account, calID, ch, self)
		if err != nil {
			return err
		}
		written := map[rawKey]bool{}
		for _, c := range p.changes {
			if c.kind == ChangeDeleted {
				if _, err := tx.ExecContext(ctx,
					"DELETE FROM cal_events WHERE account = ? AND calendar_id = ? AND provider_id = ? AND recurrence_id = ?",
					account, calID, c.before.providerID, c.before.recurrenceID); err != nil {
					return err
				}
				continue
			}
			if k := (rawKey{provider: c.after.providerID, sum: c.after.rawSum}); k.sum != "" && !written[k] {
				written[k] = true
				if _, err := tx.ExecContext(ctx,
					"INSERT OR IGNORE INTO cal_raws (account, calendar_id, provider_id, sum, raw) VALUES (?, ?, ?, ?, ?)",
					account, calID, k.provider, k.sum, p.raws.bySum[k.sum]); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT OR REPLACE INTO cal_events (account, calendar_id, "+eventColumns+") VALUES (?, ?, "+placeholders(eventColumnCount)+")",
				append([]any{account, calID}, c.after.args()...)...); err != nil {
				return err
			}
		}
		if len(p.changes) > 0 {
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM cal_raws WHERE account = ? AND calendar_id = ? AND "+unreferencedRaw, account, calID); err != nil {
				return err
			}
		}
		return nil
	})
}

// plan is what planChanges works out: the row changes, and the raw objects
// of the upserts by sum.
type plan struct {
	changes []rowChange
	raws    *rawSet
}

// planChanges works out the rows ch creates, updates and deletes in the
// calendar. ch is admitted first (Admit). The new state is the stored rows,
// less every row of a deleted ProviderID, every row of an upserted
// ProviderID when ch.ObjectReplace, and every row when ch.Full, with each
// upsert then put in its row's place. The rows of a skipped ProviderID are
// kept as they are.
func planChanges(ctx context.Context, q querier, account, calID string, ch calendar.Changes, self func(string) bool) (plan, error) {
	ch = Admit(ch)
	skipped := map[string]bool{}
	for _, p := range ch.Skipped {
		skipped[p] = true
	}
	raws := newRawSet()
	upserts := make([]record, 0, len(ch.Upserts))
	upserted := map[string]bool{}
	for _, ev := range ch.Upserts {
		if ev.ProviderID == "" {
			return plan{}, errNoProviderID
		}
		if skipped[ev.ProviderID] {
			continue
		}
		r, err := encode(account, calID, ev, self, raws)
		if err != nil {
			return plan{}, err
		}
		upserts = append(upserts, r)
		upserted[ev.ProviderID] = true
	}
	deleted := map[string]bool{}
	for _, p := range ch.Deletes {
		if !skipped[p] {
			deleted[p] = true
		}
	}

	var old map[rowKey]record
	var err error
	if ch.Full {
		old, err = loadRecords(ctx, q, account, calID, true, nil)
	} else {
		providers := slices.Sorted(maps.Keys(upserted))
		for p := range deleted {
			if !upserted[p] {
				providers = append(providers, p)
			}
		}
		if len(providers) == 0 {
			// An incremental sync that touches nothing, such as a poll that
			// only moves the token, changes no row.
			return plan{raws: raws}, nil
		}
		old, err = loadRecords(ctx, q, account, calID, false, providers)
	}
	if err != nil {
		return plan{}, err
	}

	next := make(map[rowKey]record, len(old)+len(upserts))
	for k, r := range old {
		if !skipped[k.provider] && (ch.Full || deleted[k.provider] || (ch.ObjectReplace && upserted[k.provider])) {
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
	return plan{changes: changes, raws: raws}, nil
}

// providerChunk bounds the ProviderIDs in one IN list, well under SQLite's
// variable limit.
const providerChunk = 500

// loadRecords reads the stored rows of the calendar: all of them when all
// is set, else those of the given ProviderIDs, which reads nothing when
// providers is empty.
func loadRecords(ctx context.Context, q querier, account, calID string, all bool, providers []string) (map[rowKey]record, error) {
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
	if all {
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
