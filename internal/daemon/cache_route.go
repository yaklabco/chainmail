package daemon

import (
	"cmp"
	"context"
	"errors"
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

// reasonCacheDisabled is the incomplete reason of a forced cache search of
// an account without a cache. The others are the cache package's Reason
// constants: Account.Freshness gives stale, backfilling and
// folder_not_synced, Compile text_outside_window and folder_not_synced,
// and Account.Unreadable unreadable_messages.
const reasonCacheDisabled = "cache_disabled"

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
	// The cursor checks rely on it: an incomplete answer may leave out
	// folders.
	Complete bool
	// Reason says why, when Complete is false.
	Reason string
	// Unreadable is set when the cache answered over folders holding
	// messages it recorded as unreadable. The answer covers every folder,
	// so it stays Complete for the cursor, but its AccountCount reports
	// complete false with cache.ReasonUnreadableMessages.
	Unreadable bool
}

// complete reports whether the account's answer is complete, as its
// AccountCount says.
func (r routeInfo) complete() bool { return r.Complete && !r.Unreadable }

// reason is the AccountCount's incomplete_reason: Reason, or
// unreadable_messages for a complete answer that misses messages. A
// complete answer that misses none has an empty Reason, so it gives "".
func (r routeInfo) reason() string {
	if r.Complete && r.Unreadable {
		return cache.ReasonUnreadableMessages
	}
	return r.Reason
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

// refusal refuses the search with err: item: on any account, and has:,
// filename: and label: on an account other than Google, which only the
// cache answers, when the cache cannot.
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
		return m.serverRoute(acct, p, missForced)
	}
	forced := source == cache.SourceCache
	ca := m.s.cacheAccount(acct.Name)
	if ca == nil {
		if forced && !m.unsupported(acct, p) {
			return noSource{}, routeInfo{Source: cache.SourceCache, Reason: reasonCacheDisabled}
		}
		return m.serverRoute(acct, p, missOff)
	}
	server := m.store(acct)
	v := m.cacheCanAnswer(ctx, ca, server, acct, p)
	switch {
	case v.ok:
		return ca.View(server), routeInfo{Source: cache.SourceCache, SyncedAt: v.syncedAt, Complete: true, Unreadable: v.unreadable}
	case forced && v.reason != "":
		return ca.View(server), routeInfo{Source: cache.SourceCache, SyncedAt: v.syncedAt, Reason: v.reason}
	default:
		return m.serverRoute(acct, p, v.miss)
	}
}

// cacheMiss is why an account's page went to the server, so the refusal
// of has:, filename: or label: on an account other than Google names the
// real cause. Only missWait and missPrefetch pass by waiting.
type cacheMiss int

const (
	// missWait: the cache is backfilling, stale, or has not synced a
	// folder of the search yet.
	missWait cacheMiss = iota
	// missPrefetch: the cache is still downloading the bodies inside its
	// window that the query's text needs.
	missPrefetch
	// missBound: the query has text with no top-level lower date bound
	// inside the prefetch window, which the cache holds bodies for.
	missBound
	// missUnsupported: the query holds a value the cache never evaluates.
	missUnsupported
	// missOff: the account has no cache worker, or the cache is down.
	missOff
	// missForced: the caller passed source server.
	missForced
	// missFailed: the cache failed a read after routing to it.
	missFailed
)

// serverRoute sends acct's page to its mail server. A query with item: is
// refused here on every account, and on an account other than Google so
// are has:, filename: and label:, with the cause miss, or the query's own
// cause when the cache refuses or cannot evaluate it: the server cannot
// search them.
func (m *messageService) serverRoute(acct config.ResolvedAccount, p *searchPlan, miss cacheMiss) (searchSource, routeInfo) {
	route := routeInfo{Source: cache.SourceServer, Complete: true}
	if hasItemTerm(p.tree) {
		return refusal{m.itemRefusal(acct, p, miss)}, route
	}
	if !acct.Provider.Gmail {
		if f, ok := cacheOnlyField(p.tree); ok {
			plan, err := cache.Compile(p.tree, p.in, false, m.s.cacheClock.Now(), p.itemThreads[acct.Name])
			switch {
			case err != nil:
				// The cache refuses the query itself, as the server path would.
				return refusal{err}, route
			case plan.Unsupported:
				miss = missUnsupported
			}
			return refusal{errNeedsCache(f, miss)}, route
		}
	}
	return m.store(acct), route
}

