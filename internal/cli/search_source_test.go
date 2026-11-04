package cli

import (
	"context"
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

// fakeSearch is a MessageService that records each Search request and
// answers it with resp.
type fakeSearch struct {
	chainmailv1connect.UnimplementedMessageServiceHandler

	resp *chainmailv1.SearchResponse

	mu   sync.Mutex
	reqs []*chainmailv1.SearchRequest
}

func (f *fakeSearch) Search(_ context.Context, req *connect.Request[chainmailv1.SearchRequest]) (*connect.Response[chainmailv1.SearchResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req.Msg)
	return connect.NewResponse(proto.Clone(f.resp).(*chainmailv1.SearchResponse)), nil
}

// requests returns the Search requests so far.
func (f *fakeSearch) requests() []*chainmailv1.SearchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// startFakeSearch serves f on a Unix socket until the test ends and returns the socket.
func startFakeSearch(t *testing.T, f *fakeSearch) string {
	t.Helper()
	path, handler := chainmailv1connect.NewMessageServiceHandler(f)
	return startFakeServer(t, path, handler)
}

// sourcedSearch is a page answered by the cache for gm and by the server
// for host.
func sourcedSearch() *chainmailv1.SearchResponse {
	at := timestamppb.New(time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC))
	return &chainmailv1.SearchResponse{
		Count: 2, Source: "mixed",
		Rows: []*chainmailv1.MessageRow{
			{Ref: "r1", Account: "gm", Date: at, Subject: &chainmailv1.Untrusted{Untrusted: "Lunch"}},
			{Ref: "r2", Account: "host", Date: at, Subject: &chainmailv1.Untrusted{Untrusted: "Invoice"}},
		},
		AccountCounts: []*chainmailv1.AccountCount{
			{Account: "gm", Count: 1, Source: "cache", SyncedAt: at, Complete: true},
			{Account: "host", Count: 1, Source: "server", Complete: true},
		},
	}
}

func TestSearchSourceFlag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		args   []string
		source string
	}{
		{name: "unset", args: nil, source: ""},
		{name: "auto", args: []string{"--source", "auto"}, source: "auto"},
		{name: "cache", args: []string{"--source", "cache"}, source: "cache"},
		{name: "server", args: []string{"--source", "server"}, source: "server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSearch{resp: sourcedSearch()}
			socket := startFakeSearch(t, f)
			got := runCLI(t, Env{}, append([]string{"--socket", socket, "search", "from:acme"}, tt.args...)...)
			if got.code != 0 {
				t.Fatalf("search: %+v", got)
			}
			want := []*chainmailv1.SearchRequest{{QueryString: "from:acme", Source: tt.source}}
			if reqs := f.requests(); !slices.EqualFunc(reqs, want, func(a, b *chainmailv1.SearchRequest) bool { return proto.Equal(a, b) }) {
				t.Errorf("requests = %v, want %v", reqs, want)
			}
		})
	}
}

func TestSearchSourceFlagRejectsOtherValues(t *testing.T) {
	t.Parallel()
	f := &fakeSearch{resp: sourcedSearch()}
	socket := startFakeSearch(t, f)
	for _, v := range []string{"", "Cache", "local", "mixed"} {
		got := runCLI(t, Env{}, "--socket", socket, "search", "--source", v, "from:acme")
		if got.code != api.ExitUsage || errorReason(t, got.stderr) != api.ReasonUsage {
			t.Errorf("--source %q: %+v, want exit 2 USAGE", v, got)
		}
		if !strings.Contains(got.stderr, "--source must be auto, cache or server") {
			t.Errorf("--source %q: stderr %q does not list the values", v, got.stderr)
		}
	}
	if reqs := f.requests(); len(reqs) != 0 {
		t.Errorf("a bad --source reached the daemon: %v", reqs)
	}
}

// The table footer names each account's source, and the reason when a
// forced cache answer is incomplete. A daemon that reports no source gets
// no footer.
func TestSearchSourceFooter(t *testing.T) {
	t.Parallel()
	forced := sourcedSearch()
	forced.AccountCounts[0].Complete = false
	forced.AccountCounts[0].IncompleteReason = "backfilling"
	forced.AccountCounts[1] = &chainmailv1.AccountCount{Account: "host", Count: 0, Source: "cache", IncompleteReason: "cache_disabled"}
	forced.NextCursor = "c1.next"
	unsourced := sourcedSearch()
	for _, c := range unsourced.GetAccountCounts() {
		c.Source, c.SyncedAt, c.Complete = "", nil, false
	}
	rows := "DATE              ACCOUNT  FROM  SUBJECT  REF\n" +
		"2026-11-02 08:00  gm             Lunch    r1\n" +
		"2026-11-02 08:00  host           Invoice  r2\n"
	tests := []struct {
		name  string
		r     *chainmailv1.SearchResponse
		count bool
		want  string
	}{
		{name: "rows", r: sourcedSearch(), want: rows + "source: gm cache, host server\n"},
		{
			name: "rows, forced and incomplete", r: forced,
			want: rows + "source: gm cache (incomplete: backfilling), host cache (incomplete: cache_disabled)\nmore: --cursor c1.next\n",
		},
		{name: "rows, no source", r: unsourced, want: rows},
		{
			name: "count", r: sourcedSearch(), count: true,
			want: "ACCOUNT  COUNT\ngm       1\nhost     1\ntotal    2\nsource: gm cache, host server\n",
		},
		{name: "count, no source", r: unsourced, count: true, want: "ACCOUNT  COUNT\ngm       1\nhost     1\ntotal    2\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var b strings.Builder
			var err error
			if tt.count {
				err = countTable(&b, tt.r)
			} else {
				err = searchTable(&b, time.UTC, tt.r)
			}
			if err != nil {
				t.Fatal(err)
			}
			if b.String() != tt.want {
				t.Errorf("output =\n%q\nwant\n%q", b.String(), tt.want)
			}
		})
	}
}

// Account names and reasons come from the daemon, but the footer cleans
// them for the terminal all the same.
func TestSearchSourceFooterCleansText(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	err := sourceLine(&b, []*chainmailv1.AccountCount{{Account: "a\x1b[2J", Source: "cache\t", IncompleteReason: "stale\x07"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "source: a[2J cache (incomplete: stale)\n"; b.String() != want {
		t.Errorf("sourceLine = %q, want %q", b.String(), want)
	}
}

// The search help no longer says has:, filename: and label: are Google
// only, and explains --source.
func TestSearchHelpNamesTheCache(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "search", "--help")
	if got.code != 0 {
		t.Fatalf("search --help: %+v", got)
	}
	for _, want := range []string{"once\n                                their cache is complete", "--source cache", "--source server", "auto, cache or server"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("help lacks %q:\n%s", want, got.stdout)
		}
	}
	if strings.Contains(got.stdout, "for now") {
		t.Errorf("help still says Google only for now:\n%s", got.stdout)
	}
}
