package cache

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/watch"
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

// buildStep opens the group while pos is negative, then reads the
// messages recorded as unreadable again, once per run, and otherwise
// applies the events after pos. It returns the new position.
func (a *Account) buildStep(ctx context.Context, pos int64) (int64, error) {
	if pos < 0 {
		return a.openGroup(ctx)
	}
	if !a.reread {
		if err := a.rereadUnreadable(ctx); err != nil {
			return pos, err
		}
		a.reread = true
		return pos, nil
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
	a.setApplied(acked)
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
	q := watch.Query{After: pos, Filter: watch.Filter{Accounts: []string{a.name}, Internal: true}, Limit: builderPage}
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
	a.setApplied(next)
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
	// rewalk lists, per folder, the new messages fetchNew skipped because
	// the folder was in error, for commitSegment to record a new walk.
	rewalk map[string]skipped
	// unreadable lists, per folder and UIDVALIDITY, the new messages to
	// record as unreadable: those of unreadable events, and those whose
	// headers fetchNew could not read.
	unreadable map[mailstore.Source][]uint32
}

// skipped is the highest UID of a folder's new messages that the builder
// skipped while the folder was in error, under its UIDVALIDITY.
type skipped struct {
	uidValidity, high uint32
}

// change is one flags or expunged event to apply.
type change struct {
	kind   watch.Kind
	ref    ref.Ref
	flags  []string
	labels []string
}

// planSegment sorts a segment's events into fetches, unreadable messages
// and changes. An event of a folder the cache does not track, or whose ref
// has another UIDVALIDITY than the folder's progress, is skipped: backfill,
// or the folder's gap, covers it. Status events change nothing in the
// cache.
func (a *Account) planSegment(ctx context.Context, seg []watch.Event, progress map[string]Progress) segmentPlan {
	p := segmentPlan{fetch: map[string][]uint32{}, touched: map[string]bool{}, unreadable: map[mailstore.Source][]uint32{}}
	for i := range seg {
		e := &seg[i]
		switch e.Kind {
		case watch.KindGap:
			p.gap = e
			continue
		case watch.KindNew, watch.KindFlags, watch.KindExpunged, watch.KindUnreadable:
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
		switch e.Kind {
		case watch.KindNew:
			p.fetch[r.Folder] = append(p.fetch[r.Folder], r.UID)
			continue
		case watch.KindUnreadable:
			src := mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}
			p.unreadable[src] = append(p.unreadable[src], r.UID)
			continue
		}
		p.changes = append(p.changes, change{kind: e.Kind, ref: r, flags: e.Flags, labels: e.Labels})
	}
	return p
}

// apply applies one segment: it plans against the current progress,
// fetches new headers with no lock held, then writes under wmu. A drop
// that ran during the fetch makes it plan and fetch again. Once the rows
// are written, it reads the new messages' invite parts, with wmu
// released, and hands them to OnInvites (readInvites). A segment applied
// again after a failure reads them again: OnInvites must be idempotent. A
// failure to queue the deferred parts (OnDeferredInvites) fails the
// segment, which is then applied again.
func (a *Account) apply(ctx context.Context, seg []watch.Event) error {
	if len(seg) == 0 {
		return nil
	}
	for {
		a.lock()
		epoch := a.epoch.Load()
		progress, err := a.progressMap(ctx)
		a.unlock()
		if err != nil {
			return err
		}
		plan := a.planSegment(ctx, seg, progress)
		got, err := a.fetchNew(ctx, plan.fetch, progress)
		if err != nil {
			return err
		}
		plan.rewalk = got.rewalk
		for src, uids := range got.unreadable {
			plan.unreadable[src] = append(plan.unreadable[src], uids...)
		}
		done, err := a.commitSegment(ctx, epoch, plan, got.rows)
		if err != nil {
			return err
		}
		if done {
			return a.readInvites(ctx, got.invites, got.tooLarge)
		}
	}
}

// newMail is what fetchNew read of a segment's new messages.
type newMail struct {
	rows []Row
	// unreadable lists, per folder and UIDVALIDITY, the messages whose
	// headers could not be read.
	unreadable map[mailstore.Source][]uint32
	// rewalk lists the folders skipped in error, as segmentPlan.rewalk.
	rewalk map[string]skipped
	// invites and tooLarge are the rows' invite parts to read and the
	// messages whose invite parts are over the limit, as headerBatch has
	// them.
	invites  []inviteCandidate
	tooLarge int
}

// fetchNew fetches the headers of a segment's new messages, one batch per
// folder, once no folder of the account is paused. A folder whose
// UIDVALIDITY changed since its events is skipped: its gap follows. A
// message the fetch no longer finds is left out: its expunged event
// follows. A message whose headers cannot be read is returned as
// unreadable (fetchIsolating). A folder whose sync state is error is
// skipped alone (integration note 12): its new messages are not fetched,
// and it is returned with the highest UID skipped, so commitSegment
// records a new walk of the folder before its events are acknowledged.
func (a *Account) fetchNew(ctx context.Context, fetch map[string][]uint32, progress map[string]Progress) (newMail, error) {
	if len(fetch) == 0 {
		return newMail{}, nil
	}
	if err := a.waitReady(ctx); err != nil {
		return newMail{}, err
	}
	synced, err := a.syncRows(ctx)
	if err != nil {
		return newMail{}, err
	}
	errored := erroredFolders(synced)
	out := newMail{unreadable: map[mailstore.Source][]uint32{}, rewalk: map[string]skipped{}}
	for _, folder := range slices.Sorted(maps.Keys(fetch)) {
		if errored[folder] {
			a.logger.InfoContext(ctx, "cache builder skipped new mail of a folder in error", "folder", folder)
			out.rewalk[folder] = skipped{uidValidity: progress[folder].UIDValidity, high: slices.Max(fetch[folder])}
			continue
		}
		src := mailstore.Source{Folder: folder, UIDValidity: progress[folder].UIDValidity}
		got, err := a.fetchIsolating(ctx, src, fetch[folder])
		if errors.Is(err, mailstore.ErrStaleRef) {
			continue
		}
		if err != nil {
			return newMail{}, err
		}
		out.rows = append(out.rows, got.rows...)
		out.invites = append(out.invites, got.invites...)
		out.tooLarge += got.tooLarge
		if len(got.unreadable) > 0 {
			out.unreadable[src] = got.unreadable
		}
	}
	return out, nil
}

// commitSegment writes a planned segment: the fetched rows, then the
// messages it could not read as unreadable, then the changes in log order,
// then a new walk of each folder whose new mail was skipped in error, then
// the touched folders' LastAppliedAt.
// fetchHeaders has already counted the headers against the budget. It
// reports false, having written nothing, when a drop ran since epoch.
// Every write is idempotent, so a failure part way is safe to retry. A
// closing gap is applied last. Each change is noted in the backfill's
// in-flight batch, so a header fetched before it is fetched again instead
// of written, and in every read guard, so a download that predates it is
// not written over it. A new row inside the prefetch window without a body clears
// its folder's window_done in the same transaction (spec 4.1) and wakes
// the prefetcher, so a text search never answers complete without that
// body. Stored rows wake the evictor when the file is over max_size.
func (a *Account) commitSegment(ctx context.Context, epoch uint64, plan segmentPlan, rows []Row) (bool, error) {
	a.lock()
	defer a.unlock()
	if a.epoch.Load() != epoch {
		return false, nil
	}
	if len(rows) > 0 {
		uncovered, err := a.o.Store.PutNewRows(ctx, rows, a.windowStart())
		if err != nil {
			return false, err
		}
		if uncovered {
			a.pokePrefetch()
		}
		a.noteWrite(ctx)
	}
	if err := a.recordUnreadable(ctx, plan.unreadable); err != nil {
		return false, err
	}
	for _, c := range plan.changes {
		a.noteChange(c.ref)
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
	if err := a.recordRewalks(ctx, plan.rewalk); err != nil {
		return false, err
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

// stampApplied sets LastAppliedAt to now on each folder's progress, and
// nothing else, so it never undoes a window_done clear the evictor or the
// prefetcher makes without wmu. A folder without progress is skipped. The
// caller holds wmu.
func (a *Account) stampApplied(ctx context.Context, folders map[string]bool) error {
	now := a.clock.Now()
	for _, folder := range slices.Sorted(maps.Keys(folders)) {
		if err := a.o.Store.SetLastApplied(ctx, a.name, folder, now); err != nil {
			return err
		}
	}
	return nil
}

// windowStart is where the account's prefetch window starts now, or the
// zero time when it has none (bodies only on read).
func (a *Account) windowStart() time.Time {
	window := a.settings().PrefetchBodies
	if window <= 0 {
		return time.Time{}
	}
	return a.clock.Now().Add(-window)
}

// noteWrite wakes the evictor when the file is over max_size after a write
// that stored headers, as the body writers do (spec section 5). Headers are
// never evicted: the sweep removes bodies only. A failed size check is
// logged as a reason code and fails nothing.
func (a *Account) noteWrite(ctx context.Context) {
	if err := a.o.Store.noteWrite(ctx, a.o.MaxSize()); err != nil && ctx.Err() == nil {
		a.logger.WarnContext(ctx, "checking the cache size", "reason", reasonCode(err))
		a.noteErr(err)
	}
}

// recordRewalks records, for each folder whose new mail fetchNew skipped
// in error, a new walk from above the highest skipped UID down: the
// baseline rises to cover it, and the backfill starts over below it once
// the folder leaves error. Rows already cached are upserted again, so
// nothing is lost, and Freshness reports backfilling until the walk ends.
// A folder whose progress is gone or has another UIDVALIDITY is left to
// its gap. The caller holds wmu.
func (a *Account) recordRewalks(ctx context.Context, rewalk map[string]skipped) error {
	recorded := false
	for _, folder := range slices.Sorted(maps.Keys(rewalk)) {
		s := rewalk[folder]
		p, ok, err := a.o.Store.Progress(ctx, a.name, folder)
		if err != nil {
			return err
		}
		if !ok || p.UIDValidity != s.uidValidity {
			continue
		}
		p.BaselineUIDNext = max(p.BaselineUIDNext, s.high+1)
		if err := a.o.Store.RestartBackfill(ctx, a.name, folder, p.BaselineUIDNext); err != nil {
			return err
		}
		a.logger.InfoContext(ctx, "cache backfill walks a folder again for new mail skipped in error",
			"folder", folder, "baseline_uidnext", p.BaselineUIDNext)
		recorded = true
	}
	if recorded {
		a.poke()
	}
	return nil
}

// recordUnreadable records each folder's unreadable UIDs. MarkUnreadable
// skips a folder whose progress is gone or has another UIDVALIDITY: its
// gap or drop has removed what the records would describe. The caller
// holds wmu.
func (a *Account) recordUnreadable(ctx context.Context, unreadable map[mailstore.Source][]uint32) error {
	for _, src := range slices.SortedFunc(maps.Keys(unreadable), bySource) {
		uids := unreadable[src]
		if err := a.o.Store.MarkUnreadable(ctx, a.name, src, uids); err != nil {
			return err
		}
		a.logger.InfoContext(ctx, "cache recorded messages it cannot read", "folder", src.Folder, "count", len(uids))
	}
	return nil
}

// bySource orders sources by folder, then UIDVALIDITY.
func bySource(a, b mailstore.Source) int {
	return cmp.Or(strings.Compare(a.Folder, b.Folder), cmp.Compare(a.UIDValidity, b.UIDValidity))
}
