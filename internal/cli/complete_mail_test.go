package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
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
	got := values(cands)
	if i, j := slices.Index(got, inv), slices.Index(got, m.lunch.String()); j < 0 || j < i {
		t.Errorf("get candidates %q, want the inbox invoice first, then the recent lunch outside the inbox", got)
	}
	if cands, _ := completeArgs(t, env, "get", "cm1:host:"); !slices.Equal(values(cands), []string{inv}) {
		t.Errorf("get cm1:host: %q, want the invoice once though both queries find it", cands)
	}
	if cands, _ := completeArgs(t, env, "get", "cm1:gmail:"); slices.Contains(values(cands), inv) {
		t.Errorf("a gmail prefix offered the host invoice: %q", cands)
	}
	if cands, _ := completeArgs(t, env, "archive", ""); slices.Contains(values(cands), m.lunch.String()) {
		t.Errorf("archive offered %s, which is not in an inbox", m.lunch)
	}
	if cands, _ := completeArgs(t, env, "archive", inv, ""); slices.Contains(values(cands), inv) {
		t.Errorf("archive offered %s twice", inv)
	}
	if cands, _ := completeArgs(t, env, "thread", inv, ""); len(cands) != 0 {
		t.Errorf("thread takes one ref, got %q", cands)
	}
}

// TestCompleteRefsWithAnEmptyInbox covers an account whose inbox is
// empty, as after inbox zero. The cm1:gmail: prefix searches only the
// gmail account, whose one message is outside the inbox. A command that
// reads or acts on any message still offers it, and only archive, which
// needs an inbox message, offers nothing.
func TestCompleteRefsWithAnEmptyInbox(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	env := sockEnv(m.socket)
	lunch := m.lunch.String()
	for _, args := range [][]string{
		{"get", "cm1:gmail:"},
		{"thread", "cm1:gmail:"},
		{"attachment", "cm1:gmail:"},
		{"reply", "cm1:gmail:"},
		{"forward", "cm1:gmail:"},
		{"read", "cm1:gmail:"},
		{"unread", "cm1:gmail:"},
		{"flag", "cm1:gmail:"},
		{"unflag", "cm1:gmail:"},
		{"flags", "cm1:gmail:"},
		{"label", "cm1:gmail:"},
		{"move", "cm1:gmail:"},
		{"copy", "cm1:gmail:"},
		{"trash", "cm1:gmail:"},
		{"expunge", "cm1:gmail:"},
		{"item", "link", "it_task1", "cm1:gmail:"},
	} {
		if cands, _ := completeArgs(t, env, args...); !slices.Equal(values(cands), []string{lunch}) {
			t.Errorf("%q: candidates %q, want the recent %s", args, cands, lunch)
		}
	}
	if cands, _ := completeArgs(t, env, "archive", "cm1:gmail:"); len(cands) != 0 {
		t.Errorf("archive with an empty inbox: %q, want none", cands)
	}
}

// TestCompleteRefsWithAFullInbox covers an inbox that alone fills the
// limit: get offers exactly 20 refs, all from the inbox.
func TestCompleteRefsWithAFullInbox(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	for i := range refCompletionLimit + 5 {
		m.host.Add(fake.Message{
			Folder: "INBOX", Date: cliNow.Add(-time.Duration(i+1) * time.Minute), Subject: fmt.Sprintf("Note %d", i),
			From: []mailstore.Address{{Email: "a@example.com"}},
		})
	}
	cands, _ := completeArgs(t, sockEnv(m.socket), "get", "")
	got := values(cands)
	if len(got) != refCompletionLimit {
		t.Fatalf("get offered %d refs, want %d", len(got), refCompletionLimit)
	}
	for _, v := range got {
		if !strings.HasPrefix(v, "cm1:host:INBOX:") {
			t.Errorf("get offered %s ahead of a full inbox", v)
		}
	}
}

// failingFallback answers the inbox query and fails every other one.
type failingFallback struct {
	chainmailv1connect.UnimplementedMessageServiceHandler
}

func (failingFallback) Search(_ context.Context, req *connect.Request[chainmailv1.SearchRequest]) (*connect.Response[chainmailv1.SearchResponse], error) {
	if req.Msg.GetQueryString() != "in:inbox" {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("too slow"))
	}
	row := &chainmailv1.MessageRow{Ref: "cm1:host:INBOX:1:1", Subject: &chainmailv1.Untrusted{Untrusted: "Invoice"}}
	return connect.NewResponse(&chainmailv1.SearchResponse{Rows: []*chainmailv1.MessageRow{row}}), nil
}

func TestCompleteRefsKeepTheInboxWhenTheFallbackFails(t *testing.T) {
	t.Parallel()
	path, handler := chainmailv1connect.NewMessageServiceHandler(failingFallback{})
	cands, _ := completeArgs(t, sockEnv(startFakeServer(t, path, handler)), "get", "")
	if got := values(cands); !slices.Equal(got, []string{"cm1:host:INBOX:1:1"}) {
		t.Errorf("candidates %q, want the inbox ref the first query found", got)
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
