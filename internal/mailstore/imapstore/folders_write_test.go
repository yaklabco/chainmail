package imapstore

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/imaptest"
)

// folderNames lists the account's folders, sorted.
func folderNames(t *testing.T, s *Store) []string {
	t.Helper()
	folders, err := s.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(folders))
	for _, f := range folders {
		out = append(out, f.Name)
	}
	slices.Sort(out)
	return out
}

func hasAll(names []string, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(names, w) {
			return false
		}
	}
	return true
}

// memFolders is a standard account on the in-memory server with Trash and
// Work, one message in Work, and an empty Empty.
func memFolders(t *testing.T) *Store {
	t.Helper()
	return memStore(t, imaptest.User{
		Username: "folders@example.test", Password: "pw",
		Folders: []string{"Trash", "Work", "Empty", "Parent", "Parent/Child"},
		Mail:    map[string][]string{"Work": {memMessage}},
	}, Options{})
}

func TestCreateFolderMakesParentsFirst(t *testing.T) {
	t.Parallel()
	s := memFolders(t)
	if _, err := s.CreateFolder(t.Context(), "A/B/C"); err != nil {
		t.Fatal(err)
	}
	if got := folderNames(t, s); !hasAll(got, "A", "A/B", "A/B/C") {
		t.Errorf("folders = %q, want A, A/B and A/B/C", got)
	}
	if _, err := s.CreateFolder(t.Context(), "A/B/D"); err != nil {
		t.Errorf("creating beside an existing parent: %v", err)
	}
	for _, name := range []string{"Work", "inbox", "A/B"} {
		if _, err := s.CreateFolder(t.Context(), name); !errors.Is(err, mailstore.ErrExists) {
			t.Errorf("CreateFolder(%q) = %v, want ErrExists", name, err)
		}
	}
	for _, name := range []string{"", "A//B", "/A", "A/", "bad\x01name"} {
		if _, err := s.CreateFolder(t.Context(), name); !errors.Is(err, mailstore.ErrRefused) {
			t.Errorf("CreateFolder(%q) = %v, want ErrRefused", name, err)
		}
	}
}

func TestCreateFolderUsesTheServersDelimiter(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{delim: ".", folders: []*wsFolder{wsMailbox("INBOX", "", 7), wsMailbox("A", "", 8)}})
	s := writeStore(t, srv, Options{})
	if _, err := s.CreateFolder(t.Context(), "A.B.C"); err != nil {
		t.Fatal(err)
	}
	var creates []string
	for _, c := range srv.received() {
		if strings.HasPrefix(c, "CREATE") {
			creates = append(creates, c)
		}
	}
	if want := []string{`CREATE "A.B"`, `CREATE "A.B.C"`}; !slices.Equal(creates, want) {
		t.Errorf("creates = %q, want %q: the missing parent first, split on the server's delimiter", creates, want)
	}
}

func TestRenameFolder(t *testing.T) {
	t.Parallel()
	s := memFolders(t)
	if _, err := s.RenameFolder(t.Context(), "Work", "Old/Work"); err != nil {
		t.Fatal(err)
	}
	got := folderNames(t, s)
	if !hasAll(got, "Old", "Old/Work") || slices.Contains(got, "Work") {
		t.Errorf("folders = %q, want Work renamed under a new Old", got)
	}
	for _, tc := range []struct {
		from, to string
		want     error
	}{
		{"INBOX", "Inbox2", mailstore.ErrRefused},
		{"Trash", "Bin2", mailstore.ErrRefused},
		{"Nope", "Other", mailstore.ErrNotFound},
		{"Empty", "Old/Work", mailstore.ErrExists},
		{"Empty", "Empty/Below", mailstore.ErrRefused},
		{"Empty", "", mailstore.ErrRefused},
	} {
		if _, err := s.RenameFolder(t.Context(), tc.from, tc.to); !errors.Is(err, tc.want) {
			t.Errorf("RenameFolder(%q, %q) = %v, want %v", tc.from, tc.to, err, tc.want)
		}
	}
}

