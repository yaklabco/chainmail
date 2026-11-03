package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
)

// cacheConfig has one account with the cache and one with it turned off.
const cacheConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.off]
provider = "hostinger"
address = "off@example.com"

[accounts.off.cache]
enabled = false
`

// cacheOffConfig turns the cache off for every account.
const cacheOffConfig = passwordConfig + `
[cache]
enabled = false
`

// openCache opens cache.db in CacheDir, creating the directory with mode
// 0700 and replacing a file that is not a cache, and leaves the daemon
// without a cache, never failing, when it is off or cannot open.
func TestOpenCache(t *testing.T) {
	t.Parallel()
	newDir := func(t *testing.T) string { return filepath.Join(t.TempDir(), "cache") }
	tests := []struct {
		name     string
		config   string
		disable  bool
		dir      func(t *testing.T) string // "" means no CacheDir
		wantOpen bool
		wantErr  bool
	}{
		{name: "opens and creates the directory", config: cacheConfig, dir: newDir, wantOpen: true},
		{name: "no cache directory", config: cacheConfig, dir: func(*testing.T) string { return "" }},
		{name: "off by option", config: cacheConfig, disable: true, dir: newDir},
		{name: "off for every account", config: cacheOffConfig, dir: newDir},
		{
			name: "the directory is a file", config: cacheConfig, wantErr: true,
			dir: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
		},
		{
			name: "a file that is not a cache is replaced", config: cacheConfig, wantOpen: true,
			dir: func(t *testing.T) string {
				// openCache leaves an existing directory's mode alone, as
				// runDaemon does the state directory's, so this one is
				// made 0700 here: t.TempDir's subdirectories are 0777
				// less the umask.
				d := filepath.Join(t.TempDir(), "cache")
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, cacheFile), []byte("not a database, just some text"), 0o600); err != nil {
					t.Fatal(err)
				}
				return d
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := tt.dir(t)
			s, _ := newTestServerOptions(t, tt.config, Options{CacheDir: dir, DisableCache: tt.disable})
			s.openCache(t.Context())
			t.Cleanup(s.closeCache)
			if got := s.cache != nil; got != tt.wantOpen {
				t.Fatalf("cache open = %v, want %v (open error %v)", got, tt.wantOpen, s.cacheErr)
			}
			if got := s.cacheErr != nil; got != tt.wantErr {
				t.Errorf("open error = %v, want an error: %v", s.cacheErr, tt.wantErr)
			}
			_, statErr := os.Stat(filepath.Join(dir, cacheFile))
			if !tt.wantOpen {
				if dir != "" && !tt.wantErr && !errors.Is(statErr, fs.ErrNotExist) {
					t.Errorf("cache.db exists without a cache: %v", statErr)
				}
				return
			}
			if statErr != nil {
				t.Errorf("cache.db: %v", statErr)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != 0o700 {
				t.Errorf("cache dir mode = %o, want 700", perm)
			}
			if _, err := s.cache.SizeBytes(t.Context()); err != nil {
				t.Errorf("SizeBytes: %v", err)
			}
		})
	}
}

// An existing cache_dir keeps its mode, but cache.db in it is 0600 even
// when the directory and an earlier cache.db were readable by others.
func TestOpenCachePrivateInExistingDirectory(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		t.Run("existing cache.db "+strconv.FormatBool(existing), func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "cache")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(dir, cacheFile)
			if existing {
				if err := os.WriteFile(db, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(db, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			s, _ := newTestServerOptions(t, cacheConfig, Options{CacheDir: dir})
			s.openCache(t.Context())
			t.Cleanup(s.closeCache)
			if s.cache == nil {
				t.Fatalf("cache.db did not open: %v", s.cacheErr)
			}
			for path, want := range map[string]fs.FileMode{dir: 0o755, db: 0o600} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != want {
					t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
				}
			}
		})
	}
}

// newCacheServer builds a never-served Server with cache.db open in a
// directory of its own, as Serve opens it.
func newCacheServer(t *testing.T, configTOML string) (*Server, *sql.DB) {
	t.Helper()
	s, stateDB := newTestServerOptions(t, configTOML, Options{CacheDir: t.TempDir()})
	s.openCache(t.Context())
	t.Cleanup(s.closeCache)
	if s.cache == nil {
		t.Fatalf("cache.db did not open: %v", s.cacheErr)
	}
	return s, stateDB
}

// folderCacheProto maps a worker's folder status field for field, takes
// the folder name from the stats when there is no progress row, and leaves
// last_applied_at unset before anything was applied.
func TestFolderCacheProto(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 3, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		in   cache.FolderStatus
		want *chainmailv1.FolderCacheStatus
	}{
		{
			name: "every field",
			in: cache.FolderStatus{
				Progress: cache.Progress{
					Account: "host", Folder: "INBOX", UIDValidity: 7, BaselineUIDNext: 500, BackfillLowUID: 120,
					BackfillDone: false, WindowDone: true, LastAppliedAt: at,
				},
				FolderStats: cache.FolderStats{
					Folder: "INBOX", Headers: 380, Bodies: 40, BodyBytes: 1234,
					WindowMessages: 50, WindowBodies: 40, BodyFailures: 2,
				},
				State: "backfilling",
			},
			want: &chainmailv1.FolderCacheStatus{
				Folder: "INBOX", State: "backfilling", Headers: 380, BackfillDone: false,
				BaselineUid: 500, BackfillLowUid: 120, Bodies: 40, BodyBytes: 1234,
				WindowMessages: 50, WindowBodies: 40, WindowDone: true, BodyFailures: 2,
				LastAppliedAt: timestamppb.New(at),
			},
		},
		{
			name: "stats only",
			in:   cache.FolderStatus{FolderStats: cache.FolderStats{Folder: "Sent", Headers: 3}, State: "backfilling"},
			want: &chainmailv1.FolderCacheStatus{Folder: "Sent", State: "backfilling", Headers: 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := folderCacheProto(tt.in); !proto.Equal(got, tt.want) {
				t.Errorf("folderCacheProto = %v, want %v", got, tt.want)
			}
		})
	}
}

// An account's cache status says disabled when the cache is off for it,
// off for the daemon, or has no worker, and error with a fixed reason when
// cache.db did not open.
func TestCacheStatusStates(t *testing.T) {
	t.Parallel()
	const budget = 1 << 30 // the default daily_download
	tests := []struct {
		name    string
		options Options
		mutate  func(*Server)
		account string
		want    *chainmailv1.AccountCacheStatus
	}{
		{
			name: "off for the account", account: "off",
			want: &chainmailv1.AccountCacheStatus{Account: "off", State: "disabled", BudgetBytes: budget},
		},
		{
			name: "off by option", options: Options{DisableCache: true}, account: "host",
			want: &chainmailv1.AccountCacheStatus{Account: "host", State: "disabled", BudgetBytes: budget},
		},
		{
			name: "no worker", account: "host",
			want: &chainmailv1.AccountCacheStatus{Account: "host", State: "disabled", BudgetBytes: budget},
		},
		{
			name: "cache.db did not open", account: "host",
			mutate: func(s *Server) { s.cacheErr = errors.New("open failed") },
			want:   &chainmailv1.AccountCacheStatus{Account: "host", Enabled: true, State: "error", LastError: api.ReasonInternal, BudgetBytes: budget},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := tt.options
			opts.CacheDir = t.TempDir()
			s, _ := newTestServerOptions(t, cacheConfig, opts)
			s.openCache(t.Context())
			t.Cleanup(s.closeCache)
			if tt.mutate != nil {
				tt.mutate(s)
			}
			acct, err := s.opts.Config.Account(tt.account)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.cacheStatus(t.Context(), acct, true); !proto.Equal(got, tt.want) {
				t.Errorf("cacheStatus = %v, want %v", got, tt.want)
			}
		})
	}
}

// GetCacheStatus reports the file size against max_size and every account
// in name order, resolves account names as other calls do, and is not
// audited.
func TestGetCacheStatus(t *testing.T) {
	t.Parallel()
	s, stateDB := newCacheServer(t, cacheConfig)
	svc := &cacheService{s: s}
	resp, err := svc.GetCacheStatus(peerContext(t.Context()), connect.NewRequest(&chainmailv1.GetCacheStatusRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetSizeBytes() <= 0 || resp.Msg.GetMaxSizeBytes() != 2<<30 {
		t.Errorf("size = %d of %d, want above 0 of 2 GiB", resp.Msg.GetSizeBytes(), resp.Msg.GetMaxSizeBytes())
	}
	var names []string
	for _, a := range resp.Msg.GetAccounts() {
		names = append(names, a.GetAccount())
	}
	if len(names) != 2 || names[0] != "host" || names[1] != "off" {
		t.Errorf("accounts = %v, want [host off]", names)
	}
	for _, tt := range []struct {
		accounts []string
		reason   string
	}{
		{accounts: []string{"nope"}, reason: api.ReasonNotFound},
		{accounts: []string{"Bad Name"}, reason: api.ReasonInvalidArgument},
	} {
		_, err := svc.GetCacheStatus(peerContext(t.Context()), connect.NewRequest(&chainmailv1.GetCacheStatusRequest{Accounts: tt.accounts}))
		if got := api.Detail(err).GetReason(); got != tt.reason {
			t.Errorf("GetCacheStatus(%v) reason = %q, want %q (%v)", tt.accounts, got, tt.reason, err)
		}
	}
	if n := auditCount(t, stateDB); n != 0 {
		t.Errorf("audit rows = %d, want 0", n)
	}
}

// plainStore hides every method but mailstore.Store's.
type plainStore struct{ mailstore.Store }

// A cache worker gets the account's store only when it can fetch headers
// and say which folders a search covers.
func TestCacheAccountMail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		store   mailstore.Store
		wantErr error
	}{
		{name: "fake store", store: fake.New("host", false)},
		{name: "without HeaderFetcher and Scoper", store: plainStore{fake.New("host", false)}, wantErr: mailstore.ErrUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := newTestServerOptions(t, cacheConfig, Options{NewStore: func(config.ResolvedAccount) mailstore.Store { return tt.store }})
			acct, err := s.opts.Config.Account("host")
			if err != nil {
				t.Fatal(err)
			}
			ms, release, err := s.cacheAccountOptions(acct).Mail(t.Context())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Mail err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if ms == nil {
				t.Error("Mail returned no store")
			}
			release()
		})
	}
}

// lastCacheAudit returns the newest audit row as action, targets, result
// and reason.
func lastCacheAudit(t *testing.T, stateDB *sql.DB) [4]string {
	t.Helper()
	var r [4]string
	err := stateDB.QueryRowContext(t.Context(), "SELECT action, targets, result, reason FROM audit ORDER BY id DESC LIMIT 1").
		Scan(&r[0], &r[1], &r[2], &r[3])
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// EvictCache runs a sweep now and reports what it removed: a body that
// arrived a year ago and was never read is outside the 90 day window and
// past the 30 day retention. The call is audited as cache.evict.
func TestEvictCache(t *testing.T) {
	t.Parallel()
	s, stateDB := newCacheServer(t, cacheConfig)
	ctx := peerContext(t.Context())
	old := time.Now().AddDate(-1, 0, 0)
	row := cache.Row{Account: "host", Folder: "INBOX", UIDValidity: 1, UID: 1, InternalDate: old, Date: old, Size: 30, Subject: "old"}
	if err := s.cache.PutRows(ctx, []cache.Row{row}); err != nil {
		t.Fatal(err)
	}
	raw := []byte("Subject: old\r\n\r\nold text\r\n")
	if ok, err := s.cache.PutBody(ctx, row.Ref(), cache.Body{Raw: raw}, false); err != nil || !ok {
		t.Fatalf("PutBody = %v, %v", ok, err)
	}
	resp, err := (&cacheService{s: s}).EvictCache(ctx, connect.NewRequest(&chainmailv1.EvictCacheRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetBodiesEvicted() != 1 || resp.Msg.GetFreedBytes() != int64(len(raw)) {
		t.Errorf("EvictCache = %v, want 1 body of %d bytes", resp.Msg, len(raw))
	}
	want := [4]string{"cache.evict", `["bodies=1","freed=` + strconv.Itoa(len(raw)) + `"]`, "ok", ""}
	if got := lastCacheAudit(t, stateDB); got != want {
		t.Errorf("audit row = %v, want %v", got, want)
	}
}

// Without an open cache EvictCache is refused, and the refusal is audited.
func TestEvictCacheWithoutACache(t *testing.T) {
	t.Parallel()
	s, stateDB := newTestServerOptions(t, cacheConfig, Options{})
	_, err := (&cacheService{s: s}).EvictCache(peerContext(t.Context()), connect.NewRequest(&chainmailv1.EvictCacheRequest{}))
	if got := api.Detail(err).GetReason(); got != api.ReasonInvalidArgument {
		t.Errorf("reason = %q, want INVALID_ARGUMENT (%v)", got, err)
	}
	want := [4]string{"cache.evict", `["bodies=0","freed=0"]`, "error", api.ReasonInvalidArgument}
	if got := lastCacheAudit(t, stateDB); got != want {
		t.Errorf("audit row = %v, want %v", got, want)
	}
}

// A full disk turns the cache off: status says error, and routing gets no
// worker. The hourly probe turns it back on once a write succeeds. Other
// cache errors leave it on.
func TestCacheDiskFull(t *testing.T) {
	t.Parallel()
	s, _ := newCacheServer(t, cacheConfig)
	ctx := t.Context()
	s.cacheAccounts = map[string]*cache.Account{"host": new(cache.Account)}
	var probeErr error
	s.cacheProbe = func(context.Context) error { return probeErr }

	s.noteCacheErr(errors.New("SQLITE_BUSY"))
	if s.cacheDown.Load() || s.cacheAccount("host") == nil {
		t.Fatal("an error other than a full disk turned the cache off")
	}

	probeErr = fmt.Errorf("probe: %w", cache.ErrDiskFull)
	s.cacheHousekeep(ctx)
	if !s.cacheDown.Load() || s.cacheAccount("host") != nil {
		t.Fatal("a full disk did not turn the cache off")
	}
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	want := &chainmailv1.AccountCacheStatus{Account: "host", Enabled: true, State: "error", LastError: api.ReasonInternal, BudgetBytes: 1 << 30}
	if got := s.cacheStatus(ctx, acct, true); !proto.Equal(got, want) {
		t.Errorf("status while the disk is full = %v, want %v", got, want)
	}

	probeErr = nil
	s.cacheHousekeep(ctx)
	if s.cacheDown.Load() || s.cacheAccount("host") == nil {
		t.Error("the cache stayed off after a write succeeded")
	}
}

// budgetRow reads the budget row of account and day from cache.db over a
// connection of its own, since Store.Budget says 0 for a missing row too.
func budgetRow(t *testing.T, s *Server, account, day string) (int64, bool) {
	t.Helper()
	conn, err := sql.Open("sqlite", "file:"+filepath.Join(s.opts.CacheDir, cacheFile)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }() // read-only
	var bytes int64
	err = conn.QueryRowContext(t.Context(), "SELECT bytes FROM budget WHERE account = ? AND day = ?", account, day).Scan(&bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes, true
}

// The default probe writes to cache.db: it touches the first cache
// account's budget row for today and adds nothing.
func TestProbeCacheWrites(t *testing.T) {
	t.Parallel()
	s, _ := newCacheServer(t, cacheConfig)
	day := s.opts.Now().UTC().Format(cacheBudgetDay)
	if _, ok := budgetRow(t, s, "host", day); ok {
		t.Fatal("host has a budget row before the probe")
	}
	if err := s.probeCache(t.Context()); err != nil {
		t.Fatal(err)
	}
	used, err := s.cache.Budget(t.Context(), "host", s.opts.Now().UTC().Format(cacheBudgetDay))
	if err != nil || used != 0 {
		t.Errorf("host budget after the probe = %d, %v; want 0", used, err)
	}
	if bytes, ok := budgetRow(t, s, "host", day); !ok || bytes != 0 {
		t.Errorf("host budget row after the probe = %d, present %v; want a row of 0", bytes, ok)
	}
}

// The probe keeps what the account spent today.
func TestProbeCacheKeepsTheBudget(t *testing.T) {
	t.Parallel()
	s, _ := newCacheServer(t, cacheConfig)
	day := s.opts.Now().UTC().Format(cacheBudgetDay)
	if _, err := s.cache.AddBudget(t.Context(), "host", day, 4096); err != nil {
		t.Fatal(err)
	}
	if err := s.probeCache(t.Context()); err != nil {
		t.Fatal(err)
	}
	if bytes, ok := budgetRow(t, s, "host", day); !ok || bytes != 4096 {
		t.Errorf("host budget row after the probe = %d, present %v; want 4096", bytes, ok)
	}
}

// A write the probe cannot make is its error: here cache.db is closed.
func TestProbeCacheWriteFails(t *testing.T) {
	t.Parallel()
	s, _ := newCacheServer(t, cacheConfig)
	if err := s.cache.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.probeCache(t.Context()); err == nil {
		t.Error("probe of a closed cache.db = nil, want its write error")
	}
}

// housekeepWait is one After call on a housekeepClock: how long it waits
// and the channel the test fires it on.
type housekeepWait struct {
	d    time.Duration
	fire chan time.Time
}

// housekeepClock is a watch.Clock whose timers fire only when the test
// fires them. It sends every After call on waits.
type housekeepClock struct {
	now   time.Time
	waits chan housekeepWait
}

func (c *housekeepClock) Now() time.Time { return c.now }

func (c *housekeepClock) After(d time.Duration) <-chan time.Time {
	w := housekeepWait{d: d, fire: make(chan time.Time, 1)}
	c.waits <- w
	return w.fire
}

// startHousekeeping runs the housekeeping of s on a housekeepClock until
// the test ends, counting the probes. It returns the clock and the count.
func startHousekeeping(t *testing.T, s *Server) (*housekeepClock, *atomic.Int32) {
	t.Helper()
	probes := new(atomic.Int32)
	s.cacheProbe = func(context.Context) error {
		probes.Add(1)
		return nil
	}
	clk := &housekeepClock{now: s.opts.Now(), waits: make(chan housekeepWait, 4)}
	s.cacheClock = clk
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() { s.runCacheHousekeeping(ctx) })
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return clk, probes
}

// nextWait returns the housekeeping's next wait on clk. The bound only
// catches a loop that never waits on the clock; a working one asks at once.
func nextWait(t *testing.T, clk *housekeepClock) housekeepWait {
	t.Helper()
	select {
	case w := <-clk.waits:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("the housekeeping never waited on the cache clock")
		return housekeepWait{}
	}
}

// Housekeeping waits an hour on the cache clock between runs, and each
// timer it fires runs one probe, until its context ends.
func TestCacheHousekeepingRuns(t *testing.T) {
	t.Parallel()
	s, _ := newCacheServer(t, cacheConfig)
	clk, probes := startHousekeeping(t, s)
	w := nextWait(t, clk)
	before := probes.Load()
	for range 2 {
		if w.d != cacheHousekeepInterval {
			t.Errorf("housekeeping waits %v, want %v", w.d, cacheHousekeepInterval)
		}
		w.fire <- clk.now
		// The next wait comes after the fired run's probe.
		w = nextWait(t, clk)
	}
	if n := probes.Load() - before; n != 2 {
		t.Errorf("probes after 2 timers = %d, want 2", n)
	}
}

// newCacheServerOptions is newCacheServer with opts.
func newCacheServerOptions(t *testing.T, configTOML string, opts Options) (*Server, *sql.DB) {
	t.Helper()
	opts.CacheDir = t.TempDir()
	s, stateDB := newTestServerOptions(t, configTOML, opts)
	s.openCache(t.Context())
	t.Cleanup(s.closeCache)
	if s.cache == nil {
		t.Fatalf("cache.db did not open: %v", s.cacheErr)
	}
	return s, stateDB
}

// The cache keeps the daemon's time, not the wall clock: a worker's budget
// day and the evictor's window and retention follow Options.Now, as the
// store's own stamps do.
func TestCacheUsesDaemonTime(t *testing.T) {
	t.Parallel()
	at := time.Date(2041, 3, 14, 12, 0, 0, 0, time.UTC)
	s, _ := newCacheServerOptions(t, cacheConfig, Options{Now: func() time.Time { return at }})
	ctx := peerContext(t.Context())
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.cache.AddBudget(ctx, "host", at.Format(cacheBudgetDay), 1234); err != nil {
		t.Fatal(err)
	}
	s.cacheAccounts = map[string]*cache.Account{"host": cache.NewAccount(s.cacheAccountOptions(acct))}
	if got := s.cacheStatus(ctx, acct, false).GetBudgetUsedBytes(); got != 1234 {
		t.Errorf("budget used = %d, want 1234 spent on the daemon's day %s", got, at.Format(cacheBudgetDay))
	}

	// Yesterday by the wall clock is inside the 90 day window, but years
	// before the daemon's day, so the evictor must take the body.
	recent := time.Now().AddDate(0, 0, -1)
	row := cache.Row{Account: "host", Folder: "INBOX", UIDValidity: 1, UID: 1, InternalDate: recent, Date: recent, Size: 30, Subject: "recent"}
	if err := s.cache.PutRows(ctx, []cache.Row{row}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.cache.PutBody(ctx, row.Ref(), cache.Body{Raw: []byte("Subject: recent\r\n\r\ntext\r\n")}, false); err != nil || !ok {
		t.Fatalf("PutBody = %v, %v", ok, err)
	}
	resp, err := (&cacheService{s: s}).EvictCache(ctx, connect.NewRequest(&chainmailv1.EvictCacheRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if n := resp.Msg.GetBodiesEvicted(); n != 1 {
		t.Errorf("bodies evicted = %d, want 1: the body is outside the daemon's window", n)
	}
}

// Housekeeping runs once at start, before its first hourly wait, so a
// daemon restarted more often than hourly still probes and prunes.
func TestCacheHousekeepingRunsAtStart(t *testing.T) {
	t.Parallel()
	s, _ := newCacheServer(t, cacheConfig)
	clk, probes := startHousekeeping(t, s)
	w := nextWait(t, clk)
	if n := probes.Load(); n != 1 {
		t.Errorf("probes before the first timer = %d, want 1", n)
	}
	if w.d != cacheHousekeepInterval {
		t.Errorf("first wait = %v, want %v", w.d, cacheHousekeepInterval)
	}
}

// When a sweep leaves cache.db over max_size with headers alone, status
// reports a ready or prefetching account as over_cap and keeps every other
// state, which says more about why the cache is not ready.
func TestCacheStatusOverCap(t *testing.T) {
	t.Parallel()
	const day = 1 << 30 // the default daily_download
	tests := []struct {
		name     string
		progress *cache.Progress // INBOX's, nil for none
		syncWith string          // INBOX's sync_folders state
		spent    int64
		overCap  bool
		want     string
	}{
		{name: "ready under the cap", progress: &cache.Progress{BackfillDone: true, WindowDone: true}, want: cache.StateReady},
		{name: "ready", progress: &cache.Progress{BackfillDone: true, WindowDone: true}, overCap: true, want: cache.StateOverCap},
		{name: "prefetching", progress: &cache.Progress{BackfillDone: true}, overCap: true, want: cache.StateOverCap},
		{name: "backfilling", overCap: true, want: cache.StateBackfilling},
		{name: "paused", syncWith: "auth_required", overCap: true, want: cache.StatePaused},
		{name: "budget exhausted", progress: &cache.Progress{BackfillDone: true, WindowDone: true}, spent: day, overCap: true, want: cache.StateBudgetExhausted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, stateDB := newCacheServer(t, cacheConfig)
			ctx := t.Context()
			if _, err := stateDB.ExecContext(ctx,
				"INSERT INTO sync_folders (account, folder, uidvalidity, uidnext, state) VALUES ('host', 'INBOX', 7, 100, ?)", tt.syncWith); err != nil {
				t.Fatal(err)
			}
			if p := tt.progress; p != nil {
				p.Account, p.Folder, p.UIDValidity, p.BaselineUIDNext = "host", "INBOX", 7, 100
				if err := s.cache.PutProgress(ctx, *p); err != nil {
					t.Fatal(err)
				}
			}
			if tt.spent > 0 {
				if _, err := s.cache.AddBudget(ctx, "host", s.opts.Now().UTC().Format(cacheBudgetDay), tt.spent); err != nil {
					t.Fatal(err)
				}
			}
			if tt.overCap {
				// No file fits in 1 byte, and there is no body to evict.
				ev := cache.NewEvictor(cache.EvictorOptions{
					Store:     s.cache,
					Window:    func(string) time.Duration { return 0 },
					Retention: func(string) time.Duration { return 0 },
					MaxSize:   func() int64 { return 1 },
				})
				if _, err := ev.Sweep(ctx); err != nil {
					t.Fatal(err)
				}
				if !s.cache.OverCap() {
					t.Fatal("a sweep with a 1 byte max_size left the cache under the cap")
				}
			}
			acct, err := s.opts.Config.Account("host")
			if err != nil {
				t.Fatal(err)
			}
			s.cacheAccounts = map[string]*cache.Account{"host": cache.NewAccount(s.cacheAccountOptions(acct))}
			if got := s.cacheStatus(ctx, acct, false).GetState(); got != tt.want {
				t.Errorf("state = %q, want %q", got, tt.want)
			}
		})
	}
}

// A status the worker cannot read is state error with an INTERNAL reason.
// A failure other than a full disk, here a closed cache.db, leaves the
// cache on. A full disk on a status read is not tested: Status only
// SELECTs, and making a SELECT return SQLITE_FULL from a real Store needs
// a full temporary file system or a hook the Store does not have.
func TestCacheStatusReadFails(t *testing.T) {
	t.Parallel()
	s, _ := newCacheServer(t, cacheConfig)
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	s.cacheAccounts = map[string]*cache.Account{"host": cache.NewAccount(s.cacheAccountOptions(acct))}
	if err := s.cache.Close(); err != nil {
		t.Fatal(err)
	}
	want := &chainmailv1.AccountCacheStatus{Account: "host", Enabled: true, State: "error", LastError: api.ReasonInternal, BudgetBytes: 1 << 30}
	if got := s.cacheStatus(t.Context(), acct, true); !proto.Equal(got, want) {
		t.Errorf("status of a closed cache.db = %v, want %v", got, want)
	}
	if s.cacheDown.Load() {
		t.Error("a status read that failed without a full disk turned the cache off")
	}
}

// TestCacheWorkerErrorsTurnTheCacheOff checks that a cache worker's
// OnError is the daemon's noteCacheErr: a full disk a worker meets turns
// the cache off at once, and other errors leave it on.
func TestCacheWorkerErrorsTurnTheCacheOff(t *testing.T) {
	t.Parallel()
	s, _ := newCacheServer(t, cacheConfig)
	s.cacheAccounts = map[string]*cache.Account{"host": new(cache.Account)}
	acct, err := s.opts.Config.Account("host")
	if err != nil {
		t.Fatal(err)
	}
	onError := s.cacheAccountOptions(acct).OnError
	if onError == nil {
		t.Fatal("the cache worker has no OnError")
	}
	onError(errors.New("SQLITE_BUSY"))
	if s.cacheDown.Load() {
		t.Fatal("an error other than a full disk turned the cache off")
	}
	onError(fmt.Errorf("storing headers: %w", cache.ErrDiskFull))
	if !s.cacheDown.Load() || s.cacheAccount("host") != nil {
		t.Error("a worker's full disk did not turn the cache off")
	}
}
