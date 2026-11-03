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
)

// Freshness reports whether every folder is fresh (spec 6.1 check 1), the
// first failing reason (stale, backfilling or folder_not_synced), and the
// oldest last_sync_at among them.
//
// A folder is fresh when it has a sync_folders row with a UIDVALIDITY
// (else folder_not_synced), is watching at any age or polling with a pass
// within twice sync.poll (else stale), and has finished its backfill under
// that UIDVALIDITY (else backfilling), checked in that order and in folder
// order. The account is then stale while the builder has an event of the
// account older than fresh_lag still to apply, or has not opened its group.
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
// age, or polling with a successful pass within twice sync.poll
// (amendment 8).
func (a *Account) syncFresh(r watch.FolderRow, now time.Time) bool {
	switch r.State {
	case watch.StateWatching:
		return true
	case watch.StatePolling:
		return !r.LastSyncAt.IsZero() && now.Sub(r.LastSyncAt) <= 2*a.o.Account.SyncPoll
	default:
		return false
	}
}

// lagging reports whether the builder has not opened its group yet, or has
// an event of the account older than cutoff still to apply.
func (a *Account) lagging(ctx context.Context, cutoff time.Time) (bool, error) {
	applied := a.applied.Load()
	if applied < 0 {
		return true, nil
	}
	page, err := a.o.Log.Read(ctx, watch.Query{After: applied, Filter: watch.Filter{Accounts: []string{a.name}}, Limit: 1})
	if err != nil {
		return false, err
	}
	return len(page.Events) > 0 && page.Events[0].At.Before(cutoff), nil
}

// WindowCovers reports whether since is inside the prefetch window and
// every folder has window_done. A zero since (no lower date bound) or an
// empty window never covers.
func (a *Account) WindowCovers(ctx context.Context, folders []string, since time.Time) (bool, error) {
	window := a.settings().PrefetchBodies
	if since.IsZero() || window <= 0 || since.Before(a.clock.Now().Add(-window)) {
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
