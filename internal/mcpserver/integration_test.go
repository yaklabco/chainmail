//go:build integration

package mcpserver_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/testutil/daemontest"
	"github.com/jamesainslie/chainmail/internal/testutil/dovecot"
	"github.com/jamesainslie/chainmail/internal/testutil/smtptest"
)

// mcpDoveConfig is one account on Dovecot's IMAP and an smtptest sink.
// Only *@allowed.test, and the account's own address, may be sent to.
const mcpDoveConfig = `
[accounts.dove]
provider = "custom"
address = %[1]q
imap = { host = "127.0.0.1", port = %[2]d, security = "tls" }
smtp = { host = "127.0.0.1", port = %[3]d, security = "starttls" }

[policy]
send_allow = ["*@allowed.test"]
`

// invoice is the message the agent finds, reads, files and archives.
const invoice = "From: Ann <ann@example.org>\r\nTo: mcp@example.test\r\nSubject: Invoice 42\r\n" +
	"Message-ID: <inv42@example.org>\r\nDate: Thu, 29 Oct 2026 10:00:00 +0000\r\n\r\nPlease pay invoice 42 by Friday.\r\n"

// buildChainmail compiles the binary under test from the module root.
func buildChainmail(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "chainmail")
	// -p 1 and GOMAXPROCS=1: this build runs beside the other integration
	// packages, and the outer go test flags do not bound it.
	cmd := exec.CommandContext(t.Context(), "go", "build", "-p", "1", "-o", bin, "./cmd/chainmail")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOMAXPROCS=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// session calls tools over one MCP client session and fails the test on any
// protocol error or error result.
type session struct {
	t  *testing.T
	cs *mcp.ClientSession
}

