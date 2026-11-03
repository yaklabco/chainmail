package cache

import (
	"context"
	"errors"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// errNoBackfillSource means a backfill search returned no result for the
// one folder it named.
var errNoBackfillSource = errors.New("the backfill search returned no result for its folder")

// walk is the backfill's pass down one folder: the UIDs below where it
// started, listed once when the walk starts, less those already stored.
type walk struct {
	folder      string
	uidValidity uint32
	epoch       uint64
	baseline    uint32   // the progress BaselineUIDNext the walk was listed under
	low         uint32   // the progress BackfillLowUID the walk last left
	uids        []uint32 // still to fetch, highest first
}

// matches reports whether p is still the progress the walk left: the
// builder records a new walk of a folder by raising its baseline and
// resetting its low UID.
func (w *walk) matches(p Progress) bool {
	return w.baseline == p.BaselineUIDNext && w.low == p.BackfillLowUID
}

// runBackfill fetches the headers of mail older than each folder's
// baseline, one batch at a time, until ctx ends. It pauses backfill_pause
// between batches, waits for a poke when no folder needs work (or a folder
// changed UIDVALIDITY under its walk, until the builder resets it), and
// backs off 1s doubling to 5m after a failure.
func (a *Account) runBackfill(ctx context.Context) {
	var w *walk
	failures := 0
	for ctx.Err() == nil {
		next, more, err := a.backfillStep(ctx, w)
		w = next
		if err != nil && !errors.Is(err, mailstore.ErrStaleRef) {
			if ctx.Err() != nil {
				return
			}
			a.fail(ctx, "cache backfill step failed", err, failures >= 2)
			if !a.sleep(ctx, workBackoff(failures)) {
				return
			}
			failures++
			continue
		}
		if failures > 0 {
			a.clearError()
			failures = 0
		}
		var live bool
		if more {
			live = a.sleep(ctx, a.settings().BackfillPause)
		} else {
			live = a.idle(ctx)
		}
		if !live {
			return
		}
	}
}

// idle waits until a folder is tracked or reset, and reports whether ctx
// is still live.
func (a *Account) idle(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-a.wake:
		return true
	}
}

// backfillStep fetches one batch of the first folder whose backfill is not
// done, and reports whether there may be more to do. It starts a new walk,
// listing the folder's UIDs below where the last batch stopped, when w is
// nil, is on another folder or UIDVALIDITY, predates a drop, or no longer
// matches the folder's progress because the builder recorded a new walk.
func (a *Account) backfillStep(ctx context.Context, w *walk) (*walk, bool, error) {
	a.wmu.Lock()
	epoch := a.epoch
	p, ok, err := a.nextBackfill(ctx)
	if err == nil && ok {
		a.inflight = &inflightBatch{folder: p.Folder, uidValidity: p.UIDValidity, changed: map[uint32]bool{}}
	}
	a.wmu.Unlock()
	if err != nil || !ok {
		return nil, false, err
	}
	defer a.endInflight()
	allowance, err := a.waitAllowance(ctx)
	if err != nil {
		return w, false, err
	}
	if w == nil || w.folder != p.Folder || w.uidValidity != p.UIDValidity || w.epoch != epoch || !w.matches(p) {
		uids, err := a.listBackfill(ctx, p)
		if err != nil {
			return nil, false, err
		}
		w = &walk{folder: p.Folder, uidValidity: p.UIDValidity, epoch: epoch, baseline: p.BaselineUIDNext, low: p.BackfillLowUID, uids: uids}
	}
	batch := w.uids[:min(len(w.uids), max(1, a.settings().BackfillBatch), allowance)]
	var rows []Row
	if len(batch) > 0 {
		src := mailstore.Source{Folder: w.folder, UIDValidity: w.uidValidity}
		if rows, err = a.fetchHeaders(ctx, src, batch); err != nil {
			return w, false, err
		}
	}
	return a.commitBatch(ctx, w, batch, rows)
}

