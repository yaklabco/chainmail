package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// storeSet holds one mail store per account, created on the account's first
// use. Callers get a handle that stays valid when the store behind it is
// replaced: each call holds the account's read lock, and a probe, once it
// has counted, takes the write lock just long enough to install a new store.
type storeSet struct {
	// build makes a store for an account, sized from its stored limit.
	build func(config.ResolvedAccount) mailstore.Store
	// buildSized makes a store for an account with the pool size given,
	// which replace installs. Nil means build, whatever the size.
	buildSized func(config.ResolvedAccount, int) mailstore.Store
	// firstUse, when set, runs once per account, the first time a call
	// builds its store. It must not block.
	firstUse func(config.ResolvedAccount)
	// logger, when set, hears about stores that failed to log out on replace.
	logger *slog.Logger

	mu      sync.Mutex
	entries map[string]*storeEntry
}

// storeEntry is one account's store and the lock that guards replacing it.
type storeEntry struct {
	set  *storeSet
	acct config.ResolvedAccount
	gate rwGate
	// probing holds one token, taken by a probe of the account for its
	// whole run, so two probes never count each other's connections. It is
	// a channel rather than a mutex so a waiter can give up.
	probing chan struct{}

	mu  sync.Mutex
	cur mailstore.Store // nil until first used
	// retired are stores replace closed that still held connections when
	// last looked at, such as a stream reader's. They count toward the
	// account's open connections until every one has closed.
	retired []mailstore.Store
	used    bool // a call has built the store
	closed  bool // the set was closed; no more stores are built
}

// errStoresClosed fails calls that arrive after the daemon closed its stores.
var errStoresClosed = fmt.Errorf("%w: the daemon is shutting down", mailstore.ErrUnavailable)

// get returns the store for acct. It connects on first use.
func (s *storeSet) get(acct config.ResolvedAccount) mailstore.Store {
	return &accountStore{e: s.entry(acct)}
}

// entry returns acct's entry, creating it if needed.
func (s *storeSet) entry(acct config.ResolvedAccount) *storeEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[acct.Name]; ok {
		return e
	}
	if s.entries == nil {
		s.entries = map[string]*storeEntry{}
	}
	e := &storeEntry{set: s, acct: acct, probing: make(chan struct{}, 1)}
	s.entries[acct.Name] = e
	return e
}

// acquire takes the read lock and returns the current store, building it on
// first use. The caller calls release exactly once when done with it.
func (e *storeEntry) acquire(ctx context.Context) (st mailstore.Store, release func(), err error) {
	if err := e.gate.rlock(ctx); err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		e.gate.runlock()
		return nil, nil, errStoresClosed
	}
	if e.cur == nil {
		e.cur = e.set.build(e.acct)
	}
	first := !e.used
	e.used = true
	st = e.cur
	e.mu.Unlock()
	if first && e.set.firstUse != nil {
		e.set.firstUse(e.acct)
	}
	return st, sync.OnceFunc(e.gate.runlock), nil
}

// peek returns acct's current store without waiting for any lock, or nil
// when none is built.
func (s *storeSet) peek(acct config.ResolvedAccount) mailstore.Store {
	e := s.entry(acct)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cur
}

