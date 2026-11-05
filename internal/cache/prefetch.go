package cache

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/render"
	"github.com/jamesainslie/chainmail/internal/watch"
)

const (
	// prefetchBatch is how many bodies one prefetch step lists to fetch.
	prefetchBatch = 25
	// maxBodyAttempts is how many failed fetches make a body failure final,
	// the count Stats reports as final.
	maxBodyAttempts = finalAttempts
	// bodyRetryGap is the wait after a failed body fetch before the next
	// try, so 3 attempts span about 24 hours (spec section 8).
	bodyRetryGap = 8 * time.Hour
	// prefetchIdle is the wait when there is nothing to fetch.
	prefetchIdle = time.Minute
	// retryFloor is the shortest wait after a busy pool, an unavailable
	// server, watch trouble or a cache error.
	retryFloor = time.Second
	// windowCheckAhead pushes the retryAfter of the window_done check past
	// any failure's retry gap, so a message waiting to be retried still
	// counts as left to fetch.
	windowCheckAhead = 24 * time.Hour
)

// Fixed body failure codes (spec section 8). Never server text.
const (
	failFetch = "fetch_failed"
	failParse = "parse_failed"
	failGone  = "gone"
	// failTooLarge is a message larger than the whole daily_download, so
	// no day's budget admits its body. A read still fetches it.
	failTooLarge = "too_large"
)

// prefetchOptions configures one account's prefetcher.
type prefetchOptions struct {
	Account string
	// Settings and MaxSize are read at every step.
	Settings func() config.CacheSettings
	MaxSize  func() int64
	Store    *Store
	// State, when set, pauses the prefetcher while one of the account's
	// folders is auth_required, reconnecting or error.
	State *watch.State
	// Mail returns the account's store for one download.
	Mail func(ctx context.Context) (mailstore.Store, func(), error)
	// Wake, when set, ends the wait after a step early: a value on it means
	// a folder may have newly become eligible, so the next step runs at
	// once. A step that still has nothing to do returns the same wait
	// again. Nil waits on Clock alone.
	Wake <-chan struct{}
	// Throttle is the account's pause after the server refused for a quota
	// or bandwidth limit. The prefetcher records one when a download is
	// refused so, and waits while one runs. Nil records nothing.
	Throttle *throttle
	// Clock nil means the system clock.
	Clock watch.Clock
	// Logger nil discards. It carries the account attribute: Account passes
	// its own logger, so the prefetcher does not add it again.
	Logger *slog.Logger
	// OnError, when set, receives every failure warn logs.
	OnError func(err error)
}

// prefetcher fetches the bodies of cached headers inside the account's
// prefetch window, newest first (spec section 4.3).
type prefetcher struct {
	o prefetchOptions
	// failures counts the downloads in a row that ended on an unavailable
	// server, for the 1s to 5m backoff. A download the server answered
	// resets it. Only run's goroutine steps.
	failures int
}