// unsupported reports whether p holds a term the cache cannot evaluate on
// acct, which goes to the server whatever source asked for (integration
// note 15), even without a cache. A query that does not compile for the
// cache is the server's to refuse too.
func (m *messageService) unsupported(acct config.ResolvedAccount, p *searchPlan) bool {
	plan, err := cache.Compile(p.tree, p.in, acct.Provider.Gmail, m.s.cacheClock.Now(), p.itemThreads[acct.Name])
	return err != nil || plan.Unsupported
}

// cacheVerdict is what cacheCanAnswer decides for one account.
type cacheVerdict struct {
	// ok is set when the cache answers the page completely.
	ok bool
	// reason is the incomplete_reason a forced cache reports, "" when the
	// cache cannot run the query at all and the server answers it whatever
	// source asked for.
	reason string
	// syncedAt is the oldest last sync of the folders searched.
	syncedAt time.Time
	// unreadable is set, with ok, when a folder searched holds messages
	// the cache recorded as unreadable.
	unreadable bool
	// miss is why the cache cannot answer, when ok is false.
	miss cacheMiss
}

// cacheCanAnswer runs spec 6.1's checks for acct: the cache can evaluate
// the query, every folder in the search's scope is fresh, and the cache
// holds what the query needs. Its reason is the first failing one. A query
// that does not compile for the cache, or holds a term the cache cannot
// evaluate (Plan.Unsupported), is checked first and has no reason: the
// view could not run it at any freshness, so the server answers whatever
// source asked for, and its own answer or refusal reaches the caller
// (integration note 15). miss puts text the window can never cover ahead
// of a cache that is catching up, since waiting would not help it.
func (m *messageService) cacheCanAnswer(ctx context.Context, ca *cache.Account, server mailstore.Store, acct config.ResolvedAccount, p *searchPlan) cacheVerdict {
	log := m.s.opts.Logger
	plan, err := cache.Compile(p.tree, p.in, acct.Provider.Gmail, m.s.cacheClock.Now(), p.itemThreads[acct.Name])
	if err != nil {
		// A user error the server path refuses alike; its text quotes the query.
		log.Debug("a search did not compile for the cache; the server answers it", "account", acct.Name, "reason", apiReason(err))
		return cacheVerdict{miss: missUnsupported}
	}
	if plan.Unsupported {
		return cacheVerdict{miss: missUnsupported}
	}
	needsBound := plan.Reason == cache.ReasonTextOutsideWindow || (plan.NeedsBodies && !ca.InWindow(plan.Since))
	miss := func(transient cacheMiss) cacheMiss {
		if needsBound {
			return missBound
		}
		return transient
	}
	scoper, ok := server.(mailstore.Scoper)
	if !ok {
		log.Debug("the account's mail store cannot list a search's folders; the cache cannot answer it", "account", acct.Name)
		return cacheVerdict{reason: cache.ReasonFolderNotSynced, miss: miss(missWait)}
	}
	folders, err := scoper.SearchFolders(ctx, p.in)
	if err != nil {
		log.Warn("listing a search's folders for the cache failed", "account", acct.Name, "reason", apiReason(err))
		return cacheVerdict{reason: cache.ReasonFolderNotSynced, miss: miss(missWait)}
	}
	fresh, reason, syncedAt, err := ca.Freshness(ctx, folders)
	switch {
	case err != nil:
		log.Warn("reading the cache's freshness for a search failed", "account", acct.Name, "reason", cacheFaultCode(err))
		m.s.noteCacheErr(err)
		return cacheVerdict{reason: cache.ReasonStale, miss: miss(missWait)}
	case !fresh:
		return cacheVerdict{reason: cmp.Or(reason, cache.ReasonStale), syncedAt: syncedAt, miss: miss(missWait)}
	case plan.ServerOnly:
		return cacheVerdict{reason: plan.Reason, syncedAt: syncedAt, miss: miss(missWait)}
	case needsBound:
		return cacheVerdict{reason: cache.ReasonTextOutsideWindow, syncedAt: syncedAt, miss: missBound}
	case plan.NeedsBodies:
		covers, err := ca.WindowCovers(ctx, folders, plan.Since)
		if err != nil {
			log.Warn("reading the cache's body window for a search failed", "account", acct.Name, "reason", cacheFaultCode(err))
			m.s.noteCacheErr(err)
		}
		if err != nil || !covers {
			return cacheVerdict{reason: cache.ReasonTextOutsideWindow, syncedAt: syncedAt, miss: missPrefetch}
		}
	}
	// Unreadable messages do not send the search to the server: it could
	// list them, but reading them fails the same way.
	unreadable, err := ca.Unreadable(ctx, folders)
	if err != nil {
		log.Warn("reading the cache's unreadable messages for a search failed", "account", acct.Name, "reason", cacheFaultCode(err))
		m.s.noteCacheErr(err)
		return cacheVerdict{reason: cache.ReasonStale, syncedAt: syncedAt, miss: miss(missWait)}
	}
	return cacheVerdict{ok: true, syncedAt: syncedAt, unreadable: unreadable}
}

