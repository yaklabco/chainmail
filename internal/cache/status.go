package cache

import (
	"context"

	"github.com/jamesainslie/chainmail/internal/api"
)

// Cache states, as AccountCacheStatus and FolderCacheStatus report them.
const (
	StateBackfilling     = "backfilling"
	StatePrefetching     = "prefetching"
	StateReady           = "ready"
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
	// was a cache fault), paused, over_cap, budget_exhausted, backfilling,
	// prefetching, ready.
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
	// State is paused, backfilling, prefetching or ready.
	State string
}

// Status reports the account's cache state for CacheService: settings,
// today's budget, the latest failure, and each tracked folder's progress,
// row counts and state. A synced folder without progress yet reports a
// zero progress with its sync row's UIDVALIDITY and UIDNEXT.
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
	// An empty window (bodies only on read) has nothing to prefetch, so it
	// counts as complete. The prefetcher never sets window_done for it, and
	// Status only derives the state: WindowCovers still answers false.
	emptyWindow := s.PrefetchBodies <= 0
	paused, backfilling, prefetching := false, len(rows) == 0, false
	for _, r := range rows {
		f := FolderStatus{FolderStats: byFolder[r.Folder]}
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
	case a.overCap.Load():
		st.State = StateOverCap
	case st.BudgetUsed >= s.DailyDownload:
		st.State = StateBudgetExhausted
	case backfilling:
		st.State = StateBackfilling
	case prefetching:
		st.State = StatePrefetching
	default:
		st.State = StateReady
	}
	return st, nil
}
