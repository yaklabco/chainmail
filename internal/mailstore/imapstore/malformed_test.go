package imapstore

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// fetchedRow is what the malformed-response tests check of a header or a
// summary.
type fetchedRow struct {
	uid                uint32
	subject            string
	hasAttachment      bool
	attachments        int
	attachmentsUnknown bool
}

// rowFetcher runs FetchHeaders or FetchSummaries and returns its rows.
type rowFetcher func(t *testing.T, s *Store, src mailstore.Source, uids []uint32) ([]fetchedRow, error)

// rowFetchers are FetchHeaders and FetchSummaries, the latter with
// snippets, so its text samples are fetched too.
var rowFetchers = map[string]rowFetcher{
	"FetchHeaders": func(t *testing.T, s *Store, src mailstore.Source, uids []uint32) ([]fetchedRow, error) {
		hs, err := s.FetchHeaders(t.Context(), src, uids)
		var rows []fetchedRow
		for _, h := range hs {
			rows = append(rows, fetchedRow{h.Summary.Ref.UID, h.Summary.Subject, h.Summary.HasAttachment, len(h.Attachments), h.Summary.AttachmentsUnknown})
		}
		return rows, err
	},
	"FetchSummaries": func(t *testing.T, s *Store, src mailstore.Source, uids []uint32) ([]fetchedRow, error) {
		sums, err := s.FetchSummaries(t.Context(), src, uids, mailstore.SummaryOptions{Snippets: true})
		var rows []fetchedRow
		for _, sum := range sums {
			rows = append(rows, fetchedRow{sum.Ref.UID, sum.Subject, sum.HasAttachment, -1, sum.AttachmentsUnknown})
		}
		return rows, err
	},
}

func scriptUIDs(n int) []uint32 {
	uids := make([]uint32, n)
	for i := range uids {
		uids[i] = uint32(i + 1)
	}
	return uids
}

// A malformed response fails at once, as ErrMalformed and not as an
// unavailable server: asking again on a fresh connection gets the same
// response. FetchMessages has no batch to split, so the message fails.
func TestMalformedResponseIsNotRetried(t *testing.T) {
	t.Parallel()
	srv := startScriptServer(t, &scriptServer{n: 1, badStructure: 1})
	s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
	err := fetchWithin(t, s, scriptRefs(1), 5*time.Second)[0].Err
	if !errors.Is(err, mailstore.ErrMalformed) || errors.Is(err, mailstore.ErrUnavailable) || errors.Is(err, errConnLost) {
		t.Errorf("FetchMessages error = %v, want ErrMalformed only", err)
	}
	if n, c := srv.fetches.Load(), srv.conns.Load(); n != 1 || c != 1 {
		t.Errorf("FETCH sent %d times on %d connections, want once on one", n, c)
	}
}

// A connection that drops partway through a summary FETCH is not a
// malformed response: the batch is retried on a fresh connection, whole.
func TestDroppedConnectionIsStillRetried(t *testing.T) {
	t.Parallel()
	for name, fetch := range rowFetchers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := &scriptServer{n: 4}
			srv.dropSummaries.Store(1)
			s := scriptStore(t, startScriptServer(t, srv), Options{CommandTimeout: 5 * time.Second})
			rows, err := fetch(t, s, mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, scriptUIDs(4))
			if err != nil || len(rows) != 4 {
				t.Fatalf("%s = %+v, %v; want 4 rows", name, rows, err)
			}
			if n := srv.summaryFetches("1:4"); n != 2 {
				t.Errorf("UID set 1:4 fetched %d times, want twice: once dropped, once retried", n)
			}
			if n := srv.conns.Load(); n != 2 {
				t.Errorf("%d connections, want 2", n)
			}
		})
	}
}

// A folder that cannot even be opened, since the server's answer to
// EXAMINE is malformed, fails the batch at once: splitting it would fail
// every part alike, at a login each, and report every message unreadable.
func TestMalformedFolderOpenIsNotSplit(t *testing.T) {
	t.Parallel()
	for name, fetch := range rowFetchers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: 8, badSelect: true})
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
			rows, err := fetch(t, s, mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, scriptUIDs(8))
			if _, ok := errors.AsType[*mailstore.UnreadableError](err); ok || !errors.Is(err, mailstore.ErrMalformed) || len(rows) != 0 {
				t.Errorf("%s = %+v, %v; want no rows and ErrMalformed, not an UnreadableError", name, rows, err)
			}
			if n := srv.conns.Load(); n != 1 {
				t.Errorf("%d connections, want 1", n)
			}
		})
	}
}

