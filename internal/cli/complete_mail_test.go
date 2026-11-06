package cli

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
)

func TestCompleteRefs(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	env := sockEnv(m.socket)
	inv := m.invoice.String()

	cands, _ := completeArgs(t, env, "get", "")
	if !slices.Contains(values(cands), inv) {
		t.Fatalf("get candidates %q lack the inbox invoice %s", cands, inv)
	}
	for _, c := range cands {
		if strings.HasPrefix(c, inv+"\t") && (!strings.Contains(c, "Acme Billing") || !strings.Contains(c, "Invoice 7")) {
			t.Errorf("description %q lacks the sender and subject", c)
		}
	}
	if slices.Contains(values(cands), m.lunch.String()) {
		t.Errorf("get offered %s, which is not in an inbox", m.lunch)
	}
	if cands, _ := completeArgs(t, env, "get", "cm1:gmail:"); slices.Contains(values(cands), inv) {
		t.Errorf("a gmail prefix offered the host invoice: %q", cands)
	}
	if cands, _ := completeArgs(t, env, "archive", inv, ""); slices.Contains(values(cands), inv) {
		t.Errorf("archive offered %s twice", inv)
	}
	if cands, _ := completeArgs(t, env, "thread", inv, ""); len(cands) != 0 {
		t.Errorf("thread takes one ref, got %q", cands)
	}
}

func TestCompleteDraftRefs(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	draft := m.host.Add(fake.Message{
		Folder: "Drafts", Date: cliNow.Add(-time.Hour), Subject: "Plan",
		From: []mailstore.Address{{Email: "me@example.com"}}, To: []mailstore.Address{{Name: "Alice", Email: "alice@example.com"}},
	})
	cands, _ := completeArgs(t, sockEnv(m.socket), "draft", "send", "")
	got := values(cands)
	if !slices.Contains(got, draft.String()) || slices.Contains(got, m.invoice.String()) {
		t.Errorf("draft send candidates %q, want the draft and not the invoice", got)
	}
	for _, c := range cands {
		if strings.HasPrefix(c, draft.String()+"\t") && !strings.Contains(c, "Alice") {
			t.Errorf("draft description %q lacks the recipient", c)
		}
	}
}

func TestCompleteRefDescriptionIsClean(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	evil := m.host.Add(fake.Message{
		Folder: "INBOX", Date: cliNow.Add(-time.Minute), Subject: "evil\tsub\nject \x1b[31mred" + strings.Repeat("x", 200),
		From: []mailstore.Address{{Name: "Mal\nlory", Email: "m@example.com"}},
	})
	cands, _ := completeArgs(t, sockEnv(m.socket), "get", evil.String())
	if len(cands) != 1 {
		t.Fatalf("candidates %q, want the one evil message", cands)
	}
	if strings.Count(cands[0], "\t") != 1 || strings.ContainsAny(cands[0], "\n\x1b") {
		t.Errorf("candidate line %q is not one clean value and description", cands[0])
	}
}

func TestCompleteAttachmentParts(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	cands, _ := completeArgs(t, sockEnv(m.socket), "attachment", m.lunch.String(), "")
	if len(cands) != 1 || !strings.HasPrefix(cands[0], "2\t") || !strings.Contains(cands[0], "menu.pdf") {
		t.Errorf("candidates %q, want part 2 menu.pdf", cands)
	}
}

func TestCompleteFolders(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	env := sockEnv(m.socket)
	hostFolders := []string{"Archive", "Drafts", "INBOX", "Junk", "Sent", "Trash"}

	cands, _ := completeArgs(t, env, "move", m.invoice.String(), "--to", "")
	if got := values(cands); !slices.Equal(got, hostFolders) {
		t.Errorf("move --to after a host ref: %q, want %q", got, hostFolders)
	}
	cands, _ = completeArgs(t, env, "copy", "--to", "")
	if got := values(cands); !slices.Contains(got, "[Gmail]/Trash") || !slices.Contains(got, "Archive") {
		t.Errorf("copy --to with no refs: %q, want every account's folders", got)
	}
	cands, _ = completeArgs(t, env, "folder", "delete", "--account", "gmail", "")
	if got := values(cands); slices.Contains(got, "Archive") || !slices.Contains(got, "[Gmail]/Trash") {
		t.Errorf("folder delete --account gmail: %q, want gmail's folders only", got)
	}
	cands, d := completeArgs(t, env, "folder", "create", "--account", "host", "Ar")
	if got := values(cands); !slices.Equal(got, []string{"Archive"}) || d&cobra.ShellCompDirectiveNoSpace == 0 {
		t.Errorf("folder create: %q directive %d, want Archive with NoSpace", got, d)
	}
	cands, _ = completeArgs(t, env, "cache", "rebuild", "--account", "gmail", "--folder", "")
	if got := values(cands); slices.Contains(got, "Archive") || !slices.Contains(got, "[Gmail]/Trash") {
		t.Errorf("cache rebuild --account gmail --folder: %q, want gmail's folders only", got)
	}
	cands, _ = completeArgs(t, env, "watch", "--in", "IN")
	if got := values(cands); !slices.Equal(got, []string{"INBOX"}) {
		t.Errorf("watch --in IN: %q, want INBOX once", got)
	}
}

func TestCompleteFoldersSkipTrimmedNames(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	m.gmail.AddFolder(" Reports", mailstore.RoleNone)
	m.gmail.AddFolder("Reports\u00a0", mailstore.RoleNone)
	m.gmail.AddFolder("Receipts", mailstore.RoleNone)
	for _, args := range [][]string{
		{"label", m.lunch.String(), "--add", ""},
		{"move", m.lunch.String(), "--to", ""},
	} {
		cands, _ := completeArgs(t, sockEnv(m.socket), args...)
		got := values(cands)
		if !slices.Contains(got, "Receipts") {
			t.Errorf("%q: %q lacks Receipts", args, got)
		}
		for _, v := range got {
			if strings.TrimSpace(v) != v || strings.Contains(v, "Reports") {
				t.Errorf("%q: offered %q, which cobra trims into another name", args, v)
			}
		}
	}
}

func TestCompleteLabels(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	m.gmail.AddFolder("Receipts", mailstore.RoleNone)
	cands, _ := completeArgs(t, sockEnv(m.socket), "label", m.lunch.String(), "--add", "")
	got := values(cands)
	if !slices.Contains(got, "Receipts") || slices.Contains(got, "[Gmail]/Trash") || slices.Contains(got, "Archive") {
		t.Errorf("label --add: %q, want gmail's own labels only", got)
	}
}
