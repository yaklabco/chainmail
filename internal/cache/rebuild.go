package cache

import (
	"context"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// applyGap applies the gap that closes a segment. A uidvalidity gap resets
// its folder, keeping each dropped row's Message-ID for ref recovery. A
// trimmed gap means the builder fell behind the log's retention, so the
// account's cache is rebuilt (amendment 4). The caller holds wmu.
func (a *Account) applyGap(ctx context.Context, e watch.Event) error {
	switch e.Reason {
	case watch.GapUIDValidity:
		return a.resetFolderLocked(ctx, e.Folder, true)
	case watch.GapTrimmed:
		a.logger.WarnContext(ctx, "cache builder fell behind the event log's retention; rebuilding the account's cache")
		_, err := a.rebuildLocked(ctx, "")
		return err
	default:
		return nil
	}
}

// recoverGroup handles a group acknowledged past the log's tail, which a
// reset of state.db under a kept cache.db leaves: the cache's rows follow
// a log that no longer exists. The account's cache is rebuilt, then the
// group moved back to the tail read first, so a crash between the two
// rebuilds again and no event published since is skipped. It returns the
// position to read from.
func (a *Account) recoverGroup(ctx context.Context, acked int64) (int64, error) {
	tail, err := a.o.Log.Tail(ctx)
	if err != nil {
		return 0, err
	}
	if acked <= tail {
		return acked, nil
	}
	a.logger.WarnContext(ctx, "cache group is past the event log's tail; rebuilding the account's cache")
	a.wmu.Lock()
	_, err = a.rebuildLocked(ctx, "")
	a.wmu.Unlock()
	if err != nil {
		return 0, err
	}
	moved, err := a.o.Log.ResetGroup(ctx, a.group, acked, tail)
	if err != nil {
		return 0, err
	}
	if !moved {
		now, _, err := a.o.Log.Group(ctx, a.group)
		return now, err
	}
	return tail, nil
}

// resetFolderLocked drops the folder's cached rows and records a new
// baseline from its sync_folders row, so backfill walks the folder again.
// keepRecovery keeps each dropped row's Message-ID for ref recovery. A
// folder without a sync row, or one without a UIDVALIDITY, is left
// untracked. The caller holds wmu.
func (a *Account) resetFolderLocked(ctx context.Context, folder string, keepRecovery bool) error {
	if err := a.o.Store.DropFolder(ctx, a.name, folder, keepRecovery); err != nil {
		return err
	}
	a.epoch++
	row, ok, err := a.o.State.Folder(ctx, a.name, folder)
	if err != nil {
		return err
	}
	if ok && row.UIDValidity != 0 {
		p := Progress{Account: a.name, Folder: folder, UIDValidity: row.UIDValidity, BaselineUIDNext: row.UIDNext}
		if err := a.o.Store.PutProgress(ctx, p); err != nil {
			return err
		}
	}
	a.poke()
	a.logger.InfoContext(ctx, "cache folder reset; backfill starts again", "folder", folder)
	return nil
}

// rebuildLocked resets folder, or with folder "" drops every cached row of
// the account and records a fresh baseline for each folder it tracks. It
// returns the folders reset. A folder the account does not track is a
// *mailstore.FolderNotFoundError. The caller holds wmu.
func (a *Account) rebuildLocked(ctx context.Context, folder string) ([]string, error) {
	rows, err := a.syncRows(ctx)
	if err != nil {
		return nil, err
	}
	if folder != "" {
		if !slices.ContainsFunc(rows, func(r watch.FolderRow) bool { return r.Folder == folder }) {
			return nil, &mailstore.FolderNotFoundError{Name: folder, Requested: folder}
		}
		if err := a.resetFolderLocked(ctx, folder, false); err != nil {
			return nil, err
		}
		return []string{folder}, nil
	}
	if err := a.o.Store.DropAccount(ctx, a.name); err != nil {
		return nil, err
	}
	a.epoch++
	folders := make([]string, 0, len(rows))
	for _, r := range rows {
		p := Progress{Account: a.name, Folder: r.Folder, UIDValidity: r.UIDValidity, BaselineUIDNext: r.UIDNext}
		if err := a.o.Store.PutProgress(ctx, p); err != nil {
			return nil, err
		}
		folders = append(folders, r.Folder)
	}
	a.poke()
	a.logger.InfoContext(ctx, "cache rebuilt; backfill starts again", "folders", len(folders))
	return folders, nil
}
