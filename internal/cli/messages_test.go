package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/mailstore/fake"
	"github.com/jamesainslie/chainmail/internal/ref"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
)

const cliMailConfig = `
[accounts.host]
provider = "hostinger"
address = "me@example.com"

[accounts.gmail]
provider = "gmail"
address = "me@gmail.example"
auth = "password"
`

var cliNow = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

type cliMail struct {
	socket         string
	host, gmail    *fake.Store
	invoice, lunch ref.Ref
}

func startCLIMail(t *testing.T) cliMail {
	t.Helper()
	m := cliMail{host: fake.New("host", false), gmail: fake.New("gmail", true)}
	h := daemontest.Start(t, cliMailConfig, daemontest.Options{
		NewStore: func(a config.ResolvedAccount) mailstore.Store {
			if a.Name == "gmail" {
				return m.gmail
			}
			return m.host
		},
		Now: func() time.Time { return cliNow },
	})
	m.socket = h.Socket
	m.invoice = m.host.Add(fake.Message{
		Folder: "INBOX", Date: cliNow.Add(-26 * time.Hour), Subject: "Invoice 7", MessageID: "inv@acme.example",
		From: []mailstore.Address{{Name: "Acme Billing", Email: "billing@acme.example"}}, To: []mailstore.Address{{Email: "me@example.com"}},
		Body: "Please pay by Friday.\n\nOn Mon, 26 Oct 2026 at 09:00, Alex <a@example.com> wrote:\n> old",
	})
	m.lunch = m.gmail.Add(fake.Message{
		Folder: "[Gmail]/All Mail", Date: cliNow.Add(-2 * time.Hour), Subject: "Lunch", MessageID: "l@gmail.example", Labels: []string{"Friends"},
		From: []mailstore.Address{{Name: "Sam", Email: "sam@example.org"}}, To: []mailstore.Address{{Email: "me@gmail.example"}},
		Body: "Noon?", Attachments: []fake.Attachment{{Part: "2", Filename: "menu.pdf", ContentType: "application/pdf", Data: []byte("%PDF-menu")}},
	})
	return m
}

func (m cliMail) run(t *testing.T, env Env, args ...string) cliResult {
	t.Helper()
	return runCLI(t, env, append([]string{"--socket", m.socket}, args...)...)
}

// staleRef names the host INBOX under a UIDVALIDITY it never had.
var staleRef = ref.Ref{Account: "host", Folder: "INBOX", UIDValidity: 9, UID: 1}.String()

func TestMessageCommandsGolden(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	tests := []struct {
		name string
		args []string
	}{
		{"folders_json", []string{"--output", "json", "folders"}},
		{"folders_table", []string{"--output", "table", "folders"}},
		{"search_json", []string{"--output", "json", "search"}},
		{"search_table", []string{"--output", "table", "search"}},
		{"search_count_table", []string{"--output", "table", "search", "--count"}},
		{"get_json", []string{"--output", "json", "get", m.invoice.String()}},
		{"get_text", []string{"--output", "table", "get", m.invoice.String(), m.lunch.String()}},
		{"get_item_error_json", []string{"--output", "json", "get", m.invoice.String(), staleRef}},
		{"get_item_error_text", []string{"--output", "table", "get", m.invoice.String(), staleRef}},
		{"thread_json", []string{"--output", "json", "thread", m.invoice.String()}},
		{"thread_text", []string{"--output", "table", "thread", m.invoice.String()}},
	}
	for _, tt := range tests {
		got := m.run(t, Env{}, tt.args...)
		if got.code != 0 {
			t.Fatalf("%s: exit %d, stderr %s", tt.name, got.code, got.stderr)
		}
		golden(t, tt.name, got.stdout)
	}
}

