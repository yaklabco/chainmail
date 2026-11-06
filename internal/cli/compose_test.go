package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
	"google.golang.org/protobuf/encoding/protojson"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
	"github.com/yaklabco/chainmail/internal/testutil/smtptest"
	"github.com/yaklabco/chainmail/internal/testutil/tlstest"
)

const cliSendConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"
display_name = "Me Myself"
smtp = { host = "127.0.0.1", port = %d, security = "starttls" }

# blocked.invalid is never allowed.
[policy]
send_allow = ["*@example.net", "*@example.org"]
`

// cliSend is a daemon whose host account submits to an smtptest server
// and keeps its folders in a fake store.
type cliSend struct {
	h     *daemontest.Harness
	smtp  *smtptest.Server
	store *fake.Store
	dir   string
}

func startCLISend(t *testing.T, opts smtptest.Options) cliSend {
	t.Helper()
	ca := tlstest.New(t)
	e := cliSend{
		smtp:  smtptest.StartWith(t, ca, provider.SecurityStartTLS, opts, smtptest.User{Username: "me@example.com", Password: "pw"}),
		store: fake.New("host", false),
		dir:   t.TempDir(),
	}
	e.h = daemontest.Start(t, fmt.Sprintf(cliSendConfig, e.smtp.Endpoint().Port), daemontest.Options{
		RootCAs:               ca.Pool,
		NewStore:              func(config.ResolvedAccount) mailstore.Store { return e.store },
		Now:                   func() time.Time { return cliNow },
		OutcomeSearchInterval: 10 * time.Millisecond,
	})
	if _, err := e.h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{Name: "accounts/host", Value: []byte("pw")})); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	return e
}

func (e cliSend) run(t *testing.T, env Env, args ...string) cliResult {
	t.Helper()
	return runCLI(t, env, append([]string{"--socket", e.h.Socket}, args...)...)
}

// file writes data to name in the test's directory and returns its path.
func (e cliSend) file(t *testing.T, name, data string) string {
	t.Helper()
	path := filepath.Join(e.dir, name)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// keys lists the idempotency keys the daemon stored, in insertion order.
func (e cliSend) keys(t *testing.T) []string {
	t.Helper()
	rows, err := e.h.DB.QueryContext(t.Context(), "SELECT key FROM idempotency ORDER BY created_at, key")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports any iteration error
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// sendArgs is a plain send from host, with the body last so tests can append to it.
func sendArgs(extra ...string) []string {
	return append([]string{
		"send", "--account", "host", "--to", "You <you@example.net>", "--cc", "cc@example.net",
		"--bcc", "hidden@example.net", "--subject", "Quarterly",
	}, extra...)
}

// parts maps each leaf part's media type to its decoded body.
func parts(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parsing message: %v", err)
	}
	out := map[string]string{}
	err = ent.Walk(func(_ []int, part *message.Entity, err error) error {
		if err != nil {
			return err
		}
		mt, _, _ := part.Header.ContentType()
		if strings.HasPrefix(mt, "multipart/") {
			return nil
		}
		b, err := io.ReadAll(part.Body)
		if err != nil {
			return err
		}
		out[mt] = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walking message: %v", err)
	}
	return out
}

func messageID(t *testing.T, raw []byte) string {
	t.Helper()
	ent, err := message.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parsing message: %v", err)
	}
	h := gomail.Header{Header: ent.Header}
	id, err := h.MessageID()
	if err != nil || id == "" {
		t.Fatalf("Message-ID: %q, %v", id, err)
	}
	return id
}

func TestSendUploadsAndSends(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	png := "\x89PNG\r\n\x1a\n fake image"
	pdf := e.file(t, "report.pdf", "%PDF-1.4 report")
	img := e.file(t, "chart.png", png)
	got := e.run(t, Env{}, append([]string{"--output", "table"}, sendArgs(
		"--body", "See ![chart](chart.png)", "--attach", pdf, "--inline", "chart.png="+img)...)...)
	if got.code != 0 {
		t.Fatalf("send: %+v", got)
	}
	msgs := e.smtp.Messages()
	if len(msgs) != 1 {
		t.Fatalf("captured %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.From != "me@example.com" || strings.Join(m.To, ",") != "you@example.net,cc@example.net,hidden@example.net" {
		t.Errorf("envelope = %s -> %v", m.From, m.To)
	}
	p := parts(t, m.Data)
	if p["application/pdf"] != "%PDF-1.4 report" {
		t.Errorf("attachment = %q", p["application/pdf"])
	}
	if p["image/png"] != png || !strings.Contains(p["text/html"], "cid:") {
		t.Errorf("inline image missing: types %v, html %q", keysOf(p), p["text/html"])
	}
	if n := len(e.store.Appended()); n != 1 {
		t.Errorf("Sent copies = %d, want 1", n)
	}
	golden(t, "send_table", strings.ReplaceAll(got.stdout, messageID(t, m.Data), "MESSAGE-ID"))
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSendDerivedKeyMakesRepeatsDuplicates(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	attach := e.file(t, "notes.txt", "some notes")
	args := sendArgs("--body", "Hello", "--attach", attach)

	first := e.run(t, Env{}, append([]string{"--output", "json"}, args...)...)
	if first.code != 0 {
		t.Fatalf("first: %+v", first)
	}
	id := messageID(t, e.smtp.Messages()[0].Data)
	golden(t, "send_json", strings.ReplaceAll(first.stdout, id, "MESSAGE-ID"))

	second := e.run(t, Env{}, append([]string{"--output", "table"}, args...)...)
	if second.code != 0 {
		t.Fatalf("second: %+v", second)
	}
	golden(t, "send_duplicate_table", strings.ReplaceAll(second.stdout, id, "MESSAGE-ID"))
	if n := len(e.smtp.Messages()); n != 1 {
		t.Fatalf("captured %d messages after a repeat, want 1", n)
	}
	keys := e.keys(t)
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "cli-") || len(keys[0]) != len("cli-")+52 {
		t.Fatalf("keys = %q, want one cli- key of 52 base32 characters", keys)
	}

	// The same send with other attachment bytes, or another body, is a new message.
	if err := os.WriteFile(attach, []byte("other notes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := e.run(t, Env{}, args...); got.code != 0 || strings.Contains(got.stdout, `"duplicate":true`) {
		t.Fatalf("changed attachment: %+v", got)
	}
	if got := e.run(t, Env{}, sendArgs("--body", "Hello again", "--attach", attach)...); got.code != 0 || strings.Contains(got.stdout, `"duplicate":true`) {
		t.Fatalf("changed body: %+v", got)
	}
	if n := len(e.smtp.Messages()); n != 3 {
		t.Errorf("captured %d messages, want 3", n)
	}
	if keys := e.keys(t); len(keys) != 3 {
		t.Errorf("keys = %q, want 3", keys)
	}
}

func TestSendIdempotencyKeyOverride(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	if got := e.run(t, Env{}, sendArgs("--body", "Hello", "--idempotency-key", "my-key.1")...); got.code != 0 {
		t.Fatalf("send: %+v", got)
	}
	if keys := e.keys(t); len(keys) != 1 || keys[0] != "my-key.1" {
		t.Errorf("keys = %q, want [my-key.1]", keys)
	}
	// The key now names that settled send. Its outcome is replayed before
	// the request is read, so another body under it sends nothing and gets
	// the first result.
	got := e.run(t, Env{}, sendArgs("--body", "Different", "--idempotency-key", "my-key.1")...)
	if got.code != 0 || !strings.Contains(got.stdout, `"duplicate":true`) {
		t.Errorf("reused key: %+v", got)
	}
	if n := len(e.smtp.Messages()); n != 1 {
		t.Errorf("captured %d messages, want 1", n)
	}
}

func TestSendDryRun(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})

	raw := e.run(t, Env{}, sendArgs("--body", "Hello", "--dry-run")...)
	if raw.code != 0 || !strings.Contains(raw.stdout, "Subject: Quarterly\r\n") || strings.Contains(raw.stdout, "hidden@") {
		t.Fatalf("dry run to a pipe: %+v", raw)
	}

	tty := e.run(t, Env{StdoutTerminal: true}, sendArgs("--body", "Hello", "--dry-run")...)
	if tty.code != 2 || tty.stdout != "" || errorReason(t, tty.stderr) != "USAGE" {
		t.Errorf("dry run to a terminal: %+v", tty)
	}

	js := e.run(t, Env{StdoutTerminal: true}, append([]string{"--output", "json"}, sendArgs("--body", "Hello", "--dry-run")...)...)
	if js.code != 0 {
		t.Fatalf("dry run as JSON: %+v", js)
	}
	var out struct {
		Result struct {
			DryRun struct {
				Raw          string   `json:"raw"`
				EnvelopeFrom string   `json:"envelope_from"`
				EnvelopeTo   []string `json:"envelope_to"`
			} `json:"dry_run"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(js.stdout), &out); err != nil {
		t.Fatalf("dry run JSON %q: %v", js.stdout, err)
	}
	d := out.Result.DryRun
	if !strings.Contains(d.Raw, "Subject: Quarterly\r\n") || d.EnvelopeFrom != "me@example.com" || len(d.EnvelopeTo) != 3 {
		t.Errorf("dry run JSON = %+v", d)
	}

	if n := len(e.smtp.Messages()); n != 0 {
		t.Errorf("a dry run sent %d messages", n)
	}
	if keys := e.keys(t); len(keys) != 0 {
		t.Errorf("a dry run stored keys %q", keys)
	}
}