// commitBatch stores a fetched batch, which fetchHeaders has already
// counted against the budget, and records the walk's new low UID, marking
// the folder done when the walk has no UIDs left. A batch fetched before a
// drop, or before the builder recorded a new walk of the folder, is
// discarded with its walk. A batch with a UID whose flags or
// expunge the builder applied during the fetch is not written: it returns
// w unchanged, so the next step fetches the batch again.
func (a *Account) commitBatch(ctx context.Context, w *walk, batch []uint32, rows []Row) (*walk, bool, error) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if a.epoch != w.epoch {
		return nil, true, nil
	}
	p, ok, err := a.o.Store.Progress(ctx, a.name, w.folder)
	if err != nil {
		return w, false, err
	}
	if !ok || p.UIDValidity != w.uidValidity || !w.matches(p) {
		return nil, true, nil
	}
	if a.inflight.hit(batch) {
		a.logger.InfoContext(ctx, "cache backfill fetches a batch again after a change during its fetch", "folder", w.folder)
		return w, true, nil
	}
	if len(rows) > 0 {
		if err := a.o.Store.PutRows(ctx, rows); err != nil {
			return w, false, err
		}
	}
	rest := w.uids[len(batch):]
	if len(batch) > 0 {
		p.BackfillLowUID = batch[len(batch)-1]
	}
	p.BackfillDone = len(rest) == 0
	if err := a.o.Store.PutProgress(ctx, p); err != nil {
		return w, false, err
	}
	w.uids, w.low = rest, p.BackfillLowUID
	if p.BackfillDone {
		a.logger.InfoContext(ctx, "cache backfill done", "folder", w.folder)
	}
	return w, true, nil
}

// nextBackfill returns the progress of the first tracked folder whose
// backfill is not done: the account's watched folders in config order,
// then the rest in name order. A folder whose sync state is error is
// skipped alone (integration note 12): the status event of its recovery
// wakes the backfill again. The caller holds wmu.
func (a *Account) nextBackfill(ctx context.Context) (Progress, bool, error) {
	rows, err := a.syncRows(ctx)
	if err != nil {
		return Progress{}, false, err
	}
	progress, err := a.progressMap(ctx)
	if err != nil {
		return Progress{}, false, err
	}
	synced := func(folder string) bool {
		return slices.ContainsFunc(rows, func(r watch.FolderRow) bool { return r.Folder == folder })
	}
	order := make([]string, 0, len(rows))
	for _, f := range a.o.Account.Watch {
		if synced(f) && !slices.Contains(order, f) {
			order = append(order, f)
		}
	}
	for _, r := range rows {
		if !slices.Contains(order, r.Folder) {
			order = append(order, r.Folder)
		}
	}
	errored := erroredFolders(rows)
	for _, f := range order {
		if p, ok := progress[f]; ok && !p.BackfillDone && !errored[f] {
			return p, true, nil
		}
	}
	return Progress{}, false, nil
}

// listBackfill lists the folder's UIDs, highest first, below where its
// walk resumes: the lowest UID done, or the baseline before the first
// batch. Messages at or above the baseline arrive through the builder.
func (a *Account) listBackfill(ctx context.Context, p Progress) ([]uint32, error) {
	below := p.BackfillLowUID
	if below == 0 {
		below = p.BaselineUIDNext
	}
	req := mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{p.Folder: {UIDValidity: p.UIDValidity, Below: below}}}
	var uids []uint32
	err := a.retryBusy(ctx, func() error {
		return a.withMail(ctx, func(bg context.Context, st MailStore) error {
			results, err := st.Search(bg, req)
			if err != nil {
				return err
			}
			for _, res := range results {
				if res.Source.Folder != p.Folder {
					continue
				}
				if res.Err != nil {
					return res.Err
				}
				uids = res.UIDs
				return nil
			}
			return errNoBackfillSource
		})
	})
	return uids, err
}

// inflightBatch records the flags and expunged changes the builder applies
// to one folder while the backfill fetches a batch of it with wmu
// released. The epoch catches a folder drop in that window; this catches a
// change to one message, which a write of the fetched header would undo:
// an expunged message would come back as a ghost row, a flag change would
// be lost, and no later event would repair either.
type inflightBatch struct {
	folder      string
	uidValidity uint32
	changed     map[uint32]bool
}

// note records that the builder applied a flags or expunged change to r.
// It records nothing on a nil batch or for another folder or UIDVALIDITY.
func (b *inflightBatch) note(r ref.Ref) {
	if b != nil && b.folder == r.Folder && b.uidValidity == r.UIDValidity {
		b.changed[r.UID] = true
	}
}

// hit reports whether a change was noted for any of uids.
func (b *inflightBatch) hit(uids []uint32) bool {
	return b != nil && slices.ContainsFunc(uids, func(uid uint32) bool { return b.changed[uid] })
}

// endInflight forgets the backfill's in-flight batch once its step ends.
func (a *Account) endInflight() {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	a.inflight = nil
}
