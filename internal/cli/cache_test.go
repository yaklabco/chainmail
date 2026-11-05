package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

// cacheAt is the time the cache fixtures report.
var cacheAt = time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)

// fakeCache is a CacheService for CLI tests. Each func answers its RPC;
// a nil func answers with an empty response. It records every request.
type fakeCache struct {
	chainmailv1connect.UnimplementedCacheServiceHandler

	status  func(*chainmailv1.GetCacheStatusRequest) (*chainmailv1.GetCacheStatusResponse, error)
	rebuild func(*chainmailv1.RebuildCacheRequest) (*chainmailv1.RebuildCacheResponse, error)
	evict   func() (*chainmailv1.EvictCacheResponse, error)

	mu   sync.Mutex
	reqs []proto.Message
}

func (f *fakeCache) record(m proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, m)
}

// requests returns every request so far, in order.
func (f *fakeCache) requests() []proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func (f *fakeCache) GetCacheStatus(_ context.Context, req *connect.Request[chainmailv1.GetCacheStatusRequest]) (*connect.Response[chainmailv1.GetCacheStatusResponse], error) {
	f.record(req.Msg)
	if f.status == nil {
		return connect.NewResponse(&chainmailv1.GetCacheStatusResponse{}), nil
	}
	resp, err := f.status(req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeCache) RebuildCache(_ context.Context, req *connect.Request[chainmailv1.RebuildCacheRequest]) (*connect.Response[chainmailv1.RebuildCacheResponse], error) {
	f.record(req.Msg)
	if f.rebuild == nil {
		return connect.NewResponse(&chainmailv1.RebuildCacheResponse{}), nil
	}
	resp, err := f.rebuild(req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeCache) EvictCache(_ context.Context, req *connect.Request[chainmailv1.EvictCacheRequest]) (*connect.Response[chainmailv1.EvictCacheResponse], error) {
	f.record(req.Msg)
	if f.evict == nil {
		return connect.NewResponse(&chainmailv1.EvictCacheResponse{}), nil
	}
	resp, err := f.evict()
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// startFakeCache serves f on a Unix socket until the test ends and returns the socket.
func startFakeCache(t *testing.T, f *fakeCache) string {
	t.Helper()
	path, handler := chainmailv1connect.NewCacheServiceHandler(f)
	return startFakeServer(t, path, handler)
}

// cacheStatusFixture is a cache with an account backfilling, one
// incomplete and one disabled with no folders.
func cacheStatusFixture() *chainmailv1.GetCacheStatusResponse {
	return &chainmailv1.GetCacheStatusResponse{
		SizeBytes: 12 << 20, MaxSizeBytes: 2 << 30,
		Accounts: []*chainmailv1.AccountCacheStatus{
			{
				Account: "gm", Enabled: true, State: "backfilling", BudgetUsedBytes: 3 << 19, BudgetBytes: 1 << 30,
				Folders: []*chainmailv1.FolderCacheStatus{{
					Folder: "[Gmail]/All Mail", State: "backfilling", Headers: 1200, BaselineUid: 9000, BackfillLowUid: 4100,
					Bodies: 300, BodyBytes: 1258291, WindowMessages: 310, WindowBodies: 300,
					LastAppliedAt: timestamppb.New(cacheAt.Add(-2 * time.Minute)),
				}},
			},
			{
				Account: "host", Enabled: true, State: "incomplete", BudgetUsedBytes: 512 << 10, BudgetBytes: 1 << 30,
				Folders: []*chainmailv1.FolderCacheStatus{
					{
						Folder: "INBOX", State: "incomplete", Headers: 42, BackfillDone: true, BaselineUid: 40, BackfillLowUid: 1,
						Bodies: 40, BodyBytes: 80 << 10, WindowMessages: 40, WindowBodies: 38, WindowDone: true, BodyFailures: 2,
						LastAppliedAt: timestamppb.New(cacheAt), Unreadable: 1, AttachmentsUnknown: 3,
					},
					{Folder: "Sent", State: "backfilling", BaselineUid: 7},
				},
			},
			{Account: "old", State: "disabled", BudgetBytes: 1 << 30, LastError: "AUTH_REQUIRED"},
		},
	}
}

func TestCacheStatusGolden(t *testing.T) {
	t.Parallel()
	f := &fakeCache{status: func(*chainmailv1.GetCacheStatusRequest) (*chainmailv1.GetCacheStatusResponse, error) {
		return cacheStatusFixture(), nil
	}}
	socket := startFakeCache(t, f)
	for _, tt := range []struct{ output, golden string }{
		{output: "json", golden: "cache_status_json"},
		{output: "table", golden: "cache_status_table"},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, "--socket", socket, "--output", tt.output, "cache", "status")
			if got.code != 0 {
				t.Fatalf("cache status: %+v", got)
			}
			golden(t, tt.golden, got.stdout)
		})
	}
}