func TestSendBodyKinds(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	md := e.file(t, "body.md", "From a *file*")
	html := e.file(t, "body.html", "<p>Hi <b>there</b></p>")
	tests := []struct {
		name  string
		stdin string
		args  []string
		want  map[string]string // media type -> substring of its part
		only  bool              // the message has no other leaf parts
	}{
		{"markdown flag", "", []string{"--body", "Hello *world*"}, map[string]string{"text/plain": "Hello *world*", "text/html": "<em>world</em>"}, true},
		{"markdown file", "", []string{"--body-file", md}, map[string]string{"text/plain": "From a *file*", "text/html": "<em>file</em>"}, true},
		{"markdown stdin", "Piped *in*", []string{"--body", "-"}, map[string]string{"text/plain": "Piped *in*", "text/html": "<em>in</em>"}, true},
		{"text", "", []string{"--body", "Plain *as is*", "--text"}, map[string]string{"text/plain": "Plain *as is*"}, true},
		{"text from stdin", "piped text", []string{"--body", "-", "--text"}, map[string]string{"text/plain": "piped text"}, true},
		{"html with text part", "", []string{"--html-file", html, "--body", "the text part"}, map[string]string{"text/html": "<b>there</b>", "text/plain": "the text part"}, true},
		{"html alone", "", []string{"--html-file", html}, map[string]string{"text/html": "<b>there</b>", "text/plain": "there"}, true},
	}
	for _, tt := range tests {
		got := e.run(t, Env{Stdin: strings.NewReader(tt.stdin)}, sendArgs(append(tt.args, "--dry-run", "--no-signature")...)...)
		if got.code != 0 {
			t.Errorf("%s: %+v", tt.name, got)
			continue
		}
		p := parts(t, []byte(got.stdout))
		for mt, sub := range tt.want {
			if !strings.Contains(p[mt], sub) {
				t.Errorf("%s: %s part = %q, want %q in it", tt.name, mt, p[mt], sub)
			}
		}
		if tt.only && len(p) != len(tt.want) {
			t.Errorf("%s: parts %v, want only %v", tt.name, keysOf(p), tt.want)
		}
	}
}