func newPrefetcher(o prefetchOptions) *prefetcher {
	if o.Clock == nil {
		o.Clock = systemClock{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &prefetcher{o: o}
}

// run steps until ctx ends, waiting what each step returns, or less when
// Wake fires first.
func (p *prefetcher) run(ctx context.Context) {
	for {
		wait := p.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.o.Clock.After(wait):
		case <-p.o.Wake:
		}
	}
}

// step does one unit of prefetch work and returns how long to wait before
// the next: it fetches the bodies the day's budget admits for the first
// folder with work, keeping each folder's window_done true exactly when
// its window has no body left to fetch (a final failure counts as done).
// A folder in watch error is skipped and its window_done left alone.
// While the server throttles the account, or the budget admits none of
// the folder's bodies, it fetches nothing and waits for 00:00 UTC.
func (p *prefetcher) step(ctx context.Context) time.Duration {
	set := p.o.Settings()
	retry := max(set.BackfillPause, retryFloor)
	if !set.Enabled || set.PrefetchBodies <= 0 {
		return prefetchIdle
	}
	paused, failing, err := p.watchTrouble(ctx)
	if err != nil || paused {
		if err != nil {
			p.warn(ctx, "reading the watch state for the cache prefetcher", err)
		}
		return retry
	}
	now := p.o.Clock.Now()
	if d := p.o.Throttle.wait(now); d > 0 {
		return d
	}
	left, err := p.left(ctx, now, set)
	if err != nil {
		p.warn(ctx, "reading the download budget", err)
		return retry
	}
	if left <= 0 {
		return untilUTCMidnight(now)
	}
	held, err := p.held(ctx)
	if err != nil {
		p.warn(ctx, "checking the cache size", err)
		return retry
	}
	progress, err := p.o.Store.AllProgress(ctx, p.o.Account)
	if err != nil {
		p.warn(ctx, "reading cache progress", err)
		return retry
	}
	since := now.Add(-set.PrefetchBodies)
	for _, pr := range progress {
		if !pr.BackfillDone || failing[pr.Folder] {
			continue
		}
		queue, err := p.o.Store.bodylessQueue(ctx, p.o.Account, pr.Folder, since, maxBodyAttempts, now.Add(-bodyRetryGap), prefetchBatch)
		if err != nil {
			p.warn(ctx, "listing bodies to prefetch", err)
			return retry
		}
		if len(queue) == 0 {
			if err := p.settleWindow(ctx, pr, since, now); err != nil {
				p.warn(ctx, "recording the prefetch window", err)
				return retry
			}
			continue
		}
		if pr.WindowDone {
			if err := p.o.Store.SetWindowDone(ctx, p.o.Account, pr.Folder, false); err != nil {
				p.warn(ctx, "recording the prefetch window", err)
				return retry
			}
		}
		if held {
			continue
		}
		return p.admitAndFetch(ctx, queue, left, now, set)
	}
	return prefetchIdle
}

// left is how many bytes of the day's download budget are left at now,
// zero or less when it is spent.
func (p *prefetcher) left(ctx context.Context, now time.Time, set config.CacheSettings) (int64, error) {
	used, err := p.o.Store.Budget(ctx, p.o.Account, utcDay(now))
	if err != nil {
		return 0, err
	}
	return set.DailyDownload - used, nil
}

// admit splits a folder's queue, newest first, against left bytes of the
// day's budget, by each message's size. A message larger than the whole
// daily budget goes to tooLarge, since no day admits it. The others go to
// fetch while their sizes add up to no more than left; blocked reports
// that one did not fit, which stops the admission there, so smaller
// bodies behind it never overtake it.
func admit(queue []queued, left, daily int64) (fetch, tooLarge []queued, blocked bool) {
	for _, q := range queue {
		switch {
		case q.size > daily:
			tooLarge = append(tooLarge, q)
		case q.size > left:
			return fetch, tooLarge, true
		default:
			fetch = append(fetch, q)
			left -= q.size
		}
	}
	return fetch, tooLarge, false
}

// admitAndFetch records a too_large failure for each queued body no day's
// budget admits, and fetches the bodies left bytes admit. With none
// admitted, it waits for 00:00 UTC when a body did not fit, and
// backfill_pause otherwise.
func (p *prefetcher) admitAndFetch(ctx context.Context, queue []queued, left int64, now time.Time, set config.CacheSettings) time.Duration {
	fetch, tooLarge, blocked := admit(queue, left, set.DailyDownload)
	for _, q := range tooLarge {
		if err := p.fail(ctx, q.ref, failTooLarge); err != nil {
			p.warn(ctx, "recording a body failure", err)
			return max(set.BackfillPause, retryFloor)
		}
	}
	switch {
	case len(fetch) > 0:
		return p.fetch(ctx, fetch, set)
	case blocked:
		return untilUTCMidnight(now)
	default:
		return set.BackfillPause
	}
}

// settleWindow sets the folder's window_done when no body in the window is
// left to fetch, counting failures still waiting for a retry, and clears it
// otherwise. It writes only on a change. Setting it is one guarded update
// in the Store, so an eviction between this step's check and the write
// leaves it clear.
func (p *prefetcher) settleWindow(ctx context.Context, pr Progress, since, now time.Time) error {
	if !pr.WindowDone {
		set, err := p.o.Store.CompleteWindow(ctx, p.o.Account, pr.Folder, since, maxBodyAttempts)
		if err != nil {
			return err
		}
		if set {
			p.o.Logger.Info("cache body window complete", "folder", pr.Folder)
		}
		return nil
	}
	left, err := p.o.Store.BodylessSince(ctx, p.o.Account, pr.Folder, since, maxBodyAttempts, now.Add(windowCheckAhead), 1)
	if err != nil {
		return err
	}
	if len(left) == 0 {
		return nil
	}
	return p.o.Store.SetWindowDone(ctx, p.o.Account, pr.Folder, false)
}

// fetch downloads the admitted bodies one at a time, newest first, each on
// a background connection, and stores them. Before each download it checks
// the throttle and the day's budget again, since foreground reads, the
// builder and the backfill may spend or stop it meanwhile, and stops for
// 00:00 UTC when the body no longer fits. It returns BackfillPause after
// the last body, and the wait for what ended it early otherwise.
func (p *prefetcher) fetch(ctx context.Context, queue []queued, set config.CacheSettings) time.Duration {
	retry := max(set.BackfillPause, retryFloor)
	stored := false
	defer func() {
		if stored {
			p.noteWrite(ctx)
		}
	}()
	for _, q := range queue {
		now := p.o.Clock.Now()
		if d := p.o.Throttle.wait(now); d > 0 {
			return d
		}
		left, err := p.left(ctx, now, set)
		if err != nil {
			p.warn(ctx, "reading the download budget", err)
			return retry
		}
		if left <= 0 || q.size > left {
			return untilUTCMidnight(now)
		}
		got, wait, ended := p.download(ctx, q.ref, set)
		stored = stored || got
		if ended {
			return wait
		}
	}
	return set.BackfillPause
}

// download fetches one body and stores it, or records its failure. It
// reports whether it stored the body, and whether the step must end, with
// how long to wait before the next step then.
func (p *prefetcher) download(ctx context.Context, r ref.Ref, set config.CacheSettings) (stored bool, wait time.Duration, ended bool) {
	retry := max(set.BackfillPause, retryFloor)
	ms, release, err := p.o.Mail(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.o.Logger.Debug("no mail store for the cache prefetcher", "reason", reasonCode(err))
		}
		return false, p.endWait(ctx, err, retry), true
	}
	results := ms.FetchMessages(mailstore.WithBackground(ctx), []ref.Ref{r})
	release()
	p.charge(ctx, results)
	if len(results) != 1 {
		p.o.Logger.Warn("the mail store answered a body fetch with the wrong number of results")
		return false, retry, true
	}
	res := results[0]
	if res.Err != nil {
		reason := bodyFailure(res.Err)
		if reason == "" {
			return false, p.endWait(ctx, res.Err, retry), true
		}
		p.failures = 0
		if err := p.fail(ctx, r, reason); err != nil {
			p.warn(ctx, "recording a body failure", err)
			return false, retry, true
		}
		return false, 0, false
	}
	p.failures = 0
	reason := ""
	if res.Message == nil {
		reason = failFetch
	} else if _, err := render.Render(res.Message.Raw, render.Options{Full: true}); err != nil {
		reason = failParse
	}
	if reason != "" {
		if err := p.fail(ctx, r, reason); err != nil {
			p.warn(ctx, "recording a body failure", err)
			return false, retry, true
		}
		return false, 0, false
	}
	msg := res.Message
	// A row read without its body structure gets the attachments of this
	// full read in the same transaction as the body, so a failed repair
	// leaves the message in the queue (Store.PutFullBody).
	if _, err := p.o.Store.PutFullBody(ctx, r, Body{Raw: msg.Raw, TextClipped: msg.TextClipped}, false, fullReadOf(msg)); err != nil {
		p.warn(ctx, "storing a prefetched body", err)
		return false, retry, true
	}
	return true, 0, false
}

