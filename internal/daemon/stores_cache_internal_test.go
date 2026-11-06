package daemon

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
)

// storeOnly has mailstore.Store's methods and nothing else.
type storeOnly struct{ mailstore.Store }

func TestAccountStorePassesHeadersAndScopeThrough(t *testing.T) {
	t.Parallel()
	fk := fake.New("host", false)
	r := fk.Add(fake.Message{
		Folder: "INBOX", Date: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), Subject: "s",
		Bcc: []mailstore.Address{{Email: "dave@example.net"}},
	})
	s := &storeSet{build: func(config.ResolvedAccount) mailstore.Store { return fk }}
	h := s.get(config.ResolvedAccount{Name: "host"})
	hf, ok := h.(mailstore.HeaderFetcher)
	if !ok {
		t.Fatalf("%T does not implement mailstore.HeaderFetcher", h)
	}
	hs, err := hf.FetchHeaders(t.Context(), mailstore.Source{Folder: "INBOX", UIDValidity: r.UIDValidity}, []uint32{r.UID})
	if err != nil || len(hs) != 1 || hs[0].Summary.Ref != r || len(hs[0].Bcc) != 1 {
		t.Errorf("FetchHeaders = %+v, %v; want the message with its Bcc", hs, err)
	}
	sc, ok := h.(mailstore.Scoper)
	if !ok {
		t.Fatalf("%T does not implement mailstore.Scoper", h)
	}
	if got, err := sc.SearchFolders(t.Context(), ""); err != nil || !slices.Equal(got, []string{"INBOX", "Archive", "Drafts", "Sent"}) {
		t.Errorf("SearchFolders = %q, %v", got, err)
	}
	if fk.Calls("FetchHeaders") != 1 || fk.Calls("SearchFolders") != 1 {
		t.Errorf("calls = %d, %d; want each passed through once", fk.Calls("FetchHeaders"), fk.Calls("SearchFolders"))
	}
}

func TestAccountStoreWithoutHeadersOrScopeIsUnsupported(t *testing.T) {
	t.Parallel()
	s := &storeSet{build: func(acct config.ResolvedAccount) mailstore.Store { return storeOnly{fake.New(acct.Name, false)} }}
	h := s.get(config.ResolvedAccount{Name: "host"})
	if _, err := h.(mailstore.HeaderFetcher).FetchHeaders(t.Context(), mailstore.Source{Folder: "INBOX", UIDValidity: 1}, []uint32{1}); !errors.Is(err, mailstore.ErrUnsupported) {
		t.Errorf("FetchHeaders on a store without it = %v, want ErrUnsupported", err)
	}
	if _, err := h.(mailstore.Scoper).SearchFolders(t.Context(), ""); !errors.Is(err, mailstore.ErrUnsupported) {
		t.Errorf("SearchFolders on a store without it = %v, want ErrUnsupported", err)
	}
}

func TestAccountStoreCacheReadsAfterClose(t *testing.T) {
	t.Parallel()
	s := &storeSet{build: func(acct config.ResolvedAccount) mailstore.Store { return fake.New(acct.Name, false) }}
	h := s.get(config.ResolvedAccount{Name: "host"})
	if err := s.closeAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.(mailstore.HeaderFetcher).FetchHeaders(t.Context(), mailstore.Source{Folder: "INBOX", UIDValidity: 1}, []uint32{1}); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("FetchHeaders after closeAll = %v, want ErrUnavailable", err)
	}
	if _, err := h.(mailstore.Scoper).SearchFolders(t.Context(), ""); !errors.Is(err, mailstore.ErrUnavailable) {
		t.Errorf("SearchFolders after closeAll = %v, want ErrUnavailable", err)
	}
}
