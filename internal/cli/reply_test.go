package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"

	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
)

// original adds the message replies and forwards answer to the host INBOX:
// from Alice to the account and Bob, copied to Carol and the account, with
// an attachment.
func (e cliSend) original(t *testing.T, id string) ref.Ref {
	t.Helper()
	return e.store.Add(fake.Message{
		Folder:     "INBOX",
		Date:       time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC),
		From:       []mailstore.Address{{Name: "Alice", Email: "alice@example.net"}},
		To:         []mailstore.Address{{Email: "ME@example.com"}, {Name: "Bob", Email: "bob@example.net"}},
		Cc:         []mailstore.Address{{Email: "carol@example.net"}, {Email: "me@example.com"}},
		Subject:    "Plans",
		Body:       "Original line one\r\nOriginal line two\r\n",
		MessageID:  "<" + id + "@example.net>",
		References: []string{"<root@example.net>"},
		Attachments: []fake.Attachment{
			{Part: "2", Filename: "agenda.pdf", ContentType: "application/pdf", Data: []byte("%PDF agenda bytes")},
		},
	})
}

// header parses the header of raw.
func header(t *testing.T, raw []byte) gomail.Header {
	t.Helper()
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parsing message: %v", err)
	}
	return gomail.Header{Header: ent.Header}
}

// addresses returns the bare addresses of a header field.
func addresses(t *testing.T, h gomail.Header, field string) []string {
	t.Helper()
	list, err := h.AddressList(field)
	if err != nil {
		t.Fatalf("%s: %v", field, err)
	}
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.Address
	}
	return out
}

func TestReplySendsToSenderWithHeaders(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig")
	got := e.run(t, Env{}, "--output", "table", "reply", orig.String(), "--body", "Sounds *good*", "--no-signature")
	if got.code != 0 {
		t.Fatalf("reply: %+v", got)
	}
	msgs := e.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if !slices.Equal(m.To, []string{"alice@example.net"}) {
		t.Errorf("RCPT TO = %v, want alice only", m.To)
	}
	h := header(t, m.Data)
	if subj, _ := h.Subject(); subj != "Re: Plans" {
		t.Errorf("Subject = %q", subj)
	}
	if to := addresses(t, h, "To"); !slices.Equal(to, []string{"alice@example.net"}) {
		t.Errorf("To = %v", to)
	}
	if h.Has("Cc") {
		t.Errorf("Cc = %q, want none without --all", h.Get("Cc"))
	}
	if irt, _ := h.MsgIDList("In-Reply-To"); !slices.Equal(irt, []string{"orig@example.net"}) {
		t.Errorf("In-Reply-To = %v", irt)
	}
	if refs, _ := h.MsgIDList("References"); !slices.Equal(refs, []string{"root@example.net", "orig@example.net"}) {
		t.Errorf("References = %v", refs)
	}
	p := parts(t, m.Data)
	if !strings.Contains(p["text/plain"], "Sounds *good*") || !strings.Contains(p["text/plain"], "> Original line one") {
		t.Errorf("text part = %q, want the new text and the quote", p["text/plain"])
	}
	if _, ok := p["application/pdf"]; ok {
		t.Error("a reply carried the original's attachment")
	}
	if !slices.Contains(e.store.Flags(orig), `\Answered`) {
		t.Errorf("original flags = %v, want \\Answered", e.store.Flags(orig))
	}
	golden(t, "reply_table", strings.ReplaceAll(got.stdout, messageID(t, m.Data), "MESSAGE-ID"))
}

func TestReplyAllAddsRecipientsAndExtras(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig")
	got := e.run(t, Env{}, "--output", "json", "reply", orig.String(), "--all",
		"--body", "All of you", "--cc", "dave@example.net", "--bcc", "hidden@example.net")
	if got.code != 0 {
		t.Fatalf("reply --all: %+v", got)
	}
	msgs := e.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	rcpt := slices.Sorted(slices.Values(m.To))
	want := []string{"alice@example.net", "bob@example.net", "carol@example.net", "dave@example.net", "hidden@example.net"}
	if !slices.Equal(rcpt, want) {
		t.Errorf("RCPT TO = %v, want %v", rcpt, want)
	}
	h := header(t, m.Data)
	if to := addresses(t, h, "To"); !slices.Equal(to, []string{"alice@example.net"}) {
		t.Errorf("To = %v", to)
	}
	if cc := addresses(t, h, "Cc"); !slices.Equal(cc, []string{"bob@example.net", "carol@example.net", "dave@example.net"}) {
		t.Errorf("Cc = %v", cc)
	}
	if bytes.Contains(m.Data, []byte("hidden@")) {
		t.Error("the Bcc address is in the message")
	}
	if irt, _ := h.MsgIDList("In-Reply-To"); !slices.Equal(irt, []string{"orig@example.net"}) {
		t.Errorf("In-Reply-To = %v", irt)
	}
	golden(t, "reply_all_json", strings.ReplaceAll(got.stdout, messageID(t, m.Data), "MESSAGE-ID"))
}