// endWait is the wait after a download ended on err, which records
// nothing for the message: until 00:00 UTC when the server throttles the
// account, which pauses the account's background work; 1s doubling to 5m
// while the server is unavailable, as the other workers back off; and
// retry after anything else, such as a busy pool.
func (p *prefetcher) endWait(ctx context.Context, err error, retry time.Duration) time.Duration {
	switch {
	case errors.Is(err, mailstore.ErrThrottled):
		now := p.o.Clock.Now()
		if p.o.Throttle.set(now) {
			p.o.Logger.InfoContext(ctx, logThrottled)
		}
		return untilUTCMidnight(now)
	case errors.Is(err, mailstore.ErrUnavailable):
		wait := max(retry, workBackoff(p.failures))
		p.failures++
		return wait
	default:
		return retry
	}
}

// charge adds len(raw) of every body a download received to the budget of
// the UTC day it ended on, before any of them is parsed or stored: a body
// that then fails to parse or store was still downloaded. A result
// carrying only an error costs nothing.
func (p *prefetcher) charge(ctx context.Context, results []mailstore.MessageResult) {
	var spent int64
	for _, res := range results {
		if res.Message != nil {
			spent += int64(len(res.Message.Raw))
		}
	}
	if spent == 0 {
		return
	}
	if _, err := p.o.Store.AddBudget(ctx, p.o.Account, utcDay(p.o.Clock.Now()), spent); err != nil {
		p.warn(ctx, "adding to the download budget", err)
	}
}

