package fake

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

const (
	gmailTrash = "[Gmail]/Trash"
	gmailSpam  = "[Gmail]/Spam"
	gmailSent  = "[Gmail]/Sent Mail"
)

// one returns the only result of a batch call.
func one(t *testing.T, res []mailstore.RefResult) mailstore.RefResult {
	t.Helper()
	if len(res) != 1 {
		t.Fatalf("results = %d, want 1", len(res))
	}
	return res[0]
}

// fetch reads r or fails the test.
func fetch(t *testing.T, s *Store, r ref.Ref) *mailstore.Message {
	t.Helper()
	got := s.FetchMessages(t.Context(), []ref.Ref{r})[0]
	if got.Err != nil {
		t.Fatalf("FetchMessages(%s): %v", r, got.Err)
	}
	return got.Message
}

// gone checks that r no longer resolves, with ErrStaleRef or ErrNotFound.
func gone(t *testing.T, s *Store, r ref.Ref) {
	t.Helper()
	err := s.FetchMessages(t.Context(), []ref.Ref{r})[0].Err
	if !errors.Is(err, mailstore.ErrStaleRef) && !errors.Is(err, mailstore.ErrNotFound) {
		t.Errorf("old ref %s still resolves: %v", r, err)
	}
}

func folderNames(t *testing.T, s *Store) []string {
	t.Helper()
	list, err := s.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(list))
	for i, f := range list {
		out[i] = f.Name
	}
	return out
}

// listed is how many messages ListFolders reports in folder.
func listed(t *testing.T, s *Store, folder string) uint32 {
	t.Helper()
	list, err := s.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range list {
		if f.Name == folder {
			return f.Messages
		}
	}
	t.Fatalf("no folder %q", folder)
	return 0
}

func raw(subject, messageID string) []byte {
	return []byte("From: Ann <ann@example.com>\r\nTo: bob@example.com\r\nSubject: " + subject +
		"\r\nDate: Thu, 29 Oct 2026 12:00:00 +0000\r\nMessage-ID: <" + messageID + ">\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\nhello " + subject + "\r\n")
}

// newHost is a standard account with a Projects folder.
func newHost() *Store {
	s := New("host", false)
	s.AddFolder("Projects", mailstore.RoleNone)
	return s
}

// newGmail is a Gmail account with a Receipts label.
func newGmail() *Store {
	s := New("gmail", true)
	s.AddFolder("Receipts", mailstore.RoleNone)
	return s
}

// dropRole takes a role off a folder, so a test can model an account
// without one.
func dropRole(s *Store, role mailstore.Role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.folders {
		if s.folders[i].Role == role {
			s.folders[i].Role = mailstore.RoleNone
		}
	}
}

func TestWriteAppend(t *testing.T) {
	t.Parallel()
	date := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		gmail      bool
		folder     string
		flags      []string
		wantFolder string
		wantLabels []string
		listedIn   string
		wantErr    error
	}{
		{name: "standard INBOX", folder: "INBOX", flags: []string{`\Seen`}, wantFolder: "INBOX", listedIn: "INBOX"},
		{name: "standard Sent", folder: "Sent", flags: []string{`\Seen`}, wantFolder: "Sent", listedIn: "Sent"},
		{name: "standard Drafts", folder: "Drafts", flags: []string{`\Draft`}, wantFolder: "Drafts", listedIn: "Drafts"},
		{name: "standard unknown folder", folder: "Nope", wantErr: mailstore.ErrNotFound},
		{name: "Gmail INBOX is All Mail with \\Inbox", gmail: true, folder: "INBOX", wantFolder: allMail, wantLabels: []string{`\Inbox`}, listedIn: "INBOX"},
		{name: "Gmail Sent Mail is All Mail with \\Sent", gmail: true, folder: gmailSent, wantFolder: allMail, wantLabels: []string{`\Sent`}, listedIn: gmailSent},
		{name: "Gmail label folder", gmail: true, folder: "Receipts", wantFolder: allMail, wantLabels: []string{"Receipts"}, listedIn: "Receipts"},
		{name: "Gmail All Mail", gmail: true, folder: allMail, wantFolder: allMail, listedIn: allMail},
		{name: "Gmail Trash", gmail: true, folder: gmailTrash, wantFolder: gmailTrash, listedIn: gmailTrash},
		{name: "Gmail star flag is the \\Starred label", gmail: true, folder: allMail, flags: []string{`\Flagged`}, wantFolder: allMail, wantLabels: []string{`\Starred`}, listedIn: allMail},
		{name: "Gmail unknown folder", gmail: true, folder: "Nope", wantErr: mailstore.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newHost()
			if tt.gmail {
				s = newGmail()
			}
			body := raw("Appended", "a1@example.com")
			r, err := s.Append(t.Context(), tt.folder, tt.flags, date, body)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Append = %v, want %v", err, tt.wantErr)
				}
				if len(s.Appended()) != 0 {
					t.Error("a failed Append was recorded in Appended")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Folder != tt.wantFolder || r.Account != s.account || r.UID == 0 || r.UIDValidity == 0 {
				t.Fatalf("ref = %+v, want folder %s", r, tt.wantFolder)
			}
			if tt.gmail != (r.GmailMsgID != 0) {
				t.Errorf("GmailMsgID = %d on gmail=%v", r.GmailMsgID, tt.gmail)
			}
			m := fetch(t, s, r)
			if m.Summary.Subject != "Appended" || m.MessageID != "<a1@example.com>" || !m.Summary.InternalDate.Equal(date) {
				t.Errorf("message = %+v", m.Summary)
			}
			if len(m.Summary.From) != 1 || m.Summary.From[0].Email != "ann@example.com" || len(m.Summary.To) != 1 {
				t.Errorf("addresses = %+v, %+v", m.Summary.From, m.Summary.To)
			}
			for _, f := range tt.flags {
				if !slices.Contains(m.Summary.Flags, f) {
					t.Errorf("flags = %v, want %s", m.Summary.Flags, f)
				}
			}
			for _, l := range tt.wantLabels {
				if !slices.Contains(m.Summary.Labels, l) {
					t.Errorf("labels = %v, want %s", m.Summary.Labels, l)
				}
			}
			if !bytes.Equal(s.RawMessage(r), body) {
				t.Errorf("RawMessage = %q, want the appended bytes", s.RawMessage(r))
			}
			if got := s.Appended(); len(got) != 1 || !bytes.Equal(got[0], body) {
				t.Errorf("Appended = %q", got)
			}
			if n := listed(t, s, tt.listedIn); n != 1 {
				t.Errorf("%s holds %d messages, want 1", tt.listedIn, n)
			}
			refs, err := s.FindByMessageID(t.Context(), tt.listedIn, "a1@example.com")
			if err != nil || len(refs) != 1 || refs[0] != r {
				t.Errorf("FindByMessageID(%s) = %v, %v, want [%s]", tt.listedIn, refs, err, r)
			}
		})
	}
}