// A text sample the client cannot parse drops the snippets of the page
// rather than fail it. The connection the sample broke is not used again.
func TestMalformedSampleDropsSnippets(t *testing.T) {
	t.Parallel()
	srv := startScriptServer(t, &scriptServer{n: 4, badText: 2})
	s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
	src := mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}
	sums, err := s.FetchSummaries(t.Context(), src, scriptUIDs(4), mailstore.SummaryOptions{Snippets: true})
	if err != nil || len(sums) != 4 {
		t.Fatalf("FetchSummaries = %+v, %v; want 4 summaries", sums, err)
	}
	for _, sum := range sums {
		if sum.Sample != nil || sum.AttachmentsUnknown {
			t.Errorf("UID %d = %+v, want no sample and its attachments known", sum.Ref.UID, sum)
		}
	}
	if n := srv.summaryFetches("1:4"); n != 1 {
		t.Errorf("UID set 1:4 fetched %d times, want once", n)
	}
	if _, err := s.FetchSummaries(t.Context(), src, []uint32{1}, mailstore.SummaryOptions{}); err != nil {
		t.Fatalf("FetchSummaries after: %v", err)
	}
	if n := srv.conns.Load(); n != 2 {
		t.Errorf("%d connections, want 2: the one the sample broke is not reused", n)
	}
}

// syncers run one sync pass through SyncFolder on the pool, or through a
// watch connection.
var syncers = map[string]func(t *testing.T, s *Store, req mailstore.SyncRequest) (mailstore.Delta, error){
	"SyncFolder": func(t *testing.T, s *Store, req mailstore.SyncRequest) (mailstore.Delta, error) {
		return s.SyncFolder(t.Context(), "INBOX", req)
	},
	"watch": func(t *testing.T, s *Store, req mailstore.SyncRequest) (mailstore.Delta, error) {
		w, err := s.OpenWatch(t.Context(), "INBOX")
		if err != nil {
			t.Fatalf("OpenWatch: %v", err)
		}
		t.Cleanup(func() { _ = w.Close() }) // the pass may have left it dead
		return w.Sync(t.Context(), req)
	},
}

// A sync pass whose new mail holds a malformed message still reports the
// rest. A message read without its body structure is new mail with its
// attachments unknown, and one that cannot be read at all is in Unreadable,
// so the checkpoint can move past it.
func TestSyncPassIsolatesMalformedNewMail(t *testing.T) {
	t.Parallel()
	from := mailstore.Checkpoint{UIDValidity: scriptUIDValidity, UIDNext: 1, HighestModSeq: scriptModSeq}
	for name, sync := range syncers {
		t.Run(name+" structure", func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: 4, condstore: true, badStructure: 2})
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
			d, err := sync(t, s, mailstore.SyncRequest{From: from})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			var got []uint32
			for _, sum := range d.New {
				got = append(got, sum.Ref.UID)
				if sum.AttachmentsUnknown != (sum.Ref.UID == 2) {
					t.Errorf("UID %d attachments unknown = %v", sum.Ref.UID, sum.AttachmentsUnknown)
				}
			}
			if !slices.Equal(got, []uint32{1, 2, 3, 4}) || len(d.Unreadable) != 0 || d.More {
				t.Errorf("delta = new %v, unreadable %v, more %v; want 1 to 4, none, false", got, d.Unreadable, d.More)
			}
		})
		t.Run(name+" envelope", func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: 4, condstore: true, badEnvelope: 3})
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
			d, err := sync(t, s, mailstore.SyncRequest{From: from, Batch: 3})
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			var got []uint32
			for _, sum := range d.New {
				got = append(got, sum.Ref.UID)
			}
			if !slices.Equal(got, []uint32{1, 2}) || !slices.Equal(d.Unreadable, []uint32{3}) || !d.More {
				t.Errorf("delta = new %v, unreadable %v, more %v; want [1 2], [3], true", got, d.Unreadable, d.More)
			}
		})
	}
}

// A watch connection whose pass met malformed new mail says it needs a
// redial, since the parse error ended it, and one whose pass read its new
// mail whole does not.
func TestWatchConnNeedsRedialAfterMalformedNewMail(t *testing.T) {
	t.Parallel()
	from := mailstore.Checkpoint{UIDValidity: scriptUIDValidity, UIDNext: 1, HighestModSeq: scriptModSeq}
	for name, tt := range map[string]struct {
		srv  *scriptServer
		want bool
	}{
		"well formed":   {srv: &scriptServer{n: 4, condstore: true}, want: false},
		"bad structure": {srv: &scriptServer{n: 4, condstore: true, badStructure: 2}, want: true},
		"bad envelope":  {srv: &scriptServer{n: 4, condstore: true, badEnvelope: 3}, want: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := scriptStore(t, startScriptServer(t, tt.srv), Options{CommandTimeout: 5 * time.Second})
			w, err := s.OpenWatch(t.Context(), "INBOX")
			if err != nil {
				t.Fatalf("OpenWatch: %v", err)
			}
			t.Cleanup(func() { _ = w.Close() })
			if _, err := w.Sync(t.Context(), mailstore.SyncRequest{From: from}); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			r, ok := w.(mailstore.Redialer)
			if !ok {
				t.Fatal("the watch connection is not a mailstore.Redialer")
			}
			if got := r.NeedsRedial(); got != tt.want {
				t.Errorf("NeedsRedial = %v, want %v", got, tt.want)
			}
		})
	}
}
