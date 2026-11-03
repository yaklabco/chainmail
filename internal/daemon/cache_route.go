package daemon

import (
	"context"
	"fmt"
	"time"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
)

// sourceAuto is a search's default source: the cache for an account it can
// answer completely and is fresh for, the server for the rest.
const sourceAuto = "auto"

// Incomplete reasons the router reports itself. Account.Freshness gives
// stale, backfilling and folder_not_synced, and Compile gives
// text_outside_window and folder_not_synced for queries it cannot answer.
const (
	reasonCacheDisabled     = "cache_disabled"
	reasonTextOutsideWindow = "text_outside_window"
	reasonStale             = "stale"
	reasonFolderNotSynced   = "folder_not_synced"
)

// searchSource is where one account's search page runs: the account's mail
// server store or its cache view. Both have mailstore.Store semantics, so
// paging, narrowing, merging, cursors and threads run unchanged on either.
type searchSource interface {
	Search(ctx context.Context, req mailstore.SearchRequest) ([]mailstore.SourceResult, error)
	FetchSummaries(ctx context.Context, src mailstore.Source, uids []uint32, opts mailstore.SummaryOptions) ([]mailstore.Summary, error)
}

var (
	_ searchSource = mailstore.Store(nil)
	_ searchSource = (*cache.View)(nil)
)

// routeInfo says what answered one account's page, for its AccountCount.
type routeInfo struct {
	// Source is cache.SourceCache or cache.SourceServer.
	Source string
	// SyncedAt is the oldest last sync of the folders searched, when the
	// cache answered and knew it.
	SyncedAt time.Time
	// Complete is false only when a forced cache could not answer completely.
	Complete bool
	// Reason says why, when Complete is false.
	Reason string
}

// noSource answers nothing: a forced cache search of an account whose cache
// is disabled returns no rows and no error.
type noSource struct{}

func (noSource) Search(context.Context, mailstore.SearchRequest) ([]mailstore.SourceResult, error) {
	return nil, nil
}

func (noSource) FetchSummaries(context.Context, mailstore.Source, []uint32, mailstore.SummaryOptions) ([]mailstore.Summary, error) {
	return nil, nil
}

// refusal refuses the search with err: has:, filename: and label: on an
// account other than Google, which only its cache answers, when the cache
// cannot.
type refusal struct{ err error }

func (r refusal) Search(context.Context, mailstore.SearchRequest) ([]mailstore.SourceResult, error) {
	return nil, r.err
}

func (r refusal) FetchSummaries(context.Context, mailstore.Source, []uint32, mailstore.SummaryOptions) ([]mailstore.Summary, error) {
	return nil, r.err
}

// requestSource validates a SearchRequest's source and returns it, with ""
// read as auto.
func requestSource(s string) (string, error) {
	switch s {
	case "", sourceAuto:
		return sourceAuto, nil
	case cache.SourceCache, cache.SourceServer:
		return s, nil
	}
	return "", &argError{
		msg:  "source must be auto, cache or server",
		hint: "leave source empty or pass auto for the cache when it can answer completely and is fresh, else the server; pass cache or server to force one",
	}
}

// searcher picks where acct's page of p runs, per spec 6.1 and 6.4, with
// source one of sourceAuto, cache.SourceCache or cache.SourceServer. An
// account has no cache when it has no cache worker or while the cache is
// down (Server.cacheAccount).
func (m *messageService) searcher(ctx context.Context, acct config.ResolvedAccount, p *searchPlan, source string) (searchSource, routeInfo) {
	if source == cache.SourceServer {
		return m.serverRoute(acct, p)
	}
	forced := source == cache.SourceCache
	ca := m.s.cacheAccount(acct.Name)
	if ca == nil {
		if forced {
			return noSource{}, routeInfo{Source: cache.SourceCache, Reason: reasonCacheDisabled}
		}
		return m.serverRoute(acct, p)
	}
	server := m.store(acct)
	ok, reason, syncedAt := m.cacheCanAnswer(ctx, ca, server, acct, p)
	switch {
	case ok:
		return ca.View(server), routeInfo{Source: cache.SourceCache, SyncedAt: syncedAt, Complete: true}
	case forced && reason != "":
		return ca.View(server), routeInfo{Source: cache.SourceCache, SyncedAt: syncedAt, Reason: reason}
	default:
		return m.serverRoute(acct, p)
	}
}

