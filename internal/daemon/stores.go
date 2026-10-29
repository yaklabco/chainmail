package daemon

import (
	"errors"
	"maps"
	"slices"
	"sync"

	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// storeSet holds one mail store per account, created on the account's first read.
type storeSet struct {
	newStore func(config.ResolvedAccount) mailstore.Store

	mu     sync.Mutex
	stores map[string]mailstore.Store
}

func (s *storeSet) get(acct config.ResolvedAccount) mailstore.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.stores[acct.Name]; ok {
		return st
	}
	if s.stores == nil {
		s.stores = map[string]mailstore.Store{}
	}
	st := s.newStore(acct)
	s.stores[acct.Name] = st
	return st
}

// closeAll closes every store in parallel.
func (s *storeSet) closeAll() error {
	s.mu.Lock()
	stores := slices.Collect(maps.Values(s.stores))
	s.stores = nil
	s.mu.Unlock()
	errs := make([]error, len(stores))
	var wg sync.WaitGroup
	for i, st := range stores {
		wg.Go(func() { errs[i] = st.Close() })
	}
	wg.Wait()
	return errors.Join(errs...)
}
