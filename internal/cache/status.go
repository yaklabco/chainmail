package cache

import (
	"context"

	"github.com/jamesainslie/chainmail/internal/api"
)

// Cache states, as AccountCacheStatus and FolderCacheStatus report them.
const (
	StateBackfilling = "backfilling"
	StatePrefetching = "prefetching"
	StateReady       = "ready"
	// StateIncomplete replaces ready for a folder with messages recorded
	// as unreadable, and for an account with such a folder.
	StateIncomplete      = "incomplete"
	StateBudgetExhausted = "budget_exhausted"
	StatePaused          = "paused"
	StateDisabled        = "disabled"
	StateError           = "error"
	StateOverCap         = "over_cap"
)

// AccountStatus is an account's cache state for CacheService.
type AccountStatus struct {
	Account string
	Enabled bool
	// State is the most pressing of: disabled, error (the latest failure
	// was a cache fault), paused, budget_exhausted (less than one header of
	// the day's budget left, or the server throttling the account, so
	// background work waits for 00:00 UTC),
	// backfilling, prefetching, incomplete (a folder is), ready. over_cap
	// is file-wide: the daemon applies the Store's OverCap over ready,
	// incomplete and prefetching. incomplete is the least pressing state
	// short of ready because it lasts: work still to do, which ends on its
	// own, is shown first.
	State       string
	BudgetUsed  int64 // today's, UTC
	BudgetBytes int64
	// LastError is the api reason code of the latest failed step, "" once
	// it succeeds again. It is never server text.
	LastError string
	Folders   []FolderStatus
}

// FolderStatus is one tracked folder's cache state. Progress and
// FolderStats both have a Folder field, so use Progress.Folder.
type FolderStatus struct {
	Progress
	FolderStats
	// State is paused, backfilling, prefetching, incomplete or ready.
	// incomplete replaces ready when Unreadable is not 0.
	State string
	// Unreadable counts the folder's messages recorded as unreadable,
	// which the cache has no row for. AttachmentsUnknown counts its rows
	// read without their body structure.
	Unreadable         uint64
	AttachmentsUnknown uint64
}

// Status reports the account's cache state for CacheService: settings,
// today's budget, the latest failure, and each tracked folder's progress,
// row counts, counts of messages not read whole, and state. A synced
// folder without progress yet reports a zero progress with its sync row's
// UIDVALIDITY and UIDNEXT.
func (a *Account) Status(ctx context.Context) (AccountStatus, error) {
	s := a.settings()
	st := AccountStatus{Account: a.name, Enabled: s.Enabled, BudgetBytes: s.DailyDownload, LastError: a.lastError()}
	var err error
	if st.BudgetUsed, err = a.o.Store.Budget(ctx, a.name, a.day()); err != nil {
		return AccountStatus{}, err
	}
	rows, err := a.syncRows(ctx)
	if err != nil {
		return AccountStatus{}, err
	}
	progress, err := a.progressMap(ctx)
	if err != nil {
		return AccountStatus{}, err
	}
	stats, err := a.o.Store.Stats(ctx, a.name, a.clock.Now().Add(-s.PrefetchBodies))
	if err != nil {
		return AccountStatus{}, err
	}
	byFolder := make(map[string]FolderStats, len(stats))
	for _, fs := range stats {
		byFolder[fs.Folder] = fs
	}
	counts, err := a.o.Store.Incomplete(ctx, a.name)
	if err != nil {
		return AccountStatus{}, err
	}
	incomplete := make(map[string]FolderIncomplete, len(counts))
	for _, c := range counts {
		incomplete[c.Folder] = c
	}
	// An empty window (bodies only on read) has nothing to prefetch, so it
	// counts as complete. The prefetcher never sets window_done for it, and
	// Status only derives the state: WindowCovers still answers false.
	emptyWindow := s.PrefetchBodies <= 0
	paused, backfilling, prefetching, unreadable := false, len(rows) == 0, false, false
	for _, r := range rows {
		c := incomplete[r.Folder]
		f := FolderStatus{FolderStats: byFolder[r.Folder], Unreadable: c.Unreadable, AttachmentsUnknown: c.AttachmentsUnknown}
		f.FolderStats.Folder = r.Folder
		p, ok := progress[r.Folder]
		if !ok {
			p = Progress{Account: a.name, Folder: r.Folder, UIDValidity: r.UIDValidity, BaselineUIDNext: r.UIDNext}
		}
		f.Progress = p
		switch {
		case pausedState(r.State):
			f.State, paused = StatePaused, true
		case !p.BackfillDone:
			f.State, backfilling = StateBackfilling, true
		case !p.WindowDone && !emptyWindow:
			f.State, prefetching = StatePrefetching, true
		case f.Unreadable > 0:
			f.State, unreadable = StateIncomplete, true
		default:
			f.State = StateReady
		}
		st.Folders = append(st.Folders, f)
	}
	switch {
	case !s.Enabled:
		st.State = StateDisabled
	case st.LastError == api.ReasonInternal:
		st.State = StateError
	case paused:
		st.State = StatePaused
	case headerAllowance(st.BudgetUsed, s.DailyDownload) < 1, a.throttled.wait(a.clock.Now()) > 0:
		st.State = StateBudgetExhausted
	case backfilling:
		st.State = StateBackfilling
	case prefetching:
		st.State = StatePrefetching
	case unreadable:
		st.State = StateIncomplete
	default:
		st.State = StateReady
	}
	return st, nil
}
