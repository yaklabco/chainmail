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
	"github.com/jamesainslie/chainmail/internal/ref"
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

// folderKey names one searched folder of one account.
type folderKey struct{ account, folder string }

// folderCover is how much of one folder a side's pages returned: every
// match under uidValidity whose UID is at least below, or every match
// when below is 0.
type folderCover struct{ uidValidity, below uint32 }

// coverage is what the pages read of one search returned. The daemon
// takes each folder's matches highest UID first, and a cursor source's
// Below is where that folder's next page continues, exclusive, so the
// pages read hold every match of the folder whose UID is at least Below,
// or every match once the source is Done. That holds whatever the arrival
// times, which need not fall as the UID falls: a message moved or copied
// into a folder keeps its INTERNALDATE but takes a new, higher UID. So an
// arrival time can never bound what a side has read, and a UID can.
type coverage struct {
	// all is set when the search ended: every folder is covered whole.
	all     bool
	folders map[folderKey]folderCover
}

// sideCoverage reads the coverage of p from the cursors of its pages. A
// folder Done on one page is left out of later cursors, so coverage
// builds up page by page and Done stays. A source neither Done nor
// bounded failed before any page reached it, and covers nothing.
func sideCoverage(p sourcePages) (coverage, error) {
	if !p.more() {
		return coverage{all: true}, nil
	}
	cov := coverage{folders: map[folderKey]folderCover{}}
	for i, resp := range p {
		if resp.GetNextCursor() == "" {
			continue
		}
		c, err := ref.DecodeCursor(resp.GetNextCursor())
		if err != nil {
			return coverage{}, fmt.Errorf("%w: the cursor of page %d: %w", errCannotCompare, i+1, err)
		}
		for _, s := range c.Sources {
			k := folderKey{s.Account, s.Folder}
			switch {
			case s.Done:
				cov.folders[k] = folderCover{uidValidity: s.UIDValidity}
			case s.Below == 0:
				delete(cov.folders, k)
			default:
				if prev, ok := cov.folders[k]; ok && prev.below == 0 && prev.uidValidity == s.UIDValidity {
					continue // done on an earlier page
				}
				cov.folders[k] = folderCover{uidValidity: s.UIDValidity, below: s.Below}
			}
		}
	}
	return cov, nil
}

// lowest returns the lowest UID of r's folder that c covers, 0 for all,
// and false when c does not cover the folder under r's UIDVALIDITY.
func (c coverage) lowest(r ref.Ref) (uint32, bool) {
	if c.all {
		return 0, true
	}
	fc, ok := c.folders[folderKey{r.Account, r.Folder}]
	if !ok || fc.uidValidity != r.UIDValidity {
		return 0, false
	}
	return fc.below, true
}

// validities maps each folder p's rows and cursors name to its
// UIDVALIDITY.
func validities(p sourcePages, refs map[string]ref.Ref) map[folderKey]uint32 {
	out := map[folderKey]uint32{}
	for _, r := range p.rows() {
		pr := refs[r.GetRef()]
		out[folderKey{pr.Account, pr.Folder}] = pr.UIDValidity
	}
	for _, resp := range p {
		if c, err := ref.DecodeCursor(resp.GetNextCursor()); err == nil {
			for _, s := range c.Sources {
				out[folderKey{s.Account, s.Folder}] = s.UIDValidity
			}
		}
	}
	return out
}

// errCannotCompare marks a comparison the pages read do not allow. It is a
// difference, never an agreement.
var errCannotCompare = errors.New("cannot compare")

