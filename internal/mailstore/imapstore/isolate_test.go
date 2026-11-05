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