// lockProbe takes acct's probe lock, waiting for a probe of the account
// already running, or for ctx to end. It holds no read or write lock, so
// the account's calls go on. The caller runs unlock once its probe is done.
func (s *storeSet) lockProbe(ctx context.Context, acct config.ResolvedAccount) (unlock func(), err error) {
	e := s.entry(acct)
	select {
	case e.probing <- struct{}{}:
		return func() { <-e.probing }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// replace installs a store for acct with a pool of poolSize and closes the
// one it replaces. It holds acct's
// write lock only to swap them: calls in progress finish first, new ones
// wait, and none waits for the old store to log out, which happens after
// the lock is released. No call holds the old store by then, but a stream
// reader still may: the old store counts toward the account's open
// connections until it holds none.
func (s *storeSet) replace(ctx context.Context, acct config.ResolvedAccount, poolSize int) error {
	e := s.entry(acct)
	if err := e.gate.lock(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		e.gate.unlock()
		return errStoresClosed
	}
	old := e.cur
	if s.buildSized != nil {
		e.cur = s.buildSized(acct, poolSize)
	} else {
		e.cur = s.build(acct)
	}
	if old != nil {
		e.retired = append(e.retired, old)
	}
	e.mu.Unlock()
	e.gate.unlock()
	if old == nil {
		return nil
	}
	// Logging out is best effort: the new store opens its own connections.
	if err := old.Close(); err != nil && s.logger != nil {
		s.logger.Warn("logging out of the replaced store", "account", acct.Name, "reason", errReason(err))
	}
	return nil
}

// retiredConnsLocked returns how many connections the retired stores hold
// and drops those that hold none. No call holds a retired store, so one
// that reached zero opens no more, even while its Close runs. e.mu must be
// held.
func (e *storeEntry) retiredConnsLocked() int {
	n := 0
	e.retired = slices.DeleteFunc(e.retired, func(r mailstore.Store) bool {
		c := openConns(r)
		n += c
		return c == 0
	})
	return n
}

// countConns starts counting the connections acct's stores hold on the
// server, for a probe, and returns the count so far. That is what every
// retired store not yet drained holds now, which only falls since no call
// uses them, plus the most the current store held at once since the start,
// connections that have closed since included. A store built after the
// start, on the account's first use, counts all it held. A store that
// cannot keep that peak counts what it holds at the start or the end,
// whichever is more, and one that cannot count, such as a fake, counts 0.
func (s *storeSet) countConns(acct config.ResolvedAccount) (sofar func() int) {
	e := s.entry(acct)
	e.mu.Lock()
	defer e.mu.Unlock()
	retired := e.retiredConnsLocked()
	start := e.cur
	startOpen := 0
	if start != nil {
		startOpen = openConns(start)
		if p, ok := start.(connPeaker); ok {
			p.ResetConnPeak()
		}
	}
	return func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		n := retired
		if start != nil {
			n += peakConns(start, startOpen)
		}
		if e.cur != nil && e.cur != start {
			n += peakConns(e.cur, 0)
		}
		return n
	}
}

// peakConns is the most connections st held at once since its peak was
// reset, or since it was made. A store that keeps no peak gives the larger
// of sampled and what it holds now.
func peakConns(st mailstore.Store, sampled int) int {
	if p, ok := st.(connPeaker); ok {
		return p.ConnPeak()
	}
	return max(sampled, openConns(st))
}

// closeAll closes every store in parallel. Stores are not built again.
func (s *storeSet) closeAll() error {
	s.mu.Lock()
	entries := slices.Collect(maps.Values(s.entries))
	s.entries = nil
	s.mu.Unlock()
	var stores []mailstore.Store
	for _, e := range entries {
		e.mu.Lock()
		e.closed = true
		e.retired = nil
		if e.cur != nil {
			stores = append(stores, e.cur)
			e.cur = nil
		}
		e.mu.Unlock()
	}
	errs := make([]error, len(stores))
	var wg sync.WaitGroup
	for i, st := range stores {
		wg.Go(func() { errs[i] = st.Close() })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// rwGate is a read-write lock whose waiters give up when their context
// ends. A waiting writer holds off new readers, so a probe is not starved.
type rwGate struct {
	mu      sync.Mutex
	readers int
	writer  bool          // a writer holds the lock or waits for readers to leave
	wake    chan struct{} // closed and cleared on every release
}

// waitChan returns the channel the next release closes. g.mu must be held.
func (g *rwGate) waitChan() chan struct{} {
	if g.wake == nil {
		g.wake = make(chan struct{})
	}
	return g.wake
}

// broadcast wakes every waiter. g.mu must be held.
func (g *rwGate) broadcast() {
	if g.wake != nil {
		close(g.wake)
		g.wake = nil
	}
}

func (g *rwGate) rlock(ctx context.Context) error {
	for {
		g.mu.Lock()
		if !g.writer {
			g.readers++
			g.mu.Unlock()
			return nil
		}
		w := g.waitChan()
		g.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (g *rwGate) runlock() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.readers--
	g.broadcast()
}

func (g *rwGate) lock(ctx context.Context) error {
	g.mu.Lock()
	for g.writer {
		w := g.waitChan()
		g.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			return ctx.Err()
		}
		g.mu.Lock()
	}
	g.writer = true
	for g.readers > 0 {
		w := g.waitChan()
		g.mu.Unlock()
		select {
		case <-w:
		case <-ctx.Done():
			g.mu.Lock()
			g.writer = false
			g.broadcast()
			g.mu.Unlock()
			return ctx.Err()
		}
		g.mu.Lock()
	}
	g.mu.Unlock()
	return nil
}

func (g *rwGate) unlock() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writer = false
	g.broadcast()
}

// accountStore is the handle storeSet.get returns. Each call runs on the
// account's current store under its read lock.
type accountStore struct {
	e *storeEntry
}

var _ mailstore.Store = (*accountStore)(nil)

// refsFailed gives every ref the same error.
func refsFailed(refs []ref.Ref, err error) []mailstore.RefResult {
	out := make([]mailstore.RefResult, len(refs))
	for i, r := range refs {
		out[i] = mailstore.RefResult{Ref: r, Err: err}
	}
	return out
}

// batch runs f on the current store, or fails every ref when none can be had.
func (a *accountStore) batch(ctx context.Context, refs []ref.Ref, f func(mailstore.Store) []mailstore.RefResult) []mailstore.RefResult {
	st, release, err := a.e.acquire(ctx)
	if err != nil {
		return refsFailed(refs, err)
	}
	defer release()
	return f(st)
}

// call runs f on the current store.
func (a *accountStore) call(ctx context.Context, f func(mailstore.Store) error) error {
	st, release, err := a.e.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return f(st)
}

func (a *accountStore) Append(ctx context.Context, folder string, flags []string, date time.Time, raw []byte) (r ref.Ref, err error) {
	err = a.call(ctx, func(st mailstore.Store) error {
		r, err = st.Append(ctx, folder, flags, date, raw)
		return err
	})
	return r, err
}

func (a *accountStore) Move(ctx context.Context, refs []ref.Ref, folder string) []mailstore.RefResult {
	return a.batch(ctx, refs, func(st mailstore.Store) []mailstore.RefResult { return st.Move(ctx, refs, folder) })
}

func (a *accountStore) Copy(ctx context.Context, refs []ref.Ref, folder string) []mailstore.RefResult {
	return a.batch(ctx, refs, func(st mailstore.Store) []mailstore.RefResult { return st.Copy(ctx, refs, folder) })
}

func (a *accountStore) Archive(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	return a.batch(ctx, refs, func(st mailstore.Store) []mailstore.RefResult { return st.Archive(ctx, refs) })
}

func (a *accountStore) Trash(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	return a.batch(ctx, refs, func(st mailstore.Store) []mailstore.RefResult { return st.Trash(ctx, refs) })
}

func (a *accountStore) Expunge(ctx context.Context, refs []ref.Ref) []mailstore.RefResult {
	return a.batch(ctx, refs, func(st mailstore.Store) []mailstore.RefResult { return st.Expunge(ctx, refs) })
}

func (a *accountStore) StoreFlags(ctx context.Context, refs []ref.Ref, add, remove []string) []mailstore.RefResult {
	return a.batch(ctx, refs, func(st mailstore.Store) []mailstore.RefResult { return st.StoreFlags(ctx, refs, add, remove) })
}

func (a *accountStore) StoreLabels(ctx context.Context, refs []ref.Ref, add, remove []string) []mailstore.RefResult {
	return a.batch(ctx, refs, func(st mailstore.Store) []mailstore.RefResult { return st.StoreLabels(ctx, refs, add, remove) })
}

func (a *accountStore) CreateFolder(ctx context.Context, name string) (created string, err error) {
	err = a.call(ctx, func(st mailstore.Store) error {
		created, err = st.CreateFolder(ctx, name)
		return err
	})
	return created, err
}

func (a *accountStore) RenameFolder(ctx context.Context, from, to string) (renamed string, err error) {
	err = a.call(ctx, func(st mailstore.Store) error {
		renamed, err = st.RenameFolder(ctx, from, to)
		return err
	})
	return renamed, err
}

func (a *accountStore) DeleteFolder(ctx context.Context, name string, force bool) error {
	return a.call(ctx, func(st mailstore.Store) error { return st.DeleteFolder(ctx, name, force) })
}

func (a *accountStore) FindByMessageID(ctx context.Context, folder, messageID string) (refs []ref.Ref, err error) {
	err = a.call(ctx, func(st mailstore.Store) error {
		refs, err = st.FindByMessageID(ctx, folder, messageID)
		return err
	})
	return refs, err
}

func (a *accountStore) ListFolders(ctx context.Context) (folders []mailstore.Folder, err error) {
	err = a.call(ctx, func(st mailstore.Store) error {
		folders, err = st.ListFolders(ctx)
		return err
	})
	return folders, err
}

func (a *accountStore) Search(ctx context.Context, req mailstore.SearchRequest) (res []mailstore.SourceResult, err error) {
	err = a.call(ctx, func(st mailstore.Store) error {
		res, err = st.Search(ctx, req)
		return err
	})
	return res, err
}

func (a *accountStore) FetchSummaries(ctx context.Context, src mailstore.Source, uids []uint32, opts mailstore.SummaryOptions) (sums []mailstore.Summary, err error) {
	err = a.call(ctx, func(st mailstore.Store) error {
		sums, err = st.FetchSummaries(ctx, src, uids, opts)
		return err
	})
	return sums, err
}

func (a *accountStore) FetchMessages(ctx context.Context, refs []ref.Ref) []mailstore.MessageResult {
	st, release, err := a.e.acquire(ctx)
	if err != nil {
		out := make([]mailstore.MessageResult, len(refs))
		for i := range out {
			out[i] = mailstore.MessageResult{Err: err}
		}
		return out
	}
	defer release()
	return st.FetchMessages(ctx, refs)
}

// StreamPart holds the read lock only while the part is opened, not while
// the reader is read. A caller may hold several readers and make other
// calls meanwhile, and a lock held across them would deadlock against a
// waiting replace. A store replaced while a reader is open waits for its
// connection as Close does, after the write lock is released, and the
// reader keeps working. The next probe counts that connection among the
// account's own until it closes.
func (a *accountStore) StreamPart(ctx context.Context, r ref.Ref, part string) (info mailstore.PartInfo, rc io.ReadCloser, err error) {
	err = a.call(ctx, func(st mailstore.Store) error {
		info, rc, err = st.StreamPart(ctx, r, part)
		return err
	})
	return info, rc, err
}

func (a *accountStore) Thread(ctx context.Context, r ref.Ref) (refs []ref.Ref, err error) {
	err = a.call(ctx, func(st mailstore.Store) error {
		refs, err = st.Thread(ctx, r)
		return err
	})
	return refs, err
}

// Close does nothing: the store set owns the store and closes it on shutdown.
func (a *accountStore) Close() error { return nil }
