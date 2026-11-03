package daemon

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/cache"
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