// --account names the accounts to report, and each is passed on.
func TestCacheStatusAccounts(t *testing.T) {
	t.Parallel()
	f := &fakeCache{}
	socket := startFakeCache(t, f)
	got := runCLI(t, Env{}, "--socket", socket, "cache", "status", "--account", "gm", "--account", "host")
	if got.code != 0 {
		t.Fatalf("cache status: %+v", got)
	}
	want := []proto.Message{&chainmailv1.GetCacheStatusRequest{Accounts: []string{"gm", "host"}}}
	if reqs := f.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
		t.Errorf("requests = %v, want %v", reqs, want)
	}
}

// Without folders the status table stops after the accounts, and a server
// chosen folder name or state is cleaned for the terminal.
func TestCacheStatusTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		r    *chainmailv1.GetCacheStatusResponse
		want string
	}{
		{
			name: "no folders",
			r: &chainmailv1.GetCacheStatusResponse{SizeBytes: 4096, MaxSizeBytes: 64 << 20, Accounts: []*chainmailv1.AccountCacheStatus{
				{Account: "host", Enabled: true, State: "backfilling", BudgetBytes: 16 << 20},
			}},
			want: "size  4.0 KiB of 64.0 MiB\n" +
				"\n" +
				"ACCOUNT  CACHE  STATE        BUDGET TODAY     ERROR\n" +
				"host     on     backfilling  0 B of 16.0 MiB  -\n",
		},
		{
			name: "server text cleaned",
			r: &chainmailv1.GetCacheStatusResponse{Accounts: []*chainmailv1.AccountCacheStatus{
				{Account: "host", Enabled: true, State: "ready", Folders: []*chainmailv1.FolderCacheStatus{{Folder: "a\tb\x1b[2J", State: "ready\x07", BackfillDone: true}}},
			}},
			want: "size  0 B of 0 B\n" +
				"\n" +
				"ACCOUNT  CACHE  STATE  BUDGET TODAY  ERROR\n" +
				"host     on     ready  0 B of 0 B    -\n" +
				"\n" +
				"ACCOUNT  FOLDER  STATE  HEADERS  BACKFILL  BODIES  BODY SIZE  WINDOW  FAILED  UNREADABLE  ATTACH UNKNOWN  LAST APPLIED\n" +
				"host     a b[2J  ready  0        done      0       0 B        0/0     0       0           0               -\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			if err := cacheStatusTable(&b, time.UTC, tt.r); err != nil {
				t.Fatal(err)
			}
			if b.String() != tt.want {
				t.Errorf("cacheStatusTable =\n%q\nwant\n%q", b.String(), tt.want)
			}
		})
	}
}

func TestCacheRebuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output string
		args   []string
		want   []proto.Message
		stdout string
	}{
		{
			name: "one folder", output: "json", args: []string{"--account", "host", "--folder", "INBOX"},
			want:   []proto.Message{&chainmailv1.RebuildCacheRequest{Account: "host", Folder: "INBOX"}},
			stdout: `{"accounts":[{"account":"host","folders":["INBOX"]}]}` + "\n",
		},
		{
			name: "one account", output: "table", args: []string{"--account", "host"},
			want:   []proto.Message{&chainmailv1.RebuildCacheRequest{Account: "host"}},
			stdout: "rebuilt host: INBOX, Sent\n",
		},
		{
			name: "every account", output: "json", args: nil,
			want: []proto.Message{
				&chainmailv1.GetCacheStatusRequest{},
				&chainmailv1.RebuildCacheRequest{Account: "gm"},
				&chainmailv1.RebuildCacheRequest{Account: "host"},
			},
			stdout: `{"accounts":[{"account":"gm","folders":["[Gmail]/All Mail"]},{"account":"host","folders":["INBOX","Sent"]}],"skipped":["old"]}` + "\n",
		},
		{
			name: "every account, as a table", output: "table", args: nil,
			want: []proto.Message{
				&chainmailv1.GetCacheStatusRequest{},
				&chainmailv1.RebuildCacheRequest{Account: "gm"},
				&chainmailv1.RebuildCacheRequest{Account: "host"},
			},
			stdout: "rebuilt gm: [Gmail]/All Mail\nrebuilt host: INBOX, Sent\nskipped old: cache disabled\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCache{
				status: func(*chainmailv1.GetCacheStatusRequest) (*chainmailv1.GetCacheStatusResponse, error) {
					return cacheStatusFixture(), nil
				},
				rebuild: rebuildLikeDaemon(cacheStatusFixture(), map[string][]string{"gm": {"[Gmail]/All Mail"}, "host": {"INBOX", "Sent"}}),
			}
			socket := startFakeCache(t, f)
			got := runCLI(t, Env{}, append([]string{"--socket", socket, "--output", tt.output, "cache", "rebuild"}, tt.args...)...)
			if got.code != 0 {
				t.Fatalf("cache rebuild: %+v", got)
			}
			if got.stdout != tt.stdout {
				t.Errorf("stdout = %q, want %q", got.stdout, tt.stdout)
			}
			if reqs := f.requests(); !slices.EqualFunc(reqs, tt.want, proto.Equal) {
				t.Errorf("requests = %v, want %v", reqs, tt.want)
			}
		})
	}
}