func TestWriteAppendDetails(t *testing.T) {
	t.Parallel()
	s := newHost()
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	s.SetNow(func() time.Time { return now })
	r, err := s.Append(t.Context(), "INBOX", nil, time.Time{}, raw("No date given", "a2@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fetch(t, s, r).Summary.InternalDate; !got.Equal(now) {
		t.Errorf("InternalDate = %v, want the clock's %v when no date is given", got, now)
	}
	r2, err := s.Append(t.Context(), "INBOX", nil, now, raw("second", "a3@example.com"))
	if err != nil || r2.UID != r.UID+1 {
		t.Errorf("second Append = %+v, %v, want UID %d", r2, err, r.UID+1)
	}
	if _, err := s.Append(t.Context(), "INBOX", nil, now, nil); err == nil {
		t.Error("Append of an empty message succeeded")
	}
	for _, flag := range []string{`\Recent`, `\Bogus`, "two words", ""} {
		if _, err := s.Append(t.Context(), "INBOX", []string{flag}, now, raw("x", "x@x")); err == nil {
			t.Errorf("Append with flag %q succeeded", flag)
		}
	}
	notMIME := []byte("just some bytes with no header")
	r3, err := s.Append(t.Context(), "INBOX", nil, now, notMIME)
	if err != nil {
		t.Fatalf("Append of a message without a header: %v", err)
	}
	if !bytes.Equal(s.RawMessage(r3), notMIME) {
		t.Errorf("RawMessage = %q", s.RawMessage(r3))
	}
	got := s.Appended()
	got[0][0] = 'X'
	if s.Appended()[0][0] == 'X' {
		t.Error("Appended shares memory with the store")
	}
}

func TestWriteMove(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		gmail      bool
		from       string
		labels     []string
		dest       string
		wantFolder string
		sameRef    bool
		wantLabels []string
		noLabels   []string
		wantErr    error
	}{
		// Spec 7.1, standard.
		{name: "standard move gives a new ref", from: "INBOX", dest: "Projects", wantFolder: "Projects"},
		{name: "standard move to Trash", from: "INBOX", dest: "Trash", wantFolder: "Trash"},
		// UID MOVE into the same mailbox makes a new message, as Dovecot does
		// (RFC 6851 section 3.3).
		{name: "standard move to its own folder gives a new ref", from: "INBOX", dest: "INBOX", wantFolder: "INBOX"},
		{name: "standard unknown folder", from: "INBOX", dest: "Nope", wantErr: mailstore.ErrNotFound},
		// Spec 7.1, Google.
		{
			name: "Gmail move to a label folder", gmail: true, from: allMail, labels: []string{`\Inbox`}, dest: "Receipts",
			wantFolder: allMail, sameRef: true, wantLabels: []string{"Receipts"}, noLabels: []string{`\Inbox`},
		},
		{
			name: "Gmail move to INBOX", gmail: true, from: allMail, labels: []string{"Receipts"}, dest: "INBOX",
			wantFolder: allMail, sameRef: true, wantLabels: []string{`\Inbox`, "Receipts"},
		},
		{name: "Gmail move to Trash", gmail: true, from: allMail, labels: []string{`\Inbox`}, dest: gmailTrash, wantFolder: gmailTrash},
		{name: "Gmail move to Spam", gmail: true, from: allMail, labels: []string{`\Inbox`}, dest: gmailSpam, wantFolder: gmailSpam},
		{name: "Gmail move from Trash to a label", gmail: true, from: gmailTrash, dest: "Receipts", wantFolder: allMail, wantLabels: []string{"Receipts"}},
		{name: "Gmail move from Spam to INBOX", gmail: true, from: gmailSpam, dest: "INBOX", wantFolder: allMail, wantLabels: []string{`\Inbox`}},
		{name: "Gmail move from Trash to All Mail", gmail: true, from: gmailTrash, dest: allMail, wantFolder: allMail},
		{name: "Gmail move from Spam to Trash", gmail: true, from: gmailSpam, dest: gmailTrash, wantFolder: gmailTrash},
		{name: "Gmail move to its own folder", gmail: true, from: gmailTrash, dest: gmailTrash, wantFolder: gmailTrash, sameRef: true},
		{name: "Gmail move within All Mail", gmail: true, from: allMail, labels: []string{`\Inbox`}, dest: allMail, wantFolder: allMail, sameRef: true, wantLabels: []string{`\Inbox`}},
		{name: "Gmail unknown folder", gmail: true, from: allMail, dest: "Nope", wantErr: mailstore.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newHost()
			if tt.gmail {
				s = newGmail()
			}
			r := s.Add(Message{Folder: tt.from, Subject: "moving", MessageID: "m@x", Labels: tt.labels, Flags: []string{`\Seen`}})
			res := one(t, s.Move(t.Context(), []ref.Ref{r}, tt.dest))
			if tt.wantErr != nil {
				if !errors.Is(res.Err, tt.wantErr) || res.Ref != (ref.Ref{}) {
					t.Fatalf("Move = %+v, want %v", res, tt.wantErr)
				}
				fetch(t, s, r)
				return
			}
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			if res.Ref.Folder != tt.wantFolder {
				t.Fatalf("new ref = %s, want folder %s", res.Ref, tt.wantFolder)
			}
			if tt.sameRef != (res.Ref == r) {
				t.Errorf("new ref = %s, old %s, want same=%v", res.Ref, r, tt.sameRef)
			}
			if !tt.sameRef {
				gone(t, s, r)
			}
			m := fetch(t, s, res.Ref)
			if m.Summary.Subject != "moving" || !slices.Contains(m.Summary.Flags, `\Seen`) {
				t.Errorf("moved message = %+v", m.Summary)
			}
			if tt.gmail && res.Ref.GmailMsgID != r.GmailMsgID {
				t.Errorf("X-GM-MSGID changed from %d to %d", r.GmailMsgID, res.Ref.GmailMsgID)
			}
			for _, l := range tt.wantLabels {
				if !hasLabelIn(m.Summary.Labels, l) {
					t.Errorf("labels = %v, want %s", m.Summary.Labels, l)
				}
			}
			for _, l := range tt.noLabels {
				if hasLabelIn(m.Summary.Labels, l) {
					t.Errorf("labels = %v, want no %s", m.Summary.Labels, l)
				}
			}
			if n := listed(t, s, tt.dest); n != 1 {
				t.Errorf("%s holds %d messages, want 1", tt.dest, n)
			}
		})
	}
}

func hasLabelIn(labels []string, l string) bool {
	return slices.ContainsFunc(labels, func(x string) bool { return strings.EqualFold(x, l) })
}

