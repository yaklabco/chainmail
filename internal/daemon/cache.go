package daemon

import (
	"context"
	"os"
	"path/filepath"

	"github.com/jamesainslie/chainmail/internal/cache"
)

// cacheFile is the cache database's name in Options.CacheDir.
const cacheFile = "cache.db"

// openCache opens cache.db before anything uses it. It opens nothing with
// DisableCache, without a CacheDir, or when no account has the cache
// enabled. A failure is logged and kept in cacheErr: the daemon then runs
// on the server path, as before SP7. cache.Open replaces a file that is
// corrupt or of another schema version itself.
func (s *Server) openCache(ctx context.Context) {
	if s.opts.DisableCache || s.opts.CacheDir == "" {
		return
	}
	if _, ok := s.firstCacheAccount(); !ok {
		s.opts.Logger.Info("the cache is disabled for every account")
		return
	}
	if err := os.MkdirAll(s.opts.CacheDir, 0o700); err != nil {
		s.cacheOpenFailed(err)
		return
	}
	st, err := cache.Open(ctx, filepath.Join(s.opts.CacheDir, cacheFile), cache.OpenOptions{Now: s.opts.Now, Logger: s.opts.Logger})
	if err != nil {
		s.cacheOpenFailed(err)
		return
	}
	s.cache = st
}

// cacheOpenFailed records why cache.db did not open.
func (s *Server) cacheOpenFailed(err error) {
	s.cacheErr = err
	s.opts.Logger.Error("cache.db could not be opened; searches and reads use the mail servers", "err", err)
}

// closeCache closes cache.db, once nothing uses it any more.
func (s *Server) closeCache() {
	if s.cache == nil {
		return
	}
	if err := s.cache.Close(); err != nil {
		s.opts.Logger.Warn("closing cache.db", "err", err)
	}
}

// firstCacheAccount returns the first configured account, in name order,
// with the cache enabled.
func (s *Server) firstCacheAccount() (string, bool) {
	cfg := s.opts.Config
	for _, name := range cfg.AccountNames() {
		if cfg.CacheSettings(name).Enabled {
			return name, true
		}
	}
	return "", false
}
