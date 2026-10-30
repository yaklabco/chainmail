//go:build integration

package imapstore

import (
	"errors"
	"slices"
	"testing"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

func TestDovecotCreateRenameDeleteFolders(t *testing.T) {
	user := doveUser(t)
	s := doveStore(t, user)
	ctx := t.Context()

	if err := s.CreateFolder(ctx, "Projects/2026/Q4"); err != nil {
		t.Fatal(err)
	}
	if got := folderNames(t, s); !hasAll(got, "Projects", "Projects/2026", "Projects/2026/Q4") {
		t.Errorf("folders = %q, want each level created", got)
	}
	for _, name := range []string{"Projects/2026", "inbox", "Trash"} {
		if err := s.CreateFolder(ctx, name); !errors.Is(err, mailstore.ErrExists) {
			t.Errorf("CreateFolder(%q) = %v, want ErrExists", name, err)
		}
	}
	if err := s.CreateFolder(ctx, "A//B"); !errors.Is(err, mailstore.ErrRefused) {
		t.Errorf("CreateFolder with an empty level = %v, want ErrRefused", err)
	}

	put(t, user, "Projects/2026/Q4", doveMail{from: "a@example.com", subject: "plan", messageID: "plan@x", body: "x"}, oct(1))
	if err := s.RenameFolder(ctx, "Projects", "Old/Projects"); err != nil {
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
		if err := s.RenameFolder(ctx, tc.from, tc.to); !errors.Is(err, tc.want) {
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
