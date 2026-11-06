package fake

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
)

// seedWatch is a standard account with a Projects folder and three INBOX
// messages, UIDs 1 to 3, with subjects a to c and bodies "body a" to
// "body c". INBOX is at UIDNEXT 4 and HIGHESTMODSEQ 4.
func seedWatch(t *testing.T) (*Store, []ref.Ref) {
	t.Helper()
	s := newHost()
	var refs []ref.Ref
	for i, subj := range []string{"a", "b", "c"} {
		refs = append(refs, s.Add(Message{Folder: "INBOX", Date: day(i + 1), Subject: subj, Body: "body " + subj}))
	}
	return s, refs
}

// folderState returns what FolderState reports for folder, or fails the test.
func folderState(t *testing.T, s *Store, folder string) mailstore.FolderState {
	t.Helper()
	st, err := s.FolderState(t.Context(), folder)
	if err != nil {
		t.Fatalf("FolderState(%s): %v", folder, err)
	}
	return st
}

// cp is a checkpoint.
func cp(v, next uint32, modseq uint64) mailstore.Checkpoint {
	return mailstore.Checkpoint{UIDValidity: v, UIDNext: next, HighestModSeq: modseq}
}

// passView is what a pass test compares: a Delta with each new summary
// reduced to its UID.
type passView struct {
	State     mailstore.FolderState
	Rebased   bool
	New       []uint32
	More      bool
	Changed   []mailstore.FlagState
	FullFlags bool
	UIDs      []uint32
}

func viewOf(d mailstore.Delta) passView {
	v := passView{State: d.State, Rebased: d.Rebased, More: d.More, Changed: d.Changed, FullFlags: d.FullFlags, UIDs: d.UIDs}
	for _, sum := range d.New {
		v.New = append(v.New, sum.Ref.UID)
	}
	return v
}

// TestWatchSyncFolder checks each step of a pass, with and without
// CONDSTORE, on seedWatch's INBOX.
func TestWatchSyncFolder(t *testing.T) {
	t.Parallel()
	cs := mailstore.FolderState{UIDValidity: 1, UIDNext: 4, HighestModSeq: 4, CondStore: true}
	plain := mailstore.FolderState{UIDValidity: 1, UIDNext: 4}
	all := []uint32{1, 2, 3}
	noFlags := []mailstore.FlagState{{UID: 1}, {UID: 2}, {UID: 3}}
	seen2 := []mailstore.FlagState{{UID: 1}, {UID: 2, Flags: []string{`\Seen`}}, {UID: 3}}
	tests := []struct {
		name  string
		plain bool // the server has no CONDSTORE
		act   func(s *Store)
		req   mailstore.SyncRequest
		want  passView
	}{
		{name: "baseline", want: passView{State: cs, Rebased: true, UIDs: all}},
		{
			name: "baseline without condstore", plain: true,
			want: passView{State: plain, Rebased: true, Changed: noFlags, FullFlags: true, UIDs: all},
		},
		{
			name: "uidvalidity changed",
			act:  func(s *Store) { s.SetUIDValidity("INBOX", 7) },
			req:  mailstore.SyncRequest{From: cp(1, 4, 4)},
			want: passView{State: mailstore.FolderState{UIDValidity: 7, UIDNext: 4, HighestModSeq: 4, CondStore: true}, Rebased: true, UIDs: all},
		},
		{name: "current", req: mailstore.SyncRequest{From: cp(1, 4, 4)}, want: passView{State: cs}},
		{
			// UIDs 2 and 3 have MODSEQs above 2 but are new, not changed.
			name: "new mail", req: mailstore.SyncRequest{From: cp(1, 2, 2)},
			want: passView{State: cs, New: []uint32{2, 3}},
		},
		{
			name: "batch", req: mailstore.SyncRequest{From: cp(1, 1, 1), Batch: 2},
			want: passView{State: cs, New: []uint32{1, 2}, More: true},
		},
		{
			name: "flag change",
			act:  func(s *Store) { s.ExternalFlags("INBOX", 2, []string{`\Seen`}, nil) },
			req:  mailstore.SyncRequest{From: cp(1, 4, 4)},
			want: passView{
				State:   mailstore.FolderState{UIDValidity: 1, UIDNext: 4, HighestModSeq: 5, CondStore: true},
				Changed: []mailstore.FlagState{{UID: 2, Flags: []string{`\Seen`}}},
			},
		},
		{
			name: "flags without condstore", plain: true,
			act:  func(s *Store) { s.ExternalFlags("INBOX", 2, []string{`\Seen`}, nil) },
			req:  mailstore.SyncRequest{From: cp(1, 4, 0)},
			want: passView{State: plain, Changed: seen2, FullFlags: true, UIDs: all},
		},
		{
			name: "new mail without condstore", plain: true, req: mailstore.SyncRequest{From: cp(1, 3, 0)},
			want: passView{State: plain, New: []uint32{3}, Changed: noFlags, FullFlags: true, UIDs: all},
		},
		{
			name: "expunge without a sweep",
			act:  func(s *Store) { s.ExternalExpunge("INBOX", 2) },
			req:  mailstore.SyncRequest{From: cp(1, 4, 4)},
			want: passView{State: mailstore.FolderState{UIDValidity: 1, UIDNext: 4, HighestModSeq: 5, CondStore: true}},
		},
		{
			name: "sweep",
			act:  func(s *Store) { s.ExternalExpunge("INBOX", 2) },
			req:  mailstore.SyncRequest{From: cp(1, 4, 4), Sweep: true},
			want: passView{State: mailstore.FolderState{UIDValidity: 1, UIDNext: 4, HighestModSeq: 5, CondStore: true}, UIDs: []uint32{1, 3}},
		},
		{
			// An empty set asked for is empty, not nil, so a caller can
			// tell "every message is gone" from "not asked".
			name: "sweep of an emptied folder",
			act: func(s *Store) {
				for uid := range uint32(3) {
					s.ExternalExpunge("INBOX", uid+1)
				}
			},
			req:  mailstore.SyncRequest{From: cp(1, 4, 4), Sweep: true},
			want: passView{State: mailstore.FolderState{UIDValidity: 1, UIDNext: 4, HighestModSeq: 7, CondStore: true}, UIDs: []uint32{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s, _ := seedWatch(t)
			s.SetCondStore(!tt.plain)
			if tt.act != nil {
				tt.act(s)
			}
			d, err := s.SyncFolder(t.Context(), "INBOX", tt.req)
			if err != nil {
				t.Fatal(err)
			}
			if got := viewOf(d); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("pass =\n%+v\nwant\n%+v", got, tt.want)
			}
		})
	}
}

