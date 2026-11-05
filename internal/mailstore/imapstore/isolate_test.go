package imapstore

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// A message whose body structure the client cannot parse costs the batch
// nothing: the other seven come back whole, and that one comes back without
// its attachments, marked as unknown. Each set that failed was fetched
// once, never retried, and the hunt for the message took a fresh connection
// only for each failure.
func TestMalformedBodyStructureIsIsolated(t *testing.T) {
	t.Parallel()
	for name, fetch := range rowFetchers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: 8, structure: gmailAttachedMessage, badStructure: 5})
			logs := &lockedBuffer{}
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second, Logger: slog.New(slog.NewTextHandler(logs, nil))})
			rows, err := fetch(t, s, mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, scriptUIDs(8))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			var got []uint32
			for _, r := range rows {
				got = append(got, r.uid)
				if r.subject != fmt.Sprintf("m%d", r.uid) {
					t.Errorf("UID %d subject = %q", r.uid, r.subject)
				}
				if r.uid == 5 {
					if !r.attachmentsUnknown || r.hasAttachment || r.attachments > 0 {
						t.Errorf("UID 5 = %+v, want attachments unknown and none listed", r)
					}
					continue
				}
				if r.attachmentsUnknown || !r.hasAttachment || r.attachments == 0 {
					t.Errorf("UID %d = %+v, want its attached message and attachments known", r.uid, r)
				}
			}
			if !slices.Equal(got, scriptUIDs(8)) {
				t.Errorf("UIDs = %v, want 1 to 8 in order", got)
			}
			wantWarning(t, logs.String(), "without its body structure")
			for _, set := range []string{"1:8", "5:8", "5:6", "5", "5 without BODYSTRUCTURE"} {
				if n := srv.summaryFetches(set); n != 1 {
					t.Errorf("UID set %s fetched %d times, want once", set, n)
				}
			}
			// One connection to start, and one more after each of the four
			// failed fetches.
			if n := srv.conns.Load(); n != 5 {
				t.Errorf("%d connections, want 5", n)
			}
		})
	}
}

// A message that cannot be read even without its body structure, since its
// envelope is malformed too, is left out, and the error names it, together
// with the other seven.
func TestUnreadableMessageIsReportedWithTheRest(t *testing.T) {
	t.Parallel()
	for name, fetch := range rowFetchers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: 8, badEnvelope: 5})
			logs := &lockedBuffer{}
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second, Logger: slog.New(slog.NewTextHandler(logs, nil))})
			rows, err := fetch(t, s, mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, scriptUIDs(8))
			unreadable, ok := errors.AsType[*mailstore.UnreadableError](err)
			if !ok || unreadable.Folder != "INBOX" || !slices.Equal(unreadable.UIDs, []uint32{5}) {
				t.Fatalf("%s error = %v, want an UnreadableError for UID 5 in INBOX", name, err)
			}
			if !errors.Is(err, mailstore.ErrMalformed) || errors.Is(err, mailstore.ErrUnavailable) {
				t.Errorf("%s error = %v, want ErrMalformed only", name, err)
			}
			var got []uint32
			for _, r := range rows {
				got = append(got, r.uid)
				if r.attachmentsUnknown {
					t.Errorf("UID %d has attachments unknown", r.uid)
				}
			}
			if want := []uint32{1, 2, 3, 4, 6, 7, 8}; !slices.Equal(got, want) {
				t.Errorf("UIDs = %v, want %v", got, want)
			}
			wantWarning(t, logs.String(), "cannot read a message")
			if n := srv.summaryFetches("5 without BODYSTRUCTURE"); n != 1 {
				t.Errorf("UID 5 fetched %d times without BODYSTRUCTURE, want once", n)
			}
		})
	}
}

// wantWarning checks that logs hold one warning, about UID 5 of INBOX on
// account host, whose message holds what.
func wantWarning(t *testing.T, logs, what string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) != 1 {
		t.Fatalf("logs = %q, want one warning", logs)
	}
	for _, want := range []string{"level=WARN", what, "account=host", "folder=INBOX", "uid=5"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("warning %q lacks %q", lines[0], want)
		}
	}
}

// lockedBuffer is a bytes.Buffer safe for a logger to write from any
// goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A UID asked for twice is fetched and reported once.
func TestFetchSummariesIsolatesEachUIDOnce(t *testing.T) {
	t.Parallel()
	srv := startScriptServer(t, &scriptServer{n: 8, badEnvelope: 5})
	logs := &lockedBuffer{}
	s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	sums, err := s.FetchSummaries(t.Context(), mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, []uint32{5, 5, 1}, mailstore.SummaryOptions{})
	unreadable, ok := errors.AsType[*mailstore.UnreadableError](err)
	if !ok || !slices.Equal(unreadable.UIDs, []uint32{5}) {
		t.Fatalf("FetchSummaries error = %v, want an UnreadableError for UID 5 alone", err)
	}
	if len(sums) != 1 || sums[0].Ref.UID != 1 {
		t.Errorf("FetchSummaries = %+v, want UID 1 alone", sums)
	}
	if n := srv.summaryFetches("5 without BODYSTRUCTURE"); n != 1 {
		t.Errorf("UID 5 fetched %d times without BODYSTRUCTURE, want once", n)
	}
	wantWarning(t, logs.String(), "cannot read a message")
}

