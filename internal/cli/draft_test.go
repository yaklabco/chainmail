package cli

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
)

// draftArgs is a whole message for draft create or update, after the subcommand.
func draftArgs(body string, extra ...string) []string {
	return append([]string{
		"--to", "You <you@example.net>", "--bcc", "hidden@example.net", "--subject", "Plan", "--body", body, "--no-signature",
	}, extra...)
}

// drafts lists the refs in the host Drafts folder.
func (e cliSend) drafts(t *testing.T) []string {
	t.Helper()
	res, err := e.store.Search(t.Context(), mailstore.SearchRequest{In: "Drafts"})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, src := range res {
		for _, uid := range src.UIDs {
			out = append(out, ref.Ref{Account: "host", Folder: src.Source.Folder, UIDValidity: src.Source.UIDValidity, UID: uid}.String())
		}
	}
	return out
}

func TestDraftLifecycle(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})

	created := e.run(t, Env{}, append([]string{"--output", "table", "draft", "create", "--account", "host"}, draftArgs("First try")...)...)
	if created.code != 0 {
		t.Fatalf("create: %+v", created)
	}
	golden(t, "draft_create_table", created.stdout)
	first := strings.TrimPrefix(strings.TrimSuffix(created.stdout, "\n"), "draft saved ")
	if got := e.drafts(t); !slices.Equal(got, []string{first}) {
		t.Fatalf("drafts = %v, want [%s]", got, first)
	}

	updated := e.run(t, Env{}, append([]string{"--output", "json", "draft", "update", first}, draftArgs("Second try")...)...)
	if updated.code != 0 {
		t.Fatalf("update: %+v", updated)
	}
	var up chainmailv1.UpdateDraftResponse
	if err := protojson.Unmarshal([]byte(updated.stdout), &up); err != nil {
		t.Fatalf("update output %q: %v", updated.stdout, err)
	}
	second := up.GetDraft().GetRef()
	if second == "" || second == first || up.GetDraft().GetMessageId() == "" {
		t.Fatalf("update = %+v", up.GetDraft())
	}
	golden(t, "draft_update_json", strings.ReplaceAll(updated.stdout, up.GetDraft().GetMessageId(), "MESSAGE-ID"))
	if got := e.drafts(t); !slices.Equal(got, []string{second}) {
		t.Fatalf("drafts after update = %v, want [%s]", got, second)
	}
	if n := len(e.smtp.Messages()); n != 0 {
		t.Fatalf("saving drafts sent %d messages", n)
	}

	sent := e.run(t, Env{}, "--output", "table", "draft", "send", second)
	if sent.code != 0 {
		t.Fatalf("send: %+v", sent)
	}
	msgs := e.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if strings.Join(m.To, ",") != "you@example.net,hidden@example.net" || strings.Contains(string(m.Data), "hidden@") {
		t.Errorf("envelope %v, or the Bcc is in the message", m.To)
	}
	if p := parts(t, m.Data); !strings.Contains(p["text/plain"], "Second try") {
		t.Errorf("sent text = %q, want the updated draft", p["text/plain"])
	}
	golden(t, "draft_send_table", strings.ReplaceAll(sent.stdout, messageID(t, m.Data), "MESSAGE-ID"))
	if got := e.drafts(t); len(got) != 0 {
		t.Errorf("drafts after send = %v, want none", got)
	}
	keys := e.keys(t)
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "cli-") {
		t.Fatalf("keys = %q, want one derived key", keys)
	}

	// Sending the same draft again reports the first send.
	again := e.run(t, Env{}, "--output", "table", "draft", "send", second)
	if again.code != 0 || !strings.Contains(again.stdout, "duplicate: already sent") || len(e.smtp.Messages()) != 1 {
		t.Errorf("repeat send: %+v", again)
	}
}

func TestDraftDelete(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	created := e.run(t, Env{}, append([]string{"--output", "json", "draft", "create", "--account", "host"}, draftArgs("Doomed")...)...)
	if created.code != 0 {
		t.Fatalf("create: %+v", created)
	}
	var cr chainmailv1.CreateDraftResponse
	if err := protojson.Unmarshal([]byte(created.stdout), &cr); err != nil {
		t.Fatalf("create output %q: %v", created.stdout, err)
	}
	golden(t, "draft_create_json", strings.ReplaceAll(created.stdout, cr.GetDraft().GetMessageId(), "MESSAGE-ID"))
	d := cr.GetDraft().GetRef()

	table := e.run(t, Env{}, "--output", "table", "draft", "delete", d)
	if table.code != 0 {
		t.Fatalf("delete: %+v", table)
	}
	golden(t, "draft_delete_table", table.stdout)
	if got := e.drafts(t); len(got) != 0 {
		t.Errorf("drafts after delete = %v", got)
	}

	// The draft is gone, so deleting it again finds nothing.
	gone := e.run(t, Env{}, "--output", "json", "draft", "delete", d)
	if gone.code == 0 || gone.stdout != "" {
		t.Errorf("deleting again: %+v", gone)
	}
}

func TestDraftDeleteJSON(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	created := e.run(t, Env{}, append([]string{"--output", "table", "draft", "create", "--account", "host"}, draftArgs("x")...)...)
	if created.code != 0 {
		t.Fatalf("create: %+v", created)
	}
	d := strings.TrimPrefix(strings.TrimSuffix(created.stdout, "\n"), "draft saved ")
	got := e.run(t, Env{}, "--output", "json", "draft", "delete", d)
	if got.code != 0 {
		t.Fatalf("delete: %+v", got)
	}
	golden(t, "draft_delete_json", got.stdout)
}

func TestDraftCommandsRefuseNonDrafts(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig").String()
	for _, args := range [][]string{
		{"draft", "send", orig},
		{"draft", "delete", orig},
		append([]string{"draft", "update", orig}, draftArgs("x")...),
	} {
		got := e.run(t, Env{}, args...)
		if got.code != 2 || errorReason(t, got.stderr) != "INVALID_ARGUMENT" {
			t.Errorf("%s on an inbox message: %+v, want exit 2 INVALID_ARGUMENT", args[1], got)
		}
	}
	if n := len(e.smtp.Messages()); n != 0 {
		t.Errorf("captured %d messages", n)
	}
}

func TestDraftSendKeyOverride(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	created := e.run(t, Env{}, append([]string{"--output", "table", "draft", "create", "--account", "host"}, draftArgs("x")...)...)
	if created.code != 0 {
		t.Fatalf("create: %+v", created)
	}
	d := strings.TrimPrefix(strings.TrimSuffix(created.stdout, "\n"), "draft saved ")
	if got := e.run(t, Env{}, "draft", "send", d, "--idempotency-key", "draft.1"); got.code != 0 {
		t.Fatalf("send: %+v", got)
	}
	if keys := e.keys(t); !slices.Equal(keys, []string{"draft.1"}) {
		t.Errorf("keys = %q, want [draft.1]", keys)
	}
}