func TestSendDraft(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	got := e.run(t, Env{}, append([]string{"--output", "table"}, sendArgs("--body", "Later", "--draft")...)...)
	if got.code != 0 || !strings.HasPrefix(got.stdout, "draft saved cm1:host:Drafts:") {
		t.Fatalf("draft: %+v", got)
	}
	if n := len(e.smtp.Messages()); n != 0 {
		t.Errorf("a draft sent %d messages", n)
	}
}

func TestSendOutcomeUnknownExitsEight(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{DropAfterDataEnd: true})
	got := e.run(t, Env{}, sendArgs("--body", "Hello")...)
	if got.code != 8 || errorReason(t, got.stderr) != "SEND_OUTCOME_UNKNOWN" {
		t.Errorf("send: %+v, want exit 8 SEND_OUTCOME_UNKNOWN", got)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want nothing", got.stdout)
	}
	// Repeating the same command sends nothing and says the same.
	again := e.run(t, Env{}, sendArgs("--body", "Hello")...)
	if again.code != 8 || len(e.smtp.Messages()) != 1 {
		t.Errorf("repeat: %+v, %d messages", again, len(e.smtp.Messages()))
	}
}

const rawMessage = "From: Me <me@example.com>\r\nTo: you@example.net\r\nBcc: hidden@example.net\r\nSubject: Raw\r\nMessage-ID: <%s@example.com>\r\n\r\nraw body\r\n"