// TestWriteRefErrors covers what every batch call does with a bad ref.
func TestWriteRefErrors(t *testing.T) {
	t.Parallel()
	calls := map[string]func(s *Store, refs []ref.Ref) []mailstore.RefResult{
		"Move": func(s *Store, refs []ref.Ref) []mailstore.RefResult {
			return s.Move(t.Context(), refs, s.byRole(mailstore.RoleTrash))
		},
		"Copy":    func(s *Store, refs []ref.Ref) []mailstore.RefResult { return s.Copy(t.Context(), refs, "INBOX") },
		"Archive": func(s *Store, refs []ref.Ref) []mailstore.RefResult { return s.Archive(t.Context(), refs) },
		"Trash":   func(s *Store, refs []ref.Ref) []mailstore.RefResult { return s.Trash(t.Context(), refs) },
		"Expunge": func(s *Store, refs []ref.Ref) []mailstore.RefResult { return s.Expunge(t.Context(), refs) },
		"StoreFlags": func(s *Store, refs []ref.Ref) []mailstore.RefResult {
			return s.StoreFlags(t.Context(), refs, []string{`\Seen`}, nil)
		},
		"StoreLabels": func(s *Store, refs []ref.Ref) []mailstore.RefResult {
			return s.StoreLabels(t.Context(), refs, []string{`\Inbox`}, nil)
		},
	}
	for name, call := range calls {
		for _, gmail := range []bool{false, true} {
			if name == "StoreLabels" && !gmail {
				continue // covered by TestWriteStoreLabels: every ref is ErrUnsupported
			}
			t.Run(fmt.Sprintf("%s/gmail=%v", name, gmail), func(t *testing.T) {
				t.Parallel()
				s, home := newHost(), "INBOX"
				if gmail {
					s, home = newGmail(), allMail
				}
				good := s.Add(Message{Folder: home, Subject: "good", Labels: []string{`\Inbox`}})
				missing := s.Add(Message{Folder: home, Subject: "missing"})
				stale := s.Add(Message{Folder: home, Subject: "stale"})
				s.mu.Lock()
				s.msgs = slices.DeleteFunc(s.msgs, func(m *Message) bool { return m.Subject == "missing" })
				s.mu.Unlock()
				stale.UIDValidity = 77
				other := good
				other.Account = "elsewhere"
				noFolder := good
				noFolder.Folder = "Nope"

				res := call(s, []ref.Ref{stale, good, missing, other, noFolder})
				if len(res) != 5 {
					t.Fatalf("results = %d, want 5", len(res))
				}
				want := []error{mailstore.ErrStaleRef, nil, mailstore.ErrStaleRef, mailstore.ErrNotFound, mailstore.ErrNotFound}
				for i, w := range want {
					if w == nil {
						if res[i].Err != nil {
							t.Errorf("good ref: %v", res[i].Err)
						}
						continue
					}
					if !errors.Is(res[i].Err, w) || res[i].Ref != (ref.Ref{}) {
						t.Errorf("result %d = %+v, want %v", i, res[i], w)
					}
				}
				if got := call(s, nil); len(got) != 0 {
					t.Errorf("no refs gave %d results", len(got))
				}
			})
		}
	}
}

func (s *Store) byRole(role mailstore.Role) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byRoleLocked(role)
}

func TestWriteCopy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		gmail      bool
		from       string
		dest       string
		wantFolder string
		sameRef    bool
		wantLabels []string
		wantErr    error
	}{
		{name: "standard copy", from: "INBOX", dest: "Projects", wantFolder: "Projects"},
		{name: "standard copy into its own folder duplicates", from: "INBOX", dest: "INBOX", wantFolder: "INBOX"},
		{name: "standard unknown folder", from: "INBOX", dest: "Nope", wantErr: mailstore.ErrNotFound},
		{name: "Gmail copy adds the label", gmail: true, from: allMail, dest: "Receipts", wantFolder: allMail, sameRef: true, wantLabels: []string{`\Inbox`, "Receipts"}},
		{name: "Gmail copy to INBOX adds \\Inbox", gmail: true, from: allMail, dest: "INBOX", wantFolder: allMail, sameRef: true, wantLabels: []string{`\Inbox`}},
		{name: "Gmail copy to All Mail", gmail: true, from: allMail, dest: allMail, wantFolder: allMail, sameRef: true},
		{name: "Gmail copy to Trash", gmail: true, from: allMail, dest: gmailTrash, wantErr: mailstore.ErrUnsupported},
		{name: "Gmail copy to Spam", gmail: true, from: allMail, dest: gmailSpam, wantErr: mailstore.ErrUnsupported},
		{name: "Gmail unknown folder", gmail: true, from: allMail, dest: "Nope", wantErr: mailstore.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newHost()
			if tt.gmail {
				s = newGmail()
			}
			r := s.Add(Message{Folder: tt.from, Subject: "copied", MessageID: "c@x", Labels: []string{`\Inbox`}})
			res := one(t, s.Copy(t.Context(), []ref.Ref{r}, tt.dest))
			if tt.wantErr != nil {
				if !errors.Is(res.Err, tt.wantErr) {
					t.Fatalf("Copy = %+v, want %v", res, tt.wantErr)
				}
				return
			}
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			if res.Ref.Folder != tt.wantFolder || tt.sameRef != (res.Ref == r) {
				t.Fatalf("copy ref = %s, original %s, want folder %s same=%v", res.Ref, r, tt.wantFolder, tt.sameRef)
			}
			if fetch(t, s, r).Summary.Subject != "copied" || fetch(t, s, res.Ref).Summary.Subject != "copied" {
				t.Error("original or copy unreadable")
			}
			m := fetch(t, s, res.Ref)
			for _, l := range tt.wantLabels {
				if !hasLabelIn(m.Summary.Labels, l) {
					t.Errorf("labels = %v, want %s", m.Summary.Labels, l)
				}
			}
			refs, err := s.FindByMessageID(t.Context(), tt.dest, "c@x")
			if err != nil || !slices.Contains(refs, res.Ref) {
				t.Errorf("FindByMessageID(%s) = %v, %v, want it to hold %s", tt.dest, refs, err, res.Ref)
			}
			if !tt.gmail {
				// The copy is its own message.
				if one(t, s.StoreFlags(t.Context(), []ref.Ref{res.Ref}, []string{`\Flagged`}, nil)).Err != nil {
					t.Fatal("flagging the copy failed")
				}
				if slices.Contains(s.Flags(r), `\Flagged`) {
					t.Error("flagging the copy flagged the original")
				}
			}
		})
	}
}

