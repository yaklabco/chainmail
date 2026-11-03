package fake

import (
	"context"
	"maps"
	"slices"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// SetBusy, while on, makes every read called with a background context
// (mailstore.WithBackground) fail with mailstore.ErrBusy, as imapstore's
// do when no connection is free for background work: ListFolders,
// FetchSummaries and Thread return it, as do FetchHeaders and
// SearchFolders, FetchMessages gives it in every item, and Search gives it
// in every source of a search with bounds and as its own error otherwise.
// StreamPart, the writes and the watch methods ignore it, as imapstore's
// StreamPart and OpenWatch ignore the mark.
func (s *Store) SetBusy(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = on
}

// BackgroundCalls returns how many times method, one of the reads SetBusy
// covers, ran with a background context, busy or not.
func (s *Store) BackgroundCalls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.background[method]
}

// beginRead is begin for a read SetBusy covers: it also counts a call made
// with a background context and, while SetBusy is on, fails it with
// mailstore.ErrBusy. s.mu must be held.
func (s *Store) beginRead(ctx context.Context, method string) error {
	err := s.begin(method)
	if !mailstore.IsBackground(ctx) {
		return err
	}
	if s.background == nil {
		s.background = map[string]int{}
	}
	s.background[method]++
	if s.busy {
		return mailstore.ErrBusy
	}
	return err
}

// busySourcesLocked is what a busy background search with bounds gives:
// ErrBusy in each source, with no UIDVALIDITY, as imapstore reports a
// folder it got no connection for.
func (s *Store) busySourcesLocked(bounds map[string]mailstore.Bound) []mailstore.SourceResult {
	names := slices.Sorted(maps.Keys(bounds))
	out := make([]mailstore.SourceResult, len(names))
	for i, name := range names {
		out[i] = mailstore.SourceResult{Source: mailstore.Source{Folder: name}, Err: mailstore.ErrBusy}
	}
	return out
}
