package watch

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// errNoScoper means the account's store cannot say which folders a search
// covers, so a SyncAuto account keeps polling the last set it resolved.
var errNoScoper = errors.New("the account's store does not implement mailstore.Scoper")

// autoFolders is a SyncAuto account's polled set: the folders a default
// search covers, less the watched ones, in the store's order and without
// duplicates. It lists the folders first, since a store may answer
// SearchFolders from a folder list it cached earlier (imapstore does), and
// folders created or deleted since must show. It holds the store only
// while it runs.
func (s *Supervisor) autoFolders(ctx context.Context) ([]string, error) {
	st, release, err := s.o.Store(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	sc, ok := st.(mailstore.Scoper)
	if !ok {
		return nil, errNoScoper
	}
	if _, err := st.ListFolders(ctx); err != nil {
		return nil, err
	}
	folders, err := sc.SearchFolders(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		if !slices.Contains(s.o.Account.Watch, f) && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out, nil
}

// refreshAuto resolves a SyncAuto account's polled set again (autoFolders)
// and returns sched changed to match, keeping the schedule of each folder
// that stays. A folder that appears is due at once, and its first pass
// baselines it silently. The account's rows for folders it no longer syncs
// are dropped (dropUnsynced). When the set cannot be resolved, sched stays
// as it is, nothing is dropped, and the failure is logged without its text.
func (s *Supervisor) refreshAuto(ctx context.Context, sched []*polled) []*polled {
	want, err := s.autoFolders(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("resolving the folders to poll failed; polling the last set", "reason", failureReason(err))
		}
		return sched
	}
	have := make(map[string]*polled, len(sched))
	for _, pf := range sched {
		have[pf.folder] = pf
	}
	next := make([]*polled, 0, len(want))
	for _, f := range want {
		pf, ok := have[f]
		if !ok {
			s.logger.Info("polling a folder a default search covers", "folder", f)
			pf = &polled{folder: f}
			s.know(f, ModePoll)
		}
		next = append(next, pf)
	}
	s.dropUnsynced(ctx, want)
	s.setAutoSet(want)
	return next
}

// nextPoll is when the poller next has work: the first folder due, or on a
// SyncAuto account the next resolution of its set when that comes sooner.
// listedAt is when the set was last resolved.
func (s *Supervisor) nextPoll(sched []*polled, listedAt time.Time) time.Time {
	var next time.Time
	if s.o.Account.SyncAuto {
		next = listedAt.Add(s.settings.ExpungeSweep)
	}
	for _, pf := range sched {
		if next.IsZero() || pf.next.Before(next) {
			next = pf.next
		}
	}
	return next
}

// autoSet returns the polled set a SyncAuto account last resolved.
func (s *Supervisor) autoSet() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.auto)
}

// setAutoSet records the polled set a SyncAuto account resolved, for the
// next session's poller to start from.
func (s *Supervisor) setAutoSet(folders []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auto = slices.Clone(folders)
}

// dropUnsynced deletes the account's sync_folders rows, with their UIDs,
// for folders that are neither watched nor in want, and forgets them, so a
// parking does not write them back. That covers a folder that disappeared
// since the last resolution, one deleted while the daemon was down, and one
// an earlier sync.folders named. A failure is logged and left for the next
// resolution, which finds the row again.
func (s *Supervisor) dropUnsynced(ctx context.Context, want []string) {
	rows, err := s.o.State.Folders(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("listing sync state to drop unsynced folders failed", "reason", failureReason(err))
		}
		return
	}
	for _, r := range rows {
		if r.Account != s.o.Account.Name || slices.Contains(want, r.Folder) || slices.Contains(s.o.Account.Watch, r.Folder) {
			continue
		}
		if err := s.o.State.DropFolder(ctx, s.o.Account.Name, r.Folder); err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("dropping the sync state of a folder no longer synced failed", "folder", r.Folder, "reason", failureReason(err))
			}
			continue
		}
		s.forget(r.Folder)
		s.logger.Info("stopped syncing a folder a default search no longer covers", "folder", r.Folder)
	}
}
