package live

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

const (
	// cacheReadyWithin bounds the wait for every account's cache to report
	// ready (spec section 11.3, Live). A first backfill of a large Google
	// account takes longer; its subtest then skips.
	cacheReadyWithin = 30 * time.Minute
	// cacheQueryReserve is kept from the test's -timeout for the queries
	// after the wait.
	cacheQueryReserve = 10 * time.Minute
)

// cacheReadyDeadline is when the wait for ready caches gives up: after
// cacheReadyWithin, or earlier when the test's deadline would leave less
// than cacheQueryReserve for the queries.
func cacheReadyDeadline(now, deadline time.Time, ok bool) time.Time {
	until := now.Add(cacheReadyWithin)
	if ok {
		if d := deadline.Add(-cacheQueryReserve); d.Before(until) {
			until = d
		}
	}
	return until
}

// rowsDiff compares the first page of one search from the cache and from
// the server and returns the refs only one side has, sorted. Rows that
// arrived after start are left out of both, since mail can land between
// the two searches, and so are rows at or below the newest last row of a
// full page, which such an arrival can push off the end of the later one.
func rowsDiff(cache, server []*chainmailv1.MessageRow, start time.Time, limit int) (onlyCache, onlyServer []string) {
	var floor time.Time
	for _, rows := range [][]*chainmailv1.MessageRow{cache, server} {
		if limit > 0 && len(rows) >= limit {
			if last := rows[len(rows)-1].GetInternalDate().AsTime(); last.After(floor) {
				floor = last
			}
		}
	}
	keep := func(rows []*chainmailv1.MessageRow) map[string]bool {
		out := make(map[string]bool, len(rows))
		for _, r := range rows {
			d := r.GetInternalDate().AsTime()
			if d.After(start) || (!floor.IsZero() && !d.After(floor)) {
				continue
			}
			out[r.GetRef()] = true
		}
		return out
	}
	c, s := keep(cache), keep(server)
	for r := range c {
		if !s[r] {
			onlyCache = append(onlyCache, r)
		}
	}
	for r := range s {
		if !c[r] {
			onlyServer = append(onlyServer, r)
		}
	}
	slices.Sort(onlyCache)
	slices.Sort(onlyServer)
	return onlyCache, onlyServer
}

// sourcesDiff describes how a search of account forced to the cache and the
// same search forced to the server disagree, or returns "" when they
// agree: each must be answered by the source asked for, the cache
// completely, and their first pages must hold the same refs by rowsDiff.
// It names refs and counts only, never message content.
func sourcesDiff(cache, server *chainmailv1.SearchResponse, account string, start time.Time, limit int) string {
	count := func(r *chainmailv1.SearchResponse) *chainmailv1.AccountCount {
		for _, ac := range r.GetAccountCounts() {
			if ac.GetAccount() == account {
				return ac
			}
		}
		return nil
	}
	if ac := count(cache); ac.GetSource() != "cache" || !ac.GetComplete() {
		return fmt.Sprintf("the cache request was answered by %q, complete %v, reason %q", ac.GetSource(), ac.GetComplete(), ac.GetIncompleteReason())
	}
	if ac := count(server); ac.GetSource() != "server" {
		return fmt.Sprintf("the server request was answered by %q", ac.GetSource())
	}
	onlyCache, onlyServer := rowsDiff(cache.GetRows(), server.GetRows(), start, limit)
	if len(onlyCache) == 0 && len(onlyServer) == 0 {
		return ""
	}
	return fmt.Sprintf("%d only in the cache %v, %d only on the server %v",
		len(onlyCache), onlyCache[:min(5, len(onlyCache))], len(onlyServer), onlyServer[:min(5, len(onlyServer))])
}

// row is a search row with a ref and an arrival time.
func row(ref string, at time.Time) *chainmailv1.MessageRow {
	return &chainmailv1.MessageRow{Ref: ref, InternalDate: timestamppb.New(at)}
}