func TestWriteArchive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		gmail      bool
		from       string
		setup      func(*Store)
		wantFolder string
		sameRef    bool
		wantErr    error
	}{
		{name: "standard to the \\Archive folder", from: "INBOX", wantFolder: "Archive"},
		{
			name: "standard to archive_folder", from: "INBOX", wantFolder: "Old",
			setup: func(s *Store) { s.AddFolder("Old", mailstore.RoleNone); s.SetArchiveFolder("Old") },
		},
		{name: "standard archive_folder missing", from: "INBOX", setup: func(s *Store) { s.SetArchiveFolder("Gone") }, wantErr: mailstore.ErrNotFound},
		{name: "standard neither", from: "INBOX", setup: func(s *Store) { dropRole(s, mailstore.RoleArchive) }, wantErr: mailstore.ErrNotFound},
		{name: "standard already archived moves again", from: "Archive", wantFolder: "Archive"},
		{name: "Gmail drops \\Inbox", gmail: true, from: allMail, wantFolder: allMail, sameRef: true},
		{name: "Gmail ignores archive_folder", gmail: true, from: allMail, setup: func(s *Store) { s.SetArchiveFolder("Receipts") }, wantFolder: allMail, sameRef: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newHost()
			if tt.gmail {
				s = newGmail()
			}
			if tt.setup != nil {
				tt.setup(s)
			}
			r := s.Add(Message{Folder: tt.from, Subject: "archived", Labels: []string{`\Inbox`, "Receipts"}})
			res := s.Archive(t.Context(), []ref.Ref{r})[0]
			if tt.wantErr != nil {
				if !errors.Is(res.Err, tt.wantErr) {
					t.Fatalf("Archive = %+v, want %v", res, tt.wantErr)
				}
				fetch(t, s, r)
				return
			}
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			if res.Ref.Folder != tt.wantFolder || tt.sameRef != (res.Ref == r) {
				t.Fatalf("archived ref = %s, old %s, want folder %s same=%v", res.Ref, r, tt.wantFolder, tt.sameRef)
			}
			if !tt.sameRef {
				gone(t, s, r)
				if n := listed(t, s, tt.wantFolder); n != 1 {
					t.Errorf("%s holds %d messages, want 1", tt.wantFolder, n)
				}
			}
			m := fetch(t, s, res.Ref)
			if tt.gmail {
				if hasLabelIn(m.Summary.Labels, `\Inbox`) || !hasLabelIn(m.Summary.Labels, "Receipts") {
					t.Errorf("labels = %v, want Receipts without \\Inbox", m.Summary.Labels)
				}
				if listed(t, s, "INBOX") != 0 {
					t.Error("archived message still in INBOX")
				}
			}
		})
	}
}

func TestWriteTrash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		gmail      bool
		from       string
		setup      func(*Store)
		wantFolder string
		sameRef    bool
		wantErr    error
	}{
		{name: "standard to the \\Trash folder", from: "INBOX", wantFolder: "Trash"},
		{name: "standard already in Trash", from: "Trash", wantFolder: "Trash", sameRef: true},
		{name: "standard from Junk", from: "Junk", wantFolder: "Trash"},
		{name: "standard no Trash", from: "INBOX", setup: func(s *Store) { dropRole(s, mailstore.RoleTrash) }, wantErr: mailstore.ErrNotFound},
		{name: "Gmail to [Gmail]/Trash", gmail: true, from: allMail, wantFolder: gmailTrash},
		{name: "Gmail from Spam", gmail: true, from: gmailSpam, wantFolder: gmailTrash},
		{name: "Gmail already in Trash", gmail: true, from: gmailTrash, wantFolder: gmailTrash, sameRef: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newHost()
			if tt.gmail {
				s = newGmail()
			}
			if tt.setup != nil {
				tt.setup(s)
			}
			r := s.Add(Message{Folder: tt.from, Subject: "trashed", Labels: []string{`\Inbox`}})
			res := one(t, s.Trash(t.Context(), []ref.Ref{r}))
			if tt.wantErr != nil {
				if !errors.Is(res.Err, tt.wantErr) {
					t.Fatalf("Trash = %+v, want %v", res, tt.wantErr)
				}
				return
			}
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			if res.Ref.Folder != tt.wantFolder || tt.sameRef != (res.Ref == r) {
				t.Fatalf("trashed ref = %s, old %s, want folder %s same=%v", res.Ref, r, tt.wantFolder, tt.sameRef)
			}
			if !tt.sameRef {
				gone(t, s, r)
			}
			fetch(t, s, res.Ref)
			if tt.gmail && (listed(t, s, "INBOX") != 0 || listed(t, s, allMail) != 0) {
				t.Error("a trashed Gmail message still shows in INBOX or All Mail")
			}
		})
	}
}

func TestWriteExpunge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gmail   bool
		from    string
		setup   func(*Store)
		wantErr error
	}{
		{name: "standard", from: "INBOX"},
		{name: "standard in Trash", from: "Trash"},
		{name: "Gmail from All Mail goes through Trash", gmail: true, from: allMail},
		{name: "Gmail in Trash", gmail: true, from: gmailTrash},
		{name: "Gmail from Spam", gmail: true, from: gmailSpam},
		{name: "Gmail without a Trash folder", gmail: true, from: allMail, setup: func(s *Store) { dropRole(s, mailstore.RoleTrash) }, wantErr: mailstore.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newHost()
			if tt.gmail {
				s = newGmail()
			}
			if tt.setup != nil {
				tt.setup(s)
			}
			r := s.Add(Message{Folder: tt.from, Subject: "doomed", MessageID: "d@x", Labels: []string{`\Inbox`}})
			// A message already flagged \Deleted survives: only the given UIDs go.
			keep := s.Add(Message{Folder: tt.from, Subject: "kept", Flags: []string{`\Deleted`}})
			res := one(t, s.Expunge(t.Context(), []ref.Ref{r}))
			if tt.wantErr != nil {
				if !errors.Is(res.Err, tt.wantErr) {
					t.Fatalf("Expunge = %+v, want %v", res, tt.wantErr)
				}
				fetch(t, s, r)
				return
			}
			if res.Err != nil || res.Ref != (ref.Ref{}) {
				t.Fatalf("Expunge = %+v, want a zero ref and no error", res)
			}
			gone(t, s, r)
			fetch(t, s, keep)
			for _, f := range folderNames(t, s) {
				if refs, _ := s.FindByMessageID(t.Context(), f, "d@x"); len(refs) != 0 {
					t.Errorf("expunged message still in %s: %v", f, refs)
				}
			}
			if again := one(t, s.Expunge(t.Context(), []ref.Ref{r})); !errors.Is(again.Err, mailstore.ErrStaleRef) {
				t.Errorf("second Expunge = %v, want ErrStaleRef", again.Err)
			}
		})
	}
}

