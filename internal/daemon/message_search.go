package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// Search implements chainmailv1connect.MessageServiceHandler.
func (m *messageService) Search(ctx context.Context, req *connect.Request[chainmailv1.SearchRequest]) (*connect.Response[chainmailv1.SearchResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	resp, entry, searchErr := m.search(ctx, req.Msg)
	if err := m.s.record(ctx, c, entry, messageErr(searchErr)); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// searchPlan is a validated search: what to look for, where, and from where on.
type searchPlan struct {
	tree *query.Node
	in   string
	// named is every account the call covers. accounts is the ones this
	// page searches, which leaves out those a cursor has finished.
	named    []config.ResolvedAccount
	accounts []config.ResolvedAccount
	limit    int
	// resume, when continuing from a cursor, maps account, then folder, to
	// where its next page starts. It holds the cursor's every source, done
	// ones included, so each can be checked against the sources a fresh
	// search picks.
	resume      map[string]map[string]resumePoint
	fingerprint string
	// source is where answers come from: sourceAuto, cache.SourceCache or
	// cache.SourceServer. It is not part of the fingerprint, so a cursor
	// continues on whichever source answers the next page.
	source string
	// itemThreads maps account, then item: value, to the thread keys the
	// value matches on that account, as resolveItems fills it. Nil when the
	// query has no item: term.
	itemThreads map[string]map[string][]string
}

// resumePoint is one cursor source: where its folder continues, or that
// nothing is left in it.
type resumePoint struct {
	bound mailstore.Bound
	done  bool
}

// continuing reports whether the plan resumes from a cursor.
func (p *searchPlan) continuing() bool { return p.resume != nil }

// live reports whether account has a cursor source with matches left.
func (p *searchPlan) live(account string) bool {
	for _, rp := range p.resume[account] {
		if !rp.done {
			return true
		}
	}
	return false
}

// errForgedCursor is a cursor whose sources are not the ones its search
// picks. Cursors carry no MAC, so the daemon never searches a source just
// because a cursor names it.
func errForgedCursor(why string) error {
	return fmt.Errorf("%w: %s", ref.ErrInvalidCursor, why)
}

func (m *messageService) plan(req *chainmailv1.SearchRequest) (*searchPlan, error) {
	source, err := requestSource(req.GetSource())
	if err != nil {
		return nil, err
	}
	n, err := parseSearchQuery(req)
	if err != nil {
		return nil, err
	}
	scope, rest := query.Split(n)
	limit := int(req.GetLimit())
	switch {
	case limit == 0:
		limit = defaultSearchLimit
	case limit > maxSearchLimit:
		return nil, &argError{msg: "limit is at most 500", hint: "page through larger results with the cursor"}
	}
	accts, err := m.s.resolveAccounts(append(slices.Clone(req.GetAccounts()), scope.Accounts...))
	if err != nil {
		return nil, err
	}
	names := make([]string, len(accts))
	for i, a := range accts {
		names[i] = a.Name
	}
	p := &searchPlan{
		tree: rest, in: scope.In, named: accts, accounts: slices.Clone(accts), limit: limit, source: source,
		fingerprint: ref.Fingerprint(rest.String(), scope.In, strings.Join(names, ",")),
	}
	if req.GetCursor() == "" {
		return p, nil
	}
	if req.GetCountOnly() {
		return nil, &argError{msg: "count_only does not take a cursor", hint: "a count covers every match at once"}
	}
	cur, err := ref.DecodeCursor(req.GetCursor())
	if err != nil {
		return nil, err
	}
	if cur.Query != p.fingerprint {
		return nil, &argError{msg: "the cursor belongs to a different search", hint: "pass the cursor with the same query and accounts as the page it came from"}
	}
	p.resume = map[string]map[string]resumePoint{}
	for _, src := range cur.Sources {
		if !slices.Contains(names, src.Account) {
			return nil, errForgedCursor("it names an account this search does not cover")
		}
		if p.resume[src.Account] == nil {
			p.resume[src.Account] = map[string]resumePoint{}
		}
		if _, dup := p.resume[src.Account][src.Folder]; dup {
			return nil, errForgedCursor("it names a folder twice")
		}
		p.resume[src.Account][src.Folder] = resumePoint{bound: mailstore.Bound{UIDValidity: src.UIDValidity, Below: src.Below}, done: src.Done}
	}
	// An account with nothing left is not searched again. Its cursor
	// sources are never searched either, so its folders go unchecked.
	p.accounts = slices.DeleteFunc(p.accounts, func(a config.ResolvedAccount) bool { return !p.live(a.Name) })
	return p, nil
}

// checkSources refuses a cursor naming a folder of account that a fresh
// search, with the same query, does not pick. A genuine cursor meets this
// when a folder was deleted or renamed since its page, so the error says
// to search again rather than to resend the cursor.
func (p *searchPlan) checkSources(account string, results []mailstore.SourceResult) error {
	for folder := range p.resume[account] {
		if !slices.ContainsFunc(results, func(r mailstore.SourceResult) bool { return r.Source.Folder == folder }) {
			return &argError{
				msg:  fmt.Sprintf("%s: it names a folder of %s that this search no longer covers", ref.ErrInvalidCursor, account),
				hint: "the folders changed since the first page, or the cursor was altered; search again without the cursor",
			}
		}
	}
	return nil
}

// uncovered returns account's cursor sources whose folder results lack,
// ordered by folder. A forced cache leaves out folders it does not hold;
// their cursor sources are carried, as a failed source's are, so a later
// page the server answers resumes them, and that page's checkSources
// checks them.
func (p *searchPlan) uncovered(account string, results []mailstore.SourceResult) []ref.CursorSource {
	var out []ref.CursorSource
	for _, cs := range p.cursorSources(account) {
		if !slices.ContainsFunc(results, func(r mailstore.SourceResult) bool { return r.Source.Folder == cs.Folder }) {
			out = append(out, cs)
		}
	}
	return out
}

// resumeAt returns where a folder continues. ok is false when the plan
// resumes from a cursor that has nothing left there, or never had the
// folder, as when the folder's SEARCH failed on the first page. Without a
// cursor every folder starts from the top.
func (p *searchPlan) resumeAt(account, folder string) (resumePoint, bool) {
	if !p.continuing() {
		return resumePoint{}, true
	}
	rp, ok := p.resume[account][folder]
	return rp, ok && !rp.done
}

// cursorSources returns account's cursor sources as the cursor gave them,
// ordered by folder.
func (p *searchPlan) cursorSources(account string) []ref.CursorSource {
	var out []ref.CursorSource
	for _, folder := range slices.Sorted(maps.Keys(p.resume[account])) {
		out = append(out, p.resume[account][folder].cursorSource(account, folder))
	}
	return out
}

// cursorSource is the cursor source rp was read from.
func (rp resumePoint) cursorSource(account, folder string) ref.CursorSource {
	return ref.CursorSource{Account: account, Folder: folder, UIDValidity: rp.bound.UIDValidity, Below: rp.bound.Below, Done: rp.done}
}

// narrow keeps the matches of a fresh source result below where the cursor
// left off. A changed UIDVALIDITY is ErrStaleRef, with fixed text: the
// folder's name came from the server, which a hostile one can phrase as an
// instruction, so only the daemon's logs see it.
func (rp resumePoint) narrow(r mailstore.SourceResult) (mailstore.SourceResult, error) {
	if r.Source.UIDValidity != rp.bound.UIDValidity {
		return r, &fixedTextError{
			msg: "a folder changed since the first page; search again",
			err: fmt.Errorf("%w: %s changed since the first page", mailstore.ErrStaleRef, r.Source.Folder),
		}
	}
	if rp.bound.Below > 0 {
		below := make([]uint32, 0, len(r.UIDs))
		for _, uid := range r.UIDs {
			if uid < rp.bound.Below {
				below = append(below, uid)
			}
		}
		r.UIDs = below
	}
	r.Count = uint32(len(r.UIDs))
	return r, nil
}

// sourceState follows one source through a search: its matches, the rows
// fetched for them, and how many the page used.
type sourceState struct {
	account string
	// from is where the account's page runs, which fetches the rows.
	from      searchSource
	src       mailstore.Source
	uids      []uint32 // every match below the bound, highest first
	requested int      // how many UIDs summaries were asked for
	rows      []mailstore.Summary
	next      int    // rows[:next] were used
	lowest    uint32 // the lowest UID used
	prevBelow uint32 // the bound this page started from
	err       error
	// unreadable are the requested UIDs the server's answer could not be
	// parsed for, which have no row.
	unreadable []uint32
}

func (st *sourceState) consume() {
	st.lowest = st.rows[st.next].Ref.UID
	st.next++
}

type mergedRow struct {
	sum   mailstore.Summary
	count int
}

// accountSearch is one account's part of a page: where it runs, what
// answered, what its Search returned, and what that adds to the page.
type accountSearch struct {
	src     searchSource
	route   routeInfo
	results []mailstore.SourceResult
	err     error
	// part is what collect makes of the rest.
	part accountPart
}

// accountPart is what one account's Search adds to a page.
type accountPart struct {
	// partial and failures are the account's failed Search or folders.
	partial  []*chainmailv1.PartialSource
	failures []error
	// states are its answered sources, count their matches.
	states []*sourceState
	count  uint64
	// carried are its cursor sources a failure, or an incomplete cache
	// answer, kept this page from reaching.
	carried []ref.CursorSource
}

// searchAccount runs a's Search where the router sends it. On the
// automatic route, a cache that fails a read sends a to the server
// instead, as if the router had chosen it.
func (m *messageService) searchAccount(ctx context.Context, a config.ResolvedAccount, p *searchPlan, countOnly bool) accountSearch {
	// Sources are picked afresh on every page, never taken from the cursor,
	// which resumeAt and narrow then apply to them, and so is whether the
	// cache or the server answers them.
	src, route := m.searcher(ctx, a, p, p.source)
	o := runSearch(ctx, p, a.Name, countOnly, src, route)
	if m.cacheFailed(ctx, a, p, o.route, o.err) {
		src, route = m.serverFallback(a, p)
		o = runSearch(ctx, p, a.Name, countOnly, src, route)
	}
	return o
}

// runSearch runs account's Search on src, with the account's item: thread
// keys.
func runSearch(ctx context.Context, p *searchPlan, account string, countOnly bool, src searchSource, route routeInfo) accountSearch {
	res, err := src.Search(ctx, mailstore.SearchRequest{Query: p.tree, In: p.in, CountOnly: countOnly, ItemThreads: p.itemThreads[account]})
	return accountSearch{src: src, route: route, results: res, err: err}
}

// serverFallback is where a's page runs after its cache failed a read.
func (m *messageService) serverFallback(a config.ResolvedAccount, p *searchPlan) (searchSource, routeInfo) {
	return m.serverRoute(a, p, missFailed)
}

// cacheFailed reports whether err is a cache read failure on a's automatic
// route to the cache, which the server then answers. It logs a fixed code
// and tells the daemon, which turns the cache off on a full disk. A forced
// cache keeps its error, and an ended call is no failure of the cache.
func (m *messageService) cacheFailed(ctx context.Context, a config.ResolvedAccount, p *searchPlan, route routeInfo, err error) bool {
	if p.source != sourceAuto || route.Source != cache.SourceCache || !errors.Is(err, cache.ErrRead) || ctx.Err() != nil {
		return false
	}
	m.s.noteCacheErr(err)
	m.s.opts.Logger.Warn("the cache failed a search read; the server answers the account", "account", a.Name, "reason", cacheFaultCode(err))
	return true
}

// collect turns o's Search into what it adds to the page: its states,
// count, failures and carried cursor sources. A changed UIDVALIDITY under
// a cursor fails the whole call.
func (p *searchPlan) collect(account string, o *accountSearch) (accountPart, error) {
	var part accountPart
	switch {
	case o.err != nil:
		// The whole account failed: its cursor sources carry over as they were.
		part.partial = append(part.partial, partialSource(account, "", o.err))
		part.failures = append(part.failures, o.err)
		part.carried = append(part.carried, p.cursorSources(account)...)
	case p.continuing() && !o.route.Complete:
		part.carried = append(part.carried, p.uncovered(account, o.results)...)
	}
	for _, r := range o.results {
		rp, ok := p.resumeAt(account, r.Source.Folder)
		if !ok {
			continue
		}
		if r.Err == nil && p.continuing() {
			var err error
			if r, err = rp.narrow(r); err != nil {
				return part, err
			}
		}
		if r.Err != nil {
			part.partial = append(part.partial, partialSource(account, r.Source.Folder, r.Err))
			part.failures = append(part.failures, r.Err)
			if p.continuing() {
				part.carried = append(part.carried, rp.cursorSource(account, r.Source.Folder))
			}
			continue
		}
		part.count += uint64(r.Count)
		part.states = append(part.states, &sourceState{account: account, from: o.src, src: r.Source, uids: r.UIDs, prevBelow: rp.bound.Below})
	}
	return part, nil
}

// checkOutcome refuses a cursor naming a folder o's complete answer lacks.
func (p *searchPlan) checkOutcome(account string, o *accountSearch) error {
	// An incomplete cache answer may leave out folders the cache does not
	// hold; collect carries their cursor sources instead.
	if p.continuing() && o.err == nil && o.route.Complete {
		return p.checkSources(account, o.results)
	}
	return nil
}

func (m *messageService) search(ctx context.Context, req *chainmailv1.SearchRequest) (*chainmailv1.SearchResponse, audit.Entry, error) {
	entry := audit.Entry{Action: "search"}
	p, err := m.plan(req)
	if err != nil {
		return nil, entry, err
	}
	entry.Account = singleAccount(p.named)
	resp := &chainmailv1.SearchResponse{Source: sourceServer, FetchedAt: timestamppb.New(m.s.opts.Now())}

	outcomes := make([]accountSearch, len(p.accounts))
	var wg sync.WaitGroup
	for i, a := range p.accounts {
		wg.Go(func() { outcomes[i] = m.searchAccount(ctx, a, p, req.GetCountOnly()) })
	}
	wg.Wait()
	for i, a := range p.accounts {
		if err := p.checkOutcome(a.Name, &outcomes[i]); err != nil {
			return nil, entry, err
		}
	}
	for i, a := range p.accounts {
		part, err := p.collect(a.Name, &outcomes[i])
		if err != nil {
			return nil, entry, err
		}
		outcomes[i].part = part
	}
	if !req.GetCountOnly() {
		var all []*sourceState
		for i := range outcomes {
			all = append(all, outcomes[i].part.states...)
		}
		m.fetchRows(ctx, p, all, req.GetSnippets())
		if err := m.refetchFailedCaches(ctx, p, outcomes, req.GetSnippets()); err != nil {
			return nil, entry, err
		}
	}

	var failures []error
	var states []*sourceState
	// carried are cursor sources a failure, or an incomplete cache answer,
	// kept this page from reaching. They go into the next cursor as they
	// were, so a later page retries them, and that page's plan and
	// checkSources check them again.
	var carried []ref.CursorSource
	for _, o := range outcomes {
		resp.Partial = append(resp.Partial, o.part.partial...)
		failures = append(failures, o.part.failures...)
		states = append(states, o.part.states...)
		carried = append(carried, o.part.carried...)
	}
	if len(states) == 0 && len(failures) > 0 {
		return nil, entry, mostSevere(failures)
	}
	var answered []string
	for i, a := range p.accounts {
		o := outcomes[i]
		if o.err != nil {
			continue
		}
		answered = append(answered, o.route.Source)
		// An incomplete account is listed even with nothing counted, so its
		// reason reaches the caller.
		if len(o.part.states) == 0 && o.route.complete() {
			continue
		}
		resp.AccountCounts = append(resp.AccountCounts, accountCount(a.Name, o.part.count, o.route))
		resp.Count += o.part.count
	}
	resp.Source = cache.CombineSource(answered...)
	if req.GetCountOnly() {
		entry.Targets = []string{tally("accounts", len(p.named)), tally("matches", int(resp.GetCount())), "source=" + resp.GetSource()}
		return resp, entry, nil
	}

	ok := 0
	for _, st := range states {
		if st.err != nil {
			resp.Partial = append(resp.Partial, partialSource(st.account, st.src.Folder, st.err))
			failures = append(failures, st.err)
			continue
		}
		ok++
	}
	if ok == 0 && len(failures) > 0 {
		return nil, entry, mostSevere(failures)
	}
	for _, r := range merge(states, p.limit, req.GetThreads()) {
		count := 0
		if req.GetThreads() {
			count = r.count
		}
		resp.Rows = append(resp.Rows, rowFromSummary(r.sum, req.GetSnippets(), count))
	}
	for _, st := range states {
		if part := st.unreadablePartial(); part != nil {
			resp.Partial = append(resp.Partial, part)
		}
	}
	resp.NextCursor = nextCursor(p, states, carried)
	entry.Targets = []string{tally("accounts", len(p.named)), tally("rows", len(resp.GetRows())), "source=" + resp.GetSource()}
	return resp, entry, nil
}

// refetchFailedCaches searches again, on the server, each account whose
// automatic cache route failed reading summaries, as if the router had
// chosen the server: its Search, cursor checks and rows replace the
// cache's in outcomes.
func (m *messageService) refetchFailedCaches(ctx context.Context, p *searchPlan, outcomes []accountSearch, snippets bool) error {
	var redo []int
	for i, a := range p.accounts {
		o := &outcomes[i]
		for _, st := range o.part.states {
			if m.cacheFailed(ctx, a, p, o.route, st.err) {
				redo = append(redo, i)
				break
			}
		}
	}
	if len(redo) == 0 {
		return nil
	}
	var wg sync.WaitGroup
	for _, i := range redo {
		a := p.accounts[i]
		wg.Go(func() {
			src, route := m.serverFallback(a, p)
			outcomes[i] = runSearch(ctx, p, a.Name, false, src, route)
		})
	}
	wg.Wait()
	var all []*sourceState
	for _, i := range redo {
		a := p.accounts[i]
		if err := p.checkOutcome(a.Name, &outcomes[i]); err != nil {
			return err
		}
		part, err := p.collect(a.Name, &outcomes[i])
		if err != nil {
			return err
		}
		outcomes[i].part = part
		all = append(all, outcomes[i].part.states...)
	}
	m.fetchRows(ctx, p, all, snippets)
	return nil
}

// fetchRows fetches summaries for up to limit UIDs of each source, in
// parallel, from where its account's page runs.
func (m *messageService) fetchRows(ctx context.Context, p *searchPlan, states []*sourceState, snippets bool) {
	var wg sync.WaitGroup
	for _, st := range states {
		take := st.uids[:min(len(st.uids), p.limit)]
		st.requested = len(take)
		if len(take) == 0 {
			continue
		}
		wg.Go(func() {
			st.rows, st.err = st.from.FetchSummaries(ctx, st.src, take, mailstore.SummaryOptions{Snippets: snippets})
			// The summaries read are valid: the page goes on without the
			// unreadable messages, and unreadablePartial reports them.
			if ue, ok := errors.AsType[*mailstore.UnreadableError](st.err); ok {
				st.unreadable, st.err = ue.UIDs, nil
			}
		})
	}
	wg.Wait()
}

// merge takes rows newest first by arrival date, looking only at each
// source's highest remaining UID, so what it takes from a source is always
// that source's top UIDs and a cursor can resume below the lowest one. With
// threads, rows collapse into their thread's newest row until limit threads.
func merge(states []*sourceState, limit int, threads bool) []mergedRow {
	var out []mergedRow
	index := map[string]int{}
	for {
		st := newest(states)
		if st == nil {
			return out
		}
		sum := st.rows[st.next]
		if threads && sum.ThreadID != "" {
			if i, seen := index[sum.ThreadID]; seen {
				out[i].count++
				st.consume()
				continue
			}
		}
		if len(out) == limit {
			return out
		}
		if threads && sum.ThreadID != "" {
			index[sum.ThreadID] = len(out)
		}
		out = append(out, mergedRow{sum: sum, count: 1})
		st.consume()
	}
}

// newest returns the source whose next row is the newest, or nil when the
// merge must stop. It stops when a source has used every row it fetched
// but has matches it did not fetch: those may be newer than any other
// source's next row, so the page ends short and the cursor resumes there.
// That happens when threads collapse a source's rows, or when messages
// were expunged between SEARCH and FETCH.
func newest(states []*sourceState) *sourceState {
	var best *sourceState
	for _, st := range states {
		if st.err != nil {
			continue
		}
		if st.next >= len(st.rows) {
			if len(st.uids) > st.requested {
				return nil
			}
			continue
		}
		if best == nil || later(st.rows[st.next], best.rows[best.next]) {
			best = st
		}
	}
	return best
}

// later orders rows by arrival date, then by account, folder and UID so the
// order is the same on every call.
func later(a, b mailstore.Summary) bool {
	if !a.InternalDate.Equal(b.InternalDate) {
		return a.InternalDate.After(b.InternalDate)
	}
	if a.Ref.Account != b.Ref.Account {
		return a.Ref.Account < b.Ref.Account
	}
	if a.Ref.Folder != b.Ref.Folder {
		return a.Ref.Folder < b.Ref.Folder
	}
	return a.Ref.UID > b.Ref.UID
}

// nextCursor records, per source, where the next page starts, followed by
// the carried sources a failure kept this page from reaching. It returns ""
// once every source is exhausted. A source whose fetch failed continues
// where its page started, so a later page retries it.
func nextCursor(p *searchPlan, states []*sourceState, carried []ref.CursorSource) string {
	c := ref.Cursor{Query: p.fingerprint}
	more := false
	for _, st := range states {
		cs := st.cursorSource()
		if !cs.Done {
			more = true
		}
		c.Sources = append(c.Sources, cs)
	}
	for _, cs := range carried {
		more = more || !cs.Done
		c.Sources = append(c.Sources, cs)
	}
	if !more {
		return ""
	}
	return ref.EncodeCursor(c)
}

// cursorSource is where the source's next page starts, once merge has
// used its rows.
func (st *sourceState) cursorSource() ref.CursorSource {
	cs := ref.CursorSource{Account: st.account, Folder: st.src.Folder, UIDValidity: st.src.UIDValidity, Below: st.prevBelow}
	switch {
	case st.err != nil:
		// Retry from where this page started. This case comes first: a
		// failed fetch also has next == len(rows), with no rows at all.
	case st.requested > 0 && st.next == len(st.rows):
		// Every UID fetched was used, has gone since SEARCH, or could not
		// be read.
		cs.Below = st.uids[st.requested-1]
	case st.next > 0:
		cs.Below = st.lowest
	}
	if cs.Below == 0 && len(st.uids) > 0 && st.uids[0] < math.MaxUint32 {
		// No page has reached this source yet. Bound it at what matched
		// now, so mail arriving later stays out of later pages.
		cs.Below = st.uids[0] + 1
	}
	cs.Done = st.err == nil && st.next == len(st.rows) && len(st.uids) <= st.requested
	return cs
}

// unreadablePartial reports the source's unreadable matches that this page
// passed, nil when there are none. A match below where the next page
// starts is fetched again there, and reported by the page that passes it,
// so each is reported once.
func (st *sourceState) unreadablePartial() *chainmailv1.PartialSource {
	if len(st.unreadable) == 0 {
		return nil
	}
	cs := st.cursorSource()
	n := 0
	for _, uid := range st.unreadable {
		if cs.Done || uid >= cs.Below {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	part := partialSource(st.account, st.src.Folder, &mailstore.UnreadableError{Folder: st.src.Folder, UIDs: st.unreadable})
	part.Message = fmt.Sprintf("left out %d of the folder's matches: %s", n, part.GetMessage())
	part.Unreadable = uint64(n)
	return part
}
