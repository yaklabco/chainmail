package live

import (
	"errors"
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

// cacheSkipReason says why an account whose cache status after the wait is
// st is not compared, or returns "" when its cache is ready.
func cacheSkipReason(st *chainmailv1.AccountCacheStatus) string {
	switch st.GetState() {
	case "ready":
		return ""
	case "disabled":
		return "the cache is disabled for this account"
	}
	return fmt.Sprintf("the cache is %q after the wait, last error %q; run again once chainmail cache status shows it ready",
		st.GetState(), st.GetLastError())
}

// sourcePages is one search forced to one source: every page read, first
// to last.
type sourcePages []*chainmailv1.SearchResponse

// rows returns the rows of every page read, in order.
func (p sourcePages) rows() []*chainmailv1.MessageRow {
	var out []*chainmailv1.MessageRow
	for _, resp := range p {
		out = append(out, resp.GetRows()...)
	}
	return out
}

// more reports whether the last page read has a next cursor: the source
// has matches that were not read.
func (p sourcePages) more() bool {
	return len(p) > 0 && p[len(p)-1].GetNextCursor() != ""
}

// lastDate is the arrival time of the last row read, and false when no
// row was read.
func (p sourcePages) lastDate() (time.Time, bool) {
	rows := p.rows()
	if len(rows) == 0 {
		return time.Time{}, false
	}
	return rows[len(rows)-1].GetInternalDate().AsTime(), true
}

// compareCutoff is the arrival time from which the two sides are compared,
// taken from the first pages read: the newest last row of a side with
// more pages. A late arrival can push the rows at that time, and only
// those, off the end of the later search's page, so the comparison covers
// every row at or after it. It is zero when both sides were read to the
// end, and every row is compared.
func compareCutoff(cache, server sourcePages) time.Time {
	var cutoff time.Time
	for _, p := range []sourcePages{cache, server} {
		if !p.more() {
			continue
		}
		if last, ok := p.lastDate(); ok && last.After(cutoff) {
			cutoff = last
		}
	}
	return cutoff
}

// needsMore reports whether p must be read further before it holds every
// match at or after cutoff: it has more pages and its last row is not yet
// older than cutoff. Reading on until then completes a group of rows that
// share the cutoff's time, however the page split it.
func needsMore(p sourcePages, cutoff time.Time) bool {
	if !p.more() {
		return false
	}
	last, ok := p.lastDate()
	return !ok || !last.Before(cutoff)
}

// errCannotCompare marks a comparison the pages read do not allow. It is a
// difference, never an agreement.
var errCannotCompare = errors.New("cannot compare")

// rowsDiff compares one search from the cache and from the server and
// returns the refs only one side has, sorted. Rows that arrived after
// start are left out of both, since mail can land between the two
// searches, and so are rows older than cutoff, which a side may not have
// read. Both sides must hold every match at or after cutoff (needsMore),
// and some row must be left to compare when either side read any:
// otherwise it returns errCannotCompare.
func rowsDiff(cache, server sourcePages, start, cutoff time.Time) (onlyCache, onlyServer []string, err error) {
	for _, side := range []struct {
		name string
		p    sourcePages
	}{{"cache", cache}, {"server", server}} {
		if needsMore(side.p, cutoff) {
			return nil, nil, fmt.Errorf("%w: the %s's rows at or after %v did not end within the %d pages read",
				errCannotCompare, side.name, cutoff.UTC(), len(side.p))
		}
	}
	read := 0
	keep := func(rows []*chainmailv1.MessageRow) map[string]bool {
		read += len(rows)
		out := make(map[string]bool, len(rows))
		for _, r := range rows {
			d := r.GetInternalDate().AsTime()
			if d.After(start) || d.Before(cutoff) {
				continue
			}
			out[r.GetRef()] = true
		}
		return out
	}
	c, s := keep(cache.rows()), keep(server.rows())
	if len(c) == 0 && len(s) == 0 && read > 0 {
		return nil, nil, fmt.Errorf("%w: all %d rows read arrived after the start or before %v", errCannotCompare, read, cutoff.UTC())
	}
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
	return onlyCache, onlyServer, nil
}

// partialDiff describes the folders a search page could not read, or
// returns "" when it read them all. A partial page is a difference for
// the try that got it, not a failure by itself: a later try may read
// every folder.
func partialDiff(partial []*chainmailv1.PartialSource) string {
	var parts []string
	for _, p := range partial {
		parts = append(parts, fmt.Sprintf("could not read %s folder %q: %s: %s (%s)",
			p.GetAccount(), p.GetFolder(), p.GetReason(), p.GetMessage(), p.GetHint()))
	}
	return strings.Join(parts, "; ")
}

// sourcesDiff describes how a search of account forced to the cache and the
// same search forced to the server disagree, or returns "" when they
// agree: each must be answered by the source asked for, the cache
// completely, every page must read every folder, and the rows from cutoff
// on must hold the same refs by rowsDiff. It names refs and counts only,
// never message content.
func sourcesDiff(cache, server sourcePages, account string, start, cutoff time.Time) string {
	for _, side := range []struct {
		name string
		p    sourcePages
	}{{"cache", cache}, {"server", server}} {
		for i, resp := range side.p {
			if d := partialDiff(resp.GetPartial()); d != "" {
				return fmt.Sprintf("page %d of the %s request was partial: %s", i+1, side.name, d)
			}
		}
	}
	count := func(p sourcePages) *chainmailv1.AccountCount {
		if len(p) == 0 {
			return nil
		}
		for _, ac := range p[0].GetAccountCounts() {
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
	onlyCache, onlyServer, err := rowsDiff(cache, server, start, cutoff)
	if err != nil {
		return err.Error()
	}
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

// page is a search page of rows, with a next cursor when more is set.
func page(more bool, rows ...*chainmailv1.MessageRow) *chainmailv1.SearchResponse {
	resp := &chainmailv1.SearchResponse{Rows: rows}
	if more {
		resp.NextCursor = "next"
	}
	return resp
}

// tiedRows are n rows named prefix00 and on, all arriving at at.
func tiedRows(prefix string, n int, at time.Time) []*chainmailv1.MessageRow {
	out := make([]*chainmailv1.MessageRow, n)
	for i := range out {
		out[i] = row(fmt.Sprintf("%s%02d", prefix, i), at)
	}
	return out
}

// tiedRefs are the refs of tiedRows.
func tiedRefs(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%02d", prefix, i)
	}
	return out
}

func TestRowsDiff(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	start := base.Add(time.Hour)
	at := func(minutes int) time.Time { return base.Add(-time.Duration(minutes) * time.Minute) }
	tests := []struct {
		name string
		// cache and server are every page read. The cutoff comes from
		// their first pages, as compareSources takes it.
		cache, server         sourcePages
		wantCache, wantServer []string
		wantErr               bool
	}{
		{
			name:   "the same rows",
			cache:  sourcePages{page(false, row("a", at(1)), row("b", at(2)))},
			server: sourcePages{page(false, row("a", at(1)), row("b", at(2)))},
		},
		{
			name:       "a row only the server has",
			cache:      sourcePages{page(false, row("a", at(1)))},
			server:     sourcePages{page(false, row("a", at(1)), row("b", at(2)))},
			wantServer: []string{"b"},
		},
		{
			name:      "a row only the cache has",
			cache:     sourcePages{page(false, row("a", at(1)), row("c", at(3)))},
			server:    sourcePages{page(false, row("a", at(1)))},
			wantCache: []string{"c"},
		},
		{
			name:   "a row that arrived after the start is left out",
			cache:  sourcePages{page(false, row("a", at(1)))},
			server: sourcePages{page(false, row("new", start.Add(time.Second)), row("a", at(1)))},
		},
		{
			name:   "a late arrival pushes the last row off the later full page",
			cache:  sourcePages{page(true, row("a", at(1)), row("b", at(2)), row("c", at(3)))},
			server: sourcePages{page(true, row("new", start.Add(time.Second)), row("a", at(1)), row("b", at(2))), page(false, row("c", at(3)))},
		},
		{
			name:       "a difference above the floor still counts",
			cache:      sourcePages{page(true, row("a", at(1)), row("c", at(3)), row("d", at(4)))},
			server:     sourcePages{page(true, row("a", at(1)), row("b", at(2)), row("c", at(3))), page(false, row("d", at(4)))},
			wantServer: []string{"b"},
		},
		{
			name:       "disjoint full pages that share one time are compared whole",
			cache:      sourcePages{page(true, tiedRows("c", 50, at(5))...), page(false, row("x", at(6)))},
			server:     sourcePages{page(true, tiedRows("s", 50, at(5))...), page(false, row("x", at(6)))},
			wantCache:  tiedRefs("c", 50),
			wantServer: tiedRefs("s", 50),
		},
		{
			name:    "disjoint full pages that share one time, read no further",
			cache:   sourcePages{page(true, tiedRows("c", 50, at(5))...)},
			server:  sourcePages{page(true, tiedRows("s", 50, at(5))...)},
			wantErr: true,
		},
		{
			name:       "an empty cache against a full tied server page",
			cache:      sourcePages{page(false)},
			server:     sourcePages{page(true, tiedRows("s", 50, at(5))...), page(false, row("y", at(6)))},
			wantServer: tiedRefs("s", 50),
		},
		{
			name:    "an empty cache against a full tied server page, read no further",
			cache:   sourcePages{page(false)},
			server:  sourcePages{page(true, tiedRows("s", 50, at(5))...)},
			wantErr: true,
		},
		{
			name:       "a tied group split across the server's pages is compared whole",
			cache:      sourcePages{page(true, row("a", at(1)), row("t1", at(2)), row("t2", at(2))), page(false, row("t3", at(2)), row("z", at(9)))},
			server:     sourcePages{page(true, row("a", at(1)), row("t1", at(2)), row("t3", at(2))), page(false, row("t2", at(2)), row("t4", at(2)))},
			wantServer: []string{"t4"},
		},
		{
			name:    "every row read arrived after the start",
			cache:   sourcePages{page(false, row("new", start.Add(time.Second)))},
			server:  sourcePages{page(false, row("new", start.Add(time.Second)))},
			wantErr: true,
		},
		{
			name:   "no matches on either side",
			cache:  sourcePages{page(false)},
			server: sourcePages{page(false)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cutoff := compareCutoff(tt.cache[:1], tt.server[:1])
			gotCache, gotServer, err := rowsDiff(tt.cache, tt.server, start, cutoff)
			if tt.wantErr {
				if !errors.Is(err, errCannotCompare) {
					t.Errorf("rowsDiff = %v, %v, %v, want errCannotCompare", gotCache, gotServer, err)
				}
				return
			}
			if err != nil || !slices.Equal(gotCache, tt.wantCache) || !slices.Equal(gotServer, tt.wantServer) {
				t.Errorf("rowsDiff = %v, %v, %v, want %v, %v", gotCache, gotServer, err, tt.wantCache, tt.wantServer)
			}
		})
	}
}

func TestCompareCutoff(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		cache, server sourcePages
		want          time.Time
	}{
		{name: "both read to the end", cache: sourcePages{page(false, row("a", base))}, server: sourcePages{page(false, row("a", base))}},
		{name: "the newer last row of the sides with more", cache: sourcePages{page(true, row("a", base), row("b", base.Add(-2*time.Minute)))}, server: sourcePages{page(true, row("a", base), row("c", base.Add(-time.Minute)))}, want: base.Add(-time.Minute)},
		{name: "a side read to the end does not count", cache: sourcePages{page(false, row("a", base))}, server: sourcePages{page(true, row("a", base), row("c", base.Add(-time.Minute)))}, want: base.Add(-time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := compareCutoff(tt.cache, tt.server); !got.Equal(tt.want) {
				t.Errorf("compareCutoff = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNeedsMore(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		p    sourcePages
		want bool
	}{
		{name: "read to the end", p: sourcePages{page(false, row("a", base))}},
		{name: "the last row is at the cutoff", p: sourcePages{page(true, row("a", base))}, want: true},
		{name: "the last row is older than the cutoff", p: sourcePages{page(true, row("a", base.Add(-time.Second)))}},
		{name: "more pages but no row yet", p: sourcePages{page(true)}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := needsMore(tt.p, base); got != tt.want {
				t.Errorf("needsMore = %v, want %v", got, tt.want)
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
	// tied is a full page of 50 rows sharing one time, with more after it.
	tied := func(source, prefix string) *chainmailv1.SearchResponse {
		r := resp(source, true, "")
		r.Rows = tiedRows(prefix, 50, base)
		r.NextCursor = "next"
		return r
	}
	partial := resp("server", true, "", "a")
	partial.Partial = []*chainmailv1.PartialSource{{Account: "acct", Folder: "INBOX", Reason: "SERVER_UNAVAILABLE"}}
	tests := []struct {
		name          string
		cache, server sourcePages
		want          string // a substring of the difference; "" for none
	}{
		{name: "agree", cache: sourcePages{resp("cache", true, "", "a", "b")}, server: sourcePages{resp("server", true, "", "a", "b")}},
		{name: "incomplete cache", cache: sourcePages{resp("cache", false, "stale", "a")}, server: sourcePages{resp("server", true, "", "a")}, want: `reason "stale"`},
		{name: "server answered the cache request", cache: sourcePages{resp("server", true, "", "a")}, server: sourcePages{resp("server", true, "", "a")}, want: `answered by "server"`},
		{name: "cache answered the server request", cache: sourcePages{resp("cache", true, "", "a")}, server: sourcePages{resp("cache", true, "", "a")}, want: `answered by "cache"`},
		{name: "rows differ", cache: sourcePages{resp("cache", true, "", "a")}, server: sourcePages{resp("server", true, "", "a", "b")}, want: "1 only on the server"},
		{name: "a partial page", cache: sourcePages{resp("cache", true, "", "a")}, server: sourcePages{partial}, want: `server request was partial: could not read acct folder "INBOX": SERVER_UNAVAILABLE`},
		{name: "disjoint full pages with one tied time", cache: sourcePages{tied("cache", "c")}, server: sourcePages{tied("server", "s")}, want: "cannot compare"},
		{name: "an empty cache against a full tied server page", cache: sourcePages{resp("cache", true, "")}, server: sourcePages{tied("server", "s")}, want: "cannot compare"},
		{name: "an empty cache against a full tied server page read to the end", cache: sourcePages{resp("cache", true, "")}, server: sourcePages{tied("server", "s"), resp("server", true, "")}, want: "50 only on the server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cutoff := compareCutoff(tt.cache[:1], tt.server[:1])
			got := sourcesDiff(tt.cache, tt.server, "acct", start, cutoff)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Errorf("sourcesDiff = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPartialDiff(t *testing.T) {
	t.Parallel()
	if got := partialDiff(nil); got != "" {
		t.Errorf("partialDiff(nil) = %q, want none", got)
	}
	got := partialDiff([]*chainmailv1.PartialSource{
		{Account: "acct", Folder: "INBOX", Reason: "SERVER_UNAVAILABLE", Message: "timed out", Hint: "try again"},
		{Account: "acct", Folder: "Sent", Reason: "AUTH"},
	})
	for _, want := range []string{`acct folder "INBOX": SERVER_UNAVAILABLE: timed out (try again)`, `acct folder "Sent": AUTH`} {
		if !strings.Contains(got, want) {
			t.Errorf("partialDiff = %q, want it to hold %q", got, want)
		}
	}
}

func TestCacheSkipReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		st   *chainmailv1.AccountCacheStatus
		want string // a substring of the reason; "" for none
	}{
		{name: "ready", st: &chainmailv1.AccountCacheStatus{State: "ready"}},
		{name: "disabled", st: &chainmailv1.AccountCacheStatus{State: "disabled"}, want: "the cache is disabled for this account"},
		{name: "not ready in time", st: &chainmailv1.AccountCacheStatus{State: "backfilling", LastError: "slow"}, want: `the cache is "backfilling" after the wait, last error "slow"; run again once chainmail cache status shows it ready`},
		{name: "no status", want: "run again once chainmail cache status shows it ready"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cacheSkipReason(tt.st)
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Errorf("cacheSkipReason = %q, want %q", got, tt.want)
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
