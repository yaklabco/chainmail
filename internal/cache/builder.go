package cache

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// builderPage is how many of the account's events one builder read takes.
const builderPage = 100

// runBuilder applies the account's events until ctx ends. A failed step is
// retried from the same position after a backoff of 1s doubling to 5m.
// Nothing is acknowledged before it is applied, so a failure loses nothing.
func (a *Account) runBuilder(ctx context.Context) {
	pos := int64(-1)
	failures := 0
	for ctx.Err() == nil {
		next, err := a.buildStep(ctx, pos)
		if err == nil {
			pos = next
			if failures > 0 {
				a.clearError()
				failures = 0
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		a.fail(ctx, "cache builder step failed", err, failures >= 2)
		if !a.sleep(ctx, workBackoff(failures)) {
			return
		}
		failures++
	}
}

// buildStep opens the group while pos is negative, and otherwise applies
// the events after pos. It returns the new position.
func (a *Account) buildStep(ctx context.Context, pos int64) (int64, error) {
	if pos < 0 {
		return a.openGroup(ctx)
	}
	return a.build(ctx, pos)
}

// openGroup opens the account's group, creating it at the log's tail on
// first use, then records a baseline for every synced folder the cache
// does not track yet, in that order (amendment 5). It returns the group's
// acknowledged ID.
func (a *Account) openGroup(ctx context.Context) (int64, error) {
	acked, _, err := a.o.Log.Group(ctx, a.group)
	if err != nil {
		return 0, err
	}
	if acked, err = a.recoverGroup(ctx, acked); err != nil {
		return 0, err
	}
	if a.afterGroup != nil {
		a.afterGroup()
	}
	if err := a.track(ctx); err != nil {
		return 0, err
	}
	a.applied.Store(acked)
	return acked, nil
}

// build applies the account's next events after pos, acknowledges them,
// and returns the new position. It tracks newly synced folders first, so a
// folder's events find its progress. Caught up, it waits for the log to
// grow and returns pos.
func (a *Account) build(ctx context.Context, pos int64) (int64, error) {
	if err := a.track(ctx); err != nil {
		return pos, err
	}
	q := watch.Query{After: pos, Filter: watch.Filter{Accounts: []string{a.name}}, Limit: builderPage}
	page, err := a.o.Log.Read(ctx, q)
	if err != nil {
		return pos, err
	}
	if len(page.Events) == 0 && page.Next == pos {
		return pos, a.o.Log.Wait(ctx, pos)
	}
	seg, next := segment(page)
	if err := a.apply(ctx, seg); err != nil {
		return pos, err
	}
	if err := a.o.Log.Ack(ctx, a.group, next); err != nil {
		return pos, err
	}
	a.applied.Store(next)
	// A folder skipped in error comes back with a status event; wake the
	// backfill so it looks again.
	if slices.ContainsFunc(seg, func(e watch.Event) bool { return e.Kind == watch.KindStatus }) {
		a.poke()
	}
	return next, nil
}

// segment returns the events of page up to and including its first gap,
// and the position to acknowledge once they are applied: the gap's ID (for
// a trimmed gap, the ID before its NextID), or page.Next when the page has
// none. A gap resets folders, so the events after it are planned again,
// against the progress the gap leaves.
func segment(page watch.Page) ([]watch.Event, int64) {
	for i, e := range page.Events {
		if e.Kind != watch.KindGap {
			continue
		}
		if e.Reason == watch.GapTrimmed {
			return page.Events[:i+1], e.NextID - 1
		}
		return page.Events[:i+1], e.ID
	}
	return page.Events, page.Next
}

// segmentPlan is what one segment asks of the cache.
type segmentPlan struct {
	// fetch lists, per folder, the UIDs of new messages to fetch.
	fetch map[string][]uint32
	// changes are the flags and expunged events to apply, in log order.
	changes []change
	// touched are the folders with an event to apply, for LastAppliedAt.
	touched map[string]bool
	// gap is the gap that closes the segment, or nil.
	gap *watch.Event
}

// change is one flags or expunged event to apply.
type change struct {
	kind   watch.Kind
	ref    ref.Ref
	flags  []string
	labels []string
}

// planSegment sorts a segment's events into fetches and changes. An event
// of a folder the cache does not track, or whose ref has another
// UIDVALIDITY than the folder's progress, is skipped: backfill, or the
// folder's gap, covers it. Status events change nothing in the cache.
func (a *Account) planSegment(ctx context.Context, seg []watch.Event, progress map[string]Progress) segmentPlan {
	p := segmentPlan{fetch: map[string][]uint32{}, touched: map[string]bool{}}
	for i := range seg {
		e := &seg[i]
		switch e.Kind {
		case watch.KindGap:
			p.gap = e
			continue
		case watch.KindNew, watch.KindFlags, watch.KindExpunged:
		default:
			continue
		}
		r, err := ref.Parse(e.Ref)
		if err != nil {
			a.logger.WarnContext(ctx, "cache builder skipped an event with an unreadable ref", "event", e.ID)
			continue
		}
		pr, ok := progress[r.Folder]
		if !ok || r.Account != a.name || pr.UIDValidity != r.UIDValidity {
			continue
		}
		p.touched[r.Folder] = true
		if e.Kind == watch.KindNew {
			p.fetch[r.Folder] = append(p.fetch[r.Folder], r.UID)
			continue
		}
		p.changes = append(p.changes, change{kind: e.Kind, ref: r, flags: e.Flags, labels: e.Labels})
	}
	return p
}

// apply applies one segment: it plans against the current progress,
// fetches new headers with no lock held, then writes under wmu. A drop
// that ran during the fetch makes it plan and fetch again.
func (a *Account) apply(ctx context.Context, seg []watch.Event) error {
	if len(seg) == 0 {
		return nil
	}
	for {
		a.wmu.Lock()
		epoch := a.epoch
		progress, err := a.progressMap(ctx)
		a.wmu.Unlock()
		if err != nil {
			return err
		}
		plan := a.planSegment(ctx, seg, progress)
		rows, err := a.fetchNew(ctx, plan.fetch, progress)
		if err != nil {
			return err
		}
		done, err := a.commitSegment(ctx, epoch, plan, rows)
		if err != nil || done {
			return err
		}
	}
}

// fetchNew fetches the headers of a segment's new messages, one batch per
// folder, once no folder of the account is paused. A folder whose
// UIDVALIDITY changed since its events is skipped: its gap follows. A
// message the fetch no longer finds is left out: its expunged event
// follows. A folder whose sync state is error is skipped alone
// (integration note 12): its new messages are not fetched, and its
// events are acknowledged like those of an untracked folder.
func (a *Account) fetchNew(ctx context.Context, fetch map[string][]uint32, progress map[string]Progress) ([]Row, error) {
	if len(fetch) == 0 {
		return nil, nil
	}
	if err := a.waitReady(ctx); err != nil {
		return nil, err
	}
	synced, err := a.syncRows(ctx)
	if err != nil {
		return nil, err
	}
	errored := erroredFolders(synced)
	var rows []Row
	for _, folder := range slices.Sorted(maps.Keys(fetch)) {
		if errored[folder] {
			a.logger.InfoContext(ctx, "cache builder skipped new mail of a folder in error", "folder", folder)
			continue
		}
		src := mailstore.Source{Folder: folder, UIDValidity: progress[folder].UIDValidity}
		got, err := a.fetchHeaders(ctx, src, fetch[folder])
		if errors.Is(err, mailstore.ErrStaleRef) {
			continue
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	return rows, nil
}

// commitSegment writes a planned segment: the fetched rows, then the
// changes in log order, then the touched folders' LastAppliedAt.
// fetchHeaders has already counted the headers against the budget. It
// reports false, having written nothing, when a drop ran since epoch.
// Every write is idempotent, so a failure part way is safe to retry. A
// closing gap is applied last. Each change is noted in the backfill's
// in-flight batch, so a header fetched before it is fetched again instead
// of written.
func (a *Account) commitSegment(ctx context.Context, epoch uint64, plan segmentPlan, rows []Row) (bool, error) {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if a.epoch != epoch {
		return false, nil
	}
	if len(rows) > 0 {
		if err := a.o.Store.PutRows(ctx, rows); err != nil {
			return false, err
		}
	}
	for _, c := range plan.changes {
		a.inflight.note(c.ref)
		var err error
		if c.kind == watch.KindFlags {
			_, err = a.o.Store.SetFlags(ctx, c.ref, c.flags, c.labels)
		} else {
			err = a.o.Store.DeleteRow(ctx, c.ref)
		}
		if err != nil {
			return false, err
		}
	}
	if err := a.stampApplied(ctx, plan.touched); err != nil {
		return false, err
	}
	if plan.gap != nil {
		if err := a.applyGap(ctx, *plan.gap); err != nil {
			return false, err
		}
	}
	return true, nil
}

// stampApplied sets LastAppliedAt to now on each folder's progress. The
// caller holds wmu.
func (a *Account) stampApplied(ctx context.Context, folders map[string]bool) error {
	now := a.clock.Now()
	for _, folder := range slices.Sorted(maps.Keys(folders)) {
		p, ok, err := a.o.Store.Progress(ctx, a.name, folder)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		p.LastAppliedAt = now
		if err := a.o.Store.PutProgress(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