func TestReplyDerivedKeysDifferPerRef(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	one, two := e.original(t, "one"), e.original(t, "two")
	for _, r := range []ref.Ref{one, two} {
		if got := e.run(t, Env{}, "reply", r.String(), "--body", "Same words"); got.code != 0 {
			t.Fatalf("reply to %s: %+v", r, got)
		}
	}
	if n := len(e.smtp.Messages()); n != 2 {
		t.Fatalf("captured %d messages, want 2: one reply per ref", n)
	}
	keys := e.keys(t)
	if len(keys) != 2 || keys[0] == keys[1] || !strings.HasPrefix(keys[0], "cli-") || !strings.HasPrefix(keys[1], "cli-") {
		t.Fatalf("keys = %q, want two different cli- keys", keys)
	}

	// The same reply again is the first one's result, not a second send.
	again := e.run(t, Env{}, "--output", "table", "reply", one.String(), "--body", "Same words")
	if again.code != 0 || !strings.Contains(again.stdout, "duplicate: already sent") {
		t.Errorf("repeat: %+v", again)
	}
	// Replying to all is another message, so it gets its own key.
	all := e.run(t, Env{}, "reply", one.String(), "--all", "--body", "Same words")
	if all.code != 0 || strings.Contains(all.stdout, `"duplicate":true`) {
		t.Errorf("reply --all: %+v", all)
	}
	if n := len(e.smtp.Messages()); n != 3 {
		t.Errorf("captured %d messages, want 3", n)
	}
	if keys := e.keys(t); len(keys) != 3 {
		t.Errorf("keys = %q, want 3", keys)
	}
}

func TestReplyKeyOverride(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig")
	if got := e.run(t, Env{}, "reply", orig.String(), "--body", "Hi", "--idempotency-key", "reply.1"); got.code != 0 {
		t.Fatalf("reply: %+v", got)
	}
	if keys := e.keys(t); !slices.Equal(keys, []string{"reply.1"}) {
		t.Errorf("keys = %q, want [reply.1]", keys)
	}
}

func TestReplyToSelfOnlyIsRefused(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	r := e.store.Add(fake.Message{
		Folder: "Sent", Date: cliNow, Subject: "Note to self", MessageID: "<self@example.com>",
		From: []mailstore.Address{{Email: "me@example.com"}}, To: []mailstore.Address{{Email: "me@example.com"}},
	})
	got := e.run(t, Env{}, "reply", r.String(), "--body", "Hi")
	if got.code != 2 || errorReason(t, got.stderr) != "INVALID_ARGUMENT" {
		t.Errorf("reply to self: %+v, want exit 2 INVALID_ARGUMENT", got)
	}
	if n := len(e.smtp.Messages()); n != 0 {
		t.Errorf("captured %d messages", n)
	}
}

func TestReplyAndForwardDraft(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig")
	for _, args := range [][]string{
		{"reply", orig.String(), "--body", "Later", "--draft"},
		{"forward", orig.String(), "--to", "dave@example.net", "--draft"},
	} {
		got := e.run(t, Env{}, append([]string{"--output", "table"}, args...)...)
		if got.code != 0 || !strings.HasPrefix(got.stdout, "draft saved cm1:host:Drafts:") || strings.Count(got.stdout, "\n") != 1 {
			t.Errorf("%s --draft: %+v", args[0], got)
		}
	}
	if n := len(e.smtp.Messages()); n != 0 {
		t.Errorf("a draft sent %d messages", n)
	}
	if keys := e.keys(t); len(keys) != 0 {
		t.Errorf("a draft stored keys %q", keys)
	}
	drafts := e.store.Appended()
	if len(drafts) != 2 {
		t.Fatalf("appended %d drafts, want 2", len(drafts))
	}
	replyDraft, fwdDraft := header(t, drafts[0]), header(t, drafts[1])
	if irt, _ := replyDraft.MsgIDList("In-Reply-To"); !slices.Equal(irt, []string{"orig@example.net"}) {
		t.Errorf("reply draft In-Reply-To = %v", irt)
	}
	if subj, _ := fwdDraft.Subject(); subj != "Fwd: Plans" {
		t.Errorf("forward draft Subject = %q", subj)
	}
}