func TestWriteStoreFlags(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		gmail      bool
		flags      []string
		labels     []string
		add        []string
		remove     []string
		wantFlags  []string
		wantLabels []string
		wantErr    bool
	}{
		{name: "standard add", add: []string{`\Seen`, "$Work"}, wantFlags: []string{`\Seen`, "$Work"}},
		{name: "standard remove", flags: []string{`\Seen`, `\Flagged`}, remove: []string{`\seen`}, wantFlags: []string{`\Flagged`}},
		{name: "standard add twice keeps one", flags: []string{`\Seen`}, add: []string{`\Seen`}, wantFlags: []string{`\Seen`}},
		{name: "standard add and remove", flags: []string{`\Answered`}, add: []string{`\Flagged`}, remove: []string{`\Answered`}, wantFlags: []string{`\Flagged`}},
		{name: "standard \\Recent", add: []string{`\Recent`}, wantErr: true},
		{name: "standard unknown system flag", add: []string{`\Starred`}, wantErr: true},
		{name: "standard bad keyword", remove: []string{"a b"}, wantErr: true},
		{name: "Gmail add", gmail: true, add: []string{`\Seen`}, wantFlags: []string{`\Seen`}},
		{name: "Gmail star", gmail: true, add: []string{`\Flagged`}, wantFlags: []string{`\Flagged`}, wantLabels: []string{`\Starred`}},
		{name: "Gmail unstar", gmail: true, flags: []string{`\Flagged`}, labels: []string{`\Starred`, `\Inbox`}, remove: []string{`\Flagged`}, wantFlags: []string{}, wantLabels: []string{`\Inbox`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, home := newHost(), "INBOX"
			if tt.gmail {
				s, home = newGmail(), allMail
			}
			r := s.Add(Message{Folder: home, Subject: "flagged", Flags: tt.flags, Labels: tt.labels})
			r2 := s.Add(Message{Folder: home, Subject: "second", Flags: tt.flags, Labels: tt.labels})
			res := s.StoreFlags(t.Context(), []ref.Ref{r, r2}, tt.add, tt.remove)
			for i, want := range []ref.Ref{r, r2} {
				if tt.wantErr {
					if res[i].Err == nil {
						t.Errorf("StoreFlags succeeded for ref %d", i)
					}
					if !slices.Equal(s.Flags(want), tt.flags) {
						t.Errorf("a refused StoreFlags changed flags to %v", s.Flags(want))
					}
					continue
				}
				if res[i].Err != nil || res[i].Ref != want {
					t.Fatalf("StoreFlags[%d] = %+v, want %s unchanged", i, res[i], want)
				}
				got := s.Flags(want)
				slices.Sort(got)
				wantFlags := slices.Clone(tt.wantFlags)
				slices.Sort(wantFlags)
				if !slices.Equal(got, wantFlags) && (len(got) != 0 || len(wantFlags) != 0) {
					t.Errorf("flags = %v, want %v", got, wantFlags)
				}
				if tt.gmail {
					labels := fetch(t, s, want).Summary.Labels
					slices.Sort(labels)
					wantLabels := slices.Clone(tt.wantLabels)
					slices.Sort(wantLabels)
					if !slices.Equal(labels, wantLabels) && (len(labels) != 0 || len(wantLabels) != 0) {
						t.Errorf("labels = %v, want %v", labels, wantLabels)
					}
				}
			}
		})
	}
}

func TestWriteStoreLabels(t *testing.T) {
	t.Parallel()
	t.Run("standard is unsupported for every ref", func(t *testing.T) {
		t.Parallel()
		s := newHost()
		r := s.Add(Message{Folder: "INBOX", Subject: "x"})
		bad := r
		bad.UIDValidity = 9
		for i, res := range s.StoreLabels(t.Context(), []ref.Ref{r, bad}, []string{"Projects"}, nil) {
			if !errors.Is(res.Err, mailstore.ErrUnsupported) {
				t.Errorf("result %d = %v, want ErrUnsupported", i, res.Err)
			}
		}
	})
	tests := []struct {
		name       string
		labels     []string
		flags      []string
		add        []string
		remove     []string
		wantLabels []string
		wantFlags  []string
		wantErr    error
	}{
		{name: "add a label", add: []string{"Receipts"}, wantLabels: []string{`\Inbox`, "Receipts"}},
		{name: "remove \\Inbox", remove: []string{`\Inbox`}, wantLabels: []string{}},
		{name: "INBOX names \\Inbox", labels: []string{}, add: []string{"INBOX"}, wantLabels: []string{`\Inbox`}},
		{name: "case-insensitive remove", labels: []string{`\Inbox`, "Receipts"}, remove: []string{"receipts"}, wantLabels: []string{`\Inbox`}},
		{name: "add and remove", add: []string{"Receipts", `\Important`}, remove: []string{`\Inbox`}, wantLabels: []string{"Receipts", `\Important`}},
		{name: "star label sets \\Flagged", add: []string{`\Starred`}, wantLabels: []string{`\Inbox`, `\Starred`}, wantFlags: []string{`\Flagged`}},
		{name: "unstar label clears \\Flagged", labels: []string{`\Starred`}, flags: []string{`\Flagged`}, remove: []string{`\Starred`}, wantLabels: []string{}, wantFlags: []string{}},
		{name: "unknown label", add: []string{"Nope"}, wantErr: mailstore.ErrNotFound},
		{name: "unknown system label", add: []string{`\Bogus`}, wantErr: mailstore.ErrNotFound},
		{name: "All Mail is not a label", add: []string{allMail}, wantErr: mailstore.ErrUnsupported},
		{name: "Trash is not a label", add: []string{gmailTrash}, wantErr: mailstore.ErrUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newGmail()
			labels := tt.labels
			if labels == nil {
				labels = []string{`\Inbox`}
			}
			r := s.Add(Message{Folder: allMail, Subject: "labelled", Labels: labels, Flags: tt.flags})
			res := one(t, s.StoreLabels(t.Context(), []ref.Ref{r}, tt.add, tt.remove))
			if tt.wantErr != nil {
				if !errors.Is(res.Err, tt.wantErr) {
					t.Fatalf("StoreLabels = %+v, want %v", res, tt.wantErr)
				}
				return
			}
			if res.Err != nil || res.Ref != r {
				t.Fatalf("StoreLabels = %+v, want %s unchanged", res, r)
			}
			m := fetch(t, s, r)
			got := slices.Clone(m.Summary.Labels)
			slices.Sort(got)
			want := slices.Clone(tt.wantLabels)
			slices.Sort(want)
			if !slices.Equal(got, want) && (len(got) != 0 || len(want) != 0) {
				t.Errorf("labels = %v, want %v", got, want)
			}
			if tt.wantFlags != nil && !slices.Equal(m.Summary.Flags, tt.wantFlags) && (len(m.Summary.Flags) != 0 || len(tt.wantFlags) != 0) {
				t.Errorf("flags = %v, want %v", m.Summary.Flags, tt.wantFlags)
			}
		})
	}
}

// longName is a folder name too long for a ref to carry.
var longName = strings.Repeat("x", 2100)

