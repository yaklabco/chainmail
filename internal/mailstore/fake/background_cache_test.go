package fake

import (
	"errors"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
)

// The cache's reads, FetchHeaders and SearchFolders, honor SetBusy and
// count as background calls like the others.
func TestSetBusyCoversCacheReads(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	r := s.Add(Message{Folder: "INBOX", Date: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), Subject: "s"})
	src := mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}
	bg := mailstore.WithBackground(t.Context())
	s.SetBusy(true)
	if _, err := s.FetchHeaders(bg, src, []uint32{r.UID}); !errors.Is(err, mailstore.ErrBusy) {
		t.Errorf("busy background FetchHeaders = %v, want ErrBusy", err)
	}
	if _, err := s.SearchFolders(bg, ""); !errors.Is(err, mailstore.ErrBusy) {
		t.Errorf("busy background SearchFolders = %v, want ErrBusy", err)
	}
	if hs, err := s.FetchHeaders(t.Context(), src, []uint32{r.UID}); err != nil || len(hs) != 1 {
		t.Errorf("busy foreground FetchHeaders = %v, %v; want the header", hs, err)
	}
	for _, method := range []string{"FetchHeaders", "SearchFolders"} {
		if n := s.BackgroundCalls(method); n != 1 {
			t.Errorf("BackgroundCalls(%s) = %d, want 1", method, n)
		}
	}
}