func TestReplyDryRun(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig")
	got := e.run(t, Env{}, "reply", orig.String(), "--body", "Hi", "--dry-run")
	if got.code != 0 || !strings.Contains(got.stdout, "In-Reply-To: <orig@example.net>\r\n") {
		t.Fatalf("dry run: %+v", got)
	}
	tty := e.run(t, Env{StdoutTerminal: true}, "reply", orig.String(), "--body", "Hi", "--dry-run")
	if tty.code != 2 || tty.stdout != "" || errorReason(t, tty.stderr) != "USAGE" {
		t.Errorf("dry run to a terminal: %+v", tty)
	}
	if n, keys := len(e.smtp.Messages()), e.keys(t); n != 0 || len(keys) != 0 {
		t.Errorf("a dry run sent %d messages and stored keys %q", n, keys)
	}
}

func TestForwardCarriesOriginalAttachments(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig")
	got := e.run(t, Env{}, "--output", "table", "forward", orig.String(), "--to", "dave@example.net", "--body", "FYI")
	if got.code != 0 {
		t.Fatalf("forward: %+v", got)
	}
	msgs := e.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if !slices.Equal(m.To, []string{"dave@example.net"}) {
		t.Errorf("RCPT TO = %v", m.To)
	}
	h := header(t, m.Data)
	if subj, _ := h.Subject(); subj != "Fwd: Plans" {
		t.Errorf("Subject = %q", subj)
	}
	if h.Has("In-Reply-To") || h.Has("References") {
		t.Errorf("a forward has In-Reply-To %q, References %q", h.Get("In-Reply-To"), h.Get("References"))
	}
	p := parts(t, m.Data)
	if p["application/pdf"] != "%PDF agenda bytes" {
		t.Errorf("forwarded attachment = %q", p["application/pdf"])
	}
	if !strings.Contains(p["text/plain"], "FYI") || !strings.Contains(p["text/plain"], "Forwarded message") {
		t.Errorf("text part = %q", p["text/plain"])
	}
	if !slices.Contains(e.store.Flags(orig), "$Forwarded") {
		t.Errorf("original flags = %v, want $Forwarded", e.store.Flags(orig))
	}
	golden(t, "forward_table", strings.ReplaceAll(got.stdout, messageID(t, m.Data), "MESSAGE-ID"))
}

func TestForwardNoAttachments(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig")
	notes := e.file(t, "notes.txt", "my own notes")
	got := e.run(t, Env{}, "forward", orig.String(), "--to", "dave@example.net", "--no-attachments", "--attach", notes)
	if got.code != 0 {
		t.Fatalf("forward --no-attachments: %+v", got)
	}
	msgs := e.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	p := parts(t, msgs[0].Data)
	if _, ok := p["application/pdf"]; ok {
		t.Error("--no-attachments still carried the original's attachment")
	}
	if !strings.Contains(p["text/plain"], "my own notes") {
		t.Errorf("own attachment missing: parts %v", keysOf(p))
	}

	// With the original's attachments it is another message, under another key.
	with := e.run(t, Env{}, "forward", orig.String(), "--to", "dave@example.net", "--attach", notes)
	if with.code != 0 || strings.Contains(with.stdout, `"duplicate":true`) {
		t.Fatalf("forward with attachments: %+v", with)
	}
	if n := len(e.smtp.Messages()); n != 2 {
		t.Errorf("captured %d messages, want 2", n)
	}
	if keys := e.keys(t); len(keys) != 2 || keys[0] == keys[1] {
		t.Errorf("keys = %q, want two different keys", keys)
	}
}

func TestForwardAndReplyKeysDiffer(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig")
	if got := e.run(t, Env{}, "reply", orig.String(), "--to", "dave@example.net", "--body", "Hi"); got.code != 0 {
		t.Fatalf("reply: %+v", got)
	}
	if got := e.run(t, Env{}, "forward", orig.String(), "--to", "dave@example.net", "--body", "Hi"); got.code != 0 {
		t.Fatalf("forward: %+v", got)
	}
	if keys := e.keys(t); len(keys) != 2 || keys[0] == keys[1] {
		t.Errorf("keys = %q, want a reply key and a different forward key", keys)
	}
}