// serverRoute sends acct's page to its mail server. On an account other
// than Google, has:, filename: and label: are refused here instead: the
// server cannot search them, and the refusal's hint points at the cache,
// which answers them once it is complete.
func (m *messageService) serverRoute(acct config.ResolvedAccount, p *searchPlan) (searchSource, routeInfo) {
	route := routeInfo{Source: cache.SourceServer, Complete: true}
	if !acct.Provider.Gmail {
		if f, ok := cacheOnlyField(p.tree); ok {
			return refusal{errNeedsCache(f)}, route
		}
	}
	return m.store(acct), route
}

// cacheCanAnswer runs spec 6.1's checks for acct: the cache can evaluate
// the query, every folder in the search's scope is fresh, and the cache
// holds what the query needs. It returns the first failing reason and the
// oldest last sync of the folders. reason is "" with ok false only when
// the query does not compile for the cache, or holds a term the cache
// cannot evaluate (a ServerOnly plan with no reason). Both are checked
// before scope and freshness: the view could not run such a query at any
// freshness, so the server answers whatever source asked for, and its own
// answer or refusal reaches the caller (integration note 15).
func (m *messageService) cacheCanAnswer(ctx context.Context, ca *cache.Account, server mailstore.Store, acct config.ResolvedAccount, p *searchPlan) (bool, string, time.Time) {
	log := m.s.opts.Logger
	plan, err := cache.Compile(p.tree, p.in, acct.Provider.Gmail, m.s.cacheClock.Now())
	if err != nil {
		log.Warn("a search did not compile for the cache; the server answers it", "account", acct.Name, "err", err)
		return false, "", time.Time{}
	}
	if plan.ServerOnly && plan.Reason == "" {
		return false, "", time.Time{}
	}
	scoper, ok := server.(mailstore.Scoper)
	if !ok {
		log.Debug("the account's mail store cannot list a search's folders; the cache cannot answer it", "account", acct.Name)
		return false, reasonFolderNotSynced, time.Time{}
	}
	folders, err := scoper.SearchFolders(ctx, p.in)
	if err != nil {
		log.Warn("listing a search's folders for the cache failed", "account", acct.Name, "reason", apiReason(err))
		return false, reasonFolderNotSynced, time.Time{}
	}
	fresh, reason, syncedAt, err := ca.Freshness(ctx, folders)
	switch {
	case err != nil:
		log.Warn("reading the cache's freshness for a search failed", "account", acct.Name, "err", err)
		return false, reasonStale, time.Time{}
	case !fresh && reason == "":
		return false, reasonStale, syncedAt
	case !fresh:
		return false, reason, syncedAt
	case plan.ServerOnly:
		return false, plan.Reason, syncedAt
	case plan.NeedsBodies:
		covers, err := ca.WindowCovers(ctx, folders, plan.Since)
		if err != nil {
			log.Warn("reading the cache's body window for a search failed", "account", acct.Name, "err", err)
		}
		if err != nil || !covers {
			return false, reasonTextOutsideWindow, syncedAt
		}
	}
	return true, "", syncedAt
}

// cacheOnlyField returns the first has:, filename: or label: term of n.
func cacheOnlyField(n *query.Node) (query.Field, bool) {
	for _, t := range n.Terms() {
		switch t.Field {
		case query.FieldHas, query.FieldFilename, query.FieldLabel:
			return t.Field, true
		}
	}
	return "", false
}

// errNeedsCache refuses field on an account other than Google whose cache
// cannot answer the search.
func errNeedsCache(field query.Field) error {
	return &argError{
		msg:  fmt.Sprintf("%s: is answered from the local cache on this account, and the cache cannot answer yet", field),
		hint: "the cache answers it once its backfill is done and it is fresh; see progress with: chainmail cache status",
	}
}

// accountCount is the API form of one account's count and route.
func accountCount(account string, n uint64, route routeInfo) *chainmailv1.AccountCount {
	return &chainmailv1.AccountCount{
		Account: account, Count: n, Source: route.Source, SyncedAt: timestamp(route.SyncedAt),
		Complete: route.Complete, IncompleteReason: route.Reason,
	}
}