func TestSearchJSONShape(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	got := m.run(t, Env{}, "search", "invoice", "from:acme")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	var resp struct {
		Rows []struct {
			Ref     string `json:"ref"`
			Subject struct {
				Untrusted string `json:"untrusted"`
			} `json:"subject"`
			From []struct {
				Name struct {
					Untrusted string `json:"untrusted"`
				} `json:"name"`
			} `json:"from"`
		} `json:"rows"`
		Source    string `json:"source"`
		FetchedAt string `json:"fetched_at"`
		Count     string `json:"count"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &resp); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, got.stdout)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].Ref != m.invoice.String() || resp.Rows[0].Subject.Untrusted != "Invoice 7" ||
		resp.Rows[0].From[0].Name.Untrusted != "Acme Billing" {
		t.Errorf("rows = %+v", resp.Rows)
	}
	if resp.Source != "server" || resp.FetchedAt != "2026-10-29T12:00:00Z" || resp.Count != "1" {
		t.Errorf("source %q, fetched_at %q, count %q", resp.Source, resp.FetchedAt, resp.Count)
	}
}

// A query term starting with - reads as a flag unless it follows --.
func TestSearchNegationAfterDoubleDash(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	got := m.run(t, Env{}, "--output", "table", "search", "--", "-from:acme")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "Lunch") || strings.Contains(got.stdout, "Invoice 7") {
		t.Errorf("stdout = %s", got.stdout)
	}
}

func TestMessageCommandExitCodes(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	tooMany := make([]string, 51)
	for i := range tooMany {
		tooMany[i] = m.invoice.String()
	}
	tests := []struct {
		name   string
		env    Env
		args   []string
		code   int
		reason string
	}{
		{"stale only", Env{}, []string{"get", staleRef}, 6, "STALE_REF"},
		{"stale only, as text", Env{}, []string{"--output", "table", "get", staleRef}, 6, "STALE_REF"},
		{"one good one stale", Env{}, []string{"get", m.invoice.String(), staleRef}, 0, ""},
		{"get without refs", Env{}, []string{"get"}, 2, "USAGE"},
		{"get with too many refs", Env{}, append([]string{"get"}, tooMany...), 2, "INVALID_ARGUMENT"},
		{"raw with two refs", Env{}, []string{"get", "--raw", m.invoice.String(), staleRef}, 2, "USAGE"},
		{"raw to a terminal", Env{StdoutTerminal: true}, []string{"get", "--raw", m.invoice.String()}, 2, "USAGE"},
		{"raw with full", Env{}, []string{"get", "--raw", "--full", m.invoice.String()}, 2, "USAGE"},
		{"raw stale", Env{}, []string{"get", "--raw", staleRef}, 6, "STALE_REF"},
		{"bad query", Env{}, []string{"search", "foo:bar"}, 2, "INVALID_ARGUMENT"},
		{"unknown account", Env{}, []string{"search", "--account", "nope"}, 6, "NOT_FOUND"},
		{"unknown folders account", Env{}, []string{"folders", "--account", "nope"}, 6, "NOT_FOUND"},
		{"folders argument", Env{}, []string{"folders", "extra"}, 2, "USAGE"},
		{"thread stale", Env{}, []string{"thread", staleRef}, 6, "STALE_REF"},
		{"thread without ref", Env{}, []string{"thread"}, 2, "USAGE"},
		{"thread with two refs", Env{}, []string{"thread", m.invoice.String(), m.invoice.String()}, 2, "USAGE"},
		{"attachment without part", Env{}, []string{"attachment", m.lunch.String(), "--out", "-"}, 2, "USAGE"},
		{"attachment without out", Env{}, []string{"attachment", m.lunch.String(), "2"}, 2, "USAGE"},
		{"attachment to a terminal", Env{StdoutTerminal: true}, []string{"attachment", m.lunch.String(), "2", "--out", "-"}, 2, "USAGE"},
	}
	for _, tt := range tests {
		got := m.run(t, tt.env, tt.args...)
		if got.code != tt.code {
			t.Errorf("%s: exit %d, want %d (stderr %s)", tt.name, got.code, tt.code, got.stderr)
		}
		if tt.reason != "" && errorReason(t, got.stderr) != tt.reason {
			t.Errorf("%s: reason %s, want %s", tt.name, errorReason(t, got.stderr), tt.reason)
		}
	}
}

func TestGetRawWritesTheMessage(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	got := m.run(t, Env{}, "get", "--raw", m.invoice.String())
	if got.code != 0 || got.stdout != string(m.host.RawMessage(m.invoice)) {
		t.Errorf("exit %d, %d bytes; want the raw message", got.code, len(got.stdout))
	}
}

func TestAttachmentCommand(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "menu.pdf")
	got := m.run(t, Env{}, "attachment", m.lunch.String(), "2", "--out", path)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "%PDF-menu" {
		t.Errorf("file = %q, %v", data, err)
	}
	want := `{"bytes":9,"content_type":"application/pdf","filename":{"untrusted":"menu.pdf"},"path":"` + path + `"}` + "\n"
	if got.stdout != want {
		t.Errorf("stdout = %s, want %s", got.stdout, want)
	}
	tablePath := filepath.Join(dir, "menu-copy.pdf")
	table := m.run(t, Env{StdoutTerminal: true}, "attachment", m.lunch.String(), "2", "--out", tablePath)
	if table.code != 0 || table.stdout != "saved 9 bytes to "+tablePath+"\n" {
		t.Errorf("on a terminal: exit %d, stdout %q", table.code, table.stdout)
	}
	piped := m.run(t, Env{}, "attachment", m.lunch.String(), "2", "--out", "-")
	if piped.code != 0 || piped.stdout != "%PDF-menu" {
		t.Errorf("--out - = exit %d, %q", piped.code, piped.stdout)
	}
	missing := m.run(t, Env{}, "attachment", m.lunch.String(), "9", "--out", filepath.Join(dir, "nine"))
	if missing.code != 6 {
		t.Errorf("missing part: exit %d", missing.code)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"menu-copy.pdf", "menu.pdf"}) {
		t.Errorf("files left behind: %v", names)
	}
}

// A stream that fails after the header leaves no file, temporary or final.
func TestAttachmentFailedStreamLeavesNothing(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	m.gmail.FailStreamAfter(3, mailstore.ErrUnavailable)
	dir := t.TempDir()
	got := m.run(t, Env{}, "attachment", m.lunch.String(), "2", "--out", filepath.Join(dir, "menu.pdf"))
	if got.code != 7 || errorReason(t, got.stderr) != "SERVER_UNAVAILABLE" {
		t.Errorf("exit %d, stdout %q, stderr %s", got.code, got.stdout, got.stderr)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("files left behind: %v (%v)", entries, err)
	}
}

func TestOutputDefaultsFollowTheTerminal(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{})
	tests := []struct {
		name   string
		env    Env
		args   []string
		prefix string
	}{
		{"terminal", Env{StdoutTerminal: true}, []string{"accounts", "list"}, "NAME"},
		{"pipe", Env{}, []string{"accounts", "list"}, "{"},
		{"explicit json on a terminal", Env{StdoutTerminal: true}, []string{"--output", "json", "accounts", "list"}, "{"},
		{"explicit table in a pipe", Env{}, []string{"--output", "table", "accounts", "list"}, "NAME"},
		{"status on a terminal", Env{StdoutTerminal: true}, []string{"admin", "status"}, "version "},
		{"status in a pipe", Env{}, []string{"admin", "status"}, "{"},
		{"secret on a terminal", Env{StdoutTerminal: true, Stdin: strings.NewReader("pw\n")}, []string{"secret", "set", "accounts/host"}, "stored secret accounts/host\n"},
	}
	for _, tt := range tests {
		got := runCLI(t, tt.env, append([]string{"--socket", h.Socket}, tt.args...)...)
		if got.code != 0 || !strings.HasPrefix(got.stdout, tt.prefix) {
			t.Errorf("%s: exit %d, stdout %q, want prefix %q", tt.name, got.code, got.stdout, tt.prefix)
		}
	}
	if got := runCLI(t, Env{StdoutTerminal: true}, "version"); got.stdout != "chainmail dev\n" {
		t.Errorf("version on a terminal = %q", got.stdout)
	}
	if got := runCLI(t, Env{}, "version"); got.stdout != `{"version":"dev"}`+"\n" {
		t.Errorf("version in a pipe = %q", got.stdout)
	}
}

func TestMessageCommandsDefaultToTextOnATerminal(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	tests := []struct {
		args   []string
		prefix string
	}{
		{[]string{"folders"}, "ACCOUNT "},
		{[]string{"search"}, "DATE "},
		{[]string{"search", "--count"}, "ACCOUNT "},
		{[]string{"get", m.invoice.String()}, "From: Acme Billing"},
		{[]string{"thread", m.invoice.String()}, "From: Acme Billing"},
	}
	for _, tt := range tests {
		for _, terminal := range []bool{true, false} {
			want := tt.prefix
			if !terminal {
				want = "{"
			}
			got := m.run(t, Env{StdoutTerminal: terminal}, tt.args...)
			if got.code != 0 || !strings.HasPrefix(got.stdout, want) {
				t.Errorf("%v (terminal %t): exit %d, stdout %q, want prefix %q", tt.args, terminal, got.code, got.stdout, want)
			}
		}
	}
}

func TestSearchTableFlags(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	got := m.run(t, Env{}, "--output", "table", "search", "--snippet", "--limit", "1")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	lines := strings.Split(strings.TrimSuffix(got.stdout, "\n"), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], "SNIPPET") || !strings.Contains(lines[1], "Noon?") || !strings.HasPrefix(lines[2], "more: --cursor ") {
		t.Fatalf("stdout = %s", got.stdout)
	}
	next := m.run(t, Env{}, "--output", "table", "search", "--limit", "1", "--cursor", strings.TrimPrefix(lines[2], "more: --cursor "))
	if next.code != 0 || !strings.Contains(next.stdout, "Invoice 7") || strings.Contains(next.stdout, "Lunch") {
		t.Errorf("next page: exit %d, stdout %s, stderr %s", next.code, next.stdout, next.stderr)
	}
	byAccount := m.run(t, Env{}, "--output", "table", "search", "--account", "host", "--account", "gmail", "--threads")
	if byAccount.code != 0 || !strings.Contains(byAccount.stdout, "Invoice 7") || !strings.Contains(byAccount.stdout, "Lunch") {
		t.Errorf("repeated --account: exit %d, stdout %s, stderr %s", byAccount.code, byAccount.stdout, byAccount.stderr)
	}
}

func TestThreadTextShowsOmittedRefs(t *testing.T) {
	t.Parallel()
	date := timestamppb.New(cliNow)
	msg := func(r, subject string) *chainmailv1.Message {
		return &chainmailv1.Message{
			Ref: r, Date: date, Subject: &chainmailv1.Untrusted{Untrusted: subject},
			From: []*chainmailv1.Address{{Email: &chainmailv1.Untrusted{Untrusted: "a@example.com"}}},
			To:   []*chainmailv1.Address{{Email: &chainmailv1.Untrusted{Untrusted: "b@example.com"}}},
			Body: &chainmailv1.Body{Untrusted: "big"},
		}
	}
	resp := &chainmailv1.GetThreadResponse{
		Messages:    []*chainmailv1.Message{msg("cm1:host:INBOX:1:1", "Plans"), msg("cm1:host:INBOX:1:2", "Re: Plans")},
		Truncated:   true,
		OmittedRefs: []string{"cm1:host:INBOX:1:3", "cm1:host:INBOX:1:4"},
	}
	var buf bytes.Buffer
	if err := threadText(&buf, time.UTC, resp, false); err != nil {
		t.Fatal(err)
	}
	golden(t, "thread_truncated_text", buf.String())
	buf.Reset()
	if err := threadText(&buf, time.UTC, resp, true); err != nil {
		t.Fatal(err)
	}
	if want := "\n  chainmail get --full cm1:host:INBOX:1:3 cm1:host:INBOX:1:4\n"; !strings.HasSuffix(buf.String(), want) {
		t.Errorf("--full thread text ends %q, want %q", buf.String()[max(0, buf.Len()-120):], want)
	}
}

// Render may trim a hostile body down to nothing. That is still a message.
func TestMessageTextFlagsAnEmptyTrimmedBody(t *testing.T) {
	t.Parallel()
	items := []*chainmailv1.GetItem{{Ref: "cm1:host:INBOX:1:1", Result: &chainmailv1.GetItem_Message{Message: &chainmailv1.Message{
		Ref: "cm1:host:INBOX:1:1", Body: &chainmailv1.Body{QuotedTrimmed: true, TrimmedBytes: 40},
	}}}}
	var buf bytes.Buffer
	if err := messagesText(&buf, time.UTC, items); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "[quoted text trimmed; --full shows it]") || strings.Contains(buf.String(), "error") {
		t.Errorf("text = %q", buf.String())
	}
	if err := itemsResult(items); err != nil {
		t.Errorf("itemsResult = %v", err)
	}
}

func TestItemsResult(t *testing.T) {
	t.Parallel()
	errItem := func(reason string) *chainmailv1.GetItem {
		return &chainmailv1.GetItem{Ref: "r", Result: &chainmailv1.GetItem_Error{Error: &chainmailv1.ItemError{Reason: reason, Message: "m", Hint: "h"}}}
	}
	ok := &chainmailv1.GetItem{Ref: "r", Result: &chainmailv1.GetItem_Message{Message: &chainmailv1.Message{}}}
	tests := []struct {
		name   string
		items  []*chainmailv1.GetItem
		code   int
		reason string
	}{
		{"none", nil, 0, ""},
		{"all good", []*chainmailv1.GetItem{ok}, 0, ""},
		{"one good", []*chainmailv1.GetItem{errItem("STALE_REF"), ok}, 0, ""},
		{"all failed", []*chainmailv1.GetItem{errItem("INVALID_ARGUMENT"), errItem("STALE_REF")}, 2, "INVALID_ARGUMENT"},
		{"no reason", []*chainmailv1.GetItem{errItem("")}, 1, "INTERNAL"},
	}
	for _, tt := range tests {
		err := itemsResult(tt.items)
		if tt.code == 0 {
			if err != nil {
				t.Errorf("%s: %v", tt.name, err)
			}
			continue
		}
		if c := classify(err); c.code != tt.code || c.reason != tt.reason || c.hint != "h" {
			t.Errorf("%s: classify = %+v", tt.name, c)
		}
	}
}

func TestClip(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"eleven chars", 10, "eleven ..."},
		{"ééééééééééé", 5, "éé..."},
	} {
		if got := clip(tt.in, tt.n); got != tt.want {
			t.Errorf("clip(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
	}
}

// An empty --output is a mistake, not a request for the default.
func TestOutputFlagRejectsEmpty(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--output", "", "version"}, {"--output=", "version"}} {
		got := runCLI(t, Env{StdoutTerminal: true}, args...)
		if got.code != 2 || errorReason(t, got.stderr) != "USAGE" {
			t.Errorf("%v: exit %d, stderr %s", args, got.code, got.stderr)
		}
	}
}