func TestReplyJSONIsTheResponse(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	orig := e.original(t, "orig")
	got := e.run(t, Env{}, "forward", orig.String(), "--to", "dave@example.net")
	if got.code != 0 {
		t.Fatalf("forward: %+v", got)
	}
	var out struct {
		Result struct {
			Sent struct {
				MessageID string `json:"message_id"`
				SentRef   string `json:"sent_ref"`
			} `json:"sent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &out); err != nil {
		t.Fatalf("output %q: %v", got.stdout, err)
	}
	if out.Result.Sent.MessageID == "" || !strings.HasPrefix(out.Result.Sent.SentRef, "cm1:host:Sent:") {
		t.Errorf("result = %+v", out.Result)
	}
}

func TestAnswerAndDraftUsageErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := "cm1:host:INBOX:1:1"
	d := "cm1:host:Drafts:1:1"
	draftMsg := []string{"--account", "host", "--to", "a@b.example", "--subject", "s", "--body", "x"}
	tests := []struct {
		name string
		args []string
	}{
		{"reply no ref", []string{"reply", "--body", "x"}},
		{"reply two refs", []string{"reply", r, r, "--body", "x"}},
		{"reply bad ref", []string{"reply", "INBOX:1", "--body", "x"}},
		{"reply no body", []string{"reply", r}},
		{"reply dry run and draft", []string{"reply", r, "--body", "x", "--dry-run", "--draft"}},
		{"reply body and body file", []string{"reply", r, "--body", "x", "--body-file", filepath.Join(dir, "b")}},
		{"reply missing attachment", []string{"reply", r, "--body", "x", "--attach", filepath.Join(dir, "nope")}},
		{"reply no-attachments", []string{"reply", r, "--body", "x", "--no-attachments"}},
		{"forward no recipients", []string{"forward", r, "--body", "x"}},
		{"forward no ref", []string{"forward", "--to", "a@b.example"}},
		{"forward bad ref", []string{"forward", "nope", "--to", "a@b.example"}},
		{"forward dry run and draft", []string{"forward", r, "--to", "a@b.example", "--dry-run", "--draft"}},
		{"forward all", []string{"forward", r, "--to", "a@b.example", "--all"}},
		{"draft bare", []string{"draft"}},
		{"draft unknown", []string{"draft", "publish"}},
		{"draft create no account", []string{"draft", "create", "--to", "a@b.example", "--subject", "s", "--body", "x"}},
		{"draft create no recipient", []string{"draft", "create", "--account", "host", "--subject", "s", "--body", "x"}},
		{"draft create no subject", []string{"draft", "create", "--account", "host", "--to", "a@b.example", "--body", "x"}},
		{"draft create no body", []string{"draft", "create", "--account", "host", "--to", "a@b.example", "--subject", "s"}},
		{"draft create positional", append([]string{"draft", "create", d}, draftMsg...)},
		{"draft create dry run", append([]string{"draft", "create", "--dry-run"}, draftMsg...)},
		{"draft update no ref", []string{"draft", "update", "--to", "a@b.example", "--subject", "s", "--body", "x"}},
		{"draft update bad ref", []string{"draft", "update", "x", "--to", "a@b.example", "--subject", "s", "--body", "x"}},
		{"draft update account", append([]string{"draft", "update", d}, draftMsg...)},
		{"draft update no body", []string{"draft", "update", d, "--to", "a@b.example", "--subject", "s"}},
		{"draft send no ref", []string{"draft", "send"}},
		{"draft send two refs", []string{"draft", "send", d, d}},
		{"draft send bad ref", []string{"draft", "send", "x"}},
		{"draft delete no ref", []string{"draft", "delete"}},
		{"draft delete bad ref", []string{"draft", "delete", "x"}},
	}
	for _, tt := range tests {
		// No daemon listens here, so a command that got as far as calling one exits 7.
		got := runCLI(t, Env{}, append([]string{"--socket", filepath.Join(dir, "none.sock")}, tt.args...)...)
		if got.code != 2 || errorReason(t, got.stderr) != "USAGE" {
			t.Errorf("%s: %+v, want a usage error", tt.name, got)
		}
	}
}