func TestWriteCreateFolder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		gmail     bool
		create    string
		wantNames []string
		wantErr   error
	}{
		{name: "standard new folder", create: "Clients", wantNames: []string{"Clients"}},
		{name: "standard parents first", create: "A/B/C", wantNames: []string{"A", "A/B", "A/B/C"}},
		{name: "standard under an existing parent", create: "Projects/2026", wantNames: []string{"Projects", "Projects/2026"}},
		{name: "standard existing", create: "Projects", wantErr: mailstore.ErrExists},
		{name: "standard INBOX in any case", create: "inbox", wantErr: mailstore.ErrExists},
		{name: "standard role folder", create: "Trash", wantErr: mailstore.ErrExists},
		{name: "standard empty name", create: "", wantErr: mailstore.ErrRefused},
		{name: "standard non-ASCII and punctuation", create: "Clients/Café & 100%", wantNames: []string{"Clients", "Clients/Café & 100%"}},
		{name: "standard NUL", create: "Bad\x00Name", wantErr: mailstore.ErrRefused},
		{name: "standard newline", create: "Bad\nName", wantErr: mailstore.ErrRefused},
		{name: "standard DEL", create: "Bad\x7fName", wantErr: mailstore.ErrRefused},
		{name: "standard invalid UTF-8", create: "Bad\xffName", wantErr: mailstore.ErrRefused},
		{name: "standard bad level creates no parent", create: "Fresh/Bad\x01", wantErr: mailstore.ErrRefused},
		{name: "standard too long for a ref", create: "Fresh/" + longName, wantErr: mailstore.ErrRefused},
		{name: "Gmail label", gmail: true, create: "Clients", wantNames: []string{"Clients"}},
		{name: "Gmail nested label", gmail: true, create: "Work/Q1", wantNames: []string{"Work", "Work/Q1"}},
		{name: "Gmail existing label", gmail: true, create: "Receipts", wantErr: mailstore.ErrExists},
		{name: "Gmail [Gmail] folder", gmail: true, create: "[Gmail]/Mine", wantErr: mailstore.ErrRefused},
		{name: "Gmail [Gmail] itself", gmail: true, create: "[Gmail]", wantErr: mailstore.ErrRefused},
		{name: "Gmail non-ASCII label", gmail: true, create: "Café/Reçus", wantNames: []string{"Café", "Café/Reçus"}},
		{name: "Gmail NUL", gmail: true, create: "Bad\x00Name", wantErr: mailstore.ErrRefused},
		{name: "Gmail tab", gmail: true, create: "Bad\tName", wantErr: mailstore.ErrRefused},
		{name: "Gmail invalid UTF-8", gmail: true, create: "Fresh/Bad\xc3", wantErr: mailstore.ErrRefused},
		{name: "Gmail too long for a ref", gmail: true, create: longName, wantErr: mailstore.ErrRefused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newHost()
			if tt.gmail {
				s = newGmail()
			}
			before := folderNames(t, s)
			err := s.CreateFolder(t.Context(), tt.create)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("CreateFolder(%q) = %v, want %v", tt.create, err, tt.wantErr)
				}
				if got := folderNames(t, s); !slices.Equal(got, before) {
					t.Errorf("a refused create changed folders to %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			names := folderNames(t, s)
			for _, n := range tt.wantNames {
				if !slices.Contains(names, n) {
					t.Errorf("folders = %v, want %s", names, n)
				}
			}
			if names[0] != "INBOX" || !slices.IsSortedFunc(names[1:], compareFolders) {
				t.Errorf("folders out of order: %v", names)
			}
			r, err := s.Append(t.Context(), tt.create, nil, time.Now(), raw("new", "n@x"))
			if err != nil {
				t.Fatalf("Append to the new folder: %v", err)
			}
			if back, err := ref.Parse(r.String()); err != nil || back != r {
				t.Errorf("ref %q into the new folder does not round-trip: %v, %v", r.String(), back, err)
			}
			if refs, err := s.FindByMessageID(t.Context(), tt.create, "n@x"); err != nil || len(refs) != 1 || refs[0] != r {
				t.Errorf("FindByMessageID in the new folder = %v, %v", refs, err)
			}
		})
	}
}

func TestWriteRecreatedFolderIsStale(t *testing.T) {
	t.Parallel()
	s := newHost()
	r := s.Add(Message{Folder: "Projects", Subject: "old"})
	if err := s.DeleteFolder(t.Context(), "Projects", true); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateFolder(t.Context(), "Projects"); err != nil {
		t.Fatal(err)
	}
	s.Add(Message{Folder: "Projects", Subject: "new"})
	if err := s.FetchMessages(t.Context(), []ref.Ref{r})[0].Err; !errors.Is(err, mailstore.ErrStaleRef) {
		t.Errorf("ref into a deleted and recreated folder = %v, want ErrStaleRef", err)
	}
	if res := one(t, s.Move(t.Context(), []ref.Ref{r}, "INBOX")); !errors.Is(res.Err, mailstore.ErrStaleRef) {
		t.Errorf("Move of it = %v, want ErrStaleRef", res.Err)
	}
}

func TestWriteRenameFolder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		gmail    bool
		from, to string
		wantErr  error
		// child gives from a child folder even when the rename fails.
		child bool
	}{
		{name: "standard rename", from: "Projects", to: "Work"},
		{name: "standard rename under a new parent", from: "Projects", to: "Old/Projects"},
		{name: "standard missing", from: "Nope", to: "Work", wantErr: mailstore.ErrNotFound},
		{name: "standard onto an existing folder", from: "Projects", to: "Archive", wantErr: mailstore.ErrExists},
		{name: "standard INBOX", from: "INBOX", to: "Old", wantErr: mailstore.ErrRefused},
		{name: "standard role folder", from: "Sent", to: "Sent2", wantErr: mailstore.ErrRefused},
		{name: "standard into its own child", from: "Projects", to: "Projects/Sub", wantErr: mailstore.ErrRefused},
		{name: "standard to a non-ASCII name", from: "Projects", to: "Projets/Résumés"},
		{name: "standard to a NUL", from: "Projects", to: "Bad\x00Name", wantErr: mailstore.ErrRefused},
		{name: "standard to a control byte under a new parent", from: "Projects", to: "Fresh/Bad\x1b", wantErr: mailstore.ErrRefused},
		{name: "standard to invalid UTF-8", from: "Projects", to: "Bad\xfe", wantErr: mailstore.ErrRefused},
		{name: "standard child too long for a ref", from: "Projects", to: longName[:2010], wantErr: mailstore.ErrRefused, child: true},
		{name: "Gmail label rename", gmail: true, from: "Receipts", to: "Bills"},
		{name: "Gmail onto an existing label", gmail: true, from: "Receipts", to: "INBOX", wantErr: mailstore.ErrExists},
		{name: "Gmail role folder", gmail: true, from: gmailSent, to: "Sent", wantErr: mailstore.ErrRefused},
		{name: "Gmail into [Gmail]", gmail: true, from: "Receipts", to: "[Gmail]/Receipts", wantErr: mailstore.ErrRefused},
		{name: "Gmail to a DEL", gmail: true, from: "Receipts", to: "Bad\x7f", wantErr: mailstore.ErrRefused},
		{name: "Gmail to invalid UTF-8 under a new parent", gmail: true, from: "Receipts", to: "Fresh/Bad\x80", wantErr: mailstore.ErrRefused},
		{name: "Gmail too long for a ref", gmail: true, from: "Receipts", to: longName, wantErr: mailstore.ErrRefused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, home := newHost(), "Projects"
			if tt.gmail {
				s, home = newGmail(), allMail
			}
			child := tt.from + "/Child"
			if !tt.gmail && (tt.wantErr == nil || tt.child) {
				s.AddFolder(child, mailstore.RoleNone)
			}
			r := s.Add(Message{Folder: home, Subject: "kept", MessageID: "k@x", Labels: []string{"Receipts"}})
			before := folderNames(t, s)
			err := s.RenameFolder(t.Context(), tt.from, tt.to)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("RenameFolder = %v, want %v", err, tt.wantErr)
				}
				if got := folderNames(t, s); !slices.Equal(got, before) {
					t.Errorf("a refused rename changed folders to %v", got)
				}
				fetch(t, s, r)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			names := folderNames(t, s)
			if slices.Contains(names, tt.from) || !slices.Contains(names, tt.to) {
				t.Errorf("folders = %v, want %s renamed to %s", names, tt.from, tt.to)
			}
			refs, err := s.FindByMessageID(t.Context(), tt.to, "k@x")
			if err != nil || len(refs) != 1 {
				t.Fatalf("FindByMessageID(%s) = %v, %v", tt.to, refs, err)
			}
			if tt.gmail {
				if refs[0] != r {
					t.Errorf("renaming a label changed the All Mail ref: %s, was %s", refs[0], r)
				}
				if labels := fetch(t, s, r).Summary.Labels; !slices.Equal(labels, []string{tt.to}) {
					t.Errorf("labels = %v, want [%s]", labels, tt.to)
				}
				return
			}
			gone(t, s, r)
			if !slices.Contains(names, tt.to+"/Child") || slices.Contains(names, child) {
				t.Errorf("folders = %v, want the child renamed too", names)
			}
			if fetch(t, s, refs[0]).Summary.Subject != "kept" {
				t.Error("renamed folder lost its message")
			}
		})
	}
}

