package cache

import (
	"context"
	"errors"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
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
	uids        []uint32 // still to fetch, highest first
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
// nil, is on another folder or UIDVALIDITY, or predates a drop.
func (a *Account) backfillStep(ctx context.Context, w *walk) (*walk, bool, error) {
	a.wmu.Lock()
	epoch := a.epoch
	p, ok, err := a.nextBackfill(ctx)
	a.wmu.Unlock()
	if err != nil || !ok {
		return nil, false, err
	}
	if err := a.waitReady(ctx, true); err != nil {
		return w, false, err
	}
	if w == nil || w.folder != p.Folder || w.uidValidity != p.UIDValidity || w.epoch != epoch {
		uids, err := a.listBackfill(ctx, p)
		if err != nil {
			return nil, false, err
		}
		w = &walk{folder: p.Folder, uidValidity: p.UIDValidity, epoch: epoch, uids: uids}
	}
	batch := w.uids[:min(len(w.uids), max(1, a.settings().BackfillBatch))]
	var rows []Row
	if len(batch) > 0 {
		src := mailstore.Source{Folder: w.folder, UIDValidity: w.uidValidity}
		if rows, err = a.fetchHeaders(ctx, src, batch); err != nil {
			return w, false, err
		}
	}
	return a.commitBatch(ctx, w, batch, rows)
}

// commitBatch stores a fetched batch, counts it against the budget, and
// records the walk's new low UID, marking the folder done when the walk has
// no UIDs left. A batch fetched before a drop is discarded with its walk.
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
	if !ok || p.UIDValidity != w.uidValidity {
		return nil, true, nil
	}
	if len(rows) > 0 {
		if err := a.o.Store.PutRows(ctx, rows); err != nil {
			return w, false, err
		}
		if _, err := a.o.Store.AddBudget(ctx, a.name, a.day(), int64(len(rows))*headerBudgetBytes); err != nil {
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
	w.uids = rest
	if p.BackfillDone {
		a.logger.InfoContext(ctx, "cache backfill done", "folder", w.folder)
	}
	return w, true, nil
}

// nextBackfill returns the progress of the first tracked folder whose
// backfill is not done: the account's watched folders in config order,
// then the rest in name order. The caller holds wmu.
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
	for _, f := range order {
		if p, ok := progress[f]; ok && !p.BackfillDone {
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
