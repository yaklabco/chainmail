package imapstore

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// errPoolClosed is returned by get once the store has closed.
var errPoolClosed = errors.New("the account's connections are closed")

// pooled is what the pool needs from a connection.
type pooled interface {
	Selected() string
	Writable() bool
	Dead() bool
	Noop(ctx context.Context) error
	Close() error
}

type poolOptions struct {
	size       int
	noopAfter  time.Duration
	idleLogout time.Duration
	now        func() time.Time
}

type idleConn[C pooled] struct {
	c     C
	since time.Time
	timer *time.Timer
}

// pool holds up to max connections to one account.
type pool[C pooled] struct {
	dial func(context.Context) (C, error)
	opts poolOptions

	mu     sync.Mutex
	max    int
	open   int // connections dialing, borrowed or idle
	idle   []*idleConn[C]
	wake   chan struct{}
	closed bool
}

func newPool[C pooled](dial func(context.Context) (C, error), opts poolOptions) *pool[C] {
	return &pool[C]{dial: dial, opts: opts, max: opts.size, wake: make(chan struct{})}
}

// get borrows a connection, preferring an idle one with folder open in a
// mode that will do: read-write when writable, either mode otherwise. It
// dials when the pool has room and waits otherwise. The caller hands it back
// with put, or with discard when it broke.
func (p *pool[C]) get(ctx context.Context, folder string, writable bool) (C, error) {
	var zero C
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return zero, errPoolClosed
		}
		if e := p.takeIdleLocked(folder, writable); e != nil {
			p.mu.Unlock()
			if p.opts.now().Sub(e.since) > p.opts.noopAfter {
				if err := e.c.Noop(ctx); err != nil {
					p.discard(e.c)
					if ctx.Err() != nil {
						return zero, ctx.Err()
					}
					continue
				}
			}
			return e.c, nil
		}
		if p.open < p.max {
			p.open++
			p.mu.Unlock()
			c, err := p.dial(ctx)
			if err != nil {
				p.mu.Lock()
				p.open--
				p.broadcastLocked()
				p.mu.Unlock()
				return zero, err
			}
			return c, nil
		}
		wake := p.wake
		p.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
}

// takeIdleLocked removes and returns the idle connection to reuse: one with
// folder open in a mode that will do if there is one, else the most
// recently returned.
func (p *pool[C]) takeIdleLocked(folder string, writable bool) *idleConn[C] {
	for len(p.idle) > 0 {
		i := slices.IndexFunc(p.idle, func(e *idleConn[C]) bool {
			return folder != "" && e.c.Selected() == folder && (e.c.Writable() || !writable)
		})
		if i < 0 {
			i = len(p.idle) - 1
		}
		e := p.idle[i]
		p.idle = slices.Delete(p.idle, i, i+1)
		if e.timer.Stop() {
			return e
		}
		// The idle timer fired and expire owns this connection now.
	}
	return nil
}

// put returns a borrowed connection.
func (p *pool[C]) put(c C) {
	if c.Dead() {
		p.discard(c)
		return
	}
	p.mu.Lock()
	if p.closed || p.open > p.max {
		p.open--
		p.broadcastLocked()
		p.mu.Unlock()
		_ = c.Close() // the pool closed or shrank; the LOGOUT outcome changes nothing
		return
	}
	e := &idleConn[C]{c: c, since: p.opts.now()}
	e.timer = time.AfterFunc(p.opts.idleLogout, func() { p.expire(e) })
	p.idle = append(p.idle, e)
	p.broadcastLocked()
	p.mu.Unlock()
}

// discard drops a borrowed connection that broke.
func (p *pool[C]) discard(c C) {
	p.mu.Lock()
	p.open--
	p.broadcastLocked()
	p.mu.Unlock()
	_ = c.Close() // a broken connection's LOGOUT fails fast and there is nothing to report
}

// expire logs out a connection that sat idle for idleLogout.
func (p *pool[C]) expire(e *idleConn[C]) {
	p.mu.Lock()
	if i := slices.Index(p.idle, e); i >= 0 {
		p.idle = slices.Delete(p.idle, i, i+1)
	}
	p.open--
	p.broadcastLocked()
	p.mu.Unlock()
	_ = e.c.Close() // idle cleanup; a failed LOGOUT still frees the slot
}

// shrink lowers the cap by one, never below one, and returns the new cap. A
// server that refused a connection for being over its limit keeps the lower
// cap until the store is rebuilt.
func (p *pool[C]) shrink() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.max > 1 {
		p.max--
	}
	return p.max
}

// openCount returns how many connections are dialing, borrowed or idle.
func (p *pool[C]) openCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.open
}

// size returns the cap: the most connections open at once.
func (p *pool[C]) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

// close logs out every idle connection in parallel and refuses new borrows.
// Connections still borrowed are logged out when they come back.
func (p *pool[C]) close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var mine []C
	for _, e := range p.idle {
		if e.timer.Stop() {
			mine = append(mine, e.c)
			p.open--
		}
	}
	p.idle = nil
	p.broadcastLocked()
	p.mu.Unlock()
	errs := make([]error, len(mine))
	var wg sync.WaitGroup
	for i, c := range mine {
		wg.Go(func() { errs[i] = c.Close() })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (p *pool[C]) broadcastLocked() {
	close(p.wake)
	p.wake = make(chan struct{})
}
