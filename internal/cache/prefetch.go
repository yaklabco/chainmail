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
	// prefetchBatch is how many bodies one prefetch fetch asks for.
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
	// Mail returns the account's store for one batch.
	Mail func(ctx context.Context) (mailstore.Store, func(), error)
	// Clock nil means the system clock.
	Clock watch.Clock
	// Logger nil discards.
	Logger *slog.Logger
}

// prefetcher fetches the bodies of cached headers inside the account's
// prefetch window, newest first (spec section 4.3).
type prefetcher struct {
	o prefetchOptions
}

func newPrefetcher(o prefetchOptions) *prefetcher {
	if o.Clock == nil {
		o.Clock = sysClock{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &prefetcher{o: o}
}

// run steps until ctx ends, waiting what each step returns.
func (p *prefetcher) run(ctx context.Context) {
	for {
		wait := p.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.o.Clock.After(wait):
		}
	}
}

// step does one unit of prefetch work and returns how long to wait before
// the next: it fetches one batch for the first folder with work, keeping
// each folder's window_done true exactly when its window has no body left
// to fetch (a final failure counts as done). A folder in watch error is
// skipped and its window_done left alone.
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
	day := utcDay(now)
	used, err := p.o.Store.Budget(ctx, p.o.Account, day)
	if err != nil {
		p.warn(ctx, "reading the download budget", err)
		return retry
	}
	if used >= set.DailyDownload {
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
		refs, err := p.o.Store.BodylessSince(ctx, p.o.Account, pr.Folder, since, maxBodyAttempts, now.Add(-bodyRetryGap), prefetchBatch)
		if err != nil {
			p.warn(ctx, "listing bodies to prefetch", err)
			return retry
		}
		if len(refs) == 0 {
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
		return p.fetch(ctx, refs, day, set)
	}
	return prefetchIdle
}

// settleWindow sets the folder's window_done when no body in the window is
// left to fetch, counting failures still waiting for a retry, and clears it
// otherwise. It writes only on a change.
func (p *prefetcher) settleWindow(ctx context.Context, pr Progress, since, now time.Time) error {
	left, err := p.o.Store.BodylessSince(ctx, p.o.Account, pr.Folder, since, maxBodyAttempts, now.Add(windowCheckAhead), 1)
	if err != nil {
		return err
	}
	done := len(left) == 0
	if done == pr.WindowDone {
		return nil
	}
	if err := p.o.Store.SetWindowDone(ctx, p.o.Account, pr.Folder, done); err != nil {
		return err
	}
	if done {
		p.o.Logger.Info("cache body window complete", "account", p.o.Account, "folder", pr.Folder)
	}
	return nil
}

// fetch fetches one batch of bodies on a background connection and stores
// them. It returns BackfillPause after a batch, and the retry wait when the
// batch ended early.
func (p *prefetcher) fetch(ctx context.Context, refs []ref.Ref, day string, set config.CacheSettings) time.Duration {
	retry := max(set.BackfillPause, retryFloor)
	ms, release, err := p.o.Mail(ctx)
	if err != nil {
		if ctx.Err() == nil {
			p.o.Logger.Debug("no mail store for the cache prefetcher", "account", p.o.Account, "err", err)
		}
		return retry
	}
	results := ms.FetchMessages(mailstore.WithBackground(ctx), refs)
	release()
	if len(results) != len(refs) {
		p.o.Logger.Warn("the mail store answered a body fetch with the wrong number of results", "account", p.o.Account)
		return retry
	}
	var spent int64
	defer func() { p.spend(ctx, day, spent) }()
	for i, res := range results {
		r := refs[i]
		if res.Err != nil {
			reason := bodyFailure(res.Err)
			if reason == "" {
				return retry
			}
			if err := p.fail(ctx, r, reason); err != nil {
				p.warn(ctx, "recording a body failure", err)
				return retry
			}
			continue
		}
		if res.Message == nil {
			if err := p.fail(ctx, r, failFetch); err != nil {
				p.warn(ctx, "recording a body failure", err)
				return retry
			}
			continue
		}
		msg := res.Message
		if _, err := render.Render(msg.Raw, render.Options{Full: true}); err != nil {
			if err := p.fail(ctx, r, failParse); err != nil {
				p.warn(ctx, "recording a body failure", err)
				return retry
			}
			continue
		}
		if _, err := p.o.Store.PutBody(ctx, r, Body{Raw: msg.Raw, TextClipped: msg.TextClipped}, false); err != nil {
			p.warn(ctx, "storing a prefetched body", err)
			return retry
		}
		spent += int64(len(msg.Raw))
	}
	return set.BackfillPause
}

// spend adds what a batch downloaded to the day's budget and wakes the
// evictor when the file is over max_size.
func (p *prefetcher) spend(ctx context.Context, day string, spent int64) {
	if spent == 0 {
		return
	}
	if _, err := p.o.Store.AddBudget(ctx, p.o.Account, day, spent); err != nil {
		p.warn(ctx, "adding to the download budget", err)
	}
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
		p.o.Logger.Info("cache body fetch given up", "account", r.Account, "folder", r.Folder, "uid", r.UID, "reason", reason)
	}
	return nil
}

// warn logs a failure, unless ctx has ended.
func (p *prefetcher) warn(ctx context.Context, msg string, err error) {
	if ctx.Err() != nil {
		return
	}
	p.o.Logger.Warn(msg, "account", p.o.Account, "err", err)
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
