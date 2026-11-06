package cache

import "github.com/yaklabco/chainmail/internal/mailstore"

// View is one account's cache seen through the read methods of
// mailstore.Store. The daemon picks the view or the server store per
// account and call, so search paging, merging, cursors and rendering run
// unchanged on either. server answers what the cache cannot: misses, and
// snippets of rows without a cached body.
type View struct {
	a      *Account
	server mailstore.Store
}

// View returns the account's cache read view over server, the account's
// server store, used for misses and snippets.
func (a *Account) View(server mailstore.Store) *View {
	return &View{a: a, server: server}
}
