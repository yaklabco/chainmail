package imapstore

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

var _ mailstore.Watcher = (*Store)(nil)

// OpenWatch implements mailstore.Watcher. The connection logs in through
// the same login gate and credentials as the pool, outside the pool: it
// takes no pool slot, and OpenConns and ConnPeak count it. Store.Close closes it under
// its owner without waiting for the owner to stop, so a watcher that is
// syncing or idling on it gets ErrUnavailable and dials again, through
// whatever store it holds by then.
func (s *Store) OpenWatch(ctx context.Context, folder string) (mailstore.WatchConn, error) {
	s.mu.Lock()
	if s.watchesClosed {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %w", mailstore.ErrUnavailable, errPoolClosed)
	}
	s.pool.holdOutside()
	s.mu.Unlock()
	w := &watchConn{s: s, folder: folder, signal: make(chan struct{}, 1)}
	c, err := s.connectWith(ctx, w.handler())
	if err != nil {
		s.pool.releaseOutside()
		return nil, s.watchError(err)
	}
	w.c = c
	s.mu.Lock()
	closed := s.watchesClosed
	s.watches[w] = struct{}{}
	s.mu.Unlock()
	if closed {
		_ = w.Close() // the store closed while this one dialed
		return nil, fmt.Errorf("%w: %w", mailstore.ErrUnavailable, errPoolClosed)
	}
	if _, err := s.selectForSync(ctx, c, folder); err != nil {
		_ = w.Close() // the SELECT failure is the one to report
		return nil, s.watchError(err)
	}
	return w, nil
}

// watchError is what a watch connection's caller sees for err: a dropped
// connection, or one that timed out, is the account being unavailable, as
// is a refusal from the server, as serverError says.
func (s *Store) watchError(err error) error {
	if errors.Is(err, errConnLost) && !errors.Is(err, mailstore.ErrUnavailable) {
		return fmt.Errorf("%w: %s: %w", mailstore.ErrUnavailable, s.acct.Name, err)
	}
	return s.serverError(err)
}

// closeWatches refuses new watch connections and logs out the open ones,
// in parallel.
func (s *Store) closeWatches() error {
	s.mu.Lock()
	s.watchesClosed = true
	open := slices.Collect(maps.Keys(s.watches))
	s.mu.Unlock()
	errs := make([]error, len(open))
	var wg sync.WaitGroup
	for i, w := range open {
		wg.Go(func() { errs[i] = w.Close() })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// forgetWatch drops a watch connection that has logged out.
func (s *Store) forgetWatch(w *watchConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.watches, w)
	s.pool.releaseOutside()
}

// watchConn is a mailstore.WatchConn: a connection outside the pool that
// stays selected on one folder and hears what the server reports
// unprompted.
type watchConn struct {
	s      *Store
	folder string
	c      *conn

	mu sync.Mutex
	// pending is what the server reported since Idle last took it.
	pending mailstore.Wake
	// signal holds a token once pending has a report.
	signal chan struct{}

	// idling is set while IDLE may be running, so Close shuts the socket
	// rather than queue LOGOUT behind an IDLE that only DONE would end.
	idling atomic.Bool

	closeOnce sync.Once
	closeErr  error
}

// handler hears EXISTS, EXPUNGE and FETCH that no command asked for. It
// runs on the client's goroutines, so it only records the report.
func (w *watchConn) handler() *imapclient.UnilateralDataHandler {
	return &imapclient.UnilateralDataHandler{
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data.NumMessages != nil {
				w.report(false)
			}
		},
		Expunge: func(uint32) { w.report(true) },
		Fetch: func(msg *imapclient.FetchMessageData) {
			_, _ = msg.Collect() // reading the message lets the client go on; only the report matters
			w.report(false)
		},
	}
}