// rebuildLikeDaemon answers RebuildCache as the daemon does: an account
// whose cache status is not enabled has no cache worker and is refused
// with INVALID_ARGUMENT, a folder rebuild drops that folder, and an
// account rebuild drops the folders folders lists for it.
func rebuildLikeDaemon(status *chainmailv1.GetCacheStatusResponse, folders map[string][]string) func(*chainmailv1.RebuildCacheRequest) (*chainmailv1.RebuildCacheResponse, error) {
	return func(req *chainmailv1.RebuildCacheRequest) (*chainmailv1.RebuildCacheResponse, error) {
		for _, a := range status.GetAccounts() {
			if a.GetAccount() == req.GetAccount() && !a.GetEnabled() {
				return nil, api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "run: chainmail cache status", fmt.Errorf("the cache is off for account %s", req.GetAccount()))
			}
		}
		if req.GetFolder() != "" {
			return &chainmailv1.RebuildCacheResponse{Folders: []string{req.GetFolder()}}, nil
		}
		return &chainmailv1.RebuildCacheResponse{Folders: folders[req.GetAccount()]}, nil
	}
}

// Rebuilding every account skips the accounts whose cache is disabled, even
// when one sorts first, and says which it skipped. Naming a disabled account
// still asks the daemon, so its refusal reaches the user.
func TestCacheRebuildSkipsDisabledAccounts(t *testing.T) {
	t.Parallel()
	status := &chainmailv1.GetCacheStatusResponse{Accounts: []*chainmailv1.AccountCacheStatus{
		{Account: "aaa", State: "disabled"},
		{Account: "host", Enabled: true, State: "ready"},
		{Account: "new", Enabled: true, State: "backfilling"},
		{Account: "zzz", State: "disabled"},
	}}
	tests := []struct {
		name   string
		output string
		stdout string
	}{
		{
			name: "json", output: "json",
			stdout: `{"accounts":[{"account":"host","folders":["INBOX"]},{"account":"new","folders":[]}],"skipped":["aaa","zzz"]}` + "\n",
		},
		{
			name: "table", output: "table",
			stdout: "rebuilt host: INBOX\nrebuilt new: no cached folders\nskipped aaa: cache disabled\nskipped zzz: cache disabled\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeCache{
				status: func(*chainmailv1.GetCacheStatusRequest) (*chainmailv1.GetCacheStatusResponse, error) {
					return status, nil
				},
				rebuild: rebuildLikeDaemon(status, map[string][]string{"host": {"INBOX"}}),
			}
			socket := startFakeCache(t, f)
			got := runCLI(t, Env{}, "--socket", socket, "--output", tt.output, "cache", "rebuild")
			if got.code != 0 {
				t.Fatalf("cache rebuild: %+v", got)
			}
			if got.stdout != tt.stdout {
				t.Errorf("stdout = %q, want %q", got.stdout, tt.stdout)
			}
			want := []proto.Message{
				&chainmailv1.GetCacheStatusRequest{},
				&chainmailv1.RebuildCacheRequest{Account: "host"},
				&chainmailv1.RebuildCacheRequest{Account: "new"},
			}
			if reqs := f.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
				t.Errorf("requests = %v, want %v", reqs, want)
			}
		})
	}

	t.Run("named", func(t *testing.T) {
		t.Parallel()
		f := &fakeCache{rebuild: rebuildLikeDaemon(status, nil)}
		socket := startFakeCache(t, f)
		got := runCLI(t, Env{}, "--socket", socket, "cache", "rebuild", "--account", "aaa")
		if got.code != api.ExitUsage || errorReason(t, got.stderr) != api.ReasonInvalidArgument || got.stdout != "" {
			t.Errorf("cache rebuild --account aaa: %+v, want exit %d %s and no stdout", got, api.ExitUsage, api.ReasonInvalidArgument)
		}
		want := []proto.Message{&chainmailv1.RebuildCacheRequest{Account: "aaa"}}
		if reqs := f.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
			t.Errorf("requests = %v, want %v", reqs, want)
		}
	})
}

