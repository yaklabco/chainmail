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

	mu   sync.Mutex
	max  int
	open int // connections dialing, borrowed or idle
	// closing are connections taken off open that are still logging out.
	// They no longer count against max, but the server still holds their
	// slots, so openCount reports them.
	closing int
	// outside are connections dialed outside the pool, such as watch
	// connections, that hold a slot on the server. They do not count
	// against max, but openCount and peak count them.
	outside int
	// peak is the most of open, closing and outside since the last
	// resetConnPeak. Only a dial raises the sum: a logout moves a
	// connection from open to closing.
	peak   int
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
			p.peak = max(p.peak, p.slotsLocked())
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
		p.startLogoutLocked()
		p.mu.Unlock()
		p.logout(c) // the pool closed or shrank
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
	p.startLogoutLocked()
	p.mu.Unlock()
	p.logout(c) // a broken connection's LOGOUT fails fast
}

// expire logs out a connection that sat idle for idleLogout.
func (p *pool[C]) expire(e *idleConn[C]) {
	p.mu.Lock()
	if i := slices.Index(p.idle, e); i >= 0 {
		p.idle = slices.Delete(p.idle, i, i+1)
	}
	p.startLogoutLocked()
	p.mu.Unlock()
	p.logout(e.c) // idle cleanup
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

// yieldSlot makes room for a connection dialed outside the pool that the
// server refused for being over its limit, by at most one slot. A pool
// that holds no open connection changes nothing: shrinking it frees no
// slot, and the refusal is someone else's, such as sessions the server
// still counts from before a restart. A pool at its cap steps the cap down
// by one, never below one, and reports atCap, so the caller may record the
// limit. A pool below its cap keeps the cap. Either way idle connections
// are taken off the pool until one slot is free, never the last one, and
// returned counted as closing, for the caller to log out.
func (p *pool[C]) yieldSlot() (n int, retire []C, atCap bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.open == 0 {
		return p.max, nil, false
	}
	atCap = p.open >= p.max
	target := max(1, p.open-1)
	if atCap {
		p.max = max(1, p.max-1)
		target = p.max
	}
	for p.open > target && len(p.idle) > 0 {
		e := p.idle[0] // the oldest
		p.idle = p.idle[1:]
		if !e.timer.Stop() {
			continue // the idle timer fired and expire owns this connection now
		}
		p.startLogoutLocked()
		retire = append(retire, e.c)
	}
	return p.max, retire, atCap
}

// openCount returns how many connections hold a slot on the server:
// dialing, borrowed, idle or still logging out, and those outside the pool.
func (p *pool[C]) openCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.slotsLocked()
}

// slotsLocked is openCount with p.mu held.
func (p *pool[C]) slotsLocked() int { return p.open + p.closing + p.outside }

// holdOutside counts a connection dialed outside the pool, from the start
// of its dial, toward openCount and the peak. releaseOutside ends it, once
// the connection has logged out or failed to dial.
func (p *pool[C]) holdOutside() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outside++
	p.peak = max(p.peak, p.slotsLocked())
}

// releaseOutside ends a holdOutside.
func (p *pool[C]) releaseOutside() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.outside--
}

// resetConnPeak starts a new peak from the connections holding a slot now,
// those outside the pool included.
func (p *pool[C]) resetConnPeak() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peak = p.slotsLocked()
}

// connPeak returns the most connections that held a slot at once since the
// last resetConnPeak, or since the pool was made: dialing, borrowed, idle or
// still logging out, and those outside the pool. Connections that have closed since still count.
func (p *pool[C]) connPeak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.peak
}

// startLogoutLocked moves a connection about to be logged out from open to
// closing, which frees its place in the pool. p.mu must be held.
func (p *pool[C]) startLogoutLocked() {
	p.open--
	p.closing++
	p.broadcastLocked()
}

// logout logs c out, which startLogoutLocked counted as closing. A failed
// LOGOUT still ends the connection, so its outcome changes nothing.
func (p *pool[C]) logout(c C) {
	_ = c.Close()
	p.loggedOut()
}

// loggedOut records that a closing connection finished logging out.
func (p *pool[C]) loggedOut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closing--
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
			p.closing++
		}
	}
	p.idle = nil
	p.broadcastLocked()
	p.mu.Unlock()
	errs := make([]error, len(mine))
	var wg sync.WaitGroup
	for i, c := range mine {
		wg.Go(func() {
			errs[i] = c.Close()
			p.loggedOut()
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (p *pool[C]) broadcastLocked() {
	close(p.wake)
	p.wake = make(chan struct{})
}
