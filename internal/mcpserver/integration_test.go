//go:build integration

package mcpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
// mail appended afterwards is new rather than part of the baseline. Every
// call and pause runs under the 30-second deadline, so a stalled call
// cannot hold the wait past it.
func waitInboxWatched(t *testing.T, h *daemontest.Harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var last []*chainmailv1.FolderSyncStatus
	for {
		resp, err := h.Client.Watch.GetWatchStatus(ctx, connect.NewRequest(&chainmailv1.GetWatchStatusRequest{}))
		if err != nil {
			t.Fatalf("GetWatchStatus: %v (last folders %v)", err, last)
		}
		last = resp.Msg.GetFolders()
		for _, f := range last {
			if f.GetAccount() == "dove" && f.GetFolder() == "INBOX" && f.GetState() == "watching" {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("dove/INBOX not watching within 30s: %v", last)
		case <-time.After(50 * time.Millisecond):
		}
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

// mcpCacheConfig is one account on Dovecot with the cache on and the
// default sync set, tuned to be ready in seconds as in the daemon's cache
// test. The SMTP endpoint is never dialled.
const mcpCacheConfig = `
[accounts.dove]
provider = "custom"
address = %[1]q
imap = { host = "127.0.0.1", port = %[2]d, security = "tls" }
smtp = { host = "127.0.0.1", port = 1, security = "starttls" }
sync = { poll = "30s" }

[cache]
prefetch_bodies = "30d"
backfill_batch = 10
backfill_pause = "0s"
`

// mcpCacheFolders are the folders the cache tracks on Dovecot: every folder
// a default search covers.
var mcpCacheFolders = []string{"Archive", "Drafts", "INBOX", "Sent"}

// waitDoveCache polls GetCacheStatus until dove's cache is ready with
// mcpCacheFolders backfilled and their windows fetched.
func waitDoveCache(t *testing.T, h *daemontest.Harness) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	// Every call ends by the deadline, so a stalled one fails the wait in
	// time, with the last status seen.
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	var last *chainmailv1.GetCacheStatusResponse
	for {
		resp, err := h.Client.Cache.GetCacheStatus(ctx, connect.NewRequest(&chainmailv1.GetCacheStatusRequest{Accounts: []string{"dove"}}))
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("the cache of dove is not ready within 90s: GetCacheStatus: %v; last status %v", err, last)
			}
			t.Fatalf("GetCacheStatus: %v", err)
		}
		last = resp.Msg
		for _, a := range resp.Msg.GetAccounts() {
			if a.GetAccount() != "dove" || a.GetState() != "ready" {
				continue
			}
			var names []string
			done := true
			for _, f := range a.GetFolders() {
				names = append(names, f.GetFolder())
				done = done && f.GetBackfillDone() && f.GetWindowDone()
			}
			slices.Sort(names)
			if done && slices.Equal(names, mcpCacheFolders) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the cache of dove is not ready within 90s: %v", resp.Msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// mcpSearchOut is search's structured output, as far as the cache test
// reads it.
type mcpSearchOut struct {
	Rows []struct {
		Ref string `json:"ref"`
	} `json:"rows"`
	Source        string `json:"source"`
	AccountCounts []struct {
		Account          string `json:"account"`
		Source           string `json:"source"`
		Complete         bool   `json:"complete"`
		IncompleteReason string `json:"incomplete_reason"`
	} `json:"account_counts"`
}

// TestMCPCacheAgainstDovecot runs the built binary as chainmail mcp against
// a daemon whose cache holds a Dovecot mailbox. search answers from the
// cache by itself and says so, source picks the server, an unknown source
// is an error, get_message reads from the cache, and cache_status reports
// the account ready. Search rows are audited with the source, cache_status
// is not audited.
func TestMCPCacheAgainstDovecot(t *testing.T) {
	bin := buildChainmail(t)
	dove := startDovecot(t)
	const user = "cache@example.test"
	if _, err := dove.Append(user, "INBOX", invoice, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := daemontest.Start(t, fmt.Sprintf(mcpCacheConfig, user, dove.Endpoint.Port), daemontest.Options{
		RootCAs: dove.CA.Pool, Watch: true, Cache: true,
		Secrets: map[string]string{"accounts/dove": dovecot.Password},
	})
	waitDoveCache(t, h)
	s := connectMCP(t, bin, h.Socket)

	// 1. With no source the cache answers, completely.
	var found mcpSearchOut
	s.call("search", map[string]any{"query": "subject:Invoice"}, &found)
	if found.Source != "cache" || len(found.Rows) != 1 || len(found.AccountCounts) != 1 {
		t.Fatalf("search = %+v, want one row from the cache", found)
	}
	if ac := found.AccountCounts[0]; ac.Account != "dove" || ac.Source != "cache" || !ac.Complete || ac.IncompleteReason != "" {
		t.Errorf("account count = %+v, want dove, cache, complete", ac)
	}

	// 2. source picks the server, with the same row.
	var fromServer mcpSearchOut
	s.call("search", map[string]any{"query": "subject:Invoice", "source": "server"}, &fromServer)
	if fromServer.Source != "server" || len(fromServer.Rows) != 1 || fromServer.Rows[0].Ref != found.Rows[0].Ref {
		t.Errorf("search with source server = %+v, want the same row from the server", fromServer)
	}

	// 3. An unknown source is refused.
	res, err := s.cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"query": "subject:Invoice", "source": "nearby"}})
	if err != nil {
		t.Fatalf("search with source nearby: %v", err)
	}
	if !res.IsError {
		t.Errorf("search with source nearby succeeded: %s", textOf(res))
	}

	// 4. get_message reads the prefetched message from the cache.
	var got struct {
		Messages []struct {
			Source string                     `json:"source"`
			Body   struct{ Untrusted string } `json:"body"`
		} `json:"messages"`
	}
	s.call("get_message", map[string]any{"refs": []string{found.Rows[0].Ref}}, &got)
	if len(got.Messages) != 1 || got.Messages[0].Source != "cache" || !strings.Contains(got.Messages[0].Body.Untrusted, "invoice 42") {
		t.Errorf("get_message = %+v, want the invoice from the cache", got)
	}

	// 5. cache_status reports the account ready.
	var status struct {
		Accounts []struct {
			Account string `json:"account"`
			Enabled bool   `json:"enabled"`
			State   string `json:"state"`
		} `json:"accounts"`
	}
	s.call("cache_status", map[string]any{}, &status)
	if len(status.Accounts) != 1 || status.Accounts[0].Account != "dove" || !status.Accounts[0].Enabled || status.Accounts[0].State != "ready" {
		t.Errorf("cache_status = %+v, want dove enabled and ready", status)
	}

	// 6. The audit log names the source of each search, holds no subject,
	// and has no row for cache_status.
	sources := map[string]int{}
	for _, r := range h.AuditRows(t) {
		if strings.Contains(fmt.Sprintf("%+v", r), "Invoice") {
			t.Errorf("audit row %+v holds message content", r)
		}
		if strings.HasPrefix(r.Action, "cache.") {
			t.Errorf("audit row %+v: cache_status is not audited", r)
		}
		if r.Action != "search" || r.Client != "mcp" {
			continue
		}
		for _, source := range []string{"cache", "server"} {
			if strings.Contains(r.Targets, "source="+source) {
				sources[source]++
			}
		}
	}
	if sources["cache"] < 1 || sources["server"] < 1 {
		t.Errorf("search audit rows by source = %v, want at least one each from cache and server", sources)
	}
}

// mcpItemsConfig is one account on Dovecot that watches INBOX, with the
// cache off. The SMTP endpoint is never dialled.
const mcpItemsConfig = `
[accounts.dove]
provider = "custom"
address = %[1]q
imap = { host = "127.0.0.1", port = %[2]d, security = "tls" }
smtp = { host = "127.0.0.1", port = 1, security = "starttls" }
`

// doveInvoiceReply answers invoice, as another mail client delivers it.
const doveInvoiceReply = "From: Ann <ann@example.org>\r\nTo: items@example.test\r\nSubject: Re: Invoice 42\r\n" +
	"Message-ID: <inv42r@example.org>\r\nIn-Reply-To: <inv42@example.org>\r\nReferences: <inv42@example.org>\r\n" +
	"Date: Fri, 30 Oct 2026 10:00:00 +0000\r\n\r\nPaid it this morning.\r\n"

// doveItemEvents is read_events' structured output, as far as the items
// test reads it.
type doveItemEvents struct {
	Events []struct {
		ID       int64  `json:"id"`
		Kind     string `json:"kind"`
		Account  string `json:"account"`
		Ref      string `json:"ref"`
		ItemID   string `json:"item_id"`
		FromSelf bool   `json:"from_self"`
		Message  struct {
			Subject struct{ Untrusted string } `json:"subject"`
		} `json:"message"`
	} `json:"events"`
}

// doveThreadOut is a linked thread in link_thread's and get_item's
// structured output, as far as the items test reads it.
type doveThreadOut struct {
	Account   string                     `json:"account"`
	ThreadKey string                     `json:"thread_key"`
	State     string                     `json:"state"`
	Source    string                     `json:"source"`
	Messages  int                        `json:"messages"`
	AnchorRef string                     `json:"anchor_ref"`
	Note      struct{ Untrusted string } `json:"note"`
	Latest    struct {
		Ref     string                     `json:"ref"`
		Subject struct{ Untrusted string } `json:"subject"`
	} `json:"latest"`
}

// TestMCPItemsAgainstDovecot has an agent file mail against a task the way
// spec section 10.4 describes. The built binary runs as chainmail mcp
// against a watching daemon on Dovecot. The agent finds a message, creates
// a task, links the message's thread, and waits with read_events and the
// item filter. Another IMAP client delivers a reply, read_events returns
// its item_mail event, and get_item shows the thread with the reply. Every
// call is audited with client kind mcp, and no audit row holds item text.
func TestMCPItemsAgainstDovecot(t *testing.T) {
	bin := buildChainmail(t)
	dove := startDovecot(t)
	const user = "items@example.test"
	if _, err := dove.Append(user, "INBOX", invoice, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := daemontest.Start(t, fmt.Sprintf(mcpItemsConfig, user, dove.Endpoint.Port), daemontest.Options{
		RootCAs: dove.CA.Pool, Watch: true, Secrets: map[string]string{"accounts/dove": dovecot.Password},
	})
	waitInboxWatched(t, h)
	s := connectMCP(t, bin, h.Socket)

	// 1. The agent finds the invoice.
	var found struct {
		Rows []struct {
			Ref string `json:"ref"`
		} `json:"rows"`
	}
	s.call("search", map[string]any{"query": "subject:Invoice"}, &found)
	if len(found.Rows) != 1 {
		t.Fatalf("search rows = %+v, want the invoice", found.Rows)
	}
	invoiceRef := found.Rows[0].Ref

	// 2. It creates a task in the inbox with one criterion.
	const title = "Pay the invoice"
	const note = "the thread Ann started"
	var created struct {
		Item struct {
			ID            string                     `json:"id"`
			Type          string                     `json:"type"`
			Title         struct{ Untrusted string } `json:"title"`
			EffectiveArea string                     `json:"effective_area"`
		} `json:"item"`
		Criteria []struct {
			ID   string                     `json:"id"`
			Text struct{ Untrusted string } `json:"text"`
		} `json:"criteria"`
		Duplicate bool `json:"duplicate"`
	}
	s.call("create_item", map[string]any{"type": "task", "title": title, "area": "work", "criteria": []string{"Paid in full"}}, &created)
	id := created.Item.ID
	if id == "" || created.Item.Type != "task" || created.Item.Title.Untrusted != title || created.Item.EffectiveArea != "work" || created.Duplicate {
		t.Fatalf("create_item = %+v, want a new work task titled %q", created, title)
	}
	if len(created.Criteria) != 1 || created.Criteria[0].ID == "" || created.Criteria[0].Text.Untrusted != "Paid in full" {
		t.Errorf("create_item criteria = %+v, want one", created.Criteria)
	}

	// 3. It links the invoice's thread, anchored on the invoice.
	var linked struct {
		Link    doveThreadOut `json:"link"`
		Created bool          `json:"created"`
	}
	s.call("link_thread", map[string]any{"item_id": id, "ref": invoiceRef, "anchor": true, "note": note}, &linked)
	l := linked.Link
	if !linked.Created || l.Account != "dove" || l.ThreadKey == "" || l.State != "ok" || l.Source != "server" || l.Messages != 1 {
		t.Fatalf("link_thread = %+v, want a new link on dove, ok from the server with 1 message", linked)
	}
	if l.AnchorRef != invoiceRef || l.Note.Untrusted != note {
		t.Errorf("link_thread anchor %q, note %q, want %s and %q", l.AnchorRef, l.Note.Untrusted, invoiceRef, note)
	}

	// 4. The first read creates the group at the end of the log.
	var page doveItemEvents
	s.call("read_events", map[string]any{"group": "items", "item": id}, &page)

	// 5. Another client delivers a reply, and a waiting read returns its
	// item_mail event. Status events pass the kinds filter, so a read may
	// return only those; read again.
	if _, err := dove.Append(user, "INBOX", doveInvoiceReply, time.Now()); err != nil {
		t.Fatal(err)
	}
	mailRef := ""
	deadline := time.Now().Add(30 * time.Second)
	for mailRef == "" {
		if time.Now().After(deadline) {
			t.Fatal("read_events returned no item_mail event within 30s")
		}
		page = doveItemEvents{}
		s.call("read_events", map[string]any{"group": "items", "item": id, "kinds": []string{"item_mail"}, "wait_seconds": 10}, &page)
		for _, e := range page.Events {
			if e.Kind != "item_mail" {
				continue
			}
			if e.ItemID != id || e.Account != "dove" || e.Ref == "" || e.ID == 0 || e.FromSelf || e.Message.Subject.Untrusted != "Re: Invoice 42" {
				t.Fatalf("item_mail event = %+v, want Ann's Re: Invoice 42 for item %s with a ref and an ID", e, id)
			}
			mailRef = e.Ref
		}
	}

	// 6. get_item shows the thread with the reply as its latest message.
	var view struct {
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
		Threads      []doveThreadOut `json:"threads"`
		ThreadsTotal int             `json:"threads_total"`
	}
	s.call("get_item", map[string]any{"id": id}, &view)
	if view.Item.ID != id || view.ThreadsTotal != 1 || len(view.Threads) != 1 {
		t.Fatalf("get_item = %+v, want item %s with one thread", view, id)
	}
	th := view.Threads[0]
	if th.ThreadKey != l.ThreadKey || th.State != "ok" || th.Source != "server" || th.Messages != 2 || th.AnchorRef != invoiceRef {
		t.Errorf("get_item thread = %+v, want key %s, ok from the server with 2 messages and anchor %s", th, l.ThreadKey, invoiceRef)
	}
	if th.Latest.Ref != mailRef || th.Latest.Subject.Untrusted != "Re: Invoice 42" {
		t.Errorf("get_item latest = %+v, want the reply %s", th.Latest, mailRef)
	}

	// 7. Every call is audited with client kind mcp, and no row holds a
	// title, criterion text, note or subject. Rows of the daemon's own
	// work, such as a background probe, are not the agent's calls.
	byAction := map[string]int{}
	for _, r := range h.AuditRows(t) {
		line := fmt.Sprintf("%+v", r)
		for _, text := range []string{title, "Paid in full", note, "Invoice"} {
			if strings.Contains(line, text) {
				t.Errorf("audit row %s holds %q", line, text)
			}
		}
		if r.Client == "daemon" {
			continue
		}
		if r.Client != "mcp" {
			t.Errorf("audit row %s has client %q, want mcp", line, r.Client)
		}
		byAction[r.Action]++
	}
	for action, want := range map[string]int{"search": 1, "item.create": 1, "item.link": 1, "item.get": 1, "watch.read": 2} {
		if byAction[action] < want {
			t.Errorf("audit has %d %s rows, want at least %d (rows by action: %v)", byAction[action], action, want, byAction)
		}
	}
}