// rowsDiff compares one search from the cache and from the server and
// returns the refs only one side has, sorted. It compares only what both
// sides' pages cover (coverage): in each folder, the matches whose UID is
// at least the higher of the two sides' bounds. A late arrival that
// pushed a row off one side's page lowers neither bound past that row,
// so the row is left out of both. Rows that arrived after start are left
// out too, since mail can land between the two searches. A folder under
// different UIDVALIDITY on the two sides is a difference. When no row
// read is left to compare, or a ref or cursor cannot be read, it returns
// errCannotCompare: never agreement.
func rowsDiff(cache, server sourcePages, start time.Time) (onlyCache, onlyServer []string, err error) {
	covCache, err := sideCoverage(cache)
	if err != nil {
		return nil, nil, err
	}
	covServer, err := sideCoverage(server)
	if err != nil {
		return nil, nil, err
	}
	refs := map[string]ref.Ref{}
	for _, r := range append(cache.rows(), server.rows()...) {
		pr, err := ref.Parse(r.GetRef())
		if err != nil {
			return nil, nil, fmt.Errorf("%w: a row's ref: %w", errCannotCompare, err)
		}
		refs[r.GetRef()] = pr
	}
	vServer := validities(server, refs)
	for k, vc := range validities(cache, refs) {
		if vs, ok := vServer[k]; ok && vs != vc {
			return nil, nil, fmt.Errorf("folder %q of %s is under UIDVALIDITY %d in the cache and %d on the server", k.folder, k.account, vc, vs)
		}
	}
	read := 0
	keep := func(rows []*chainmailv1.MessageRow) map[string]bool {
		read += len(rows)
		out := make(map[string]bool, len(rows))
		for _, r := range rows {
			if r.GetInternalDate().AsTime().After(start) {
				continue
			}
			pr := refs[r.GetRef()]
			c, okCache := covCache.lowest(pr)
			s, okServer := covServer.lowest(pr)
			if !okCache || !okServer || pr.UID < max(c, s) {
				continue
			}
			out[r.GetRef()] = true
		}
		return out
	}
	c, s := keep(cache.rows()), keep(server.rows())
	if len(c) == 0 && len(s) == 0 && read > 0 {
		return nil, nil, fmt.Errorf("%w: none of the %d rows read is covered by both sides and older than the start", errCannotCompare, read)
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
// completely, every page must read every folder, and what both sides
// cover must hold the same refs by rowsDiff. It names refs, folders and
// counts only, never message content.
func sourcesDiff(cache, server sourcePages, account string, start time.Time) string {
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
	onlyCache, onlyServer, err := rowsDiff(cache, server, start)
	if err != nil {
		return err.Error()
	}
	if len(onlyCache) == 0 && len(onlyServer) == 0 {
		return ""
	}
	return fmt.Sprintf("%d only in the cache %v, %d only on the server %v",
		len(onlyCache), onlyCache[:min(5, len(onlyCache))], len(onlyServer), onlyServer[:min(5, len(onlyServer))])
}

// msg is a search row of INBOX on acct under UIDVALIDITY 7.
func msg(uid uint32, at time.Time) *chainmailv1.MessageRow {
	return &chainmailv1.MessageRow{Ref: msgRef(uid), InternalDate: timestamppb.New(at)}
}

// msgRef is the ref of msg(uid).
func msgRef(uid uint32) string {
	return ref.Ref{Account: "acct", Folder: "INBOX", UIDValidity: 7, UID: uid}.String()
}

// msgRefs are the refs of msg for uids.
func msgRefs(uids ...uint32) []string {
	out := make([]string, len(uids))
	for i, u := range uids {
		out[i] = msgRef(u)
	}
	slices.Sort(out)
	return out
}

// uidRange are UIDs from high down to low.
func uidRange(high, low uint32) []uint32 {
	var out []uint32
	for u := high; u >= low; u-- {
		out = append(out, u)
	}
	return out
}

// tiedMsgs are msg rows for uids, all arriving at at.
func tiedMsgs(uids []uint32, at time.Time) []*chainmailv1.MessageRow {
	out := make([]*chainmailv1.MessageRow, len(uids))
	for i, u := range uids {
		out[i] = msg(u, at)
	}
	return out
}

// inbox is a cursor source for INBOX of acct under UIDVALIDITY 7.
func inbox(below uint32, done bool) ref.CursorSource {
	return ref.CursorSource{Account: "acct", Folder: "INBOX", UIDValidity: 7, Below: below, Done: done}
}

// page is a search page of rows. With sources it has a next cursor
// holding them; without, the search ended on it.
func page(sources []ref.CursorSource, rows ...*chainmailv1.MessageRow) *chainmailv1.SearchResponse {
	resp := &chainmailv1.SearchResponse{Rows: rows}
	if len(sources) > 0 {
		resp.NextCursor = ref.EncodeCursor(ref.Cursor{Query: ref.Fingerprint("q"), Sources: sources})
	}
	return resp
}

// next is a cursor of sources, for page.
func next(sources ...ref.CursorSource) []ref.CursorSource { return sources }

func TestRowsDiff(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	start := base.Add(time.Hour)
	at := func(minutes int) time.Time { return base.Add(-time.Duration(minutes) * time.Minute) }
	tests := []struct {
		name string
		// cache and server are every page read; rows run in descending
		// UID order, as the daemon takes them from one folder.
		cache, server         sourcePages
		wantCache, wantServer []string
		wantErr               bool
	}{
		{
			name:   "the same rows",
			cache:  sourcePages{page(nil, msg(10, at(1)), msg(9, at(2)))},
			server: sourcePages{page(nil, msg(10, at(1)), msg(9, at(2)))},
		},
		{
			name:       "a row only the server has",
			cache:      sourcePages{page(nil, msg(10, at(1)))},
			server:     sourcePages{page(nil, msg(10, at(1)), msg(9, at(2)))},
			wantServer: msgRefs(9),
		},
		{
			name:      "a row only the cache has",
			cache:     sourcePages{page(nil, msg(10, at(1)), msg(8, at(3)))},
			server:    sourcePages{page(nil, msg(10, at(1)))},
			wantCache: msgRefs(8),
		},
		{
			name:   "a row that arrived after the start is left out",
			cache:  sourcePages{page(nil, msg(10, at(1)))},
			server: sourcePages{page(nil, msg(11, start.Add(time.Second)), msg(10, at(1)))},
		},
		{
			name:   "a late arrival pushes the last row off the later full page",
			cache:  sourcePages{page(next(inbox(8, false)), msg(10, at(1)), msg(9, at(2)), msg(8, at(3)))},
			server: sourcePages{page(next(inbox(9, false)), msg(11, start.Add(time.Second)), msg(10, at(1)), msg(9, at(2)))},
		},
		{
			name:       "a difference above the floor still counts",
			cache:      sourcePages{page(next(inbox(7, false)), msg(10, at(1)), msg(8, at(3)), msg(7, at(4)))},
			server:     sourcePages{page(next(inbox(8, false)), msg(10, at(1)), msg(9, at(2)), msg(8, at(3)))},
			wantServer: msgRefs(9),
		},
		{
			// UIDs 10, 9, 8 arrive at T, before T, T: an arrival-time
			// cutoff at T dropped 9 although both sides read past it.
			name:      "arrival times T, older, T in descending UIDs",
			cache:     sourcePages{page(next(inbox(8, false)), msg(10, at(5)), msg(9, at(60)), msg(8, at(5))), page(next(inbox(7, false)), msg(7, at(90)))},
			server:    sourcePages{page(next(inbox(8, false)), msg(10, at(5)), msg(8, at(5))), page(next(inbox(7, false)), msg(7, at(90)))},
			wantCache: msgRefs(9),
		},
		{
			name:      "arrival times T, older, T differing on the last T row both covered",
			cache:     sourcePages{page(next(inbox(10, false)), msg(10, at(5))), page(next(inbox(9, false)), msg(9, at(60))), page(next(inbox(8, false)), msg(8, at(5)))},
			server:    sourcePages{page(next(inbox(10, false)), msg(10, at(5))), page(next(inbox(9, false)), msg(9, at(60))), page(next(inbox(7, false)), msg(7, at(90)))},
			wantCache: msgRefs(8),
		},
		{
			name:   "a row only one side has read so far is not compared",
			cache:  sourcePages{page(next(inbox(10, false)), msg(10, at(5))), page(next(inbox(9, false)), msg(9, at(60)))},
			server: sourcePages{page(next(inbox(10, false)), msg(10, at(5))), page(next(inbox(9, false)), msg(9, at(60))), page(next(inbox(8, false)), msg(8, at(5)))},
		},
		{
			name:       "full pages that share one time and hold different UIDs",
			cache:      sourcePages{page(next(inbox(51, false)), tiedMsgs(uidRange(100, 51), at(5))...)},
			server:     sourcePages{page(next(inbox(151, false)), tiedMsgs(uidRange(200, 151), at(5))...)},
			wantServer: msgRefs(uidRange(200, 151)...),
		},
		{
			name:       "an empty cache against a full tied server page",
			cache:      sourcePages{page(nil)},
			server:     sourcePages{page(next(inbox(1, false)), tiedMsgs(uidRange(50, 1), at(5))...)},
			wantServer: msgRefs(uidRange(50, 1)...),
		},
		{
			name:   "a folder done on an earlier page stays covered",
			cache:  sourcePages{page(next(inbox(9, true), ref.CursorSource{Account: "acct", Folder: "Sent", UIDValidity: 3, Below: 4}), msg(10, at(1)), msg(9, at(2))), page(next(ref.CursorSource{Account: "acct", Folder: "Sent", UIDValidity: 3, Below: 2}))},
			server: sourcePages{page(nil, msg(10, at(1)), msg(9, at(2)))},
		},
		{
			name:    "a folder under another UIDVALIDITY",
			cache:   sourcePages{page(nil, msg(10, at(1)))},
			server:  sourcePages{page(nil, &chainmailv1.MessageRow{Ref: ref.Ref{Account: "acct", Folder: "INBOX", UIDValidity: 8, UID: 10}.String(), InternalDate: timestamppb.New(at(1))})},
			wantErr: true,
		},
		{
			name:    "a folder neither side covers both of",
			cache:   sourcePages{page(next(ref.CursorSource{Account: "acct", Folder: "Sent", UIDValidity: 3, Below: 4}), msg(10, at(1)))},
			server:  sourcePages{page(next(ref.CursorSource{Account: "acct", Folder: "Sent", UIDValidity: 3, Below: 4}), msg(10, at(1)))},
			wantErr: true,
		},
		{
			name:    "every row read arrived after the start",
			cache:   sourcePages{page(nil, msg(11, start.Add(time.Second)))},
			server:  sourcePages{page(nil, msg(11, start.Add(time.Second)))},
			wantErr: true,
		},
		{
			name:    "a ref that does not parse",
			cache:   sourcePages{page(nil, &chainmailv1.MessageRow{Ref: "a", InternalDate: timestamppb.New(at(1))})},
			server:  sourcePages{page(nil, msg(10, at(1)))},
			wantErr: true,
		},
		{
			name:   "no matches on either side",
			cache:  sourcePages{page(nil)},
			server: sourcePages{page(nil)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotCache, gotServer, err := rowsDiff(tt.cache, tt.server, start)
			if tt.wantErr {
				if err == nil {
					t.Errorf("rowsDiff = %v, %v, want an error", gotCache, gotServer)
				}
				return
			}
			if err != nil || !slices.Equal(gotCache, tt.wantCache) || !slices.Equal(gotServer, tt.wantServer) {
				t.Errorf("rowsDiff = %v, %v, %v, want %v, %v", gotCache, gotServer, err, tt.wantCache, tt.wantServer)
			}
		})
	}
}

func TestSideCoverage(t *testing.T) {
	t.Parallel()
	sent := func(below uint32, done bool) ref.CursorSource {
		return ref.CursorSource{Account: "acct", Folder: "Sent", UIDValidity: 3, Below: below, Done: done}
	}
	tests := []struct {
		name string
		p    sourcePages
		want coverage
	}{
		{name: "the search ended", p: sourcePages{page(next(inbox(9, false))), page(nil)}, want: coverage{all: true}},
		{name: "a bound per folder", p: sourcePages{page(next(inbox(9, false), sent(4, false)))}, want: coverage{folders: map[folderKey]folderCover{{"acct", "INBOX"}: {7, 9}, {"acct", "Sent"}: {3, 4}}}},
		{name: "later pages lower the bound", p: sourcePages{page(next(inbox(9, false))), page(next(inbox(5, false)))}, want: coverage{folders: map[folderKey]folderCover{{"acct", "INBOX"}: {7, 5}}}},
		{name: "done stays done", p: sourcePages{page(next(inbox(9, true), sent(4, false))), page(next(sent(2, false)))}, want: coverage{folders: map[folderKey]folderCover{{"acct", "INBOX"}: {7, 0}, {"acct", "Sent"}: {3, 2}}}},
		{name: "a source no page reached covers nothing", p: sourcePages{page(next(inbox(0, false), sent(4, false)))}, want: coverage{folders: map[folderKey]folderCover{{"acct", "Sent"}: {3, 4}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := sideCoverage(tt.p)
			if err != nil || got.all != tt.want.all || len(got.folders) != len(tt.want.folders) {
				t.Fatalf("sideCoverage = %+v, %v, want %+v", got, err, tt.want)
			}
			for k, v := range tt.want.folders {
				if got.folders[k] != v {
					t.Errorf("sideCoverage %v = %+v, want %+v", k, got.folders[k], v)
				}
			}
		})
	}
	if _, err := sideCoverage(sourcePages{{NextCursor: "not a cursor"}}); !errors.Is(err, errCannotCompare) {
		t.Errorf("sideCoverage of a bad cursor: %v, want errCannotCompare", err)
	}
}

func TestSourcesDiff(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	start := base.Add(time.Hour)
	resp := func(source string, complete bool, reason string, uids ...uint32) *chainmailv1.SearchResponse {
		r := &chainmailv1.SearchResponse{Source: source, AccountCounts: []*chainmailv1.AccountCount{
			{Account: "acct", Source: source, Complete: complete, IncompleteReason: reason},
		}}
		for i, uid := range uids {
			r.Rows = append(r.Rows, msg(uid, base.Add(-time.Duration(i)*time.Minute)))
		}
		return r
	}
	// tied is a full page of 50 rows, UIDs high down, sharing one time,
	// with more after it.
	tied := func(source string, high uint32) *chainmailv1.SearchResponse {
		r := page(next(inbox(high-49, false)), tiedMsgs(uidRange(high, high-49), base)...)
		r.Source = source
		r.AccountCounts = []*chainmailv1.AccountCount{{Account: "acct", Source: source, Complete: true}}
		return r
	}
	// sentOnly is a cache page whose cursor covers Sent but not INBOX.
	sentOnly := resp("cache", true, "", 300)
	sentOnly.NextCursor = ref.EncodeCursor(ref.Cursor{Query: ref.Fingerprint("q"), Sources: []ref.CursorSource{{Account: "acct", Folder: "Sent", UIDValidity: 3, Below: 4}}})
	partial := resp("server", true, "", 10)
	partial.Partial = []*chainmailv1.PartialSource{{Account: "acct", Folder: "INBOX", Reason: "SERVER_UNAVAILABLE"}}
	tests := []struct {
		name          string
		cache, server sourcePages
		want          string // a substring of the difference; "" for none
	}{
		{name: "agree", cache: sourcePages{resp("cache", true, "", 10, 9)}, server: sourcePages{resp("server", true, "", 10, 9)}},
		{name: "incomplete cache", cache: sourcePages{resp("cache", false, "stale", 10)}, server: sourcePages{resp("server", true, "", 10)}, want: `reason "stale"`},
		{name: "server answered the cache request", cache: sourcePages{resp("server", true, "", 10)}, server: sourcePages{resp("server", true, "", 10)}, want: `answered by "server"`},
		{name: "cache answered the server request", cache: sourcePages{resp("cache", true, "", 10)}, server: sourcePages{resp("cache", true, "", 10)}, want: `answered by "cache"`},
		{name: "rows differ", cache: sourcePages{resp("cache", true, "", 10)}, server: sourcePages{resp("server", true, "", 10, 9)}, want: "1 only on the server"},
		{name: "a partial page", cache: sourcePages{resp("cache", true, "", 10)}, server: sourcePages{partial}, want: `server request was partial: could not read acct folder "INBOX": SERVER_UNAVAILABLE`},
		{name: "full tied pages holding different UIDs", cache: sourcePages{tied("cache", 100)}, server: sourcePages{tied("server", 200)}, want: "50 only on the server"},
		{name: "an empty cache against a full tied server page", cache: sourcePages{resp("cache", true, "")}, server: sourcePages{tied("server", 50)}, want: "50 only on the server"},
		{name: "nothing both sides cover", cache: sourcePages{sentOnly}, server: sourcePages{tied("server", 50)}, want: "cannot compare"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sourcesDiff(tt.cache, tt.server, "acct", start)
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
