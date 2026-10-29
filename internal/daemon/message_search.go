package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/audit"
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
	if err := m.s.record(ctx, c, entry, searchErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// searchPlan is a validated search: what to look for, where, and from where on.
type searchPlan struct {
	tree     *query.Node
	in       string
	accounts []config.ResolvedAccount
	limit    int
	// resume, when continuing from a cursor, maps account, then folder, to
	// where its next page starts. It holds the cursor's every source, done
	// ones included, so each can be checked against the sources a fresh
	// search picks.
	resume      map[string]map[string]resumePoint
	fingerprint string
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
	accts, err := m.accounts(append(slices.Clone(req.GetAccounts()), scope.Accounts...))
	if err != nil {
		return nil, err
	}
	names := make([]string, len(accts))
	for i, a := range accts {
		names[i] = a.Name
	}
	p := &searchPlan{
		tree: rest, in: scope.In, accounts: accts, limit: limit,
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
// search, with the same query, does not pick.
func (p *searchPlan) checkSources(account string, results []mailstore.SourceResult) error {
	for folder := range p.resume[account] {
		if !slices.ContainsFunc(results, func(r mailstore.SourceResult) bool { return r.Source.Folder == folder }) {
			return errForgedCursor("it names a folder this search does not cover")
		}
	}
	return nil
}

// resumeAt returns where a folder continues. ok is false when the plan
// resumes from a cursor that has nothing left there, or never had the
// folder, as when it failed on an earlier page. Without a cursor every
// folder starts from the top.
func (p *searchPlan) resumeAt(account, folder string) (resumePoint, bool) {
	if !p.continuing() {
		return resumePoint{}, true
	}
	rp, ok := p.resume[account][folder]
	return rp, ok && !rp.done
}

// narrow keeps the matches of a fresh source result below where the cursor
// left off. A changed UIDVALIDITY is ErrStaleRef.
func (rp resumePoint) narrow(r mailstore.SourceResult) (mailstore.SourceResult, error) {
	if r.Source.UIDValidity != rp.bound.UIDValidity {
		return r, fmt.Errorf("%w: %s changed since the first page; search again", mailstore.ErrStaleRef, r.Source.Folder)
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
	account   string
	src       mailstore.Source
	uids      []uint32 // every match below the bound, highest first
	requested int      // how many UIDs summaries were asked for
	rows      []mailstore.Summary
	next      int    // rows[:next] were used
	lowest    uint32 // the lowest UID used
	prevBelow uint32 // the bound this page started from
	err       error
}

func (st *sourceState) consume() {
	st.lowest = st.rows[st.next].Ref.UID
	st.next++
}

type mergedRow struct {
	sum   mailstore.Summary
	count int
}

func (m *messageService) search(ctx context.Context, req *chainmailv1.SearchRequest) (*chainmailv1.SearchResponse, audit.Entry, error) {
	entry := audit.Entry{Action: "search"}
	p, err := m.plan(req)
	if err != nil {
		return nil, entry, err
	}
	entry.Account = singleAccount(p.accounts)
	resp := &chainmailv1.SearchResponse{Source: sourceServer, FetchedAt: timestamppb.New(m.s.opts.Now())}

	type outcome struct {
		results []mailstore.SourceResult
		err     error
	}
	outcomes := make([]outcome, len(p.accounts))
	var wg sync.WaitGroup
	for i, a := range p.accounts {
		wg.Go(func() {
			// Sources are picked afresh on every page, never taken from the
			// cursor, which resumeAt and narrow then apply to them.
			res, err := m.store(a).Search(ctx, mailstore.SearchRequest{Query: p.tree, In: p.in, CountOnly: req.GetCountOnly()})
			outcomes[i] = outcome{res, err}
		})
	}
	wg.Wait()
	if p.continuing() {
		for i, a := range p.accounts {
			if outcomes[i].err != nil {
				continue
			}
			if err := p.checkSources(a.Name, outcomes[i].results); err != nil {
				return nil, entry, err
			}
		}
	}

	var failures []error
	var states []*sourceState
	counts := map[string]uint64{}
	for i, a := range p.accounts {
		o := outcomes[i]
		if o.err != nil {
			resp.Partial = append(resp.Partial, partialSource(a.Name, "", o.err))
			failures = append(failures, o.err)
			continue
		}
		for _, r := range o.results {
			rp, ok := p.resumeAt(a.Name, r.Source.Folder)
			if !ok {
				continue
			}
			if r.Err == nil && p.continuing() {
				if r, err = rp.narrow(r); err != nil {
					return nil, entry, err
				}
			}
			if r.Err != nil {
				resp.Partial = append(resp.Partial, partialSource(a.Name, r.Source.Folder, r.Err))
				failures = append(failures, r.Err)
				continue
			}
			counts[a.Name] += uint64(r.Count)
			states = append(states, &sourceState{account: a.Name, src: r.Source, uids: r.UIDs, prevBelow: rp.bound.Below})
		}
	}
	if len(states) == 0 && len(failures) > 0 {
		return nil, entry, mostSevere(failures)
	}
	for _, a := range p.accounts {
		if n, ok := counts[a.Name]; ok {
			resp.AccountCounts = append(resp.AccountCounts, &chainmailv1.AccountCount{Account: a.Name, Count: n})
			resp.Count += n
		}
	}
	if req.GetCountOnly() {
		entry.Targets = []string{tally("accounts", len(p.accounts)), tally("matches", int(resp.GetCount()))}
		return resp, entry, nil
	}

	m.fetchRows(ctx, p, states, req.GetSnippets())
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
	resp.NextCursor = nextCursor(p, states)
	entry.Targets = []string{tally("accounts", len(p.accounts)), tally("rows", len(resp.GetRows()))}
	return resp, entry, nil
}

// fetchRows fetches summaries for up to limit UIDs of each source, in parallel.
func (m *messageService) fetchRows(ctx context.Context, p *searchPlan, states []*sourceState, snippets bool) {
	byName := map[string]config.ResolvedAccount{}
	for _, a := range p.accounts {
		byName[a.Name] = a
	}
	var wg sync.WaitGroup
	for _, st := range states {
		take := st.uids[:min(len(st.uids), p.limit)]
		st.requested = len(take)
		if len(take) == 0 {
			continue
		}
		wg.Go(func() {
			st.rows, st.err = m.store(byName[st.account]).FetchSummaries(ctx, st.src, take, mailstore.SummaryOptions{Snippets: snippets})
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

func newest(states []*sourceState) *sourceState {
	var best *sourceState
	for _, st := range states {
		if st.err != nil || st.next >= len(st.rows) {
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

// nextCursor records, per source, where the next page starts. It returns ""
// once every source is exhausted. Sources that failed are left out.
func nextCursor(p *searchPlan, states []*sourceState) string {
	c := ref.Cursor{Query: p.fingerprint}
	more := false
	for _, st := range states {
		if st.err != nil {
			continue
		}
		cs := ref.CursorSource{Account: st.account, Folder: st.src.Folder, UIDValidity: st.src.UIDValidity, Below: st.prevBelow}
		if st.next > 0 {
			cs.Below = st.lowest
		}
		cs.Done = st.next == len(st.rows) && len(st.uids) <= st.requested
		if !cs.Done {
			more = true
		}
		c.Sources = append(c.Sources, cs)
	}
	if !more {
		return ""
	}
	return ref.EncodeCursor(c)
}
