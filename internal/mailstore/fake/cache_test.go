package fake

import (
	"bytes"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

func cacheDay(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }

// A message in Sent or Drafts has a Bcc header, which bcc: matches, as IMAP
// SEARCH BCC does. One without Bcc never matches bcc:.
func TestFakeBccMatches(t *testing.T) {
	t.Parallel()
	for _, gmail := range []bool{false, true} {
		s := New("acct", gmail)
		folder := "INBOX"
		if gmail {
			folder = "[Gmail]/All Mail"
		}
		r := s.Add(Message{Folder: folder, Subject: "sent", From: []mailstore.Address{{Email: "me@example.test"}}, Bcc: []mailstore.Address{{Name: "Dave", Email: "dave@example.net"}}})
		s.Add(Message{Folder: folder, Subject: "plain", To: []mailstore.Address{{Email: "dave@example.net"}}})
		if n := count(t, s, "bcc:dave", ""); n != 1 {
			t.Errorf("gmail=%v: bcc:dave matched %d, want 1", gmail, n)
		}
		if n := count(t, s, "-bcc:dave", ""); n != 1 {
			t.Errorf("gmail=%v: -bcc:dave matched %d, want 1", gmail, n)
		}
		if raw := s.RawMessage(r); !bytes.Contains(raw, []byte("\r\nBcc: \"Dave\" <dave@example.net>\r\n")) {
			t.Errorf("gmail=%v: raw message lacks the Bcc header:\n%s", gmail, raw)
		}
	}
}

// Add keeps a copy of Bcc, so the caller's slice can change afterwards.
func TestFakeAddCopiesBcc(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	bcc := []mailstore.Address{{Email: "dave@example.net"}}
	s.Add(Message{Folder: "INBOX", Subject: "s", Bcc: bcc})
	bcc[0].Email = "mallory@example.net"
	if n := count(t, s, "bcc:dave", ""); n != 1 {
		t.Errorf("bcc:dave matched %d after the caller changed its slice, want 1", n)
	}
}

// A message appended with a Bcc header keeps it, as a server stores a draft.
func TestFakeAppendKeepsBcc(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	raw := "From: me@example.test\r\nTo: bob@example.net\r\nBcc: dave@example.net\r\nSubject: draft\r\n\r\nbody\r\n"
	if _, err := s.Append(t.Context(), "Drafts", nil, cacheDay(20), []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "bcc:dave", "drafts"); n != 1 {
		t.Errorf("bcc:dave in Drafts matched %d, want 1", n)
	}
}
