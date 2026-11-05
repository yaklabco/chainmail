package cache

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

const (
	// rereadChunk bounds the UIDs of one reread fetch. It is under
	// mailstore.IsolatedBatch, so the store isolates every message of a
	// chunk it still cannot read, at no more than a few logins, and never
	// gives up on the chunk.
	rereadChunk = 4
	// rereadLimit bounds the records one start reads again, those
	// attempted longest ago first, so many records never hold the builder
	// for long. The rest wait for later starts.
	rereadLimit = 200
)

// rereadBatch is one reread fetch: up to rereadChunk recorded UIDs of one
// folder.
type rereadBatch struct {
	src  mailstore.Source
	uids []uint32
}

// rereadUnreadable fetches the messages recorded as unreadable once more,
// as the builder starts, so a daemon upgraded with a reader that parses
// them heals itself. It takes the rereadLimit records attempted longest
// ago and fetches them rereadChunk at a time. A message now read is
// stored, which deletes its record. One still unreadable counts another
// attempt. One its folder no longer holds loses its record. A folder in
// error, or one whose answers cannot be parsed at all, waits for a later
// start, and one whose UIDVALIDITY changed is left to its gap: the rest of
// that folder's records are skipped for this start. It waits while the
// account is paused. A failure returns with the chunks not yet committed
// kept, so the builder's retry resumes there and counts no attempt twice.
func (a *Account) rereadUnreadable(ctx context.Context) error {
	if !a.rereadLoaded {
		recorded, err := a.o.Store.OldestUnreadable(ctx, a.name, rereadLimit)
		if err != nil {
			return err
		}
		a.rereadQueue, a.rereadLoaded = rereadBatches(recorded), true
	}
	if len(a.rereadQueue) == 0 {
		return nil
	}
	if err := a.waitReady(ctx); err != nil {
		return err
	}
	synced, err := a.syncRows(ctx)
	if err != nil {
		return err
	}
	skip := erroredFolders(synced)
	for len(a.rereadQueue) > 0 {
		b := a.rereadQueue[0]
		if !skip[b.src.Folder] {
			epoch := a.writeEpoch()
			rows, unreadable, err := a.fetchIsolating(ctx, b.src, b.uids)
			switch {
			case errors.Is(err, mailstore.ErrStaleRef), errors.Is(err, mailstore.ErrNotFound):
				// Left to the folder's gap or drop.
				skip[b.src.Folder] = true
			case errors.Is(err, mailstore.ErrMalformed):
				// A fault in the folder, not in a message: the store
				// isolates every message of a chunk this small. The
				// reread is best effort, so the folder's records wait for
				// a later start rather than hold the builder.
				skip[b.src.Folder] = true
				a.logger.WarnContext(ctx, "cache could not read a folder's unreadable messages again; it tries at a later start",
					"folder", b.src.Folder, "reason", reasonCode(err))
			case err != nil:
				return err
			default:
				if err := a.commitReread(ctx, epoch, b, rows, unreadable); err != nil {
					return err
				}
			}
		}
		a.rereadQueue = a.rereadQueue[1:]
	}
	return nil
}

// rereadBatches groups records by folder and UIDVALIDITY, then cuts each
// group's UIDs, ascending, into batches of rereadChunk.
func rereadBatches(recorded []UnreadableMessage) []rereadBatch {
	bySrc := map[mailstore.Source][]uint32{}
	for _, m := range recorded {
		bySrc[m.Source] = append(bySrc[m.Source], m.UID)
	}
	out := []rereadBatch{}
	for _, src := range slices.SortedFunc(maps.Keys(bySrc), bySource) {
		for chunk := range slices.Chunk(slices.Sorted(slices.Values(bySrc[src])), rereadChunk) {
			out = append(out, rereadBatch{src: src, uids: chunk})
		}
	}
	return out
}

// commitReread writes what rereadUnreadable read of one batch: the rows
// read, another attempt for the UIDs still unreadable, and the end of the
// records of UIDs the folder no longer holds. It writes nothing when a
// drop ran since epoch. A new row inside the prefetch window wakes the
// prefetcher, as the builder's do.
func (a *Account) commitReread(ctx context.Context, epoch uint64, b rereadBatch, rows []Row, unreadable []uint32) error {
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
	if err := a.o.Store.MarkUnreadable(ctx, a.name, b.src, unreadable); err != nil {
		return err
	}
	seen := make(map[uint32]bool, len(rows)+len(unreadable))
	for _, r := range rows {
		seen[r.UID] = true
	}
	for _, uid := range unreadable {
		seen[uid] = true
	}
	gone := slices.DeleteFunc(slices.Clone(b.uids), func(uid uint32) bool { return seen[uid] })
	if err := a.o.Store.ForgetUnreadable(ctx, a.name, b.src, gone); err != nil {
		return err
	}
	if len(rows) > 0 || len(gone) > 0 {
		a.logger.InfoContext(ctx, "cache read unreadable messages again", "folder", b.src.Folder,
			"read", len(rows), "gone", len(gone))
	}
	return nil
}