func TestSendRaw(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	path := e.file(t, "msg.eml", fmt.Sprintf(rawMessage, "file"))
	tests := []struct {
		name  string
		stdin string
		args  []string
	}{
		{"file", "", []string{"--file", path}},
		{"stdin", fmt.Sprintf(rawMessage, "stdin"), []string{"-"}},
	}
	for i, tt := range tests {
		got := e.run(t, Env{Stdin: strings.NewReader(tt.stdin)}, append([]string{"--output", "json", "send-raw", "--account", "host"}, tt.args...)...)
		if got.code != 0 || !strings.Contains(got.stdout, `"message_id":"`+tt.name+`@example.com"`) {
			t.Fatalf("%s: %+v", tt.name, got)
		}
		msgs := e.smtp.Messages()
		if len(msgs) != i+1 {
			t.Fatalf("%s: captured %d messages", tt.name, len(msgs))
		}
		m := msgs[i]
		if strings.Join(m.To, ",") != "you@example.net,hidden@example.net" || bytes.Contains(m.Data, []byte("Bcc:")) {
			t.Errorf("%s: envelope %v, data %q", tt.name, m.To, m.Data)
		}
	}
	// The derived key makes a repeat of the same bytes a duplicate.
	again := e.run(t, Env{}, "--output", "table", "send-raw", "--account", "host", "--file", path)
	if again.code != 0 || !strings.Contains(again.stdout, "duplicate: already sent") || len(e.smtp.Messages()) != 2 {
		t.Errorf("repeat: %+v", again)
	}
}

func TestComposeUsageErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := filepath.Join(dir, "b.md")
	if err := os.WriteFile(body, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		args []string
	}{
		{"no account", []string{"send", "--to", "a@b.example", "--subject", "s", "--body", "x"}},
		{"no recipient", []string{"send", "--account", "host", "--subject", "s", "--body", "x"}},
		{"no body", sendArgs()},
		{"body and body file", sendArgs("--body", "x", "--body-file", body)},
		{"text with html", sendArgs("--body", "x", "--text", "--html-file", body)},
		{"text without body", sendArgs("--text", "--html-file", body)},
		{"dry run and draft", sendArgs("--body", "x", "--dry-run", "--draft")},
		{"inline without name", sendArgs("--body", "x", "--inline", body)},
		{"inline empty name", sendArgs("--body", "x", "--inline", "="+body)},
		{"missing attachment", sendArgs("--body", "x", "--attach", filepath.Join(dir, "nope"))},
		{"missing body file", sendArgs("--body-file", filepath.Join(dir, "nope"))},
		{"positional args", append(sendArgs("--body", "x"), "extra")},
		{"raw without input", []string{"send-raw", "--account", "host"}},
		{"raw with both", []string{"send-raw", "--account", "host", "--file", body, "-"}},
		{"raw other arg", []string{"send-raw", "--account", "host", "msg.eml"}},
		{"raw no account", []string{"send-raw", "--file", body}},
	}
	for _, tt := range tests {
		// No daemon listens here, so a command that got as far as calling one exits 7.
		got := runCLI(t, Env{}, append([]string{"--socket", filepath.Join(dir, "none.sock")}, tt.args...)...)
		if got.code != 2 || errorReason(t, got.stderr) != "USAGE" {
			t.Errorf("%s: %+v, want a usage error", tt.name, got)
		}
	}
}