func TestDeleteFolder(t *testing.T) {
	t.Parallel()
	s := memFolders(t)
	for _, tc := range []struct {
		name string
		want error
	}{
		{"INBOX", mailstore.ErrRefused},
		{"Trash", mailstore.ErrRefused},
		{"Work", mailstore.ErrRefused},   // holds mail
		{"Parent", mailstore.ErrRefused}, // has a child
		{"Nope", mailstore.ErrNotFound},
	} {
		if err := s.DeleteFolder(t.Context(), tc.name, false); !errors.Is(err, tc.want) {
			t.Errorf("DeleteFolder(%q) = %v, want %v", tc.name, err, tc.want)
		}
	}
	if got := folderNames(t, s); !hasAll(got, "INBOX", "Trash", "Work", "Parent") {
		t.Errorf("folders = %q after refusals", got)
	}
	if err := s.DeleteFolder(t.Context(), "Empty", false); err != nil {
		t.Errorf("deleting an empty folder: %v", err)
	}
	if err := s.DeleteFolder(t.Context(), "Work", true); err != nil {
		t.Errorf("deleting a folder with mail, with force: %v", err)
	}
	if err := s.DeleteFolder(t.Context(), "Trash", true); !errors.Is(err, mailstore.ErrRefused) {
		t.Errorf("force deleting Trash = %v, want ErrRefused", err)
	}
	if got := folderNames(t, s); slices.Contains(got, "Empty") || slices.Contains(got, "Work") {
		t.Errorf("folders = %q, want Empty and Work gone", got)
	}
}