// TestWatchSyncDefaultBatch checks that Batch 0 means DefaultSyncBatch.
func TestWatchSyncDefaultBatch(t *testing.T) {
	t.Parallel()
	s := New("host", false)
	for range mailstore.DefaultSyncBatch + 1 {
		s.Add(Message{Folder: "INBOX", Subject: "x"})
	}
	d, err := s.SyncFolder(t.Context(), "INBOX", mailstore.SyncRequest{From: cp(1, 1, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.New) != mailstore.DefaultSyncBatch || !d.More || d.New[len(d.New)-1].Ref.UID != mailstore.DefaultSyncBatch {
		t.Errorf("pass = %d new, more %v, want %d and more", len(d.New), d.More, mailstore.DefaultSyncBatch)
	}
}

// TestWatchSyncSnippets checks that new summaries are search summaries,
// with a snippet sample only when asked for.
func TestWatchSyncSnippets(t *testing.T) {
	t.Parallel()
	s, refs := seedWatch(t)
	req := mailstore.SyncRequest{From: cp(1, 3, 3)}
	d, err := s.SyncFolder(t.Context(), "INBOX", req)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.New) != 1 || d.New[0].Ref != refs[2] || d.New[0].Subject != "c" || d.New[0].Sample != nil {
		t.Fatalf("new = %+v, want c without a snippet", d.New)
	}
	req.Snippets = true
	if d, err = s.SyncFolder(t.Context(), "INBOX", req); err != nil {
		t.Fatal(err)
	}
	if sum := d.New[0]; sum.Sample == nil || string(sum.Sample.Data) != "body c" || sum.Sample.MediaType != "text/plain" {
		t.Errorf("sample = %+v, want the body of c", sum.Sample)
	}
}

// TestWatchSyncGmail checks Gmail passes, with and without CONDSTORE: a
// rebase lists every message's X-GM-MSGID and labels in Changed, with
// FullFlags only without CONDSTORE; new summaries carry them too; and label
// folders have no passes.
func TestWatchSyncGmail(t *testing.T) {
	t.Parallel()
	for _, condstore := range []bool{true, false} {
		t.Run(map[bool]string{true: "condstore", false: "plain"}[condstore], func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			s := newGmail()
			s.SetCondStore(condstore)
			r := s.Add(Message{Folder: allMail, Subject: "a", Labels: []string{`\Inbox`}})
			d, err := s.SyncFolder(ctx, allMail, mailstore.SyncRequest{})
			if err != nil {
				t.Fatal(err)
			}
			// With CONDSTORE a rebase fetches (UID X-GM-MSGID) alone, as
			// imapstore does: identifiers, no flags or labels.
			want := []mailstore.FlagState{{UID: r.UID, GmailMsgID: r.GmailMsgID}}
			if !condstore {
				want[0].Labels = []string{`\Inbox`}
			}
			if !d.Rebased || d.FullFlags != !condstore || !reflect.DeepEqual(d.Changed, want) || !slices.Equal(d.UIDs, []uint32{r.UID}) {
				t.Errorf("baseline = %+v, want Changed %+v, FullFlags %v and UIDs [%d]", d, want, !condstore, r.UID)
			}
			r2 := s.Add(Message{Folder: allMail, Subject: "b", Labels: []string{"Receipts"}})
			d, err = s.SyncFolder(ctx, allMail, mailstore.SyncRequest{From: cp(d.State.UIDValidity, d.State.UIDNext, d.State.HighestModSeq)})
			if err != nil {
				t.Fatal(err)
			}
			if len(d.New) != 1 || d.New[0].Ref != r2 || r2.GmailMsgID == 0 || !slices.Equal(d.New[0].Labels, []string{"Receipts"}) {
				t.Errorf("new = %+v, want b with its X-GM-MSGID and labels", d.New)
			}
			if condstore && (d.FullFlags || d.Changed != nil) {
				t.Errorf("a CONDSTORE pass after the rebase gave Changed %+v, FullFlags %v; want neither", d.Changed, d.FullFlags)
			}
		})
	}
	ctx := t.Context()
	s := newGmail()
	for _, f := range []string{"INBOX", "Receipts"} {
		if _, err := s.SyncFolder(ctx, f, mailstore.SyncRequest{}); !errors.Is(err, mailstore.ErrUnsupported) {
			t.Errorf("SyncFolder(%s) = %v, want ErrUnsupported", f, err)
		}
		if _, err := s.FolderState(ctx, f); !errors.Is(err, mailstore.ErrUnsupported) {
			t.Errorf("FolderState(%s) = %v, want ErrUnsupported", f, err)
		}
	}
}

