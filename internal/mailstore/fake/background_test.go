package fake

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
)

// backgroundRead is one read SetBusy covers, returning its error: a
// source's for Search with bounds, an item's for FetchMessages.
type backgroundRead struct {
	method string
	read   func(ctx context.Context) error
}

func backgroundReads(s *Store, r ref.Ref) []backgroundRead {
	src := mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}
	return []backgroundRead{
		{"ListFolders", func(ctx context.Context) error {
			_, err := s.ListFolders(ctx)
			return err
		}},
		{"Search", func(ctx context.Context) error {
			res, err := s.Search(ctx, mailstore.SearchRequest{Bounds: map[string]mailstore.Bound{r.Folder: {UIDValidity: r.UIDValidity}}})
			if err != nil {
				return err
			}
			return res[0].Err
		}},
		{"FetchSummaries", func(ctx context.Context) error {
			_, err := s.FetchSummaries(ctx, src, []uint32{r.UID}, mailstore.SummaryOptions{})
			return err
		}},
		{"FetchMessages", func(ctx context.Context) error { return s.FetchMessages(ctx, []ref.Ref{r})[0].Err }},
		{"Thread", func(ctx context.Context) error {
			_, err := s.Thread(ctx, r)
			return err
		}},
	}
}

func TestSetBusy(t *testing.T) {
	t.Parallel()
	for _, gmail := range []bool{false, true} {
		folder := "INBOX"
		if gmail {
			folder = "[Gmail]/All Mail"
		}
		s := New("acct", gmail)
		r := s.Add(Message{Folder: folder, Date: time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC), Subject: "s", Body: "b"})
		bg := mailstore.WithBackground(t.Context())
		s.SetBusy(true)
		for _, rd := range backgroundReads(s, r) {
			if err := rd.read(bg); !errors.Is(err, mailstore.ErrBusy) {
				t.Errorf("gmail=%v: busy background %s = %v, want ErrBusy", gmail, rd.method, err)
			}
			if err := rd.read(t.Context()); err != nil {
				t.Errorf("gmail=%v: busy foreground %s = %v, want it to work", gmail, rd.method, err)
			}
		}
		s.SetBusy(false)
		for _, rd := range backgroundReads(s, r) {
			if err := rd.read(bg); err != nil {
				t.Errorf("gmail=%v: background %s after SetBusy(false) = %v", gmail, rd.method, err)
			}
			if n := s.BackgroundCalls(rd.method); n != 2 {
				t.Errorf("gmail=%v: BackgroundCalls(%s) = %d, want 2, busy or not", gmail, rd.method, n)
			}
			if n := s.Calls(rd.method); n != 3 {
				t.Errorf("gmail=%v: Calls(%s) = %d, want 3, background or not", gmail, rd.method, n)
			}
		}
	}
}

// A search without bounds fails as a whole, as imapstore's does when it
// cannot list the folders.
func TestSetBusySearchWithoutBounds(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	s.SetBusy(true)
	if _, err := s.Search(mailstore.WithBackground(t.Context()), mailstore.SearchRequest{}); !errors.Is(err, mailstore.ErrBusy) {
		t.Errorf("busy background Search without bounds = %v, want ErrBusy", err)
	}
}

// StreamPart ignores the background mark and SetBusy, as imapstore's does.
func TestSetBusyLeavesStreamPartAlone(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	r := s.Add(Message{Folder: "INBOX", Subject: "s", Body: "b"})
	s.SetBusy(true)
	_, rc, err := s.StreamPart(mailstore.WithBackground(t.Context()), r, "")
	if err != nil {
		t.Fatalf("busy background StreamPart = %v, want it to work", err)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Error(err)
	}
	if n := s.BackgroundCalls("StreamPart"); n != 0 {
		t.Errorf("BackgroundCalls(StreamPart) = %d, want 0", n)
	}
}

// A Fail error still applies to background calls when not busy.
func TestBackgroundCallsKeepFail(t *testing.T) {
	t.Parallel()
	s := New("acct", false)
	boom := errors.New("boom")
	s.Fail("ListFolders", boom)
	if _, err := s.ListFolders(mailstore.WithBackground(t.Context())); !errors.Is(err, boom) {
		t.Errorf("background ListFolders with Fail set = %v, want %v", err, boom)
	}
}

// StreamPartBackground is StreamPart admitted as background work: it
// fails with ErrBusy while SetBusy is on, whatever ctx says, and otherwise
// opens the part. Each call counts as a StreamPart call made with a
// background context, and Fail("StreamPart") applies to it.
func TestStreamPartBackground(t *testing.T) {
	t.Parallel()
	var _ mailstore.BackgroundPartStreamer = (*Store)(nil)
	s := New("acct", false)
	r := s.Add(Message{Folder: "INBOX", Subject: "s", Body: "b", Attachments: []Attachment{{Part: "2", ContentType: "text/calendar; method=REQUEST", Data: []byte("ics")}}})
	s.SetBusy(true)
	if _, _, err := s.StreamPartBackground(t.Context(), r, "2"); !errors.Is(err, mailstore.ErrBusy) {
		t.Errorf("busy StreamPartBackground = %v, want ErrBusy", err)
	}
	s.SetBusy(false)
	info, rc, err := s.StreamPartBackground(t.Context(), r, "2")
	if err != nil {
		t.Fatalf("StreamPartBackground = %v", err)
	}
	data, err := io.ReadAll(rc)
	if err != nil || string(data) != "ics" || info.ContentType != "text/calendar; method=REQUEST" {
		t.Errorf("read %q, %v, info %+v, want the part", data, err, info)
	}
	if n := s.Calls("StreamPart"); n != 2 {
		t.Errorf("Calls(StreamPart) = %d, want 2", n)
	}
	if n := s.BackgroundCalls("StreamPart"); n != 2 {
		t.Errorf("BackgroundCalls(StreamPart) = %d, want 2", n)
	}
	boom := errors.New("boom")
	s.Fail("StreamPart", boom)
	if _, _, err := s.StreamPartBackground(t.Context(), r, "2"); !errors.Is(err, boom) {
		t.Errorf("StreamPartBackground with Fail set = %v, want %v", err, boom)
	}
}
