package cache

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// rereadUnreadable fetches every message recorded as unreadable once more,
// as the builder starts, so a daemon upgraded with a reader that parses
// them heals itself. A message now read is stored, which deletes its
// record. One still unreadable counts another attempt. One its folder no
// longer holds loses its record. A folder in error waits for the next run,
// and one whose UIDVALIDITY changed is left to its gap. It waits while the
// account is paused.
func (a *Account) rereadUnreadable(ctx context.Context) error {
	recorded, err := a.o.Store.UnreadableUIDs(ctx, a.name)
	if err != nil || len(recorded) == 0 {
		return err
	}
	if err := a.waitReady(ctx); err != nil {
		return err
	}
	synced, err := a.syncRows(ctx)
	if err != nil {
		return err
	}
	errored := erroredFolders(synced)
	for _, src := range slices.SortedFunc(maps.Keys(recorded), bySource) {
		if errored[src.Folder] {
			continue
		}
		uids := recorded[src]
		epoch := a.writeEpoch()
		rows, unreadable, err := a.fetchIsolating(ctx, src, uids)
		if errors.Is(err, mailstore.ErrStaleRef) || errors.Is(err, mailstore.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if err := a.commitReread(ctx, epoch, src, uids, rows, unreadable); err != nil {
			return err
		}
	}
	return nil
}

// commitReread writes what rereadUnreadable read of src's recorded uids:
// the rows read, another attempt for the UIDs still unreadable, and the
// end of the records of UIDs the folder no longer holds. It writes nothing
// when a drop ran since epoch. A new row inside the prefetch window wakes
// the prefetcher, as the builder's do.
func (a *Account) commitReread(ctx context.Context, epoch uint64, src mailstore.Source, uids []uint32, rows []Row, unreadable []uint32) error {
	a.lock()
	defer a.unlock()
	if a.epoch.Load() != epoch {
		return nil
	}
	if len(rows) > 0 {
		uncovered, err := a.o.Store.PutNewRows(ctx, rows, a.windowStart())
		if err != nil {
			return err
		}
		if uncovered {
			a.pokePrefetch()
		}
		a.noteWrite(ctx)
	}
	if err := a.o.Store.MarkUnreadable(ctx, a.name, src, unreadable); err != nil {
		return err
	}
	seen := make(map[uint32]bool, len(rows)+len(unreadable))
	for _, r := range rows {
		seen[r.UID] = true
	}
	for _, uid := range unreadable {
		seen[uid] = true
	}
	gone := slices.DeleteFunc(slices.Clone(uids), func(uid uint32) bool { return seen[uid] })
	if err := a.o.Store.ForgetUnreadable(ctx, a.name, src, gone); err != nil {
		return err
	}
	a.logger.InfoContext(ctx, "cache read its unreadable messages again", "folder", src.Folder,
		"read", len(rows), "unreadable", len(unreadable), "gone", len(gone))
	return nil
}
