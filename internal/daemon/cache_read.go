package daemon

import (
	"context"

	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// messageReader reads messages and threads for one account and says where
// each answer came from, cache.SourceCache or cache.SourceServer. An
// account with a cache reads through its view, which answers from the
// cache when it can; the rest read from the server. ThreadFrom also
// returns the seed it threaded: r, or the ref a stale r was recovered to.
type messageReader interface {
	FetchMessagesFrom(ctx context.Context, refs []ref.Ref) ([]mailstore.MessageResult, []string)
	ThreadFrom(ctx context.Context, r ref.Ref) (refs []ref.Ref, seed ref.Ref, source string, err error)
}

var (
	_ messageReader = (*cache.View)(nil)
	_ messageReader = serverReader{}
)

// reader returns acct's messageReader: its cache view over its store when
// the account has a cache that is up, else the store itself.
func (m *messageService) reader(acct config.ResolvedAccount) messageReader {
	st := m.store(acct)
	if ca := m.s.cacheAccount(acct.Name); ca != nil {
		return ca.View(st)
	}
	return serverReader{st: st}
}

// serverReader reads through an account's mail server.
type serverReader struct {
	st mailstore.Store
}

func (r serverReader) FetchMessagesFrom(ctx context.Context, refs []ref.Ref) ([]mailstore.MessageResult, []string) {
	res := r.st.FetchMessages(ctx, refs)
	sources := make([]string, len(res))
	for i := range sources {
		sources[i] = cache.SourceServer
	}
	return res, sources
}

func (r serverReader) ThreadFrom(ctx context.Context, seed ref.Ref) ([]ref.Ref, ref.Ref, string, error) {
	refs, err := r.st.Thread(ctx, seed)
	return refs, seed, cache.SourceServer, err
}
