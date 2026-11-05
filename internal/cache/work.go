package cache

import (
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// sleep waits d on the account's clock and reports whether ctx is still
// live. A d of zero or less returns at once.
func (a *Account) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-a.clock.After(d):
		return true
	}
}

// poke wakes an idle backfill without blocking.
func (a *Account) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// pokePrefetch wakes an idle prefetcher without blocking.
func (a *Account) pokePrefetch() {
	select {
	case a.prefetchWake <- struct{}{}:
	default:
	}
}

// pausedState reports whether a folder in sync state s pauses the
// account's cache work. A folder in error does not: the backfill and the
// builder skip that folder alone (master plan integration note 12), and a
// test pins that the other folders keep working.
func pausedState(s watch.Status) bool {
	switch s {
	case watch.StateAuthRequired, watch.StateReconnecting:
		return true
	default:
		return false
	}
}

// erroredFolders returns the folders of rows whose sync state is error.
// The workers skip each alone, leaving the account's other folders
// working (master plan integration note 12).
func erroredFolders(rows []watch.FolderRow) map[string]bool {
	out := map[string]bool{}
	for _, r := range rows {
		if r.State == watch.StateError {
			out[r.Folder] = true
		}
	}
	return out
}

// paused reports whether any of the account's folders is in a sync state
// that pauses cache work.
func (a *Account) paused(ctx context.Context) (bool, error) {
	rows, err := a.o.State.Folders(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(rows, func(r watch.FolderRow) bool {
		return r.Account == a.name && pausedState(r.State)
	}), nil
}

// waitReady blocks while the account is paused, checking again every
// backfill_pause and at least every second. It returns ctx's error when
// ctx ends first.
func (a *Account) waitReady(ctx context.Context) error {
	for {
		paused, err := a.paused(ctx)
		if err != nil {
			return err
		}
		if !paused {
			return nil
		}
		if !a.sleep(ctx, max(a.settings().BackfillPause, pauseRecheck)) {
			return ctx.Err()
		}
	}
}

// headerAllowance is how many headers the day's remaining download budget
// holds when used of limit bytes are spent, never negative. Background work
// proceeds only while it is at least one: the backfill's admission and
// Status's budget_exhausted both use it.
func headerAllowance(used, limit int64) int64 {
	return max(0, (limit-used)/headerBudgetBytes)
}

// waitAllowance blocks as waitReady does, and also while the day's
// remaining download budget holds less than one header, or while the
// server throttles the account, until 00:00 UTC. It returns how many
// headers the remaining budget holds, at least one, which bounds the next
// background batch.
func (a *Account) waitAllowance(ctx context.Context) (int, error) {
	logged := false
	for {
		if err := a.waitReady(ctx); err != nil {
			return 0, err
		}
		if d := a.throttled.wait(a.clock.Now()); d > 0 {
			if !a.sleep(ctx, d) {
				return 0, ctx.Err()
			}
			continue
		}
		used, err := a.o.Store.Budget(ctx, a.name, a.day())
		if err != nil {
			return 0, err
		}
		limit := a.settings().DailyDownload
		if n := headerAllowance(used, limit); n >= 1 {
			return int(min(n, math.MaxInt32)), nil
		}
		if !logged {
			a.logger.InfoContext(ctx, "cache download budget spent; background work resumes at 00:00 UTC",
				"used_bytes", used, "budget_bytes", limit)
			logged = true
		}
		if !a.sleep(ctx, untilUTCMidnight(a.clock.Now())) {
			return 0, ctx.Err()
		}
	}
}

// noteThrottle records that the server refused a backfill or prefetch
// fetch for a quota or bandwidth limit: the backfill and the prefetcher
// wait until 00:00 UTC, as for a spent budget. It logs when that starts a
// pause.
func (a *Account) noteThrottle(ctx context.Context) {
	if a.throttled.set(a.clock.Now()) {
		a.logger.InfoContext(ctx, logThrottled)
	}
}

// logThrottled is the info log line of a throttle that starts a pause.
const logThrottled = "the mail server throttled background downloads; cache background work resumes at 00:00 UTC"

// withMail runs f with a store Mail hands out and a background context,
// so the store never waits for a connection a foreground request needs,
// and releases the store when f returns.
func (a *Account) withMail(ctx context.Context, f func(context.Context, MailStore) error) error {
	st, release, err := a.o.Mail(ctx)
	if err != nil {
		return err
	}
	defer release()
	return f(mailstore.WithBackground(ctx), st)
}

// retryBusy runs f until it fails with something other than
// mailstore.ErrBusy, waiting backfill_pause (at least 50ms) after each
// ErrBusy, so foreground requests get the connections first.
func (a *Account) retryBusy(ctx context.Context, f func() error) error {
	for {
		err := f()
		if !errors.Is(err, mailstore.ErrBusy) {
			return err
		}
		if !a.sleep(ctx, max(a.settings().BackfillPause, busyRetryFloor)) {
			return ctx.Err()
		}
	}
}

// fetchHeaders fetches the headers of uids in src as cache rows, on a
// background context, retrying while the pool is busy. UIDs the folder no
// longer holds are left out. Every header received counts 2048 bytes
// against the day's budget at once, whether or not its row is written, so
// a fetch whose result is discarded is still accounted for. The UIDs of a
// *mailstore.UnreadableError come back as unreadable beside the rows read,
// with no error, so the caller records them and moves on.
func (a *Account) fetchHeaders(ctx context.Context, src mailstore.Source, uids []uint32) ([]Row, []uint32, error) {
	var (
		rows       []Row
		unreadable []uint32
	)
	err := a.retryBusy(ctx, func() error {
		var err error
		rows, unreadable, err = a.fetchHeadersOnce(ctx, src, uids)
		return err
	})
	return rows, unreadable, err
}

// fetchHeadersOnce is one attempt of fetchHeaders, with no retry: it fails
// with mailstore.ErrBusy when the pool is busy. A mailstore.ErrMalformed it
// returns is plain: the store could not isolate the messages it could not
// read, and nothing it read is kept.
func (a *Account) fetchHeadersOnce(ctx context.Context, src mailstore.Source, uids []uint32) ([]Row, []uint32, error) {
	var (
		rows       []Row
		unreadable []uint32
	)
	err := a.withMail(ctx, func(bg context.Context, st MailStore) error {
		hs, err := st.FetchHeaders(bg, src, uids)
		if ue, ok := errors.AsType[*mailstore.UnreadableError](err); ok {
			unreadable, err = ue.UIDs, nil
		}
		if err != nil {
			return err
		}
		rows = make([]Row, len(hs))
		for i, h := range hs {
			rows[i] = RowFromHeader(h)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(rows) > 0 {
		if _, err := a.o.Store.AddBudget(ctx, a.name, a.day(), int64(len(rows))*headerBudgetBytes); err != nil {
			return nil, nil, err
		}
	}
	return rows, unreadable, nil
}

// fetchIsolating is fetchHeaders that also copes with a plain
// mailstore.ErrMalformed, which the store returns when it could not
// isolate the messages it could not read: it fetches each half of uids in
// turn, until the store isolates them. A plain ErrMalformed from a fetch
// of mailstore.IsolatedBatch UIDs or fewer, which the store always
// isolates, is a fault in the folder and is returned.
func (a *Account) fetchIsolating(ctx context.Context, src mailstore.Source, uids []uint32) ([]Row, []uint32, error) {
	rows, unreadable, err := a.fetchHeaders(ctx, src, uids)
	if !errors.Is(err, mailstore.ErrMalformed) || len(uids) <= mailstore.IsolatedBatch {
		return rows, unreadable, err
	}
	mid := len(uids) / 2
	for _, half := range [][]uint32{uids[:mid], uids[mid:]} {
		r, u, err := a.fetchIsolating(ctx, src, half)
		if err != nil {
			return nil, nil, err
		}
		rows, unreadable = append(rows, r...), append(unreadable, u...)
	}
	return rows, unreadable, nil
}

// track records a baseline for each folder the account syncs that the
// cache does not track yet: its sync_folders UIDVALIDITY and current
// UIDNEXT (amendment 5). The builder's group must already exist, so mail at
// or above the baseline arrives as events after the group's position and
// backfill covers the rest. A tracked folder whose sync_folders row is
// gone, because the folder no longer exists or is no longer synced, is
// dropped from the cache.
func (a *Account) track(ctx context.Context) error {
	rows, err := a.syncRows(ctx)
	if err != nil {
		return err
	}
	a.lock()
	defer a.unlock()
	have, err := a.progressMap(ctx)
	if err != nil {
		return err
	}
	synced := make(map[string]bool, len(rows))
	added := false
	for _, r := range rows {
		synced[r.Folder] = true
		if _, ok := have[r.Folder]; ok {
			continue
		}
		p := Progress{Account: a.name, Folder: r.Folder, UIDValidity: r.UIDValidity, BaselineUIDNext: r.UIDNext}
		if err := a.o.Store.PutProgress(ctx, p); err != nil {
			return err
		}
		a.logger.InfoContext(ctx, "cache backfill started", "folder", r.Folder, "baseline_uidnext", r.UIDNext)
		added = true
	}
	for _, folder := range slices.Sorted(maps.Keys(have)) {
		if synced[folder] {
			continue
		}
		if err := a.o.Store.DropFolder(ctx, a.name, folder, false); err != nil {
			return err
		}
		a.epoch.Add(1)
		a.logger.InfoContext(ctx, "cache dropped a folder that is no longer synced", "folder", folder)
	}
	if added {
		a.poke()
	}
	return nil
}

// fail records a failed step's reason code and logs it, at warn once the
// step has failed repeatedly or when the cache's disk is full, and hands
// err to OnError.
func (a *Account) fail(ctx context.Context, msg string, err error, repeated bool) {
	reason := reasonCode(err)
	a.setError(reason)
	a.noteErr(err)
	if repeated || errors.Is(err, ErrDiskFull) {
		a.logger.WarnContext(ctx, msg, "reason", reason)
		return
	}
	a.logger.InfoContext(ctx, msg, "reason", reason)
}