// A message that is gone by the time it is fetched without its body
// structure is not reported as read, nor as unreadable.
func TestIsolateWarnsOnlyOfMessagesRead(t *testing.T) {
	t.Parallel()
	for name, fetch := range rowFetchers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: 8, badStructure: 5, hideReduced: true})
			logs := &lockedBuffer{}
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second, Logger: slog.New(slog.NewTextHandler(logs, nil))})
			rows, err := fetch(t, s, mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, scriptUIDs(8))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			var got []uint32
			for _, r := range rows {
				got = append(got, r.uid)
			}
			if want := []uint32{1, 2, 3, 4, 6, 7, 8}; !slices.Equal(got, want) {
				t.Errorf("UIDs = %v, want %v", got, want)
			}
			if l := logs.String(); strings.Contains(l, "without its body structure") {
				t.Errorf("logs = %q, want no warning of a message read without its body structure", l)
			}
		})
	}
}

// Another error partway through the hunt, here the folder's UIDVALIDITY
// changing before the bad message is fetched without its body structure,
// ends it. FetchHeaders returns only the error, and FetchSummaries the
// summaries of the half it read before it, never an UnreadableError.
func TestIsolateStopsAtAnotherError(t *testing.T) {
	t.Parallel()
	for name, fetch := range rowFetchers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The FETCHes of 1:8, 1:4, 5:8, 5:6 and 5 are answered before
			// the connection for the refetch of 5 opens the folder.
			srv := startScriptServer(t, &scriptServer{n: 8, badStructure: 5, staleAfter: 5})
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
			rows, err := fetch(t, s, mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, scriptUIDs(8))
			if !errors.Is(err, mailstore.ErrStaleRef) || errors.Is(err, mailstore.ErrMalformed) {
				t.Fatalf("%s error = %v, want ErrStaleRef alone", name, err)
			}
			var got []uint32
			for _, r := range rows {
				got = append(got, r.uid)
			}
			want := []uint32{1, 2, 3, 4}
			if name == "FetchHeaders" {
				want = nil
			}
			if !slices.Equal(got, want) {
				t.Errorf("UIDs = %v, want %v", got, want)
			}
			if n := srv.summaryFetches("5 without BODYSTRUCTURE"); n != 0 {
				t.Errorf("UID 5 fetched %d times without BODYSTRUCTURE, want none: the folder open failed first", n)
			}
		})
	}
}

// A batch where every message is malformed costs a bounded number of logins:
// once the hunt has used its budget of failures, the rest of the batch is
// read in one go without body structures. When even that fails, the result
// is a plain ErrMalformed, so the caller backs off rather than record every
// message as unreadable.
func TestIsolateBoundsItsLogins(t *testing.T) {
	t.Parallel()
	const n, maxLogins = 500, 30
	for name, fetch := range rowFetchers {
		t.Run(name+" structures", func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: n, badStructure: -1})
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
			rows, err := fetch(t, s, mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, scriptUIDs(n))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(rows) != n {
				t.Errorf("%d rows, want %d", len(rows), n)
			}
			for _, r := range rows {
				if !r.attachmentsUnknown {
					t.Errorf("UID %d has its attachments known, want unknown", r.uid)
					break
				}
			}
			if c := srv.conns.Load(); c > maxLogins {
				t.Errorf("%d connections, want at most %d", c, maxLogins)
			}
		})
		t.Run(name+" envelopes", func(t *testing.T) {
			t.Parallel()
			srv := startScriptServer(t, &scriptServer{n: n, badEnvelope: -1})
			s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
			rows, err := fetch(t, s, mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, scriptUIDs(n))
			if _, ok := errors.AsType[*mailstore.UnreadableError](err); ok || !errors.Is(err, mailstore.ErrMalformed) {
				t.Errorf("%s error = %v, want a plain ErrMalformed", name, err)
			}
			if len(rows) != 0 {
				t.Errorf("%d rows, want none", len(rows))
			}
			if c := srv.conns.Load(); c > maxLogins {
				t.Errorf("%d connections, want at most %d", c, maxLogins)
			}
		})
	}
}

// A batch of mailstore.IsolatedBatch UIDs or fewer never runs out of its
// hunt's budget: even when every message in it is unreadable, each comes
// back in an UnreadableError and never as a plain ErrMalformed. The cache
// relies on this to tell a fault in a message from one in the folder.
func TestIsolateAlwaysIsolatesASmallBatch(t *testing.T) {
	t.Parallel()
	for name, fetch := range rowFetchers {
		for n := 1; n <= mailstore.IsolatedBatch; n++ {
			t.Run(fmt.Sprintf("%s %d", name, n), func(t *testing.T) {
				t.Parallel()
				srv := startScriptServer(t, &scriptServer{n: n, badEnvelope: -1})
				s := scriptStore(t, srv, Options{CommandTimeout: 5 * time.Second})
				rows, err := fetch(t, s, mailstore.Source{Folder: "INBOX", UIDValidity: scriptUIDValidity}, scriptUIDs(n))
				ue, ok := errors.AsType[*mailstore.UnreadableError](err)
				if !ok || !slices.Equal(ue.UIDs, scriptUIDs(n)) || len(rows) != 0 {
					t.Errorf("%s of %d bad messages = %d rows, %v; want an UnreadableError naming all of them", name, n, len(rows), err)
				}
			})
		}
	}
}
