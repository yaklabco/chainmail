//go:build integration

package mcpserver_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "./cmd/chainmail")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
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

// TestMCPAgainstDovecot runs the built binary as chainmail mcp, the way an
// agent's MCP client starts it, against a real daemon, Dovecot and an SMTP
// sink. The agent searches, reads, files and archives a message, saves a
// draft, and has a send off the allowlist held in Drafts. The audit log
// records every call with client kind mcp.
func TestMCPAgainstDovecot(t *testing.T) {
	bin := buildChainmail(t)
	dove, err := dovecot.Start()
	if err != nil {
		t.Fatalf("starting Dovecot: %v", err)
	}
	t.Cleanup(func() {
		if err := dove.Stop(); err != nil {
			t.Errorf("stopping Dovecot: %v", err)
		}
	})
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

	cmd := exec.Command(bin, "--socket", h.Socket, "mcp")
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
	s := session{t: t, cs: cs}

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
	var folder struct{ Name string }
	s.call("create_folder", map[string]any{"account": "dove", "name": "Projects"}, &folder)
	var moved, archived struct {
		Outcomes []struct {
			NewRef string `json:"new_ref"`
		} `json:"outcomes"`
		Succeeded int `json:"succeeded"`
	}
	s.call("move", map[string]any{"refs": []string{found.Rows[0].Ref}, "folder": folder.Name}, &moved)
	if moved.Succeeded != 1 || moved.Outcomes[0].NewRef == "" {
		t.Fatalf("move = %+v", moved)
	}
	s.call("archive", map[string]any{"refs": []string{moved.Outcomes[0].NewRef}}, &archived)
	if archived.Succeeded != 1 {
		t.Fatalf("archive = %+v", archived)
	}

	// 3. a draft, then a send off the allowlist comes back held and lands in Drafts.
	var draft, held struct {
		Status   string   `json:"status"`
		DraftRef string   `json:"draft_ref"`
		Blocked  []string `json:"blocked"`
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
	if held.Status != "held" || held.DraftRef == "" || len(held.Blocked) != 1 || held.Blocked[0] != "b@blocked.test" {
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

	// 4. the audit log names every MCP call with client kind mcp.
	sawHeld := false
	for _, r := range h.AuditRows(t) {
		if r.Action == "secret.set" {
			continue // set by the harness's own client
		}
		if r.Client != "mcp" {
			t.Errorf("audit row %+v has client %q, want mcp", r, r.Client)
		}
		if r.Action == "send" && r.Reason == "HELD" {
			sawHeld = true
		}
	}
	if !sawHeld {
		t.Error("no audit row records the held send")
	}
}