// cacheFaultCode is the fixed code a log gives a cache failure, never its
// text: disk_full or corrupt when SQLite said so, else the API reason.
func cacheFaultCode(err error) string {
	switch {
	case errors.Is(err, cache.ErrDiskFull):
		return "disk_full"
	case errors.Is(err, cache.ErrCorrupt):
		return "corrupt"
	default:
		return apiReason(err)
	}
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
// cannot answer the search, saying why. Every hint names cache status
// (integration note 16); only a cache catching up says to wait.
func errNeedsCache(field query.Field, miss cacheMiss) error {
	lead := fmt.Sprintf("%s: is answered from the local cache on this account", field)
	switch miss {
	case missUnsupported:
		return &argError{
			msg:  "the search holds a value the local cache cannot evaluate on this account, and its mail server cannot search has:, filename: or label:",
			hint: `label: takes a keyword here, not a system flag such as \Seen; use is:read, is:unread or is:starred for those. Waiting does not change this; the cache's state is in: chainmail cache status`,
		}
	case missBound:
		return &argError{
			msg:  lead + ", and the cache holds message text only for mail inside its body window",
			hint: cacheBoundHint,
		}
	case missPrefetch:
		return &argError{
			msg:  lead + ", and the cache cannot answer yet",
			hint: "the cache answers it once it has downloaded the bodies inside its window; see progress with: chainmail cache status",
		}
	case missOff:
		return &argError{
			msg:  lead + ", and the account's cache is not running",
			hint: "the cache needs [cache] enabled and sync on the account, and stays off while the disk holding it is full; see: chainmail cache status",
		}
	case missForced:
		return &argError{
			msg:  lead + ", and source server leaves the cache out",
			hint: "leave source empty or pass auto or cache; see the cache with: chainmail cache status",
		}
	case missFailed:
		return &argError{
			msg:  lead + ", and the cache failed reading it",
			hint: cacheFailedHint,
		}
	default:
		return &argError{
			msg:  lead + ", and the cache cannot answer yet",
			hint: "the cache answers it once its backfill is done and it is fresh; see progress with: chainmail cache status",
		}
	}
}

// accountCount is the API form of one account's count and route.
func accountCount(account string, n uint64, route routeInfo) *chainmailv1.AccountCount {
	return &chainmailv1.AccountCount{
		Account: account, Count: n, Source: route.Source, SyncedAt: timestamp(route.SyncedAt),
		Complete: route.complete(), IncompleteReason: route.reason(),
	}
}