func (w *watchConn) report(expunged bool) {
	w.mu.Lock()
	w.pending.Changed = true
	w.pending.Expunged = w.pending.Expunged || expunged
	w.mu.Unlock()
	select {
	case w.signal <- struct{}{}:
	default: // a token is already waiting
	}
}

// take returns the pending report and clears it.
func (w *watchConn) take() mailstore.Wake {
	w.mu.Lock()
	defer w.mu.Unlock()
	wake := w.pending
	w.pending = mailstore.Wake{}
	select {
	case <-w.signal:
	default:
	}
	return wake
}

// Sync implements mailstore.WatchConn. It does not retry: a dropped
// connection is ErrUnavailable, and the caller dials again.
func (w *watchConn) Sync(ctx context.Context, req mailstore.SyncRequest) (mailstore.Delta, error) {
	d, err := w.s.syncPass(ctx, w.c, w.folder, req)
	if err != nil {
		return mailstore.Delta{}, w.s.watchError(err)
	}
	return d, nil
}

// IdleSupported implements mailstore.WatchConn.
func (w *watchConn) IdleSupported() bool { return w.c.client().Caps().Has(imap.CapIdle) }

// Idle implements mailstore.WatchConn. A report that arrived since Idle last
// returned, during a Sync included, makes it return at once without IDLE.
// Starting IDLE and ending it with DONE are commands, each bounded by the
// command timeout and neither cut short by ctx; the wait between them is
// bounded only by limit and ctx. When ctx ends, even before the server has
// answered IDLE, Idle still waits for the answer, sends DONE and waits for
// IDLE to complete, then returns ctx.Err() with the folder still selected,
// so the next Sync works. A DONE that fails leaves the connection dead and
// returns the failure.
func (w *watchConn) Idle(ctx context.Context, limit time.Duration) (mailstore.Wake, error) {
	if !w.IdleSupported() {
		return mailstore.Wake{}, fmt.Errorf("%w: %s does not offer IDLE", mailstore.ErrUnsupported, w.s.acct.Name)
	}
	if err := ctx.Err(); err != nil {
		return mailstore.Wake{}, err
	}
	if wake := w.take(); wake.Changed {
		return wake, nil
	}
	if limit <= 0 {
		return mailstore.Wake{}, nil
	}
	w.idling.Store(true)
	defer w.idling.Store(false)
	// Cancelling while the server has yet to answer IDLE would shut the
	// connection, so ctx ends the wait below instead, after which DONE ends
	// IDLE cleanly.
	var cmd *imapclient.IdleCommand
	if err := w.c.do(context.WithoutCancel(ctx), func() error {
		var err error
		cmd, err = w.c.client().Idle()
		return err
	}); err != nil {
		return mailstore.Wake{}, w.s.watchError(err)
	}
	select {
	case <-w.signal:
	case <-w.s.after(limit):
	case <-ctx.Done():
	case <-w.c.client().Closed():
	}
	if err := w.c.do(context.WithoutCancel(ctx), func() error {
		if err := cmd.Close(); err != nil {
			return err
		}
		return cmd.Wait()
	}); err != nil {
		w.c.dead.Store(true) // a server that refused DONE is in no known state
		return mailstore.Wake{}, w.s.watchError(err)
	}
	if err := ctx.Err(); err != nil {
		return mailstore.Wake{}, err
	}
	return w.take(), nil
}

// Close implements mailstore.WatchConn. It logs out, bounded by the
// session's close timeout of 5 seconds, and is safe to call more than once.
// Store.Close calls it while the owner may still be using the connection:
// it marks the connection dead first, so the owner's next command fails at
// once, and shuts the socket instead of logging out when IDLE may be
// running, so neither waits on the other.
func (w *watchConn) Close() error {
	w.closeOnce.Do(func() {
		w.c.dead.Store(true)
		if w.idling.Load() {
			w.c.kill()
		} else {
			w.closeErr = w.c.Close()
		}
		w.s.forgetWatch(w)
	})
	return w.closeErr
}