func TestDeriveKey(t *testing.T) {
	t.Parallel()
	base := keyInput{
		account: "host", to: []string{"b@x.example", "a@x.example"}, cc: []string{"c@x.example"},
		subject: "s", content: [32]byte{1},
	}
	key := deriveKey(base)
	if !strings.HasPrefix(key, "cli-") || len(key) != 56 {
		t.Fatalf("key = %q", key)
	}
	if deriveKey(base) != key {
		t.Error("the key is not stable")
	}
	reordered := base
	reordered.to = []string{"a@x.example", "b@x.example"}
	if deriveKey(reordered) != key {
		t.Error("recipient order changed the key")
	}
	for name, change := range map[string]func(*keyInput){
		"account": func(k *keyInput) { k.account = "other" },
		"to":      func(k *keyInput) { k.to = []string{"a@x.example"} },
		"to->cc":  func(k *keyInput) { k.to, k.cc = []string{"a@x.example"}, []string{"b@x.example", "c@x.example"} },
		"bcc":     func(k *keyInput) { k.bcc = []string{"d@x.example"} },
		"subject": func(k *keyInput) { k.subject = "t" },
		"content": func(k *keyInput) { k.content = [32]byte{2} },
		"raw":     func(k *keyInput) { k.raw = []byte("x") },
		"ref":     func(k *keyInput) { k.ref = "host:INBOX:1:1" },
		"mode":    func(k *keyInput) { k.mode = "reply-all" },
		"split":   func(k *keyInput) { k.subject, k.ref = "", "s" },
	} {
		in := base
		in.to = append([]string(nil), base.to...)
		change(&in)
		if deriveKey(in) == key {
			t.Errorf("changing %s kept the key", name)
		}
	}
}

func TestUploadAllBoundsConcurrency(t *testing.T) {
	t.Parallel()
	files := make([]uploadFile, 10)
	for i := range files {
		files[i] = uploadFile{path: fmt.Sprintf("f%d", i)}
	}
	var running, peak atomic.Int32
	// overlap closes once two uploads run at once. Each upload waits for it,
	// so the overlap does not depend on how the scheduler runs them.
	overlap := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	seen := map[string]bool{}
	got, err := uploadAll(t.Context(), files, func(_ context.Context, f uploadFile) (uploaded, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if n >= 2 {
			once.Do(func() { close(overlap) })
		}
		select {
		case <-overlap:
		case <-time.After(10 * time.Second):
			t.Error("no two uploads ran at once")
		}
		mu.Lock()
		seen[f.path] = true
		mu.Unlock()
		return uploaded{handle: "h-" + f.path}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p > maxUploads || p < 2 {
		t.Errorf("peak concurrency = %d, want 2 to %d", p, maxUploads)
	}
	for i, u := range got {
		if u.handle != "h-"+files[i].path {
			t.Errorf("result %d = %q, out of order", i, u.handle)
		}
	}
	if len(seen) != len(files) {
		t.Errorf("uploaded %d of %d", len(seen), len(files))
	}
}

func TestUploadAllStopsOnError(t *testing.T) {
	t.Parallel()
	files := make([]uploadFile, 20)
	var calls atomic.Int32
	_, err := uploadAll(t.Context(), files, func(ctx context.Context, _ uploadFile) (uploaded, error) {
		if calls.Add(1) == 1 {
			return uploaded{}, fmt.Errorf("boom")
		}
		<-ctx.Done()
		return uploaded{}, ctx.Err()
	})
	if err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want the first failure", err)
	}
	if n := calls.Load(); n == int32(len(files)) {
		t.Errorf("all %d uploads started after a failure", n)
	}
}

func TestSendTableReportsSentCopyError(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	e.store.FailWrite("Append", errors.New("quota exceeded"))
	got := e.run(t, Env{}, append([]string{"--output", "table"}, sendArgs("--body", "Hello")...)...)
	if got.code != 0 {
		t.Fatalf("send: %+v", got)
	}
	id := messageID(t, e.smtp.Messages()[0].Data)
	golden(t, "send_copy_error_table", strings.ReplaceAll(got.stdout, id, "MESSAGE-ID"))
}

// A draft sent as is keeps the Message-ID it was saved with, which any
// program could have written, so the terminal output cleans it.
func TestComposeResultCleansMessageIDOnTerminal(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	env := Env{Stdout: &out, StdoutTerminal: true}
	g := &globals{output: "table"}
	res := &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Sent{Sent: &chainmailv1.Sent{
		MessageId: "evil\x1b]0;pwned\x07\u202e@example.com", SentRef: "cm1:a:Sent:1:2",
	}}}
	if err := writeComposeResult(env, g, false, res, res); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.ContainsAny(s, "\x1b\x07\u202e") {
		t.Errorf("terminal output = %q, still holds control or bidi characters", s)
	}
}