// noteWrite wakes the evictor when the file is over max_size after a batch
// stored bodies.
func (p *prefetcher) noteWrite(ctx context.Context) {
	if err := p.o.Store.noteWrite(ctx, p.o.MaxSize()); err != nil {
		p.warn(ctx, "checking the cache size", err)
	}
}

// fail records a body failure and logs when it becomes final.
func (p *prefetcher) fail(ctx context.Context, r ref.Ref, reason string) error {
	n, err := p.o.Store.RecordBodyFailure(ctx, r, reason)
	if err != nil {
		return err
	}
	if n >= maxBodyAttempts {
		p.o.Logger.Info("cache body fetch given up", "folder", r.Folder, "uid", r.UID, "reason", reason)
	}
	return nil
}

// warn logs a failure and hands it to OnError, unless ctx has ended.
func (p *prefetcher) warn(ctx context.Context, msg string, err error) {
	if ctx.Err() != nil {
		return
	}
	p.o.Logger.Warn(msg, "err", err)
	if p.o.OnError != nil {
		p.o.OnError(err)
	}
}

// bodyFailure maps a per-message fetch error to the fixed code to record,
// or "" for an error about the connection, the account or the call rather
// than the message, which records nothing and ends the batch.
func bodyFailure(err error) string {
	switch {
	case errors.Is(err, mailstore.ErrBusy),
		errors.Is(err, mailstore.ErrUnavailable),
		errors.Is(err, auth.ErrAuthRequired),
		errors.Is(err, auth.ErrRejected),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return ""
	case errors.Is(err, mailstore.ErrStaleRef), errors.Is(err, mailstore.ErrNotFound):
		return failGone
	default:
		return failFetch
	}
}

// watchTrouble reads the account's watch states. paused is set while one
// of its folders is auth_required or reconnecting, which the cache workers
// wait out for the whole account; failing names the folders in error, which
// are skipped alone (integration note 12).
func (p *prefetcher) watchTrouble(ctx context.Context) (paused bool, failing map[string]bool, err error) {
	if p.o.State == nil {
		return false, nil, nil
	}
	rows, err := p.o.State.Folders(ctx)
	if err != nil {
		return false, nil, err
	}
	for _, r := range rows {
		if r.Account != p.o.Account {
			continue
		}
		switch r.State {
		case watch.StateAuthRequired, watch.StateReconnecting:
			paused = true
		case watch.StateError:
			if failing == nil {
				failing = map[string]bool{}
			}
			failing[r.Folder] = true
		}
	}
	return paused, failing, nil
}

// held reports whether the cap hold stops prefetching: it is set after an
// eviction under the cap removed window bodies, and lasts while the file
// is at or over 80% of max_size. Under 80% it clears the hold.
func (p *prefetcher) held(ctx context.Context) (bool, error) {
	if !p.o.Store.evict.hold.Load() {
		return false, nil
	}
	size, err := p.o.Store.SizeBytes(ctx)
	if err != nil {
		return true, err
	}
	if size*100 < p.o.MaxSize()*refetchPercent {
		p.o.Store.evict.hold.Store(false)
		return false, nil
	}
	return true, nil
}