func TestWriteDeleteFolder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gmail   bool
		folder  string
		full    bool
		force   bool
		child   bool
		wantErr error
	}{
		{name: "standard empty", folder: "Projects"},
		{name: "standard with mail", folder: "Projects", full: true, wantErr: mailstore.ErrRefused},
		{name: "standard with mail and force", folder: "Projects", full: true, force: true},
		{name: "standard missing", folder: "Nope", wantErr: mailstore.ErrNotFound},
		{name: "standard INBOX", folder: "INBOX", force: true, wantErr: mailstore.ErrRefused},
		{name: "standard role folder", folder: "Trash", force: true, wantErr: mailstore.ErrRefused},
		{name: "standard with a child", folder: "Projects", child: true, force: true, wantErr: mailstore.ErrRefused},
		{name: "Gmail empty label", gmail: true, folder: "Receipts"},
		{name: "Gmail label with mail", gmail: true, folder: "Receipts", full: true, wantErr: mailstore.ErrRefused},
		{name: "Gmail label with mail and force", gmail: true, folder: "Receipts", full: true, force: true},
		{name: "Gmail INBOX", gmail: true, folder: "INBOX", force: true, wantErr: mailstore.ErrRefused},
		{name: "Gmail role folder", gmail: true, folder: gmailTrash, force: true, wantErr: mailstore.ErrRefused},
		{name: "Gmail [Gmail] folder without a role", gmail: true, folder: "[Gmail]/Important", force: true, wantErr: mailstore.ErrRefused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newHost()
			if tt.gmail {
				s = newGmail()
				s.AddFolder("[Gmail]/Important", mailstore.RoleNone)
			}
			if tt.child {
				s.AddFolder(tt.folder+"/Child", mailstore.RoleNone)
			}
			var r ref.Ref
			if tt.full {
				if tt.gmail {
					r = s.Add(Message{Folder: allMail, Subject: "labelled", Labels: []string{tt.folder, `\Inbox`}})
				} else {
					r = s.Add(Message{Folder: tt.folder, Subject: "inside"})
				}
			}
			before := folderNames(t, s)
			err := s.DeleteFolder(t.Context(), tt.folder, tt.force)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("DeleteFolder = %v, want %v", err, tt.wantErr)
				}
				if got := folderNames(t, s); !slices.Equal(got, before) {
					t.Errorf("a refused delete changed folders to %v", got)
				}
				if tt.full {
					fetch(t, s, r)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(folderNames(t, s), tt.folder) {
				t.Errorf("%s still listed", tt.folder)
			}
			if !tt.full {
				return
			}
			if tt.gmail {
				// Deleting a label keeps the message in All Mail.
				if labels := fetch(t, s, r).Summary.Labels; !slices.Equal(labels, []string{`\Inbox`}) {
					t.Errorf("labels = %v, want [\\Inbox]", labels)
				}
				return
			}
			gone(t, s, r)
		})
	}
}

