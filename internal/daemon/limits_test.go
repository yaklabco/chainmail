package daemon

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
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
	cfg, err := config.Parse([]byte(limitsConfig))
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

func TestConnLimitLowersStoredCap(t *testing.T) {
	t.Parallel()
	s, log := newLimitsServer(t, probeable(&capServer{cap: 0}))
	if err := s.lim.store.Put(t.Context(), limits.Limit{Account: "host", IMAPCap: 9, ProbedAt: limitsNow.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.stores.get(mustAccount(t, s, "host")).ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.lim.wait()
	onConnLimit := log.of("host")[0].params.onConnLimit
	if onConnLimit == nil {
		t.Fatal("store built without OnConnLimit")
	}
	onConnLimit(5)
	l, _ := storedLimit(t, s, "host")
	if l.IMAPCap != 7 || !l.ProbedAt.Equal(limitsNow) {
		t.Errorf("after shrink to 5: stored = %+v, want cap 7 at %v", l, limitsNow)
	}
	// A refusal never raises the cap.
	onConnLimit(8)
	if l, _ := storedLimit(t, s, "host"); l.IMAPCap != 7 {
		t.Errorf("after shrink to 8: cap = %d, want 7 kept", l.IMAPCap)
	}
}

func TestConnLimitWithoutStoredLimit(t *testing.T) {
	t.Parallel()
	s, log := newLimitsServer(t, probeable(&capServer{cap: 0}))
	if _, err := s.stores.get(mustAccount(t, s, "host")).ListFolders(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.lim.wait()
	log.of("host")[0].params.onConnLimit(2)
	if l, ok := storedLimit(t, s, "host"); !ok || l.IMAPCap != 4 {
		t.Errorf("stored = %+v (ok %v), want cap 4", l, ok)
	}
}

func TestProbeWaitsForCallsInProgress(t *testing.T) {
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
		_ = s.stores.exclusive(t.Context(), acct, func(_ mailstore.Store, replace func() mailstore.Store) error {
			replace()
			return nil
		})
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
