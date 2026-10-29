package fake

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
)

func mustParse(t *testing.T, s string) *query.Node {
	t.Helper()
	n, err := query.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	_, rest := query.Split(n)
	return rest
}

func TestFakeSearchAndFetch(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s.Add(Message{
		Folder: "INBOX", Date: now.Add(-time.Hour), Subject: "Invoice 7", Body: "pay me",
		From: []mailstore.Address{{Name: "Acme", Email: "billing@acme.example"}},
	})
	r2 := s.Add(Message{Folder: "INBOX", Date: now, Subject: "Lunch", Body: "invoice attached", Flags: []string{`\Seen`}})
	s.Add(Message{Folder: "Trash", Date: now, Subject: "Invoice old"})

	res, err := s.Search(t.Context(), mailstore.SearchRequest{Query: mustParse(t, "invoice")})
	if err != nil {
		t.Fatal(err)
	}
	var total int
	for _, r := range res {
		if r.Source.Folder == "Trash" {
			t.Error("Trash searched without in:trash")
		}
		total += len(r.UIDs)
	}
	if total != 2 {
		t.Errorf("matches = %d, want 2", total)
	}

	got := s.FetchMessages(t.Context(), []ref.Ref{r2})
	if got[0].Err != nil || got[0].Message.Summary.Subject != "Lunch" {
		t.Fatalf("FetchMessages = %+v", got[0])
	}
	s.SetUIDValidity("INBOX", 99)
	if got := s.FetchMessages(t.Context(), []ref.Ref{r2}); !errors.Is(got[0].Err, mailstore.ErrStaleRef) {
		t.Errorf("after a UIDVALIDITY change: %v, want ErrStaleRef", got[0].Err)
	}
}

func TestFakeRefusesGmailOnlyOperators(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"has:attachment", "filename:pdf", "label:x"} {
		if _, err := New("host", false).Search(t.Context(), mailstore.SearchRequest{Query: mustParse(t, q)}); !errors.Is(err, mailstore.ErrUnsupportedQuery) {
			t.Errorf("%s on a standard account: %v", q, err)
		}
		if _, err := New("gmail", true).Search(t.Context(), mailstore.SearchRequest{Query: mustParse(t, q)}); err != nil {
			t.Errorf("%s on Gmail: %v", q, err)
		}
	}
}

func TestFakeStreamPart(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	r := s.Add(Message{Folder: "INBOX", Subject: "s", Body: "b", Attachments: []Attachment{{Part: "2", Filename: "a.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.7")}}})
	info, rc, err := s.StreamPart(t.Context(), r, "2")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if info.Filename != "a.pdf" || string(data) != "%PDF-1.7" {
		t.Errorf("StreamPart = %+v %q", info, data)
	}
	if _, _, err := s.StreamPart(t.Context(), r, "9"); !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("missing part: %v", err)
	}
}