func TestCacheEvict(t *testing.T) {
	t.Parallel()
	f := &fakeCache{evict: func() (*chainmailv1.EvictCacheResponse, error) {
		return &chainmailv1.EvictCacheResponse{BodiesEvicted: 12, FreedBytes: 3 << 19}, nil
	}}
	socket := startFakeCache(t, f)
	for _, tt := range []struct{ output, want string }{
		{output: "json", want: `{"bodies_evicted":"12","freed_bytes":"1572864"}` + "\n"},
		{output: "table", want: "evicted 12 bodies, freed 1.5 MiB\n"},
	} {
		got := runCLI(t, Env{}, "--socket", socket, "--output", tt.output, "cache", "evict")
		if got.code != 0 || got.stdout != tt.want {
			t.Errorf("cache evict --output %s: %+v, want stdout %q", tt.output, got, tt.want)
		}
	}
}

// Daemon errors keep their reason and exit code, and a failed rebuild of
// one account stops before the next.
func TestCacheReportsDaemonErrors(t *testing.T) {
	t.Parallel()
	f := &fakeCache{
		status: func(*chainmailv1.GetCacheStatusRequest) (*chainmailv1.GetCacheStatusResponse, error) {
			return cacheStatusFixture(), nil
		},
		rebuild: func(req *chainmailv1.RebuildCacheRequest) (*chainmailv1.RebuildCacheResponse, error) {
			return nil, api.New(connect.CodeNotFound, api.ReasonNotFound, "run: chainmail cache status", fmt.Errorf("no account %s", req.GetAccount()))
		},
		evict: func() (*chainmailv1.EvictCacheResponse, error) {
			return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("peer refused"))
		},
	}
	socket := startFakeCache(t, f)
	tests := []struct {
		name   string
		args   []string
		code   int
		reason string
	}{
		{name: "rebuild unknown account", args: []string{"cache", "rebuild", "--account", "nope"}, code: api.ExitNotFound, reason: api.ReasonNotFound},
		{name: "evict refused", args: []string{"cache", "evict"}, code: api.ExitDenied, reason: api.ReasonForbidden},
	}
	for _, tt := range tests {
		got := runCLI(t, Env{}, append([]string{"--socket", socket}, tt.args...)...)
		if got.code != tt.code || errorReason(t, got.stderr) != tt.reason || got.stdout != "" {
			t.Errorf("%s: %+v, want exit %d %s and no stdout", tt.name, got, tt.code, tt.reason)
		}
	}
	all := runCLI(t, Env{}, "--socket", socket, "cache", "rebuild")
	if all.code != api.ExitNotFound {
		t.Errorf("rebuild of every account: %+v, want exit %d", all, api.ExitNotFound)
	}
	var rebuilds int
	for _, r := range f.requests() {
		if _, ok := r.(*chainmailv1.RebuildCacheRequest); ok {
			rebuilds++
		}
	}
	if rebuilds != 2 { // nope, then gm; host and old are never tried
		t.Errorf("%d RebuildCache calls, want 2", rebuilds)
	}
}

func TestCacheUsageErrors(t *testing.T) {
	t.Parallel()
	f := &fakeCache{}
	socket := startFakeCache(t, f)
	for _, args := range [][]string{
		{"cache"},
		{"cache", "nope"},
		{"cache", "status", "extra"},
		{"cache", "rebuild", "--folder", "INBOX"},
		{"cache", "evict", "extra"},
	} {
		got := runCLI(t, Env{}, append([]string{"--socket", socket}, args...)...)
		if got.code != api.ExitUsage || errorReason(t, got.stderr) != api.ReasonUsage {
			t.Errorf("%v: %+v, want exit 2 USAGE", args, got)
		}
	}
	if reqs := f.requests(); len(reqs) != 0 {
		t.Errorf("usage errors reached the daemon: %v", reqs)
	}
}

func TestByteSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{80 << 10, "80.0 KiB"},
		{1258291, "1.2 MiB"},
		{2 << 30, "2.0 GiB"},
		{1 << 40, "1.0 TiB"},
		{5 << 50, "5120.0 TiB"},
	}
	for _, tt := range tests {
		if got := byteSize(tt.n); got != tt.want {
			t.Errorf("byteSize(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