// A draft the server stored without naming it has no ref. The table output
// says where to find it instead of printing an empty ref.
func TestDraftWithoutARef(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	res := &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Draft{Draft: &chainmailv1.Draft{MessageId: "d.1@example.com"}}}
	if err := writeComposeResult(Env{Stdout: &out}, &globals{output: "table"}, false, &chainmailv1.SendResponse{Result: res}, res); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "search the Drafts folder") || !strings.Contains(got, "<d.1@example.com>") {
		t.Errorf("output = %q", got)
	}
}

// A draft the daemon names a hint for prints that hint, cleaned for the
// terminal.
func TestDraftWithoutARefPrintsTheHint(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := &chainmailv1.Draft{MessageId: "d.1@example.com", Hint: "look in the Drafts folder\x1b]0;x\x07"}
	res := &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Draft{Draft: d}}
	if err := writeComposeResult(Env{Stdout: &out}, &globals{output: "table"}, false, &chainmailv1.SendResponse{Result: res}, res); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "draft saved; look in the Drafts folder") || strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("output = %q, want the hint without control characters", got)
	}
}

// A held send prints where the draft went, the blocked recipients and
// whether there were too many, each cleaned for the terminal.
func TestComposeResultHeldTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		held *chainmailv1.Held
		want string
	}{
		{
			name: "with a ref",
			held: &chainmailv1.Held{Draft: &chainmailv1.Draft{Ref: "cm1:a:Drafts:1:2", MessageId: "h.1@example.com"}},
			want: "held by policy, not sent: saved to Drafts cm1:a:Drafts:1:2\n",
		},
		{
			name: "empty ref prints the hint",
			held: &chainmailv1.Held{
				Draft: &chainmailv1.Draft{MessageId: "h.1@example.com"},
				Hint:  "held by policy: saved to Drafts for James to review and send\x1b]0;x\x07",
			},
			// termText drops the control bytes and keeps the printable rest.
			want: "held by policy, not sent: held by policy: saved to Drafts for James to review and send]0;x\n",
		},
		{
			name: "empty ref prints the draft's own hint first",
			held: &chainmailv1.Held{
				Draft: &chainmailv1.Draft{MessageId: "h.1@example.com", Hint: "search the Drafts folder for h.1@example.com"},
				Hint:  "held by policy: saved to Drafts for James to review and send",
			},
			want: "held by policy, not sent: search the Drafts folder for h.1@example.com\n",
		},
		{
			name: "no ref and no hint names the Message-ID",
			held: &chainmailv1.Held{Draft: &chainmailv1.Draft{MessageId: "h.1@example.com"}},
			want: "held by policy, not sent: search the Drafts folder for Message-ID <h.1@example.com>\n",
		},
		{
			name: "blocked and too many recipients",
			held: &chainmailv1.Held{
				Draft:             &chainmailv1.Draft{Ref: "cm1:a:Drafts:1:3"},
				Blocked:           []string{"x@evil.example", "Y <y@other.example>\u202e"},
				TooManyRecipients: true,
			},
			want: "held by policy, not sent: saved to Drafts cm1:a:Drafts:1:3\n" +
				"blocked: x@evil.example, Y <y@other.example>\n" +
				"too many recipients\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			res := &chainmailv1.ComposeResult{Result: &chainmailv1.ComposeResult_Held{Held: tt.held}}
			err := writeComposeResult(Env{Stdout: &out, StdoutTerminal: true}, &globals{output: "table"}, false, &chainmailv1.SendResponse{Result: res}, res)
			if err == nil {
				t.Fatal("err = nil, want HELD")
			}
			if c := classify(err); c.code != api.ExitHeld || c.reason != api.ReasonHeld || c.hint != tt.held.GetHint() {
				t.Errorf("err = %v (%+v), want HELD, exit 4 and the held hint", err, c)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

// A send the policy holds prints its result as usual on stdout, and
// reports HELD on stderr with exit code 4. So does sending a held draft.
func TestSendHeldExitsFour(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	args := []string{"send", "--account", "host", "--to", "stranger@blocked.invalid", "--subject", "Quarterly", "--body", "Hello"}

	got := e.run(t, Env{}, append([]string{"--output", "json"}, args...)...)
	if got.code != 4 {
		t.Fatalf("exit code = %d, want 4: %+v", got.code, got)
	}
	var resp chainmailv1.SendResponse
	if err := protojson.Unmarshal([]byte(got.stdout), &resp); err != nil {
		t.Fatalf("stdout %q: %v", got.stdout, err)
	}
	held := resp.GetResult().GetHeld()
	if held == nil || held.GetDraft().GetRef() == "" || strings.Join(held.GetBlocked(), ",") != "stranger@blocked.invalid" {
		t.Fatalf("result = %+v, want held", resp.GetResult())
	}
	var report struct {
		Error struct{ Reason, Message, Hint string }
	}
	if err := json.Unmarshal([]byte(got.stderr), &report); err != nil {
		t.Fatalf("stderr %q: %v", got.stderr, err)
	}
	if report.Error.Reason != "HELD" || report.Error.Hint != held.GetHint() || report.Error.Hint == "" {
		t.Errorf("stderr = %+v, want reason HELD with the result's hint", report)
	}
	if n := len(e.smtp.Messages()); n != 0 {
		t.Errorf("captured %d messages, want none", n)
	}

	table := e.run(t, Env{}, append([]string{"--output", "table"}, args...)...)
	if table.code != 4 || !strings.HasPrefix(table.stdout, "held by policy, not sent: saved to Drafts ") || !strings.Contains(table.stderr, "HELD") {
		t.Errorf("table: %+v, want the held text, HELD and exit 4", table)
	}

	sent := e.run(t, Env{}, "--output", "json", "draft", "send", held.GetDraft().GetRef())
	if sent.code != 4 || !strings.Contains(sent.stdout, `"held"`) || !strings.Contains(sent.stderr, `"HELD"`) {
		t.Errorf("draft send: %+v, want held and exit 4", sent)
	}

	dry := e.run(t, Env{}, append([]string{"--output", "json"}, append(args, "--dry-run")...)...)
	if dry.code != 0 || dry.stderr != "" {
		t.Errorf("dry run: %+v, want exit 0", dry)
	}
	if n := len(e.smtp.Messages()); n != 0 {
		t.Errorf("captured %d messages, want none", n)
	}
}

// json chosen through CHAINMAIL_OUTPUT or cli.toml is as explicit as
// --output json: a dry run then prints JSON, even to a terminal.
func TestDryRunJSONFromSettings(t *testing.T) {
	t.Parallel()
	e := startCLISend(t, smtptest.Options{})
	fromEnv := e.run(t, Env{StdoutTerminal: true, Getenv: vars(map[string]string{"CHAINMAIL_OUTPUT": "json"})}, sendArgs("--body", "Hello", "--dry-run")...)
	xdg := writeCLIConfig(t, `output = "json"`)
	fromFile := e.run(t, Env{StdoutTerminal: true, Getenv: vars(map[string]string{"XDG_CONFIG_HOME": xdg})}, sendArgs("--body", "Hello", "--dry-run")...)
	for name, got := range map[string]cliResult{"env": fromEnv, "cli.toml": fromFile} {
		var out dryRunJSON
		if got.code != 0 || json.Unmarshal([]byte(got.stdout), &out) != nil || !strings.Contains(out.Result.DryRun.Raw, "Subject: Quarterly\r\n") {
			t.Errorf("json from %s: %+v, want a JSON dry run", name, got)
		}
	}
}