func TestWriteFindByMessageID(t *testing.T) {
	t.Parallel()
	host := newHost()
	a := host.Add(Message{Folder: "INBOX", Subject: "a", MessageID: "<same@x>"})
	host.Add(Message{Folder: "Projects", Subject: "b", MessageID: "same@x"})
	b := host.Add(Message{Folder: "INBOX", Subject: "c", MessageID: "same@x"})
	host.Add(Message{Folder: "INBOX", Subject: "no id"})

	gmail := newGmail()
	sent := gmail.Add(Message{Folder: allMail, Subject: "sent", MessageID: "s@x", Labels: []string{`\Sent`}})
	gmail.Add(Message{Folder: gmailTrash, Subject: "trashed", MessageID: "s@x"})

	tests := []struct {
		name    string
		s       *Store
		folder  string
		id      string
		want    []ref.Ref
		wantErr error
	}{
		{name: "standard with brackets", s: host, folder: "INBOX", id: "<same@x>", want: []ref.Ref{a, b}},
		{name: "standard without brackets", s: host, folder: "INBOX", id: "same@x", want: []ref.Ref{a, b}},
		{name: "standard no match", s: host, folder: "INBOX", id: "other@x"},
		{name: "standard empty id", s: host, folder: "INBOX", id: "<>"},
		{name: "standard unknown folder", s: host, folder: "Nope", id: "same@x", wantErr: mailstore.ErrNotFound},
		{name: "Gmail Sent Mail gives All Mail refs", s: gmail, folder: gmailSent, id: "s@x", want: []ref.Ref{sent}},
		{name: "Gmail All Mail", s: gmail, folder: allMail, id: "s@x", want: []ref.Ref{sent}},
		{name: "Gmail INBOX does not have it", s: gmail, folder: "INBOX", id: "s@x"},
		{name: "Gmail unknown folder", s: gmail, folder: "Nope", id: "s@x", wantErr: mailstore.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.s.FindByMessageID(t.Context(), tt.folder, tt.id)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("FindByMessageID = %v, want %v", err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("FindByMessageID = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteHooks(t *testing.T) {
	t.Parallel()
	s := newHost()
	r := s.Add(Message{Folder: "INBOX", Subject: "x"})
	refs := []ref.Ref{r}

	s.FailWrite("Move", errBoom)
	if res := s.Move(t.Context(), refs, "Projects"); !errors.Is(res[0].Err, errBoom) {
		t.Errorf("failed Move = %v, want boom", res[0].Err)
	}
	fetch(t, s, r) // a failed Move changes nothing
	s.FailWrite("Append", errBoom)
	if _, err := s.Append(t.Context(), "INBOX", nil, time.Now(), raw("y", "y@x")); !errors.Is(err, errBoom) {
		t.Errorf("failed Append = %v, want boom", err)
	}
	if len(s.Appended()) != 0 {
		t.Error("failed Append recorded")
	}
	s.FailWrite("CreateFolder", errBoom)
	s.FailWrite("CreateFolder", nil)
	if err := s.CreateFolder(t.Context(), "Cleared"); err != nil {
		t.Errorf("FailWrite(nil) did not clear: %v", err)
	}
	s.FailWrite("DeleteFolder", errBoom)
	if err := s.DeleteFolder(t.Context(), "Cleared", false); !errors.Is(err, errBoom) {
		t.Errorf("failed DeleteFolder = %v", err)
	}
	s.FailWrite("FindByMessageID", errBoom)
	if _, err := s.FindByMessageID(t.Context(), "INBOX", "x@x"); !errors.Is(err, errBoom) {
		t.Errorf("failed FindByMessageID = %v", err)
	}

	// The failures were one-shot.
	moved := one(t, s.Move(t.Context(), refs, "Projects"))
	if moved.Err != nil {
		t.Fatalf("Move after a one-shot failure: %v", moved.Err)
	}
	if _, err := s.Append(t.Context(), "INBOX", nil, time.Now(), raw("y", "y@x")); err != nil {
		t.Errorf("Append after a one-shot failure: %v", err)
	}
	if err := s.DeleteFolder(t.Context(), "Cleared", false); err != nil {
		t.Errorf("DeleteFolder after a one-shot failure: %v", err)
	}
	moved2 := []ref.Ref{moved.Ref}
	s.Copy(t.Context(), moved2, "INBOX")
	s.Archive(t.Context(), moved2)
	s.Trash(t.Context(), moved2)
	s.StoreFlags(t.Context(), moved2, []string{`\Seen`}, nil)
	s.StoreLabels(t.Context(), moved2, []string{"x"}, nil)
	_ = s.RenameFolder(t.Context(), "Projects", "Work")
	s.Expunge(t.Context(), moved2)

	want := []string{
		"Move", "Append", "CreateFolder", "DeleteFolder", "FindByMessageID", "Move", "Append", "DeleteFolder",
		"Copy", "Archive", "Trash", "StoreFlags", "StoreLabels", "RenameFolder", "Expunge",
	}
	got := s.Ops()
	if !slices.Equal(got, want) {
		t.Errorf("Ops = %v\nwant  %v", got, want)
	}
	got[0] = "changed"
	if s.Ops()[0] != "Move" {
		t.Error("Ops shares memory with the store")
	}
	if s.Calls("Move") != 2 || s.Calls("Expunge") != 1 {
		t.Errorf("Calls(Move) = %d, Calls(Expunge) = %d", s.Calls("Move"), s.Calls("Expunge"))
	}

	// Fail is a standing failure for writes too.
	s.Fail("StoreFlags", errBoom)
	for range 2 {
		if res := s.StoreFlags(t.Context(), []ref.Ref{r}, []string{`\Seen`}, nil); !errors.Is(res[0].Err, errBoom) {
			t.Errorf("StoreFlags under Fail = %v", res[0].Err)
		}
	}
}

func TestWriteCanceledContext(t *testing.T) {
	t.Parallel()
	s := newHost()
	r := s.Add(Message{Folder: "INBOX", Subject: "x"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if res := one(t, s.Move(ctx, []ref.Ref{r}, "Projects")); !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Move with a canceled context = %v", res.Err)
	}
	if err := s.CreateFolder(ctx, "New"); !errors.Is(err, context.Canceled) {
		t.Errorf("CreateFolder with a canceled context = %v", err)
	}
	fetch(t, s, r)
	if slices.Contains(folderNames(t, s), "New") {
		t.Error("CreateFolder with a canceled context created the folder")
	}
}

// TestWriteConcurrent runs writers and readers together, for -race, and
// checks every result and the state each surviving message ends in.
func TestWriteConcurrent(t *testing.T) {
	t.Parallel()
	for _, gmail := range []bool{false, true} {
		t.Run(fmt.Sprintf("gmail=%v", gmail), func(t *testing.T) {
			t.Parallel()
			s, inbox, archive := newHost(), "INBOX", "Archive"
			if gmail {
				s, archive = newGmail(), allMail
			}
			type survivor struct {
				r        ref.Ref
				archived bool
			}
			const workers, each = 8, 20
			var (
				wg        sync.WaitGroup
				mu        sync.Mutex
				survivors []survivor
			)
			for w := range workers {
				wg.Go(func() {
					for i := range each {
						r, err := s.Append(t.Context(), inbox, nil, time.Now(), raw("c", fmt.Sprintf("%d.%d@x", w, i)))
						if err != nil {
							t.Error(err)
							return
						}
						if res := s.StoreFlags(t.Context(), []ref.Ref{r}, []string{`\Seen`}, nil)[0]; res.Err != nil || res.Ref != r {
							t.Errorf("StoreFlags = %+v, want %s", res, r)
							return
						}
						archived := i%2 == 0
						if archived {
							res := s.Archive(t.Context(), []ref.Ref{r})[0]
							if res.Err != nil || res.Ref.Folder != archive {
								t.Errorf("Archive = %+v, want a ref in %s", res, archive)
								return
							}
							r = res.Ref
						}
						if i%5 == 0 {
							if res := s.Expunge(t.Context(), []ref.Ref{r})[0]; res.Err != nil || res.Ref != (ref.Ref{}) {
								t.Errorf("Expunge = %+v, want a zero ref", res)
								return
							}
						} else {
							mu.Lock()
							survivors = append(survivors, survivor{r: r, archived: archived})
							mu.Unlock()
						}
						if _, err := s.ListFolders(t.Context()); err != nil {
							t.Error(err)
						}
						_ = s.Ops()
						_ = s.Appended()
					}
				})
			}
			wg.Wait()
			if n := len(s.Appended()); n != workers*each {
				t.Errorf("Appended = %d, want %d", n, workers*each)
			}
			want := workers * (each - each/5)
			if len(survivors) != want {
				t.Fatalf("survivors = %d, want %d", len(survivors), want)
			}
			s.mu.Lock()
			left := len(s.msgs)
			s.mu.Unlock()
			if left != want {
				t.Errorf("messages left = %d, want %d", left, want)
			}
			var inInbox uint32
			for _, sv := range survivors {
				m := fetch(t, s, sv.r)
				if !slices.Contains(m.Summary.Flags, `\Seen`) {
					t.Errorf("%s flags = %v, want \\Seen", sv.r, m.Summary.Flags)
				}
				switch {
				case gmail && sv.archived == hasLabelIn(m.Summary.Labels, `\Inbox`):
					t.Errorf("%s labels = %v, archived=%v", sv.r, m.Summary.Labels, sv.archived)
				case !gmail && sv.archived && sv.r.Folder != archive, !gmail && !sv.archived && sv.r.Folder != inbox:
					t.Errorf("%s is in the wrong folder, archived=%v", sv.r, sv.archived)
				}
				if !sv.archived {
					inInbox++
				}
			}
			if n := listed(t, s, inbox); n != inInbox {
				t.Errorf("INBOX holds %d messages, want %d", n, inInbox)
			}
		})
	}
}
