package daemon

import (
	"context"
	"errors"
	"slices"

	"github.com/yaklabco/chainmail/internal/cache"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/items"
	"github.com/yaklabco/chainmail/internal/query"
)

// itemResolver turns item: values into thread keys, from the item store.
type itemResolver interface {
	// ThreadKeys returns, per account, the thread keys linked to item id or
	// any live item below it. An unknown or deleted id is items.ErrNotFound.
	ThreadKeys(ctx context.Context, id string) (map[string][]string, error)
	// LinkedThreadKeys returns account's thread keys linked to any live item.
	LinkedThreadKeys(ctx context.Context, account string) ([]string, error)
}

const (
	// cacheNotReadyHint is the hint of a CACHE_NOT_READY refusal that
	// waiting can clear.
	cacheNotReadyHint = "the cache has not caught up; run: chainmail cache status"
	// cacheBoundHint answers text the cache holds no bodies for, which
	// waiting does not change.
	cacheBoundHint = "add a top-level newer_than: or after: inside the account's prefetch_bodies window, or drop the text terms; waiting does not change this; see the window with: chainmail cache status"
	// cacheFailedHint answers a cache that failed a read.
	cacheFailedHint = "the daemon log has the cause; see the cache with: chainmail cache status"
)

// errNotReady marks a search only the cache answers, on an account whose
// cache cannot answer it completely yet. toConnectError maps it to
// CACHE_NOT_READY.
var errNotReady = errors.New("the cache cannot answer the search completely yet")

// notReadyCause is errNotReady with a hint of its own, for a cache that
// waiting does not bring to an answer.
type notReadyCause struct{ hint string }

func (c *notReadyCause) Error() string { return errNotReady.Error() }

func (c *notReadyCause) Unwrap() error { return errNotReady }

// notReadyHint is the hint of a CACHE_NOT_READY refusal: its cause's own,
// or cacheNotReadyHint.
func notReadyHint(err error) string {
	if c, ok := errors.AsType[*notReadyCause](err); ok {
		return c.hint
	}
	return cacheNotReadyHint
}

var (
	// errItemCacheNotReady refuses item: on an account whose cache cannot
	// answer the search completely yet: no cache, a cache catching up, or
	// folders it never holds.
	errItemCacheNotReady = &fixedTextError{
		msg: "item: is answered from the local cache, which cannot answer this account completely yet",
		err: errNotReady,
	}
	// errItemCacheBound refuses item: with text outside the cache's body
	// window, which waiting never brings in.
	errItemCacheBound = &fixedTextError{
		msg: "item: is answered from the local cache, which holds message text only for mail inside its body window",
		err: &notReadyCause{hint: cacheBoundHint},
	}
	// errItemCacheFailed refuses item: on an account whose cache failed a
	// read after the search was routed to it.
	errItemCacheFailed = &fixedTextError{
		msg: "item: is answered from the local cache, which failed reading this account",
		err: &notReadyCause{hint: cacheFailedHint},
	}
	// errItemSourceServer refuses item: with source server, for every
	// account at once.
	errItemSourceServer = &argError{
		msg:  "item: is answered from the local cache, and source server leaves the cache out",
		hint: "item: is answered from the cache; drop source=server",
	}
	// errItemsOff refuses item: on a daemon with no item store.
	errItemsOff = &argError{
		msg:  "item: needs the daemon's item store, which is not running",
		hint: "drop the item: term from the query",
	}
	// errItemUnsupported refuses item: in a query the cache can never
	// evaluate on the account, so waiting would not help.
	errItemUnsupported = &argError{
		msg:  "the search holds a value the local cache cannot evaluate on this account, and item: is answered from the cache only",
		hint: `label: takes a keyword here, not a system flag such as \Seen; use is:read, is:unread or is:starred for those; the cache's state is in: chainmail cache status`,
	}
)

// itemValues returns the distinct item: values of n, in the order they
// first appear.
func itemValues(n *query.Node) []string {
	var out []string
	for _, t := range n.Terms() {
		if t.Field == query.FieldItem && !slices.Contains(out, t.Value) {
			out = append(out, t.Value)
		}
	}
	return out
}

// hasItemTerm reports whether n holds an item: term anywhere.
func hasItemTerm(n *query.Node) bool {
	for _, t := range n.Terms() {
		if t.Field == query.FieldItem {
			return true
		}
	}
	return false
}

// itemTargets is a search's audit target for an item: term: item_op=1 when
// n holds one, never which item, and nil otherwise.
func itemTargets(n *query.Node) []string {
	if hasItemTerm(n) {
		return []string{"item_op=1"}
	}
	return nil
}

// resolveItems fills p.itemThreads for the item: terms of p's query, before
// any account is searched: per account p searches, each item: value maps to
// the account's thread keys. An unknown or deleted item fails the whole
// search, as do source server and a daemon with no item store. A query
// with no item: term is left alone.
func (m *messageService) resolveItems(ctx context.Context, p *searchPlan) error {
	values := itemValues(p.tree)
	if len(values) == 0 {
		return nil
	}
	if p.source == cache.SourceServer {
		return errItemSourceServer
	}
	r := m.s.itemThreads
	if r == nil {
		return errItemsOff
	}
	threads := make(map[string]map[string][]string, len(p.accounts))
	for _, a := range p.accounts {
		threads[a.Name] = make(map[string][]string, len(values))
	}
	for _, v := range values {
		if v == query.ItemNone {
			for _, a := range p.accounts {
				keys, err := r.LinkedThreadKeys(ctx, a.Name)
				if err != nil {
					return itemLookupErr(err)
				}
				threads[a.Name][v] = keys
			}
			continue
		}
		byAccount, err := r.ThreadKeys(ctx, v)
		if err != nil {
			return itemLookupErr(err)
		}
		for _, a := range p.accounts {
			threads[a.Name][v] = byAccount[a.Name]
		}
	}
	p.itemThreads = threads
	return nil
}

// itemLookupErr is the error for a failed item: lookup, with fixed text:
// the store's own error stays on the chain for the reason and the logs.
func itemLookupErr(err error) error {
	if errors.Is(err, items.ErrNotFound) {
		return &fixedTextError{msg: "an item: term names no live item", err: err}
	}
	return &fixedTextError{msg: "the item store failed", err: err}
}

// itemRefusal is why acct's page of an item: search cannot go to its mail
// server, which never answers item:, Google's included: X-GM-RAW would
// read the term as text. A query the cache refuses keeps the cache's
// error, and one it can never evaluate is an invalid argument. Text
// outside the body window and a failed cache read are CACHE_NOT_READY
// with their own text and hints, since waiting does not help them, and
// anything else is a cache that has not caught up.
func (m *messageService) itemRefusal(acct config.ResolvedAccount, p *searchPlan, miss cacheMiss) error {
	if miss == missForced {
		return errItemSourceServer
	}
	plan, err := cache.Compile(p.tree, p.in, acct.Provider.Gmail, m.s.cacheClock.Now(), p.itemThreads[acct.Name])
	switch {
	case err != nil:
		return err
	case plan.Unsupported:
		return errItemUnsupported
	case miss == missBound:
		return errItemCacheBound
	case miss == missFailed:
		return errItemCacheFailed
	default:
		return errItemCacheNotReady
	}
}
