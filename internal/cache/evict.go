package cache

import (
	"context"
	"sync"
	"sync/atomic"
)

// evictState is the size cap state the prefetchers, the read path and the
// evictor share. Its zero value is ready to use.
type evictState struct {
	once sync.Once
	// kick wakes the evictor. It holds one value, so wakes coalesce.
	kick chan struct{}
	// hold is set when an eviction under the cap removed window bodies. The
	// prefetchers then fetch nothing until the file is under 80% of
	// max_size, so they do not refill what the cap just took (spec section 5).
	hold atomic.Bool
	// over is the last sweep's OverCap.
	over atomic.Bool
}

// kickChan returns the channel the evictor waits on, made on first use.
func (e *evictState) kickChan() chan struct{} {
	e.once.Do(func() { e.kick = make(chan struct{}, 1) })
	return e.kick
}

// noteWrite checks the file size after a write that stored a body and
// wakes the evictor when the file is over maxSize. A maxSize of zero or
// less means no cap. Every caller of PutBody in this package calls it.
func (s *Store) noteWrite(ctx context.Context, maxSize int64) error {
	size, err := s.SizeBytes(ctx)
	if err != nil {
		return err
	}
	if maxSize > 0 && size > maxSize {
		select {
		case s.evict.kickChan() <- struct{}{}:
		default:
		}
	}
	return nil
}

// OverCap reports whether the last eviction sweep left the file over
// max_size with no body left to evict: headers alone exceed the cap.
func (s *Store) OverCap() bool { return s.evict.over.Load() }

// SetWindowDone records whether the folder's prefetch window is complete
// and changes nothing else in its progress row, so it never undoes a
// concurrent builder update or a rebuild. A folder with no progress row is
// left without one.
func (s *Store) SetWindowDone(ctx context.Context, account, folder string, done bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE folders SET window_done = ? WHERE account = ? AND folder = ?`, boolInt(done), account, folder)
	if err != nil {
		return s.fail("setting window_done", err)
	}
	return nil
}
