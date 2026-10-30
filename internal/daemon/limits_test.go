package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"filippo.io/age"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/db"
	"github.com/jamesainslie/chainmail/internal/limits"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/vault"
)

const limitsConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.gmail]
provider = "gmail"
address = "me@gmail.example"
auth = "password"

[accounts.fixed]
provider = "hostinger"
address = "fixed@example.com"
pool = 4
`

var limitsNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// capServer stands in for a mail server that accepts cap connections at once.
type capServer struct {
	mu    sync.Mutex
	cap   int
	open  int
	dials int
}

func (c *capServer) dial(ctx context.Context) (io.Closer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dials++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.open >= c.cap {
		return nil, mailstore.ErrUnavailable
	}
	c.open++
	return closerFunc(func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.open--
		return nil
	}), nil
}

func (c *capServer) dialCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dials
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// probeFake is a fake store that can be probed.
type probeFake struct {
	*fake.Store
	srv *capServer
}

func (p *probeFake) ProbeDial(ctx context.Context) (io.Closer, error) { return p.srv.dial(ctx) }

// built is one store the server built.
type built struct {
	account string
	params  storeParams
	store   mailstore.Store
}

// buildLog records every store the server builds.
type buildLog struct {
	mu     sync.Mutex
	stores []built
}

func (b *buildLog) add(e built) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stores = append(b.stores, e)
}

func (b *buildLog) of(account string) []built {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []built
	for _, e := range b.stores {
		if e.account == account {
			out = append(out, e)
		}
	}
	return out
}

// newLimitsServer builds an unserved Server whose stores come from mk,
// with a probe interval short enough for tests.
func newLimitsServer(t *testing.T, mk func(config.ResolvedAccount) mailstore.Store) (*Server, *buildLog) {
	t.Helper()
	return newLimitsServerWith(t, limitsConfig, mk)
}

// newLimitsServerWith is newLimitsServer with the config text given.
func newLimitsServerWith(t *testing.T, cfgText string, mk func(config.ResolvedAccount) mailstore.Store) (*Server, *buildLog) {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgText))
	if err != nil {
		t.Fatal(err)
	}
	dir := socketDir(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	v, err := vault.Open(filepath.Join(dir, "vault.age"), id)
	if err != nil {
		t.Fatal(err)
	}
	stateDB, err := db.OpenState(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stateDB.Close() })
	ln, err := Listen(t.Context(), filepath.Join(dir, "d.sock"), -1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s, err := New(Options{
		Config:     cfg,
		Vault:      v,
		StateDB:    stateDB,
		Listener:   ln,
		Authorizer: NewAuthorizer(uint32(os.Getuid()), -1),
		Now:        func() time.Time { return limitsNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	log := &buildLog{}
	s.newStore = func(acct config.ResolvedAccount, p storeParams) mailstore.Store {
		st := mk(acct)
		log.add(built{account: acct.Name, params: p, store: st})
		return st
	}
	s.lim.probeOpts = limits.ProbeOptions{Interval: time.Millisecond}
	t.Cleanup(s.drain)
	return s, log
}

// probeable makes probe fakes for every account against srv.
func probeable(srv *capServer) func(config.ResolvedAccount) mailstore.Store {
	return func(acct config.ResolvedAccount) mailstore.Store {
		return &probeFake{Store: fake.New(acct.Name, acct.Provider.Gmail), srv: srv}
	}
}

func probeLimits(t *testing.T, s *Server, accounts ...string) *chainmailv1.ProbeLimitsResponse {
	t.Helper()
	a := &accountService{s: s}
	resp, err := a.ProbeLimits(peerContext(t.Context()), connect.NewRequest(&chainmailv1.ProbeLimitsRequest{Accounts: accounts}))
	if err != nil {
		t.Fatalf("ProbeLimits: %v", err)
	}
	return resp.Msg
}

func storedLimit(t *testing.T, s *Server, account string) (limits.Limit, bool) {
	t.Helper()
	l, ok, err := s.lim.store.Get(t.Context(), account)
	if err != nil {
		t.Fatal(err)
	}
	return l, ok
}

func TestProbeLimitsReportsCapAndPool(t *testing.T) {
	t.Parallel()
	s, _ := newLimitsServer(t, probeable(&capServer{cap: 7}))
	got := probeLimits(t, s, "host").GetLimits()
	if len(got) != 1 {
		t.Fatalf("limits = %v, want one", got)
	}
	l := got[0]
	if l.GetAccount() != "host" || l.GetImapCap() != 7 || l.GetPoolSize() != 5 || l.GetError() != nil {
		t.Errorf("limit = %v, want host cap 7 pool 5", l)
	}
	if !l.GetProbedAt().AsTime().Equal(limitsNow) {
		t.Errorf("probed_at = %v, want %v", l.GetProbedAt().AsTime(), limitsNow)
	}
	stored, ok := storedLimit(t, s, "host")
	if !ok || stored.IMAPCap != 7 || !stored.ProbedAt.Equal(limitsNow) {
		t.Errorf("stored = %+v (ok %v), want cap 7 at %v", stored, ok, limitsNow)
	}
	rows := auditRows(t, s)
	if len(rows) != 1 || rows[0] != (limitsAudit{client: "unknown", account: "host", action: "limits.probe", targets: `["imap_cap=7","pool=5"]`, result: "ok"}) {
		t.Errorf("audit = %+v, want one ok limits.probe row for host", rows)
	}
}

func TestProbeLimitsConfigPoolWins(t *testing.T) {
	t.Parallel()
	s, log := newLimitsServer(t, probeable(&capServer{cap: 10}))
	got := probeLimits(t, s, "fixed").GetLimits()
	if len(got) != 1 || got[0].GetImapCap() != 10 || got[0].GetPoolSize() != 4 {
		t.Fatalf("limits = %v, want fixed cap 10 pool 4", got)
	}
	stores := log.of("fixed")
	if last := stores[len(stores)-1]; last.params.poolSize != 4 {
		t.Errorf("rebuilt store pool = %d, want 4", last.params.poolSize)
	}
}

func TestProbeLimitsEveryAccountConcurrently(t *testing.T) {
	t.Parallel()
	// One server per account, as each account has its own.
	servers := map[string]*capServer{"host": {cap: 9}, "fixed": {cap: 9}}
	s, _ := newLimitsServer(t, func(acct config.ResolvedAccount) mailstore.Store {
		if acct.Name == "gmail" {
			// A store without ProbeDial cannot be probed.
			return fake.New(acct.Name, true)
		}
		return &probeFake{Store: fake.New(acct.Name, false), srv: servers[acct.Name]}
	})
	got := probeLimits(t, s).GetLimits()
	names := make([]string, len(got))
	for i, l := range got {
		names[i] = l.GetAccount()
	}
	if want := []string{"fixed", "gmail", "host"}; !slices.Equal(names, want) {
		t.Fatalf("accounts = %v, want %v", names, want)
	}
	for _, l := range got {
		if l.GetAccount() == "gmail" {
			e := l.GetError()
			if e.GetReason() != api.ReasonInternal || e.GetMessage() != "probe not supported" {
				t.Errorf("gmail error = %v, want INTERNAL probe not supported", e)
			}
			if l.GetImapCap() != 0 || l.GetPoolSize() != 0 || l.GetProbedAt() != nil {
				t.Errorf("gmail limit = %v, want only the error", l)
			}
			continue
		}
		if l.GetError() != nil || l.GetImapCap() != 9 {
			t.Errorf("%s = %v, want cap 9", l.GetAccount(), l)
		}
	}
	if _, ok := storedLimit(t, s, "gmail"); ok {
		t.Error("gmail limit stored, want none")
	}
}

func TestProbeLimitsFailureStoresNothing(t *testing.T) {
	t.Parallel()
	s, _ := newLimitsServer(t, probeable(&capServer{cap: 0}))
	got := probeLimits(t, s, "host").GetLimits()
	if len(got) != 1 {
		t.Fatalf("limits = %v, want one", got)
	}
	e := got[0].GetError()
	if e.GetReason() != api.ReasonServerUnavailable || got[0].GetImapCap() != 0 {
		t.Errorf("limit = %v, want SERVER_UNAVAILABLE and no cap", got[0])
	}
	if _, ok := storedLimit(t, s, "host"); ok {
		t.Error("limit stored after a failed probe")
	}
	rows := auditRows(t, s)
	if len(rows) != 1 || rows[0].result != "error" || rows[0].reason != api.ReasonServerUnavailable {
		t.Errorf("audit = %+v, want one error row", rows)
	}
}

func TestProbeLimitsRejectsBadAccounts(t *testing.T) {
	t.Parallel()
	s, _ := newLimitsServer(t, probeable(&capServer{cap: 5}))
	a := &accountService{s: s}
	for _, accounts := range [][]string{{"nobody"}, {"Bad/Name"}} {
		_, err := a.ProbeLimits(peerContext(t.Context()), connect.NewRequest(&chainmailv1.ProbeLimitsRequest{Accounts: accounts}))
		if err == nil {
			t.Errorf("ProbeLimits(%v) = nil, want an error", accounts)
		}
	}
}

func TestProbeReplacesStore(t *testing.T) {
	t.Parallel()
	s, log := newLimitsServer(t, probeable(&capServer{cap: 8}))
	h := s.stores.get(mustAccount(t, s, "host"))
	if _, err := h.ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.lim.wait() // the first use starts a background probe; let it finish
	before := len(log.of("host"))
	probeLimits(t, s, "host")
	stores := log.of("host")
	if len(stores) <= before {
		t.Fatalf("stores built = %d, want more than %d after a probe", len(stores), before)
	}
	for _, b := range stores[:len(stores)-1] {
		if !b.store.(*probeFake).Closed() {
			t.Error("an old store was left open")
		}
	}
	last := stores[len(stores)-1]
	if last.store.(*probeFake).Closed() {
		t.Error("the new store is closed")
	}
	if last.params.poolSize != 6 {
		t.Errorf("new store pool = %d, want 6", last.params.poolSize)
	}
	// The handle taken before the probe now reaches the new store.
	if _, err := h.ListFolders(t.Context()); err != nil {
		t.Fatalf("ListFolders after probe: %v", err)
	}
	if n := last.store.(*probeFake).Calls("ListFolders"); n != 1 {
		t.Errorf("new store ListFolders calls = %d, want 1", n)
	}
}

func TestStorePoolSizeFromLimits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		account string
		stored  int // 0 for none
		want    int
	}{
		{name: "preset default for others", account: "host", want: 3},
		{name: "preset default for google", account: "gmail", want: 6},
		{name: "from the stored cap", account: "host", stored: 9, want: 7},
		{name: "config wins", account: "fixed", stored: 9, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Cap 0 keeps the background probe from storing anything.
			s, log := newLimitsServer(t, probeable(&capServer{cap: 0}))
			if tt.stored > 0 {
				if err := s.lim.store.Put(t.Context(), limits.Limit{Account: tt.account, IMAPCap: tt.stored}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.stores.get(mustAccount(t, s, tt.account)).ListFolders(t.Context()); err != nil {
				t.Fatal(err)
			}
			stores := log.of(tt.account)
			if len(stores) == 0 || stores[0].params.poolSize != tt.want {
				t.Fatalf("stores = %+v, want the first sized %d", stores, tt.want)
			}
		})
	}
}

func TestStaleLimitStartsOneBackgroundProbe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		stored    *limits.Limit
		wantDials int // cap 4, so a probe dials 5 times
	}{
		{name: "no limit", stored: nil, wantDials: 5},
		{name: "stale limit", stored: &limits.Limit{Account: "host", IMAPCap: 12, ProbedAt: limitsNow.Add(-limits.StaleAfter)}, wantDials: 5},
		{name: "fresh limit", stored: &limits.Limit{Account: "host", IMAPCap: 12, ProbedAt: limitsNow.Add(-time.Hour)}, wantDials: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := &capServer{cap: 4}
			s, _ := newLimitsServer(t, probeable(srv))
			if tt.stored != nil {
				if err := s.lim.store.Put(t.Context(), *tt.stored); err != nil {
					t.Fatal(err)
				}
			}
			acct := mustAccount(t, s, "host")
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					if _, err := s.stores.get(acct).ListFolders(t.Context()); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			s.lim.wait()
			if got := srv.dialCount(); got != tt.wantDials {
				t.Errorf("probe dials = %d, want %d", got, tt.wantDials)
			}
			l, ok := storedLimit(t, s, "host")
			wantCap := 4
			if tt.wantDials == 0 {
				wantCap = 12
			}
			if !ok || l.IMAPCap != wantCap {
				t.Errorf("stored = %+v (ok %v), want cap %d", l, ok, wantCap)
			}
			if tt.wantDials > 0 {
				rows := auditRows(t, s)
				if len(rows) != 1 || rows[0].client != "daemon" || rows[0].action != "limits.probe" {
					t.Errorf("audit = %+v, want one limits.probe row from the daemon", rows)
				}
			}
		})
	}
}

func TestBackgroundProbeSkipsUnprobeableStores(t *testing.T) {
	t.Parallel()
	s, _ := newLimitsServer(t, func(acct config.ResolvedAccount) mailstore.Store { return fake.New(acct.Name, false) })
	st := s.stores.get(mustAccount(t, s, "host"))
	if _, err := st.ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.lim.wait()
	if rows := auditRows(t, s); len(rows) != 0 {
		t.Errorf("audit = %+v, want none for a store that cannot be probed", rows)
	}
	if _, err := st.ListFolders(t.Context()); err != nil {
		t.Errorf("ListFolders after the skipped probe: %v", err)
	}
}

func TestConnLimitRecordsEveryRefusal(t *testing.T) {
	t.Parallel()
	monthAgo := limitsNow.Add(-31 * 24 * time.Hour)
	tests := []struct {
		name    string
		stored  *limits.Limit
		newMax  int
		wantCap int
	}{
		{name: "lowers a higher cap", stored: &limits.Limit{Account: "host", IMAPCap: 9, ProbedAt: limitsNow.Add(-time.Hour)}, newMax: 5, wantCap: 7},
		{name: "keeps an equal cap", stored: &limits.Limit{Account: "host", IMAPCap: 7, ProbedAt: monthAgo}, newMax: 5, wantCap: 7},
		{name: "never raises a lower cap", stored: &limits.Limit{Account: "host", IMAPCap: 3, ProbedAt: monthAgo}, newMax: 5, wantCap: 3},
		{name: "pool at its minimum", stored: &limits.Limit{Account: "host", IMAPCap: 3, ProbedAt: monthAgo}, newMax: 1, wantCap: 3},
		{name: "no stored limit", newMax: 2, wantCap: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Cap 0 keeps the background probe from storing anything.
			s, log := newLimitsServer(t, probeable(&capServer{cap: 0}))
			if tt.stored != nil {
				if err := s.lim.store.Put(t.Context(), *tt.stored); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.stores.get(mustAccount(t, s, "host")).ListFolders(t.Context()); err != nil {
				t.Fatal(err)
			}
			s.lim.wait()
			onConnLimit := log.of("host")[0].params.onConnLimit
			if onConnLimit == nil {
				t.Fatal("store built without OnConnLimit")
			}
			onConnLimit(tt.newMax)
			// The refusal is an observation of the limit, so probed_at moves
			// to now and postpones the next background probe.
			l, ok := storedLimit(t, s, "host")
			if !ok || l.IMAPCap != tt.wantCap || !l.ProbedAt.Equal(limitsNow) {
				t.Errorf("stored = %+v (ok %v), want cap %d at %v", l, ok, tt.wantCap, limitsNow)
			}
		})
	}
}

// heldFake is a probe fake whose pooled connections are still open, as a
// stream reader's is after its store was replaced. They hold slots on srv.
type heldFake struct {
	*probeFake
	held int
}

func (h *heldFake) OpenConns() int { return h.held }

func TestProbeCountsTheStoresOpenConnections(t *testing.T) {
	t.Parallel()
	srv := &capServer{cap: 4}
	// Two stream readers hold two of the server's four slots throughout.
	for range 2 {
		if _, err := srv.dial(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	var heldBuilt atomic.Bool
	s, _ := newLimitsServer(t, func(acct config.ResolvedAccount) mailstore.Store {
		p := &probeFake{Store: fake.New(acct.Name, acct.Provider.Gmail), srv: srv}
		if acct.Name == "host" && heldBuilt.CompareAndSwap(false, true) {
			return &heldFake{probeFake: p, held: 2}
		}
		return p
	})
	// A fresh limit, so no background probe runs first.
	if err := s.lim.store.Put(t.Context(), limits.Limit{Account: "host", IMAPCap: 12, ProbedAt: limitsNow.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.stores.get(mustAccount(t, s, "host")).ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := probeLimits(t, s, "host").GetLimits()
	if len(got) != 1 || got[0].GetImapCap() != 4 || got[0].GetPoolSize() != 2 {
		t.Fatalf("limits = %v, want host cap 4 pool 2", got)
	}
	if l, ok := storedLimit(t, s, "host"); !ok || l.IMAPCap != 4 {
		t.Errorf("stored = %+v (ok %v), want cap 4", l, ok)
	}
}

// streams stands in for stream readers that hold slots on srv. The
// account's first store opened them, and they stay open after it is
// replaced, as a stream reader's connection does.
type streams struct {
	srv   *capServer
	mu    sync.Mutex
	conns []io.Closer
	// peak is the most open at once since resetPeak.
	peak int
}

// open takes n of srv's slots.
func (s *streams) open(t *testing.T, n int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for range n {
		c, err := s.srv.dial(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		s.conns = append(s.conns, c)
		s.peak = max(s.peak, len(s.conns))
	}
}

// count is how many are still open.
func (s *streams) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// resetPeak starts a new peak from those open now.
func (s *streams) resetPeak() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.peak = len(s.conns)
}

// maxOpen is the most open at once since resetPeak.
func (s *streams) maxOpen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// closeAll frees every slot they hold.
func (s *streams) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

// streamFake is the store that opened the streams: it reports them open
// until they close.
type streamFake struct {
	*probeFake
	streams *streams
}

func (f *streamFake) OpenConns() int { return f.streams.count() }

// ResetConnPeak and ConnPeak count the streams as imapstore.Store counts
// its connections.
func (f *streamFake) ResetConnPeak() { f.streams.resetPeak() }

func (f *streamFake) ConnPeak() int { return f.streams.maxOpen() }

// cleanupFake is a probe fake that runs onClose as it closes its first probe
// connection, before that connection gives its slot back.
type cleanupFake struct {
	*probeFake
	once    sync.Once
	onClose func()
}

func (f *cleanupFake) ProbeDial(ctx context.Context) (io.Closer, error) {
	c, err := f.probeFake.ProbeDial(ctx)
	if err != nil {
		return nil, err
	}
	return closerFunc(func() error {
		f.once.Do(f.onClose)
		return c.Close()
	}), nil
}

// newStreamsServer builds a server whose "host" account's first store
// opened held streams on srv, with a fresh limit so no background probe
// runs, and uses that store once. Later stores come from later.
func newStreamsServer(t *testing.T, srv *capServer, held *streams, later func(*probeFake) mailstore.Store) *Server {
	t.Helper()
	var first atomic.Bool
	s, _ := newLimitsServer(t, func(acct config.ResolvedAccount) mailstore.Store {
		p := &probeFake{Store: fake.New(acct.Name, acct.Provider.Gmail), srv: srv}
		if acct.Name != "host" {
			return p
		}
		if first.CompareAndSwap(false, true) {
			return &streamFake{probeFake: p, streams: held}
		}
		return later(p)
	})
	if err := s.lim.store.Put(t.Context(), limits.Limit{Account: "host", IMAPCap: 12, ProbedAt: limitsNow.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.stores.get(mustAccount(t, s, "host")).ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProbeCountsStreamsOfEveryRetiredStore(t *testing.T) {
	t.Parallel()
	srv := &capServer{cap: 4}
	held := &streams{srv: srv}
	held.open(t, 2)
	s := newStreamsServer(t, srv, held, func(p *probeFake) mailstore.Store { return p })
	// The streams outlive both probes: the second must still count them,
	// though the store that opened them was retired by the first.
	for i := range 2 {
		got := probeLimits(t, s, "host").GetLimits()
		if len(got) != 1 || got[0].GetImapCap() != 4 || got[0].GetPoolSize() != 2 {
			t.Fatalf("probe %d: limits = %v, want host cap 4 pool 2", i+1, got)
		}
		if l, ok := storedLimit(t, s, "host"); !ok || l.IMAPCap != 4 {
			t.Fatalf("probe %d: stored = %+v (ok %v), want cap 4", i+1, l, ok)
		}
	}
	// Once they close, the retired store holds nothing and the server's
	// whole cap is the probe's.
	held.closeAll()
	if got := probeLimits(t, s, "host").GetLimits(); len(got) != 1 || got[0].GetImapCap() != 4 {
		t.Fatalf("after the streams closed: limits = %v, want host cap 4", got)
	}
}

func TestProbeSamplesStreamsBeforeItDials(t *testing.T) {
	t.Parallel()
	srv := &capServer{cap: 4}
	held := &streams{srv: srv}
	held.open(t, 2)
	// The streams finish while the probe logs its connections out, after
	// the refusal that ended it.
	s := newStreamsServer(t, srv, held, func(p *probeFake) mailstore.Store {
		return &cleanupFake{probeFake: p, onClose: held.closeAll}
	})
	got := probeLimits(t, s, "host").GetLimits()
	if len(got) != 1 || got[0].GetImapCap() != 4 || got[0].GetPoolSize() != 2 {
		t.Fatalf("limits = %v, want host cap 4 pool 2", got)
	}
	if l, ok := storedLimit(t, s, "host"); !ok || l.IMAPCap != 4 {
		t.Errorf("stored = %+v (ok %v), want cap 4", l, ok)
	}
	if held.count() != 0 {
		t.Errorf("streams open = %d, want 0: the probe's cleanup did not run", held.count())
	}
}

// gate holds every probe dial until released, counting the accounts with a
// dial waiting at once.
type gate struct {
	mu      sync.Mutex
	active  int
	most    int
	release chan struct{}
}

func (g *gate) counts() (active, most int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.active, g.most
}

// gatedFake is a probe fake whose dials wait at a gate.
type gatedFake struct {
	*probeFake
	g *gate
}

func (f *gatedFake) ProbeDial(ctx context.Context) (io.Closer, error) {
	f.g.mu.Lock()
	f.g.active++
	f.g.most = max(f.g.most, f.g.active)
	f.g.mu.Unlock()
	defer func() {
		f.g.mu.Lock()
		f.g.active--
		f.g.mu.Unlock()
	}()
	select {
	case <-f.g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return f.probeFake.ProbeDial(ctx)
}

func TestProbeLimitsBoundsConcurrentProbes(t *testing.T) {
	t.Parallel()
	var cfg strings.Builder
	for i := range 2 * maxProbes {
		fmt.Fprintf(&cfg, "[accounts.a%d]\nprovider = \"hostinger\"\naddress = \"a%d@example.com\"\n\n", i, i)
	}
	g := &gate{release: make(chan struct{})}
	var srvMu sync.Mutex
	servers := map[string]*capServer{}
	s, _ := newLimitsServerWith(t, cfg.String(), func(acct config.ResolvedAccount) mailstore.Store {
		srvMu.Lock()
		defer srvMu.Unlock()
		srv, ok := servers[acct.Name]
		if !ok {
			srv = &capServer{cap: 1}
			servers[acct.Name] = srv
		}
		return &gatedFake{probeFake: &probeFake{Store: fake.New(acct.Name, false), srv: srv}, g: g}
	})
	type answer struct {
		resp *connect.Response[chainmailv1.ProbeLimitsResponse]
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		a := &accountService{s: s}
		resp, err := a.ProbeLimits(peerContext(t.Context()), connect.NewRequest(&chainmailv1.ProbeLimitsRequest{}))
		done <- answer{resp, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if active, _ := g.counts(); active == maxProbes {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probes waiting = %d, want %d", func() int { a, _ := g.counts(); return a }(), maxProbes)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give any probe beyond the bound time to start.
	time.Sleep(50 * time.Millisecond)
	if active, most := g.counts(); active != maxProbes || most != maxProbes {
		t.Errorf("probes at once = %d (most %d), want %d", active, most, maxProbes)
	}
	close(g.release)
	ans := <-done
	if ans.err != nil {
		t.Fatalf("ProbeLimits: %v", ans.err)
	}
	resp := ans.resp.Msg
	if got := len(resp.GetLimits()); got != 2*maxProbes {
		t.Fatalf("limits = %d, want %d", got, 2*maxProbes)
	}
	for _, l := range resp.GetLimits() {
		if l.GetError() != nil || l.GetImapCap() != 1 {
			t.Errorf("%s = %v, want cap 1", l.GetAccount(), l)
		}
	}
	if _, most := g.counts(); most > maxProbes {
		t.Errorf("most probes at once = %d, want at most %d", most, maxProbes)
	}
}

// A probe takes seconds of dials and logins, so it holds no lock while it
// dials: the account's calls go on meanwhile.
func TestReadsDoNotWaitForAProbeThatDials(t *testing.T) {
	t.Parallel()
	g := &gate{release: make(chan struct{})}
	srv := &capServer{cap: 3}
	s, log := newLimitsServer(t, func(acct config.ResolvedAccount) mailstore.Store {
		return &gatedFake{probeFake: &probeFake{Store: fake.New(acct.Name, false), srv: srv}, g: g}
	})
	// A fresh limit, so no background probe runs first.
	if err := s.lim.store.Put(t.Context(), limits.Limit{Account: "host", IMAPCap: 12, ProbedAt: limitsNow.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h := s.stores.get(mustAccount(t, s, "host"))
	if _, err := h.ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		resp *connect.Response[chainmailv1.ProbeLimitsResponse]
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		a := &accountService{s: s}
		resp, err := a.ProbeLimits(peerContext(t.Context()), connect.NewRequest(&chainmailv1.ProbeLimitsRequest{Accounts: []string{"host"}}))
		done <- answer{resp, err}
	}()
	// The probe must be released whatever happens below, or the server's
	// cleanup waits for it forever.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(g.release) }) }
	defer release()
	deadline := time.Now().Add(5 * time.Second)
	for active, _ := g.counts(); active == 0; active, _ = g.counts() {
		if time.Now().After(deadline) {
			t.Fatal("the probe never dialed")
		}
		time.Sleep(time.Millisecond)
	}
	// The probe is dialing and will not finish until released.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := h.ListFolders(ctx); err != nil {
		t.Fatalf("ListFolders while a probe dials: %v, want it to run without waiting for the probe", err)
	}
	if n := log.of("host")[0].store.(*gatedFake).Calls("ListFolders"); n != 2 {
		t.Errorf("ListFolders calls on the store in use = %d, want 2", n)
	}
	release()
	ans := <-done
	if ans.err != nil {
		t.Fatalf("ProbeLimits: %v", ans.err)
	}
	got := ans.resp.Msg.GetLimits()
	if len(got) != 1 || got[0].GetImapCap() != 3 || got[0].GetPoolSize() != 1 {
		t.Fatalf("limits = %v, want host cap 3 pool 1", got)
	}
	// Once the probe is done, calls reach the store sized from its result.
	stores := log.of("host")
	last := stores[len(stores)-1]
	if last.params.poolSize != 1 {
		t.Errorf("store after the probe has pool %d, want 1", last.params.poolSize)
	}
	if _, err := h.ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := last.store.(*gatedFake).Calls("ListFolders"); n != 1 {
		t.Errorf("ListFolders calls on the new store = %d, want 1", n)
	}
}

// dialHookFake is a probe fake that runs onDial once, before its first
// probe dial.
type dialHookFake struct {
	*probeFake
	once   sync.Once
	onDial func()
}

func (f *dialHookFake) ProbeDial(ctx context.Context) (io.Closer, error) {
	f.once.Do(f.onDial)
	return f.probeFake.ProbeDial(ctx)
}

// Calls go on while a probe dials, and the connections they open meanwhile
// hold server slots when the probe is refused. They count toward the cap,
// so it does not read low.
func TestProbeCountsConnectionsOpenedWhileItDials(t *testing.T) {
	t.Parallel()
	srv := &capServer{cap: 4}
	held := &streams{srv: srv}
	s := newStreamsServer(t, srv, held, func(p *probeFake) mailstore.Store {
		return &dialHookFake{probeFake: p, onDial: func() { held.open(t, 2) }}
	})
	got := probeLimits(t, s, "host").GetLimits()
	if len(got) != 1 || got[0].GetImapCap() != 4 || got[0].GetPoolSize() != 2 {
		t.Fatalf("limits = %v, want host cap 4 pool 2", got)
	}
}

// Two probes of one account at once would each count the other's
// connections as the server's limit, so they take turns.
func TestProbesOfOneAccountTakeTurns(t *testing.T) {
	t.Parallel()
	srv := &capServer{cap: 5}
	s, _ := newLimitsServer(t, probeable(srv))
	if err := s.lim.store.Put(t.Context(), limits.Limit{Account: "host", IMAPCap: 12, ProbedAt: limitsNow.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]*chainmailv1.ProbeLimitsResponse, 4)
	for i := range results {
		wg.Go(func() { results[i] = probeLimits(t, s, "host") })
	}
	wg.Wait()
	for i, r := range results {
		if got := r.GetLimits(); len(got) != 1 || got[0].GetImapCap() != 5 {
			t.Errorf("probe %d: limits = %v, want host cap 5", i+1, got)
		}
	}
}

func TestReplaceWaitsForCallsInProgress(t *testing.T) {
	t.Parallel()
	s, log := newLimitsServer(t, probeable(&capServer{cap: 0}))
	// A fresh limit, so no background probe wants the write lock.
	if err := s.lim.store.Put(t.Context(), limits.Limit{Account: "host", IMAPCap: 5}); err != nil {
		t.Fatal(err)
	}
	acct := mustAccount(t, s, "host")
	e := s.stores.entry(acct)
	st, release, err := e.acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.stores.replace(t.Context(), acct)
	}()
	select {
	case <-done:
		t.Fatal("the store was replaced while a call held it")
	case <-time.After(50 * time.Millisecond):
	}
	if st.(*probeFake).Closed() {
		t.Fatal("the store was closed while a call held it")
	}
	release()
	<-done
	if !st.(*probeFake).Closed() {
		t.Error("the old store was not closed")
	}
	if n := len(log.of("host")); n != 2 {
		t.Errorf("stores built = %d, want 2", n)
	}
}

func TestGateWaitersGiveUp(t *testing.T) {
	t.Parallel()
	var g rwGate
	if err := g.rlock(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := g.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock while read-held = %v, want DeadlineExceeded", err)
	}
	// The writer gave up, so readers are not held off.
	if err := g.rlock(t.Context()); err != nil {
		t.Fatalf("second rlock: %v", err)
	}
	g.runlock()
	g.runlock()
	if err := g.lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel2()
	if err := g.rlock(ctx2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rlock while write-held = %v, want DeadlineExceeded", err)
	}
	g.unlock()
	if err := g.rlock(t.Context()); err != nil {
		t.Fatalf("rlock after unlock: %v", err)
	}
	g.runlock()
}

func mustAccount(t *testing.T, s *Server, name string) config.ResolvedAccount {
	t.Helper()
	acct, err := s.opts.Config.Account(name)
	if err != nil {
		t.Fatal(err)
	}
	return acct
}

type limitsAudit struct {
	client, account, action, targets, result, reason string
}

func auditRows(t *testing.T, s *Server) []limitsAudit {
	t.Helper()
	rows, err := s.opts.StateDB.QueryContext(t.Context(), "SELECT client, account, action, targets, result, reason FROM audit ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []limitsAudit
	for rows.Next() {
		var r limitsAudit
		if err := rows.Scan(&r.client, &r.account, &r.action, &r.targets, &r.result, &r.reason); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// lockedBuffer is a bytes.Buffer safe for the daemon's concurrent logging.
type lockedBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// refusingFake is a probeable fake whose every dial fails with err, as a
// server refusing the first LOGIN does.
type refusingFake struct {
	*fake.Store
	err error
}

func (r *refusingFake) ProbeDial(context.Context) (io.Closer, error) { return nil, r.err }

// TestProbeFailuresKeepServerTextOutOfLogs fails an explicit and a
// background probe with a login refusal whose text holds a private marker,
// as a server echoing the rejected password would. Each failure is logged
// with its reason, at warning level with debug on, and the marker reaches
// no log line.
func TestProbeFailuresKeepServerTextOutOfLogs(t *testing.T) {
	t.Parallel()
	const private = "PRIVATE-LOGIN-TEXT"
	refusal := fmt.Errorf("%w: NO [AUTHENTICATIONFAILED] %s", mailstore.ErrUnavailable, private)
	s, _ := newLimitsServer(t, func(acct config.ResolvedAccount) mailstore.Store {
		return &refusingFake{Store: fake.New(acct.Name, acct.Provider.Gmail), err: refusal}
	})
	var logs lockedBuffer
	s.opts.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	probeLimits(t, s, "host")
	if _, err := s.stores.get(mustAccount(t, s, "gmail")).ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.lim.wait()

	out := logs.String()
	for _, msg := range []string{"connection probe failed", "background connection probe failed"} {
		if !strings.Contains(out, msg) {
			t.Errorf("no %q log line; the test proves nothing:\n%s", msg, out)
		}
	}
	if !strings.Contains(out, "reason="+api.ReasonServerUnavailable) {
		t.Errorf("the logs do not give the failures' reason:\n%s", out)
	}
	if strings.Contains(out, private) {
		t.Errorf("the logs hold the server's text:\n%s", out)
	}
}

// busyFake is a probe fake whose dial number at runs while the account's
// calls are busy: busy runs before it dials and idle once the server has
// answered, both before the dial returns.
type busyFake struct {
	*probeFake
	at         int
	busy, idle func()

	mu    sync.Mutex
	dials int
}

func (f *busyFake) ProbeDial(ctx context.Context) (io.Closer, error) {
	f.mu.Lock()
	f.dials++
	n := f.dials
	f.mu.Unlock()
	if n != f.at {
		return f.probeFake.ProbeDial(ctx)
	}
	f.busy()
	c, err := f.probeFake.ProbeDial(ctx)
	f.idle()
	return c, err
}

// Connections the account's calls open after the probe starts and close
// before the server's refusal reaches the probe held server slots at the
// refusal, so they count toward the cap, which does not read low.
func TestProbeCountsConnectionsThatClosedBeforeTheRefusal(t *testing.T) {
	t.Parallel()
	srv := &capServer{cap: 4}
	held := &streams{srv: srv}
	// Two probe connections succeed; the third is refused while the calls
	// hold the other two slots, which they give back before it returns.
	s := newStreamsServer(t, srv, held, func(p *probeFake) mailstore.Store {
		return &busyFake{probeFake: p, at: 3, busy: func() { held.open(t, 2) }, idle: held.closeAll}
	})
	got := probeLimits(t, s, "host").GetLimits()
	if len(got) != 1 || got[0].GetImapCap() != 4 || got[0].GetPoolSize() != 2 {
		t.Fatalf("limits = %v, want host cap 4 pool 2", got)
	}
	if l, ok := storedLimit(t, s, "host"); !ok || l.IMAPCap != 4 {
		t.Errorf("stored = %+v (ok %v), want cap 4", l, ok)
	}
	if held.count() != 0 {
		t.Errorf("calls' connections open = %d, want 0 before the probe returned", held.count())
	}
}