func TestWatchFolderState(t *testing.T) {
	t.Parallel()
	s, _ := seedWatch(t)
	if got, want := folderState(t, s, "INBOX"), (mailstore.FolderState{UIDValidity: 1, UIDNext: 4, HighestModSeq: 4, CondStore: true}); got != want {
		t.Errorf("INBOX = %+v, want %+v", got, want)
	}
	if got, want := folderState(t, s, "Projects"), (mailstore.FolderState{UIDValidity: 1, UIDNext: 1, HighestModSeq: 1, CondStore: true}); got != want {
		t.Errorf("Projects = %+v, want %+v", got, want)
	}
	// A folder without mod-sequences (NOMODSEQ) reports no CONDSTORE and
	// syncs as a server without it does.
	s.mu.Lock()
	s.modseq["Projects"] = 0
	s.mu.Unlock()
	if got, want := folderState(t, s, "Projects"), (mailstore.FolderState{UIDValidity: 1, UIDNext: 1}); got != want {
		t.Errorf("Projects without mod-sequences = %+v, want %+v", got, want)
	}
	if d, err := s.SyncFolder(t.Context(), "Projects", mailstore.SyncRequest{From: cp(1, 1, 0)}); err != nil || !d.FullFlags || d.UIDs == nil || d.State.CondStore {
		t.Errorf("pass without mod-sequences = %+v, %v; want the full-flags path", d, err)
	}
	s.SetCondStore(false)
	if got, want := folderState(t, s, "INBOX"), (mailstore.FolderState{UIDValidity: 1, UIDNext: 4}); got != want {
		t.Errorf("INBOX without CONDSTORE = %+v, want %+v", got, want)
	}

	var nf *mailstore.FolderNotFoundError
	if _, err := s.FolderState(t.Context(), "Nope"); !errors.As(err, &nf) || nf.Requested != "Nope" {
		t.Errorf("FolderState(Nope) = %v, want a FolderNotFoundError", err)
	}
	nf = nil
	if _, err := s.SyncFolder(t.Context(), "Nope", mailstore.SyncRequest{}); !errors.As(err, &nf) || nf.Requested != "Nope" {
		t.Errorf("SyncFolder(Nope) = %v, want a FolderNotFoundError", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.FolderState(ctx, "INBOX"); !errors.Is(err, context.Canceled) {
		t.Errorf("FolderState with a canceled context = %v", err)
	}
	if _, err := s.SyncFolder(ctx, "INBOX", mailstore.SyncRequest{}); !errors.Is(err, context.Canceled) {
		t.Errorf("SyncFolder with a canceled context = %v", err)
	}
	if n := s.Calls("FolderState"); n != 6 {
		t.Errorf("Calls(FolderState) = %d, want 6", n)
	}
	if n := s.Calls("SyncFolder"); n != 3 {
		t.Errorf("Calls(SyncFolder) = %d, want 3", n)
	}
}

func TestWatchFailures(t *testing.T) {
	t.Parallel()
	s, _ := seedWatch(t)
	ctx := t.Context()
	pass := func() error {
		_, err := s.SyncFolder(ctx, "INBOX", mailstore.SyncRequest{})
		return err
	}
	s.Fail("SyncFolder", errBoom)
	for range 2 {
		if err := pass(); !errors.Is(err, errBoom) {
			t.Errorf("SyncFolder under Fail = %v, want boom", err)
		}
	}
	s.Fail("SyncFolder", nil)
	s.Fail("FolderState", errBoom)
	if _, err := s.FolderState(ctx, "INBOX"); !errors.Is(err, errBoom) {
		t.Errorf("FolderState under Fail = %v, want boom", err)
	}
	s.Fail("FolderState", nil)

	s.FailWatch(errBoom)
	if _, err := s.FolderState(ctx, "INBOX"); err != nil {
		t.Errorf("FolderState took the FailWatch: %v", err)
	}
	if err := pass(); !errors.Is(err, errBoom) {
		t.Errorf("SyncFolder after FailWatch = %v, want boom", err)
	}
	if err := pass(); err != nil {
		t.Errorf("FailWatch was not one-shot: %v", err)
	}
	s.FailWatch(errBoom)
	s.FailWatch(nil)
	if err := pass(); err != nil {
		t.Errorf("FailWatch(nil) did not clear: %v", err)
	}
}

// TestWatchExternalChanges checks the other-client helpers: they change the
// message, record no Ops, and panic on a message that is not there.
func TestWatchExternalChanges(t *testing.T) {
	t.Parallel()
	s, refs := seedWatch(t)
	s.ExternalFlags("INBOX", refs[0].UID, []string{`\Seen`, "$Work"}, nil)
	s.ExternalFlags("INBOX", refs[0].UID, nil, []string{"$work"})
	if got := s.Flags(refs[0]); !slices.Equal(got, []string{`\Seen`}) {
		t.Errorf("flags = %v, want [\\Seen]", got)
	}
	s.ExternalExpunge("INBOX", refs[1].UID)
	gone(t, s, refs[1])
	if ops := s.Ops(); len(ops) != 0 {
		t.Errorf("external changes recorded Ops %v", ops)
	}
	mustPanic(t, "ExternalFlags on a missing UID", func() { s.ExternalFlags("INBOX", 99, []string{`\Seen`}, nil) })
	mustPanic(t, "ExternalExpunge in a missing folder", func() { s.ExternalExpunge("Nope", 1) })

	g := newGmail()
	r := g.Add(Message{Folder: allMail, Subject: "g"})
	g.ExternalFlags(allMail, r.UID, []string{`\Flagged`}, nil)
	if m := fetch(t, g, r); !slices.Contains(m.Summary.Labels, `\Starred`) {
		t.Errorf("labels = %v, want \\Starred to follow \\Flagged", m.Summary.Labels)
	}
}

// TestWatchNewUIDValidity checks that a rebuilt folder has a new
// UIDVALIDITY and its messages new UIDs from 1, so a pass rebases.
func TestWatchNewUIDValidity(t *testing.T) {
	t.Parallel()
	s, refs := seedWatch(t)
	s.ExternalExpunge("INBOX", refs[0].UID)
	s.NewUIDValidity("INBOX")
	d, err := s.SyncFolder(t.Context(), "INBOX", mailstore.SyncRequest{From: cp(1, 4, 5)})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Rebased || d.State.UIDValidity != 2 || d.State.UIDNext != 3 || !slices.Equal(d.UIDs, []uint32{1, 2}) {
		t.Errorf("pass after NewUIDValidity = %+v, want a rebase under UIDVALIDITY 2 with UIDs [1 2]", d)
	}
	gone(t, s, refs[1])
	mustPanic(t, "NewUIDValidity on a missing folder", func() { s.NewUIDValidity("Nope") })
}