// TestRenameMakesConnectionsOpenTheFolderAgain checks that a connection
// that had a folder open before it was renamed does not take the folder of
// the same name created afterwards to be already open.
func TestRenameMakesConnectionsOpenTheFolderAgain(t *testing.T) {
	t.Parallel()
	s := memFolders(t) // one connection, so every call shares it
	if got := s.mustCount(t, "in:Work"); got != 1 {
		t.Fatalf("Work holds %d messages, want 1", got)
	}
	if _, err := s.RenameFolder(t.Context(), "Work", "Play"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFolder(t.Context(), "Work"); err != nil {
		t.Fatal(err)
	}
	if got := s.mustCount(t, "in:Work"); got != 0 {
		t.Errorf("the new Work holds %d messages, want 0: the search ran in the renamed folder", got)
	}
}

func (s *Store) mustCount(t *testing.T, q string) int {
	t.Helper()
	tree, in := searchTree(t, q)
	res, err := s.Search(t.Context(), mailstore.SearchRequest{Query: tree, In: in})
	if err != nil {
		t.Fatalf("Search(%q): %v", q, err)
	}
	n := 0
	for _, r := range res {
		if r.Err != nil {
			t.Fatalf("Search(%q) in %s: %v", q, r.Source.Folder, r.Err)
		}
		n += len(r.UIDs)
	}
	return n
}

func TestGmailFolders(t *testing.T) {
	t.Parallel()
	w := newGmailWrites(t)
	s := w.store
	ctx := t.Context()

	if _, err := s.CreateFolder(ctx, "Projects/2026"); err != nil {
		t.Fatal(err)
	}
	if got := folderNames(t, s); !hasAll(got, "Projects", "Projects/2026") {
		t.Errorf("folders = %q, want the label and its parent", got)
	}
	for _, tc := range []struct {
		name string
		want error
	}{
		{"receipts", mailstore.ErrExists}, // Gmail labels ignore case
		{"INBOX", mailstore.ErrExists},
		{"[Gmail]/Stuff", mailstore.ErrRefused},
		{"[Gmail]", mailstore.ErrRefused},
	} {
		if _, err := s.CreateFolder(ctx, tc.name); !errors.Is(err, tc.want) {
			t.Errorf("CreateFolder(%q) = %v, want %v", tc.name, err, tc.want)
		}
	}

	if _, err := s.RenameFolder(ctx, "Work", "Jobs/Work"); err != nil {
		t.Fatal(err)
	}
	if got := w.labels(t, gwWork, allMail); !slices.Equal(got, []string{"Jobs/Work"}) {
		t.Errorf("labels = %q, want the renamed label", got)
	}
	if got := folderNames(t, s); !hasAll(got, "Jobs", "Jobs/Work") || slices.Contains(got, "Work") {
		t.Errorf("folders = %q, want Work renamed under a new Jobs", got)
	}
	for _, tc := range []struct {
		from, to string
		want     error
	}{
		{"INBOX", "Box", mailstore.ErrRefused},
		{gmailTrash, "Bin", mailstore.ErrRefused},
		{"[Gmail]/Sent Mail", "Sent", mailstore.ErrRefused},
		{"Receipts", "[Gmail]/Receipts", mailstore.ErrRefused},
		{"Receipts", "jobs/work", mailstore.ErrExists},
		{"Nope", "Other", mailstore.ErrNotFound},
	} {
		if _, err := s.RenameFolder(ctx, tc.from, tc.to); !errors.Is(err, tc.want) {
			t.Errorf("RenameFolder(%q, %q) = %v, want %v", tc.from, tc.to, err, tc.want)
		}
	}

	for _, tc := range []struct {
		name string
		want error
	}{
		{"INBOX", mailstore.ErrRefused},
		{gmailSpam, mailstore.ErrRefused},
		{allMail, mailstore.ErrRefused},
		{"Receipts", mailstore.ErrRefused}, // a message carries it
		{"Projects", mailstore.ErrRefused}, // Projects/2026 is under it
		{"Nope", mailstore.ErrNotFound},
	} {
		if err := s.DeleteFolder(ctx, tc.name, false); !errors.Is(err, tc.want) {
			t.Errorf("DeleteFolder(%q) = %v, want %v", tc.name, err, tc.want)
		}
	}
	if err := s.DeleteFolder(ctx, "Projects/2026", false); err != nil {
		t.Errorf("deleting an empty label: %v", err)
	}
	if err := s.DeleteFolder(ctx, "Receipts", true); err != nil {
		t.Fatalf("deleting a label in use, with force: %v", err)
	}
	if got := w.where(t, gwInbox); !in(got, allMail, "INBOX") {
		t.Errorf("message is in %v; deleting its label must leave it in All Mail and INBOX", got)
	}
}

// A server such as Hostinger's Dovecot keeps every folder under the
// personal namespace prefix "INBOX.", with "." between levels. A name the
// caller gives with "/" and without the prefix is written the server's way.
func TestFolderNamesFollowThePersonalNamespace(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "NAMESPACE", delim: ".", namespace: "INBOX.", folders: []*wsFolder{
		wsMailbox("INBOX", "", 7), wsMailbox("INBOX.Sent", `\Sent`, 8), wsMailbox("INBOX.Archive", `\Archive`, 9),
	}})
	s := writeStore(t, srv, Options{})
	for _, tc := range []struct {
		name string
		want []string // the CREATEs it sends
	}{
		{"chainmail-test-x", []string{`CREATE "INBOX.chainmail-test-x"`}},
		{"Projects/2026", []string{`CREATE "INBOX.Projects"`, `CREATE "INBOX.Projects.2026"`}},
		{"INBOX.Kept", []string{`CREATE "INBOX.Kept"`}},
		{"inbox.Lower", []string{`CREATE "inbox.Lower"`}},
	} {
		before := srv.sent("CREATE")
		if _, err := s.CreateFolder(t.Context(), tc.name); err != nil {
			t.Fatalf("CreateFolder(%q): %v", tc.name, err)
		}
		var creates []string
		for _, c := range srv.received() {
			if strings.HasPrefix(c, "CREATE") {
				creates = append(creates, c)
			}
		}
		if got := creates[before:]; !slices.Equal(got, tc.want) {
			t.Errorf("CreateFolder(%q) sent %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, err := s.CreateFolder(t.Context(), "chainmail-test-x"); !errors.Is(err, mailstore.ErrExists) {
		t.Errorf("CreateFolder of a name the server has under the prefix = %v, want ErrExists", err)
	}
	if _, err := s.RenameFolder(t.Context(), "INBOX.Kept", "Moved/Here"); err != nil {
		t.Fatalf("RenameFolder: %v", err)
	}
	if n := srv.sent(`RENAME "INBOX.Kept" "INBOX.Moved.Here"`); n != 1 {
		t.Errorf("commands = %q, want the target renamed under the prefix with the server's delimiter", srv.received())
	}
	if n := srv.sent("NAMESPACE"); n != 1 {
		t.Errorf("NAMESPACE sent %d times, want once: the store keeps the answer", n)
	}
}

// Without NAMESPACE, or with an empty prefix, names are the caller's, with
// "/" written as the server's delimiter.
func TestFolderNamesWithoutANamespacePrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		srv  *writeServer
		want string
	}{
		{"no NAMESPACE", &writeServer{delim: "."}, `CREATE "A.B"`},
		{"empty prefix", &writeServer{caps: "NAMESPACE", delim: "/"}, `CREATE "A/B"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.srv.folders = []*wsFolder{wsMailbox("INBOX", "", 7), wsMailbox("A", "", 8)}
			srv := startWriteServer(t, tc.srv)
			s := writeStore(t, srv, Options{})
			if _, err := s.CreateFolder(t.Context(), "A/B"); err != nil {
				t.Fatal(err)
			}
			if srv.sent(tc.want) != 1 {
				t.Errorf("commands = %q, want %s", srv.received(), tc.want)
			}
		})
	}
}

// On a server that keeps every folder under "INBOX.", a move, copy, rename
// or delete names a folder the way CreateFolder does: without the prefix,
// with "/" between levels, or as the server has it.
func TestFolderWritesFollowThePersonalNamespace(t *testing.T) {
	t.Parallel()
	srv := startWriteServer(t, &writeServer{caps: "NAMESPACE MOVE UIDPLUS", delim: ".", namespace: "INBOX.", folders: []*wsFolder{
		wsMailbox("INBOX", "", 7, 1, 2), wsMailbox("INBOX.Projects.2026", "", 20),
		wsMailbox("INBOX.Old", "", 21), wsMailbox("INBOX.Empty", "", 22),
	}})
	s := writeStore(t, srv, Options{})
	ctx := t.Context()

	if res := s.Move(ctx, []ref.Ref{hostRef("INBOX", 7, 1)}, "Projects/2026"); res[0].Err != nil || res[0].Ref.Folder != "INBOX.Projects.2026" {
		t.Errorf("Move = %+v, want a ref in INBOX.Projects.2026", res[0])
	}
	if srv.sent(`UID MOVE 1 "INBOX.Projects.2026"`) != 1 {
		t.Errorf("commands = %q, want UID MOVE into INBOX.Projects.2026", srv.received())
	}
	for _, name := range []string{"Old", "INBOX.Old"} {
		if res := s.Copy(ctx, []ref.Ref{hostRef("INBOX", 7, 2)}, name); res[0].Err != nil || res[0].Ref.Folder != "INBOX.Old" {
			t.Errorf("Copy to %q = %+v, want a ref in INBOX.Old", name, res[0])
		}
	}
	if n := srv.sent(`UID COPY 2 "INBOX.Old"`); n != 2 {
		t.Errorf("UID COPY into INBOX.Old sent %d times, want 2: commands %q", n, srv.received())
	}
	if got, err := s.RenameFolder(ctx, "Old", "New"); err != nil || got != "INBOX.New" {
		t.Errorf("RenameFolder(Old, New) = %q, %v, want INBOX.New", got, err)
	}
	if srv.sent(`RENAME "INBOX.Old" "INBOX.New"`) != 1 {
		t.Errorf("commands = %q, want INBOX.Old renamed", srv.received())
	}
	if err := s.DeleteFolder(ctx, "Empty", false); err != nil {
		t.Errorf("DeleteFolder(Empty): %v", err)
	}
	if srv.sent(`DELETE "INBOX.Empty"`) != 1 {
		t.Errorf("commands = %q, want INBOX.Empty deleted", srv.received())
	}
	var nf *mailstore.FolderNotFoundError
	if res := s.Move(ctx, []ref.Ref{hostRef("INBOX", 7, 2)}, "Nope"); !errors.As(res[0].Err, &nf) || nf.Name != "INBOX.Nope" {
		t.Errorf("Move to Nope = %+v, want FolderNotFoundError for INBOX.Nope", res[0])
	}
	if _, err := s.RenameFolder(ctx, "Nope", "X"); !errors.As(err, &nf) || nf.Name != "INBOX.Nope" {
		t.Errorf("RenameFolder(Nope) = %v", err)
	}
	if err := s.DeleteFolder(ctx, "Nope", false); !errors.As(err, &nf) || nf.Name != "INBOX.Nope" {
		t.Errorf("DeleteFolder(Nope) = %v", err)
	}
	if n := srv.sent("NAMESPACE"); n != 1 {
		t.Errorf("NAMESPACE sent %d times, want once", n)
	}
}
