package cache

import (
	"cmp"
	"context"
	"time"

	"github.com/jamesainslie/chainmail/internal/watch"
)

// Reasons the cache cannot answer completely, as AccountCount's
// incomplete_reason reports them.
const (
	ReasonBackfilling     = "backfilling"
	ReasonStale           = "stale"
	ReasonFolderNotSynced = "folder_not_synced"
	// ReasonTextOutsideWindow: the query has text the cache holds bodies
	// for only inside the prefetch window, and nothing keeps it there.
	ReasonTextOutsideWindow = "text_outside_window"
)

// Freshness reports whether every folder is fresh (spec 6.1 check 1), the
// first failing reason (stale, backfilling or folder_not_synced), and the
// oldest last_sync_at among them.
//
// A folder is fresh when it has a sync_folders row with a UIDVALIDITY
// (else folder_not_synced), is watching at any age or polling with a pass,
// or a poll whose STATUS confirmed the checkpoint, within twice sync.poll,
// and has been synced by a pass since the run
// started, AccountOptions.Started (else stale), and has finished its
// backfill under that UIDVALIDITY (else backfilling), checked in that
// order and in folder order. The account is then stale while the builder
// has an event of the account older than fresh_lag, or a trimmed gap,
// still to apply, or has not opened its group.
func (a *Account) Freshness(ctx context.Context, folders []string) (ok bool, reason string, syncedAt time.Time, err error) {
	rows, err := a.syncRows(ctx)
	if err != nil {
		return false, "", time.Time{}, err
	}
	byFolder := make(map[string]watch.FolderRow, len(rows))
	for _, r := range rows {
		byFolder[r.Folder] = r
	}
	progress, err := a.progressMap(ctx)
	if err != nil {
		return false, "", time.Time{}, err
	}
	now := a.clock.Now()
	seen := false
	for _, f := range folders {
		r, synced := byFolder[f]
		if !synced {
			reason = cmp.Or(reason, ReasonFolderNotSynced)
			continue
		}
		if !seen || r.LastSyncAt.Before(syncedAt) {
			syncedAt, seen = r.LastSyncAt, true
		}
		p, tracked := progress[f]
		switch {
		case !a.syncFresh(r, now):
			reason = cmp.Or(reason, ReasonStale)
		case !tracked || p.UIDValidity != r.UIDValidity || !p.BackfillDone:
			reason = cmp.Or(reason, ReasonBackfilling)
		}
	}
	if reason == "" {
		lag, err := a.lagging(ctx, now.Add(-a.settings().FreshLag))
		if err != nil {
			return false, "", time.Time{}, err
		}
		if lag {
			reason = ReasonStale
		}
	}
	return reason == "", reason, syncedAt, nil
}

// syncFresh reports whether SP6 keeps the folder current: watching at any
// age, or polling with a successful pass, or a poll whose STATUS confirmed
// the checkpoint, within twice sync.poll (amendment 8), and in either state
// synced by a pass of this run. The
// state survives a restart, but the mail that arrived while the daemon was
// down reaches the log only through this run's first pass, which stamps
// last_sync_at.
func (a *Account) syncFresh(r watch.FolderRow, now time.Time) bool {
	if r.LastSyncAt.IsZero() || r.LastSyncAt.Before(a.started) {
		return false
	}
	switch r.State {
	case watch.StateWatching:
		return true
	case watch.StatePolling:
		return now.Sub(r.LastSyncAt) <= 2*a.o.Account.SyncPoll
	default:
		return false
	}
}

// lagging reports whether the builder has not opened its group yet, or has
// an event of the account still to apply that is older than cutoff or is a
// trimmed gap. A trimmed gap means events were lost, so it is stale at once,
// though Read stamps it with the current time. A Read examines at most 5000
// events, so an empty page short of the tail reads on from its Next, up to
// the tail; a ctx that has ended before a further read reports stale.
func (a *Account) lagging(ctx context.Context, cutoff time.Time) (bool, error) {
	applied := a.applied.Load()
	if applied < 0 {
		return true, nil
	}
	tail, err := a.o.Log.Tail(ctx)
	if err != nil {
		return false, err
	}
	for after := applied; ; {
		if after != applied {
			select {
			case <-ctx.Done():
				return true, nil
			default:
			}
		}
		page, err := a.o.Log.Read(ctx, watch.Query{After: after, Filter: watch.Filter{Accounts: []string{a.name}}, Limit: 1})
		if err != nil {
			return false, err
		}
		if len(page.Events) > 0 {
			e := page.Events[0]
			return (e.Kind == watch.KindGap && e.Reason == watch.GapTrimmed) || e.At.Before(cutoff), nil
		}
		if page.Next >= tail || page.Next <= after {
			return false, nil
		}
		after = page.Next
	}
}

// InWindow reports whether since, a query's top-level lower date bound,
// is inside the prefetch window, so the cache can hold every body text
// after it needs once the window is fetched. A zero since (no bound) or an
// empty window never is.
func (a *Account) InWindow(since time.Time) bool {
	window := a.settings().PrefetchBodies
	return !since.IsZero() && window > 0 && !since.Before(a.clock.Now().Add(-window))
}

// WindowCovers reports whether since is inside the prefetch window and
// every folder has window_done. A zero since (no lower date bound) or an
// empty window never covers.
func (a *Account) WindowCovers(ctx context.Context, folders []string, since time.Time) (bool, error) {
	if !a.InWindow(since) {
		return false, nil
	}
	progress, err := a.progressMap(ctx)
	if err != nil {
		return false, err
	}
	for _, f := range folders {
		if p, ok := progress[f]; !ok || !p.WindowDone {
			return false, nil
		}
	}
	return true, nil
}
