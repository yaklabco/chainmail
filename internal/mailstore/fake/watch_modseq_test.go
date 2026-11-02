package fake

import (
	"slices"
	"testing"
	"time"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// modseqAccount is a standard account with Projects and one INBOX message,
// or a Gmail account with Receipts and Work and one All Mail message
// labelled \Inbox and Work. It returns the store, the folder the message is
// stored in, and its ref.
func modseqAccount(gmail bool) (*Store, string, ref.Ref) {
	if gmail {
		s := newGmail()
		s.AddFolder("Work", mailstore.RoleNone)
		return s, allMail, s.Add(Message{Folder: allMail, Subject: "a", MessageID: "a@x", Labels: []string{`\Inbox`, "Work"}})
	}
	s := newHost()
	return s, "INBOX", s.Add(Message{Folder: "INBOX", Subject: "a"})
}

// noErr fails the test when the only result of a batch call failed.
func noErr(t *testing.T, res []mailstore.RefResult) {
	t.Helper()
	if r := one(t, res); r.Err != nil {
		t.Fatal(r.Err)
	}
}

// TestWatchModSeq checks that every change to a folder raises its
// HIGHESTMODSEQ, that no other folder's moves, and that a change to a
// message's flags or labels puts it in a CHANGEDSINCE pass from before the
// change.
func TestWatchModSeq(t *testing.T) {
	t.Parallel()
	refs := func(r ref.Ref) []ref.Ref { return []ref.Ref{r} }
	tests := []struct {
		name    string
		gmail   bool
		act     func(t *testing.T, s *Store, r ref.Ref)
		grows   []string // folders whose HIGHESTMODSEQ must rise
		same    []string // folders whose HIGHESTMODSEQ must not change
		changed bool     // r is in a CHANGEDSINCE pass of its folder afterwards
	}{
		{
			name:  "add",
			act:   func(_ *testing.T, s *Store, _ ref.Ref) { s.Add(Message{Folder: "INBOX", Subject: "b"}) },
			grows: []string{"INBOX"}, same: []string{"Projects"},
		},
		{
			name: "append",
			act: func(t *testing.T, s *Store, _ ref.Ref) {
				if _, err := s.Append(t.Context(), "Projects", nil, time.Time{}, raw("b", "b@x")); err != nil {
					t.Fatal(err)
				}
			},
			grows: []string{"Projects"}, same: []string{"INBOX"},
		},
		{
			name: "store flags",
			act: func(t *testing.T, s *Store, r ref.Ref) {
				noErr(t, s.StoreFlags(t.Context(), refs(r), []string{`\Seen`}, nil))
			},
			grows: []string{"INBOX"}, same: []string{"Projects"}, changed: true,
		},
		{
			name:  "external flags",
			act:   func(_ *testing.T, s *Store, r ref.Ref) { s.ExternalFlags("INBOX", r.UID, []string{`\Flagged`}, nil) },
			grows: []string{"INBOX"}, same: []string{"Projects"}, changed: true,
		},
		{
			name:  "move",
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Move(t.Context(), refs(r), "Projects")) },
			grows: []string{"INBOX", "Projects"}, same: []string{"Trash"},
		},
		{
			name:  "copy",
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Copy(t.Context(), refs(r), "Projects")) },
			grows: []string{"Projects"}, same: []string{"INBOX"},
		},
		{
			name:  "trash",
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Trash(t.Context(), refs(r))) },
			grows: []string{"INBOX", "Trash"}, same: []string{"Projects"},
		},
		{
			name:  "archive",
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Archive(t.Context(), refs(r))) },
			grows: []string{"INBOX", "Archive"}, same: []string{"Projects"},
		},
		{
			name:  "expunge",
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Expunge(t.Context(), refs(r))) },
			grows: []string{"INBOX"}, same: []string{"Projects", "Trash"},
		},
		{
			name:  "external expunge",
			act:   func(_ *testing.T, s *Store, r ref.Ref) { s.ExternalExpunge("INBOX", r.UID) },
			grows: []string{"INBOX"}, same: []string{"Projects"},
		},
		{
			name: "gmail store flags", gmail: true,
			act: func(t *testing.T, s *Store, r ref.Ref) {
				noErr(t, s.StoreFlags(t.Context(), refs(r), []string{`\Flagged`}, nil))
			},
			grows: []string{allMail}, same: []string{gmailSpam}, changed: true,
		},
		{
			name: "gmail store labels", gmail: true,
			act: func(t *testing.T, s *Store, r ref.Ref) {
				noErr(t, s.StoreLabels(t.Context(), refs(r), []string{"Receipts"}, nil))
			},
			grows: []string{allMail}, same: []string{gmailSpam}, changed: true,
		},
		{
			name: "gmail archive", gmail: true,
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Archive(t.Context(), refs(r))) },
			grows: []string{allMail}, same: []string{gmailTrash}, changed: true,
		},
		{
			name: "gmail move to a label", gmail: true,
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Move(t.Context(), refs(r), "Receipts")) },
			grows: []string{allMail}, same: []string{gmailTrash}, changed: true,
		},
		{
			// Moving from INBOX to All Mail drops the \Inbox label.
			name: "gmail move out of a label", gmail: true,
			act: func(t *testing.T, s *Store, _ ref.Ref) {
				in, err := s.FindByMessageID(t.Context(), "INBOX", "a@x")
				if err != nil || len(in) != 1 {
					t.Fatalf("FindByMessageID = %v, %v", in, err)
				}
				noErr(t, s.Move(t.Context(), in, allMail))
			},
			grows: []string{allMail}, same: []string{gmailTrash}, changed: true,
		},
		{
			name: "gmail copy to a label", gmail: true,
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Copy(t.Context(), refs(r), "Receipts")) },
			grows: []string{allMail}, same: []string{gmailTrash}, changed: true,
		},
		{
			name: "gmail trash", gmail: true,
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Trash(t.Context(), refs(r))) },
			grows: []string{allMail, gmailTrash}, same: []string{gmailSpam},
		},
		{
			name: "gmail expunge", gmail: true,
			act:   func(t *testing.T, s *Store, r ref.Ref) { noErr(t, s.Expunge(t.Context(), refs(r))) },
			grows: []string{allMail, gmailTrash}, same: []string{gmailSpam},
		},
		{
			name: "gmail delete label", gmail: true,
			act: func(t *testing.T, s *Store, _ ref.Ref) {
				if err := s.DeleteFolder(t.Context(), "Work", true); err != nil {
					t.Fatal(err)
				}
			},
			grows: []string{allMail}, same: []string{gmailTrash}, changed: true,
		},
		{
			name: "gmail rename label", gmail: true,
			act: func(t *testing.T, s *Store, _ ref.Ref) {
				if _, err := s.RenameFolder(t.Context(), "Work", "Jobs"); err != nil {
					t.Fatal(err)
				}
			},
			grows: []string{allMail}, same: []string{gmailTrash}, changed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, folder, r := modseqAccount(tt.gmail)
			start := folderState(t, s, folder)
			before := map[string]uint64{}
			for _, f := range slices.Concat(tt.grows, tt.same) {
				before[f] = folderState(t, s, f).HighestModSeq
			}
			tt.act(t, s, r)
			for _, f := range tt.grows {
				if got := folderState(t, s, f).HighestModSeq; got <= before[f] {
					t.Errorf("%s: HIGHESTMODSEQ %d, want above %d", f, got, before[f])
				}
			}
			for _, f := range tt.same {
				if got := folderState(t, s, f).HighestModSeq; got != before[f] {
					t.Errorf("%s: HIGHESTMODSEQ %d, want %d unchanged", f, got, before[f])
				}
			}
			d, err := s.SyncFolder(t.Context(), folder, mailstore.SyncRequest{From: cp(start.UIDValidity, start.UIDNext, start.HighestModSeq)})
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.ContainsFunc(d.Changed, func(f mailstore.FlagState) bool { return f.UID == r.UID }); got != tt.changed {
				t.Errorf("message in CHANGEDSINCE = %v, want %v (changed %+v)", got, tt.changed, d.Changed)
			}
		})
	}
}

// TestWatchRenameKeepsModSeq checks that a renamed folder keeps its
// HIGHESTMODSEQ along with its UIDVALIDITY and UIDNEXT, as on a server.
func TestWatchRenameKeepsModSeq(t *testing.T) {
	t.Parallel()
	s := newHost()
	s.Add(Message{Folder: "Projects", Subject: "a"})
	before := folderState(t, s, "Projects")
	if _, err := s.RenameFolder(t.Context(), "Projects", "Work"); err != nil {
		t.Fatal(err)
	}
	if got := folderState(t, s, "Work"); got != before {
		t.Errorf("Work = %+v, want %+v", got, before)
	}
}
