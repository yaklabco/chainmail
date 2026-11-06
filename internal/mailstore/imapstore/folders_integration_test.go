//go:build integration

package imapstore

import (
	"errors"
	"slices"
	"testing"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/testutil/dovecot"
)

func TestDovecotCreateRenameDeleteFolders(t *testing.T) {
	user := doveUser(t)
	s := doveStore(t, user)
	ctx := t.Context()

	if _, err := s.CreateFolder(ctx, "Projects/2026/Q4"); err != nil {
		t.Fatal(err)
	}
	if got := folderNames(t, s); !hasAll(got, "Projects", "Projects/2026", "Projects/2026/Q4") {
		t.Errorf("folders = %q, want each level created", got)
	}
	for _, name := range []string{"Projects/2026", "inbox", "Trash"} {
		if _, err := s.CreateFolder(ctx, name); !errors.Is(err, mailstore.ErrExists) {
			t.Errorf("CreateFolder(%q) = %v, want ErrExists", name, err)
		}
	}
	if _, err := s.CreateFolder(ctx, "A//B"); !errors.Is(err, mailstore.ErrRefused) {
		t.Errorf("CreateFolder with an empty level = %v, want ErrRefused", err)
	}

	put(t, user, "Projects/2026/Q4", doveMail{from: "a@example.com", subject: "plan", messageID: "plan@x", body: "x"}, oct(1))
	if _, err := s.RenameFolder(ctx, "Projects", "Old/Projects"); err != nil {
		t.Fatal(err)
	}
	got := folderNames(t, s)
	if !hasAll(got, "Old", "Old/Projects", "Old/Projects/2026", "Old/Projects/2026/Q4") || slices.Contains(got, "Projects") {
		t.Errorf("folders = %q, want Projects and its children under a new Old", got)
	}
	if n := s.mustCount(t, "in:Old/Projects/2026/Q4"); n != 1 {
		t.Errorf("the renamed folder holds %d messages, want its 1", n)
	}
	for _, tc := range []struct {
		from, to string
		want     error
	}{
		{"INBOX", "Box", mailstore.ErrRefused},
		{"Sent", "Sent2", mailstore.ErrRefused},
		{"Old", "Old/Inside", mailstore.ErrRefused},
		{"Old/Projects", "Archive", mailstore.ErrExists},
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
		{"Trash", mailstore.ErrRefused},
		{"Junk", mailstore.ErrRefused},
		{"Old/Projects", mailstore.ErrRefused},         // has a child
		{"Old/Projects/2026/Q4", mailstore.ErrRefused}, // holds mail
		{"Nope", mailstore.ErrNotFound},
	} {
		if err := s.DeleteFolder(ctx, tc.name, false); !errors.Is(err, tc.want) {
			t.Errorf("DeleteFolder(%q) = %v, want %v", tc.name, err, tc.want)
		}
	}
	if err := s.DeleteFolder(ctx, "Old/Projects/2026/Q4", true); err != nil {
		t.Fatalf("deleting a folder with mail, with force: %v", err)
	}
	for _, name := range []string{"Old/Projects/2026", "Old/Projects", "Old"} {
		if err := s.DeleteFolder(ctx, name, false); err != nil {
			t.Fatalf("deleting the empty %s: %v", name, err)
		}
	}
	if got := folderNames(t, s); slices.ContainsFunc(got, func(n string) bool { return n == "Old" || n == "Projects" }) {
		t.Errorf("folders = %q, want the test's folders gone", got)
	}
}

// TestDovecotPrefixedNamespace runs folder writes against a Dovecot that
// keeps every folder under "INBOX.", with "." between levels, as Hostinger's
// does. Names given without the prefix, and with "/" between levels, reach
// the server's folders, and a missing one is named as the server has it.
func TestDovecotPrefixedNamespace(t *testing.T) {
	srv, err := dovecot.StartPrefixed()
	if err != nil {
		t.Fatalf("starting Dovecot: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(); err != nil {
			t.Errorf("stopping Dovecot: %v", err)
		}
	})
	user := doveUser(t)
	acct := testAccount("dove", srv.Endpoint, false)
	acct.Username = user
	s := New(acct, passwordCreds(user, dovecot.Password), Options{RootCAs: srv.CA.Pool, Backoff: noBackoff})
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()

	for name, want := range map[string]string{"Projects/2026": "INBOX.Projects.2026", "Keep": "INBOX.Keep"} {
		if got, err := s.CreateFolder(ctx, name); err != nil || got != want {
			t.Fatalf("CreateFolder(%q) = %q, %v, want %q", name, got, err, want)
		}
	}
	uid, err := srv.Append(user, "INBOX", doveMail{from: "a@example.com", subject: "ns", messageID: "ns@x", body: "x"}.raw(), oct(1))
	if err != nil {
		t.Fatal(err)
	}
	validity, err := srv.UIDValidity(user, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	inInbox := ref.Ref{Account: "dove", Folder: "INBOX", UIDValidity: validity, UID: uint32(uid)}

	moved := s.Move(ctx, []ref.Ref{inInbox}, "Projects/2026")
	if moved[0].Err != nil || moved[0].Ref.Folder != "INBOX.Projects.2026" {
		t.Fatalf("Move to Projects/2026 = %+v, want a ref in INBOX.Projects.2026", moved[0])
	}
	copied := s.Copy(ctx, []ref.Ref{moved[0].Ref}, "Keep")
	if copied[0].Err != nil || copied[0].Ref.Folder != "INBOX.Keep" {
		t.Fatalf("Copy to Keep = %+v, want a ref in INBOX.Keep", copied[0])
	}
	if got, err := s.RenameFolder(ctx, "Keep", "Kept"); err != nil || got != "INBOX.Kept" {
		t.Fatalf("RenameFolder(Keep, Kept) = %q, %v, want INBOX.Kept", got, err)
	}
	if err := s.DeleteFolder(ctx, "Kept", true); err != nil {
		t.Fatalf("DeleteFolder(Kept, force): %v", err)
	}
	if got := folderNames(t, s); !hasAll(got, "INBOX.Projects", "INBOX.Projects.2026") || slices.Contains(got, "INBOX.Kept") {
		t.Errorf("folders = %q", got)
	}

	var nf *mailstore.FolderNotFoundError
	if res := s.Move(ctx, []ref.Ref{moved[0].Ref}, "Nope"); !errors.As(res[0].Err, &nf) || nf.Name != "INBOX.Nope" || nf.Requested != "Nope" {
		t.Errorf("Move to Nope = %+v, want FolderNotFoundError for INBOX.Nope, requested as Nope", res[0])
	}
	if _, err := s.RenameFolder(ctx, "Nope", "Other"); !errors.As(err, &nf) || nf.Name != "INBOX.Nope" || nf.Requested != "Nope" {
		t.Errorf("RenameFolder(Nope) = %v, want FolderNotFoundError for INBOX.Nope, requested as Nope", err)
	}
	if err := s.DeleteFolder(ctx, "Nope", false); !errors.As(err, &nf) || nf.Name != "INBOX.Nope" || nf.Requested != "Nope" {
		t.Errorf("DeleteFolder(Nope) = %v, want FolderNotFoundError for INBOX.Nope, requested as Nope", err)
	}
}