// call runs tool name with args and decodes its structured content into out.
func (s session) call(name string, args map[string]any, out any) {
	s.t.Helper()
	res, err := s.cs.CallTool(s.t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		s.t.Fatalf("%s: error result %v", name, textOf(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		s.t.Fatalf("%s: %s: %v", name, b, err)
	}
}

// textOf joins a result's text content for failure messages.
func textOf(res *mcp.CallToolResult) string {
	var b bytes.Buffer
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// startDovecot runs a Dovecot container for the test and removes it
// afterwards.
func startDovecot(t *testing.T) *dovecot.Server {
	t.Helper()
	dove, err := dovecot.Start()
	if err != nil {
		t.Fatalf("starting Dovecot: %v", err)
	}
	t.Cleanup(func() {
		if err := dove.Stop(); err != nil {
			t.Errorf("stopping Dovecot: %v", err)
		}
	})
	return dove
}

// connectMCP starts bin as chainmail mcp against the daemon at socket, the
// way an agent's MCP client starts it, and returns a session on it. The
// test's cleanup closes the session, and logs the process's stderr when
// the test failed.
func connectMCP(t *testing.T, bin, socket string) session {
	t.Helper()
	cmd := exec.Command(bin, "--socket", socket, "mcp")
	cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	c := mcp.NewClient(&mcp.Implementation{Name: "integration", Version: "0"},
		&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	cs, err := c.Connect(t.Context(), &mcp.CommandTransport{Command: cmd},
		&mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		t.Fatalf("connecting to chainmail mcp: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() {
		_ = cs.Close() // closes stdin and waits for the process
		if t.Failed() {
			t.Logf("chainmail mcp stderr:\n%s", stderr.String())
		}
	})
	if v := cs.InitializeResult().ProtocolVersion; v != "2026-07-28" {
		t.Errorf("negotiated %q, want 2026-07-28", v)
	}
	return session{t: t, cs: cs}
}

// TestMCPAgainstDovecot runs the built binary as chainmail mcp, the way an
// agent's MCP client starts it, against a real daemon, Dovecot and an SMTP
// sink. The agent searches, reads, files and archives a message, saves a
// draft, and has a send off the allowlist held in Drafts. The audit log
// records every call with client kind mcp.
func TestMCPAgainstDovecot(t *testing.T) {
	bin := buildChainmail(t)
	dove := startDovecot(t)
	const user = "mcp@example.test"
	sink := smtptest.StartWith(t, dove.CA, provider.SecurityStartTLS, smtptest.Options{},
		smtptest.User{Username: user, Password: dovecot.Password})
	h := daemontest.Start(t, fmt.Sprintf(mcpDoveConfig, user, dove.Endpoint.Port, sink.Endpoint().Port),
		daemontest.Options{RootCAs: dove.CA.Pool})
	if _, err := h.Client.Admin.SetSecret(t.Context(), connect.NewRequest(&chainmailv1.SetSecretRequest{
		Name: "accounts/dove", Value: []byte(dovecot.Password),
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := dove.Append(user, "INBOX", invoice, time.Now()); err != nil {
		t.Fatal(err)
	}

	s := connectMCP(t, bin, h.Socket)

	// 1. search and read.
	var found struct {
		Rows []struct {
			Ref     string                     `json:"ref"`
			Subject struct{ Untrusted string } `json:"subject"`
		} `json:"rows"`
	}
	s.call("search", map[string]any{"query": "subject:Invoice"}, &found)
	if len(found.Rows) != 1 || found.Rows[0].Subject.Untrusted != "Invoice 42" {
		t.Fatalf("search rows = %+v", found.Rows)
	}
	var got struct {
		Messages []struct {
			Body struct{ Untrusted string } `json:"body"`
		} `json:"messages"`
	}
	s.call("get_message", map[string]any{"refs": []string{found.Rows[0].Ref}}, &got)
	if len(got.Messages) != 1 || !bytes.Contains([]byte(got.Messages[0].Body.Untrusted), []byte("invoice 42")) {
		t.Fatalf("get_message = %+v", got)
	}

	// 2. organize: a new folder, a move into it, then archive.
	var folder struct {
		Name struct{ Untrusted string } `json:"name"`
	}
	s.call("create_folder", map[string]any{"account": "dove", "name": "Projects"}, &folder)
	var moved, archived struct {
		Outcomes []struct {
			NewRef string `json:"new_ref"`
		} `json:"outcomes"`
		Succeeded int `json:"succeeded"`
	}
	s.call("move", map[string]any{"refs": []string{found.Rows[0].Ref}, "folder": folder.Name.Untrusted}, &moved)
	if moved.Succeeded != 1 || moved.Outcomes[0].NewRef == "" {
		t.Fatalf("move = %+v", moved)
	}
	s.call("archive", map[string]any{"refs": []string{moved.Outcomes[0].NewRef}}, &archived)
	if archived.Succeeded != 1 {
		t.Fatalf("archive = %+v", archived)
	}

	// 3. a draft, then a send off the allowlist comes back held and lands in Drafts.
	var draft, held struct {
		Status   string `json:"status"`
		DraftRef string `json:"draft_ref"`
		Blocked  []struct {
			Untrusted string `json:"untrusted"`
		} `json:"blocked"`
	}
	s.call("create_draft", map[string]any{
		"account": "dove", "to": []string{"a@allowed.test"}, "subject": "Notes", "body": "wip",
	}, &draft)
	if draft.Status != "draft" || draft.DraftRef == "" {
		t.Fatalf("create_draft = %+v", draft)
	}
	s.call("send_email", map[string]any{
		"account": "dove", "to": []string{"b@blocked.test"}, "subject": "Held for James", "body": "Please review.",
	}, &held)
	if held.Status != "held" || held.DraftRef == "" || len(held.Blocked) != 1 || held.Blocked[0].Untrusted != "b@blocked.test" {
		t.Fatalf("send_email = %+v, want held", held)
	}
	if n := len(sink.Messages()); n != 0 {
		t.Fatalf("SMTP sink got %d messages, want none", n)
	}
	var drafts struct {
		Rows []struct {
			Ref string `json:"ref"`
		} `json:"rows"`
	}
	s.call("search", map[string]any{"query": `in:drafts subject:"Held for James"`}, &drafts)
	if len(drafts.Rows) != 1 || drafts.Rows[0].Ref != held.DraftRef {
		t.Fatalf("Drafts search = %+v, want the held draft %s", drafts.Rows, held.DraftRef)
	}

	// 4. the audit log names every MCP call with client kind mcp: each
	// exercised action has at least as many rows as calls made above.
	sawHeld := false
	byAction := map[string]int{}
	for _, r := range h.AuditRows(t) {
		if r.Action == "secret.set" {
			continue // set by the harness's own client
		}
		if r.Client != "mcp" {
			t.Errorf("audit row %+v has client %q, want mcp", r, r.Client)
		}
		byAction[r.Action]++
		if r.Action == "send" && r.Reason == "HELD" {
			sawHeld = true
		}
	}
	for action, want := range map[string]int{
		"search": 2, "get": 1, "folders.create": 1, "move": 1, "archive": 1, "draft.create": 1, "send": 1,
	} {
		if byAction[action] < want {
			t.Errorf("audit has %d %s rows, want at least %d (rows by action: %v)", byAction[action], action, want, byAction)
		}
	}
	if !sawHeld {
		t.Error("no audit row records the held send")
	}
}

// eventsPage is read_events' structured output (T8's eventsOut), as far
// as the test reads it.
type eventsPage struct {
	Events []struct {
		ID      int64                      `json:"id"`
		Kind    string                     `json:"kind"`
		Account string                     `json:"account"`
		Folder  struct{ Untrusted string } `json:"folder"`
		Ref     string                     `json:"ref"`
		Message struct {
			Subject struct{ Untrusted string } `json:"subject"`
		} `json:"message"`
	} `json:"events"`
	NextCursor int64 `json:"next_cursor"`
}

// waitInboxWatched polls GetWatchStatus until dove's INBOX is watched, so
// mail appended afterwards is new rather than part of the baseline.
func waitInboxWatched(t *testing.T, h *daemontest.Harness) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := h.Client.Watch.GetWatchStatus(t.Context(), connect.NewRequest(&chainmailv1.GetWatchStatusRequest{}))
		if err != nil {
			t.Fatalf("GetWatchStatus: %v", err)
		}
		for _, f := range resp.Msg.GetFolders() {
			if f.GetAccount() == "dove" && f.GetFolder() == "INBOX" && f.GetState() == "watching" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dove/INBOX not watching within 30s: %v", resp.Msg.GetFolders())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMCPReadEventsAgainstDovecot has an agent wait for mail the way spec
// section 4 describes: repeated read_events with a group and a wait. The
// built binary runs as chainmail mcp against a watching daemon on
// Dovecot. Mail delivered after the group exists comes back once, the
// group acknowledges it, and every read is audited as watch.read with
// client kind mcp.
func TestMCPReadEventsAgainstDovecot(t *testing.T) {
	bin := buildChainmail(t)
	dove := startDovecot(t)
	const user = "events@example.test"
	sink := smtptest.StartWith(t, dove.CA, provider.SecurityStartTLS, smtptest.Options{},
		smtptest.User{Username: user, Password: dovecot.Password})
	h := daemontest.Start(t, fmt.Sprintf(mcpDoveConfig, user, dove.Endpoint.Port, sink.Endpoint().Port),
		daemontest.Options{RootCAs: dove.CA.Pool, Watch: true, Secrets: map[string]string{"accounts/dove": dovecot.Password}})
	waitInboxWatched(t, h)
	s := connectMCP(t, bin, h.Socket)

	// 1. The first read creates the group at the end of the log.
	var page eventsPage
	s.call("read_events", map[string]any{"group": "agent"}, &page)

	// 2. Mail delivered now comes back from a waiting read. Status events
	// pass a kinds filter, so a read may return only those; read again.
	if _, err := dove.Append(user, "INBOX", invoice, time.Now()); err != nil {
		t.Fatal(err)
	}
	ref := ""
	deadline := time.Now().Add(30 * time.Second)
	for ref == "" {
		if time.Now().After(deadline) {
			t.Fatal("read_events returned no new event within 30s")
		}
		page = eventsPage{}
		s.call("read_events", map[string]any{"group": "agent", "kinds": []string{"new"}, "wait_seconds": 10}, &page)
		for _, e := range page.Events {
			if e.Kind != "new" {
				continue
			}
			if e.Account != "dove" || e.Folder.Untrusted != "INBOX" || e.Message.Subject.Untrusted != "Invoice 42" || e.Ref == "" || e.ID == 0 {
				t.Fatalf("new event = %+v, want Invoice 42 in dove/INBOX with a ref and an ID", e)
			}
			if page.NextCursor < e.ID {
				t.Errorf("next_cursor %d is below the returned event %d", page.NextCursor, e.ID)
			}
			ref = e.Ref
		}
	}

	// 3. The group read acknowledged it, so the next read does not return it.
	page = eventsPage{}
	s.call("read_events", map[string]any{"group": "agent", "kinds": []string{"new"}}, &page)
	for _, e := range page.Events {
		if e.Kind == "new" {
			t.Errorf("read_events returned new event %s again after the group read it", e.Ref)
		}
	}

	// 4. Every read is audited as watch.read, with client kind mcp and the
	// group, and never the subject.
	reads := 0
	for _, r := range h.AuditRows(t) {
		if strings.Contains(fmt.Sprintf("%+v", r), "Invoice") {
			t.Errorf("audit row %+v holds message content", r)
		}
		if r.Action != "watch.read" {
			continue
		}
		reads++
		if r.Client != "mcp" || !strings.Contains(r.Targets, "group=agent") {
			t.Errorf("watch.read row %+v, want client mcp and target group=agent", r)
		}
	}
	if reads < 3 {
		t.Errorf("%d watch.read audit rows, want at least 3", reads)
	}
}