func TestRowsDiff(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	start := base.Add(time.Hour)
	at := func(minutes int) time.Time { return base.Add(-time.Duration(minutes) * time.Minute) }
	tests := []struct {
		name                  string
		cache, server         []*chainmailv1.MessageRow
		limit                 int
		wantCache, wantServer []string
	}{
		{
			name:   "the same rows",
			cache:  []*chainmailv1.MessageRow{row("a", at(1)), row("b", at(2))},
			server: []*chainmailv1.MessageRow{row("a", at(1)), row("b", at(2))},
			limit:  50,
		},
		{
			name:       "a row only the server has",
			cache:      []*chainmailv1.MessageRow{row("a", at(1))},
			server:     []*chainmailv1.MessageRow{row("a", at(1)), row("b", at(2))},
			limit:      50,
			wantServer: []string{"b"},
		},
		{
			name:      "a row only the cache has",
			cache:     []*chainmailv1.MessageRow{row("a", at(1)), row("c", at(3))},
			server:    []*chainmailv1.MessageRow{row("a", at(1))},
			limit:     50,
			wantCache: []string{"c"},
		},
		{
			name:   "a row that arrived after the start is left out",
			cache:  []*chainmailv1.MessageRow{row("a", at(1))},
			server: []*chainmailv1.MessageRow{row("new", start.Add(time.Second)), row("a", at(1))},
			limit:  50,
		},
		{
			name:   "a late arrival pushes the last row off the later full page",
			cache:  []*chainmailv1.MessageRow{row("a", at(1)), row("b", at(2)), row("c", at(3))},
			server: []*chainmailv1.MessageRow{row("new", start.Add(time.Second)), row("a", at(1)), row("b", at(2))},
			limit:  3,
		},
		{
			name:       "a difference above the floor still counts",
			cache:      []*chainmailv1.MessageRow{row("a", at(1)), row("c", at(3)), row("d", at(4))},
			server:     []*chainmailv1.MessageRow{row("a", at(1)), row("b", at(2)), row("c", at(3))},
			limit:      3,
			wantServer: []string{"b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotCache, gotServer := rowsDiff(tt.cache, tt.server, start, tt.limit)
			if !slices.Equal(gotCache, tt.wantCache) || !slices.Equal(gotServer, tt.wantServer) {
				t.Errorf("rowsDiff = %v, %v, want %v, %v", gotCache, gotServer, tt.wantCache, tt.wantServer)
			}
		})
	}
}

func TestSourcesDiff(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	start := base.Add(time.Hour)
	resp := func(source string, complete bool, reason string, refs ...string) *chainmailv1.SearchResponse {
		r := &chainmailv1.SearchResponse{Source: source, AccountCounts: []*chainmailv1.AccountCount{
			{Account: "acct", Source: source, Complete: complete, IncompleteReason: reason},
		}}
		for i, ref := range refs {
			r.Rows = append(r.Rows, row(ref, base.Add(-time.Duration(i)*time.Minute)))
		}
		return r
	}
	tests := []struct {
		name          string
		cache, server *chainmailv1.SearchResponse
		want          string // a substring of the difference; "" for none
	}{
		{name: "agree", cache: resp("cache", true, "", "a", "b"), server: resp("server", true, "", "a", "b")},
		{name: "incomplete cache", cache: resp("cache", false, "stale", "a"), server: resp("server", true, "", "a"), want: `reason "stale"`},
		{name: "server answered the cache request", cache: resp("server", true, "", "a"), server: resp("server", true, "", "a"), want: `answered by "server"`},
		{name: "cache answered the server request", cache: resp("cache", true, "", "a"), server: resp("cache", true, "", "a"), want: `answered by "cache"`},
		{name: "rows differ", cache: resp("cache", true, "", "a"), server: resp("server", true, "", "a", "b"), want: "1 only on the server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sourcesDiff(tt.cache, tt.server, "acct", start, 50)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Errorf("sourcesDiff = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCacheReadyDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		deadline time.Time
		ok       bool
		want     time.Time
	}{
		{name: "no test deadline", want: now.Add(cacheReadyWithin)},
		{name: "a far deadline", deadline: now.Add(2 * time.Hour), ok: true, want: now.Add(cacheReadyWithin)},
		{name: "a deadline that leaves less than the wait", deadline: now.Add(35 * time.Minute), ok: true, want: now.Add(35*time.Minute - cacheQueryReserve)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := cacheReadyDeadline(now, tt.deadline, tt.ok); !got.Equal(tt.want) {
				t.Errorf("cacheReadyDeadline = %v, want %v", got, tt.want)
			}
		})
	}
}
