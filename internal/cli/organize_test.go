package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/client"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/mailstore"
	"github.com/yaklabco/chainmail/internal/mailstore/fake"
	"github.com/yaklabco/chainmail/internal/ref"
	"github.com/yaklabco/chainmail/internal/testutil/daemontest"
)

// offlineStores gives every account a fake store, so the probe that follows
// secret set and auth login never dials a real server. A fake store cannot
// be probed, so that probe fails with INTERNAL "probe not supported".
func offlineStores(a config.ResolvedAccount) mailstore.Store {
	return fake.New(a.Name, a.Provider.Gmail)
}

// connLimit is a server's connection limit, shared by every store built
// for one account.
type connLimit struct {
	mu   sync.Mutex
	open int
	max  int
}

// probingStore is a fake store a probe can count: ProbeDial accepts up to
// max connections at once and refuses the next.
type probingStore struct {
	*fake.Store
	limit *connLimit
}

func (s *probingStore) ProbeDial(context.Context) (io.Closer, error) {
	s.limit.mu.Lock()
	defer s.limit.mu.Unlock()
	if s.limit.open >= s.limit.max {
		return nil, errors.New("too many connections")
	}
	s.limit.open++
	return closerFunc(func() error {
		s.limit.mu.Lock()
		defer s.limit.mu.Unlock()
		s.limit.open--
		return nil
	}), nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// probingStores builds stores whose named accounts allow max connections.
// Other accounts get plain fakes, which cannot be probed.
func probingStores(maxConns map[string]int) func(config.ResolvedAccount) mailstore.Store {
	limits := map[string]*connLimit{}
	for name, n := range maxConns {
		limits[name] = &connLimit{max: n}
	}
	return func(a config.ResolvedAccount) mailstore.Store {
		s := fake.New(a.Name, a.Provider.Gmail)
		if l, ok := limits[a.Name]; ok {
			return &probingStore{Store: s, limit: l}
		}
		return s
	}
}

func TestOrganizeCommandsGolden(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup [][]string
		// args builds the command from the seeded refs.
		args func(m cliMail) []string
		code int
	}{
		{name: "move", args: func(m cliMail) []string { return []string{"move", m.invoice.String(), "--to", "Junk"} }},
		{name: "copy", args: func(m cliMail) []string {
			return []string{"copy", m.invoice.String(), m.lunch.String(), "--to", "INBOX"}
		}},
		{name: "archive", args: func(m cliMail) []string { return []string{"archive", m.invoice.String(), staleRef, m.lunch.String()} }, code: api.ExitNotFound},
		{name: "trash", args: func(m cliMail) []string { return []string{"trash", m.invoice.String(), m.lunch.String()} }},
		{name: "expunge", args: func(m cliMail) []string { return []string{"expunge", m.invoice.String()} }},
		{name: "flag", args: func(m cliMail) []string { return []string{"flag", m.invoice.String(), m.lunch.String()} }},
		{name: "unflag", args: func(m cliMail) []string { return []string{"unflag", m.invoice.String()} }},
		{name: "read", args: func(m cliMail) []string { return []string{"read", m.invoice.String()} }},
		{name: "unread", args: func(m cliMail) []string { return []string{"unread", m.invoice.String()} }},
		{name: "flags", args: func(m cliMail) []string {
			return []string{"flags", m.invoice.String(), "--add", "$Work", "--add", `\Answered`, "--remove", `\Seen`}
		}},
		{name: "label", args: func(m cliMail) []string {
			return []string{"label", m.lunch.String(), m.invoice.String(), "--add", `\Important`, "--remove", `\Starred`}
		}, code: api.ExitUsage},
		{name: "folder_create", args: func(cliMail) []string { return []string{"folder", "create", "--account", "host", "Projects/2026"} }},
		{
			name:  "folder_rename",
			setup: [][]string{{"folder", "create", "--account", "host", "Projects"}},
			args:  func(cliMail) []string { return []string{"folder", "rename", "--account", "host", "Projects", "Work"} },
		},
		{
			name:  "folder_delete",
			setup: [][]string{{"folder", "create", "--account", "host", "Old"}},
			args:  func(cliMail) []string { return []string{"folder", "delete", "--account", "host", "Old", "--force"} },
		},
	}
	for _, tt := range tests {
		for _, output := range []string{"table", "json"} {
			name := tt.name + "_" + output
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				m := startCLIMailConfig(t, cliDestructiveConfig)
				for _, s := range tt.setup {
					if got := m.run(t, Env{}, s...); got.code != 0 {
						t.Fatalf("setup %v: %+v", s, got)
					}
				}
				got := m.run(t, Env{}, append([]string{"--output", output}, tt.args(m)...)...)
				if got.code != tt.code {
					t.Fatalf("exit %d, want %d; stdout %q, stderr %s", got.code, tt.code, got.stdout, got.stderr)
				}
				golden(t, "organize_"+name, got.stdout)
			})
		}
	}
}

func TestOrganizeChangesTheStore(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	if got := m.run(t, Env{}, "read", m.invoice.String()); got.code != 0 {
		t.Fatalf("read: %+v", got)
	}
	if flags := m.host.Flags(m.invoice); !slices.Contains(flags, `\Seen`) {
		t.Errorf("flags after read = %v", flags)
	}
	if got := m.run(t, Env{}, "unread", m.invoice.String()); got.code != 0 {
		t.Fatalf("unread: %+v", got)
	}
	if flags := m.host.Flags(m.invoice); slices.Contains(flags, `\Seen`) {
		t.Errorf("flags after unread = %v", flags)
	}
	if got := m.run(t, Env{}, "flag", m.invoice.String()); got.code != 0 {
		t.Fatalf("flag: %+v", got)
	}
	if flags := m.host.Flags(m.invoice); !slices.Contains(flags, `\Flagged`) {
		t.Errorf("flags after flag = %v", flags)
	}
}

func TestOrganizeExitCodes(t *testing.T) {
	t.Parallel()
	m := startCLIMailConfig(t, cliDestructiveConfig)
	tests := []struct {
		name   string
		args   []string
		code   int
		reason string
	}{
		{"partial failure", []string{"trash", m.invoice.String(), staleRef}, api.ExitNotFound, api.ReasonStaleRef},
		{"first failure wins", []string{"label", m.invoice.String(), m.lunch.String(), "--add", "Nope"}, api.ExitUsage, api.ReasonInvalidArgument},
		{"whole call refused", []string{"flags", m.invoice.String()}, api.ExitUsage, api.ReasonInvalidArgument},
		{"unknown account", []string{"read", ref.Ref{Account: "nope", Folder: "INBOX", UIDValidity: 1, UID: 1}.String()}, api.ExitNotFound, api.ReasonNotFound},
		{"bad ref", []string{"read", "not-a-ref"}, api.ExitUsage, api.ReasonInvalidArgument},
		{"no refs", []string{"archive"}, api.ExitUsage, api.ReasonUsage},
		{"move without --to", []string{"move", m.invoice.String()}, api.ExitUsage, api.ReasonUsage},
		{"copy without --to", []string{"copy", m.invoice.String()}, api.ExitUsage, api.ReasonUsage},
		{"folder without --account", []string{"folder", "create", "X"}, api.ExitUsage, api.ReasonUsage},
		{"folder rename args", []string{"folder", "rename", "--account", "host", "X"}, api.ExitUsage, api.ReasonUsage},
		{"delete inbox", []string{"folder", "delete", "--account", "host", "INBOX"}, api.ExitUsage, api.ReasonInvalidArgument},
		{"rename missing folder", []string{"folder", "rename", "--account", "host", "Nope", "Other"}, api.ExitNotFound, api.ReasonNotFound},
		{"stdin mixed with refs", []string{"archive", "-", m.invoice.String()}, api.ExitUsage, api.ReasonUsage},
		{"empty stdin", []string{"archive", "-"}, api.ExitUsage, api.ReasonUsage},
	}
	for _, tt := range tests {
		got := m.run(t, Env{}, tt.args...)
		if got.code != tt.code {
			t.Errorf("%s: exit %d, want %d; stdout %q, stderr %s", tt.name, got.code, tt.code, got.stdout, got.stderr)
			continue
		}
		if r := errorReason(t, got.stderr); r != tt.reason {
			t.Errorf("%s: reason %s, want %s", tt.name, r, tt.reason)
		}
	}
	// The daemon refused the label call on host, the first ref, so the
	// message on host is untouched and every ref still got an outcome.
	got := m.run(t, Env{}, "--output", "json", "label", m.invoice.String(), m.lunch.String(), "--add", `\Important`)
	var resp struct {
		Outcomes []struct {
			Ref   string `json:"ref"`
			Error *struct {
				Reason string `json:"reason"`
			} `json:"error"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &resp); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, got.stdout)
	}
	if len(resp.Outcomes) != 2 || resp.Outcomes[0].Error == nil || resp.Outcomes[1].Error != nil {
		t.Errorf("outcomes = %+v", resp.Outcomes)
	}
}

func TestOrganizeRefsFromStdin(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	const n = 1200
	refs := make([]string, n)
	var in strings.Builder
	in.WriteString("\n")
	for i := range refs {
		r := m.host.Add(fake.Message{Folder: "INBOX", Date: cliNow, Subject: fmt.Sprintf("m%d", i), MessageID: fmt.Sprintf("m%d@example.com", i)})
		refs[i] = r.String()
		fmt.Fprintf(&in, "  %s \n", refs[i])
		if i%100 == 0 {
			in.WriteString("\n   \n")
		}
	}
	before := m.host.Calls("Archive")
	got := m.run(t, Env{Stdin: strings.NewReader(in.String())}, "--output", "json", "archive", "-")
	if got.code != 0 {
		t.Fatalf("archive -: exit %d, stderr %s", got.code, got.stderr)
	}
	if calls := m.host.Calls("Archive") - before; calls != 3 {
		t.Errorf("Archive calls = %d, want 3 (500, 500 and 200 refs)", calls)
	}
	var resp chainmailv1.ArchiveResponse
	if err := protojson.Unmarshal([]byte(got.stdout), &resp); err != nil {
		t.Fatalf("stdout is not an ArchiveResponse: %v", err)
	}
	if len(resp.GetOutcomes()) != n {
		t.Fatalf("outcomes = %d, want %d", len(resp.GetOutcomes()), n)
	}
	for i, o := range resp.GetOutcomes() {
		if o.GetRef() != refs[i] || o.GetError() != nil || o.GetNewRef() == "" {
			t.Fatalf("outcome %d = %v, want ref %s archived", i, o, refs[i])
		}
	}

	table := m.run(t, Env{Stdin: strings.NewReader(strings.Join(refs[:3], "\n") + "\n" + staleRef + "\n")}, "--output", "table", "read", "-")
	want := "marked read 0, failed 4\n"
	if !strings.HasPrefix(table.stdout, want) || table.code != api.ExitNotFound {
		t.Errorf("read - after archive: exit %d, stdout %q, want prefix %q", table.code, table.stdout, want)
	}
}

// fakeOutcomes answers every ref of a chunk, failing the refs given.
func fakeOutcomes(chunk []string, fail map[string]bool) []*chainmailv1.RefOutcome {
	out := make([]*chainmailv1.RefOutcome, len(chunk))
	for i, r := range chunk {
		out[i] = &chainmailv1.RefOutcome{Ref: r}
		if fail[r] {
			out[i].Error = &chainmailv1.ItemError{Reason: api.ReasonStaleRef, Message: "gone"}
		}
	}
	return out
}

func numberedRefs(n int) []string {
	refs := make([]string, n)
	for i := range refs {
		refs[i] = fmt.Sprintf("r%d", i)
	}
	return refs
}

func TestCallChunkedBoundsAndOrder(t *testing.T) {
	t.Parallel()
	refs := numberedRefs(2600)
	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	var sizes []int
	// Each call waits until maxCallsInFlight calls run at once, so the peak
	// reaches the bound without relying on a sleep. A callChunked that runs
	// fewer at once waits out the deadline and fails the peak check.
	overlap := make(chan struct{})
	var overlapped sync.Once
	outs, err := callChunked(t.Context(), refs, func(_ context.Context, chunk []string) ([]*chainmailv1.RefOutcome, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if n >= maxCallsInFlight {
			overlapped.Do(func() { close(overlap) })
		}
		mu.Lock()
		sizes = append(sizes, len(chunk))
		mu.Unlock()
		select {
		case <-overlap:
		case <-time.After(5 * time.Second):
		}
		return fakeOutcomes(chunk, nil), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p != maxCallsInFlight {
		t.Errorf("peak concurrency = %d, want %d", p, maxCallsInFlight)
	}
	slices.Sort(sizes)
	if !slices.Equal(sizes, []int{100, 500, 500, 500, 500, 500}) {
		t.Errorf("chunk sizes = %v", sizes)
	}
	for i, o := range outs {
		if o.GetRef() != refs[i] {
			t.Fatalf("outcome %d is %s, want %s", i, o.GetRef(), refs[i])
		}
	}
}

func TestCallChunkedFoldsFailedCalls(t *testing.T) {
	t.Parallel()
	refs := numberedRefs(1200)
	failed := api.New(connect.CodeUnavailable, api.ReasonServerUnavailable, "try again", errors.New("the server is busy"))
	outs, err := callChunked(t.Context(), refs, func(_ context.Context, chunk []string) ([]*chainmailv1.RefOutcome, error) {
		if chunk[0] == "r500" {
			return nil, failed
		}
		if chunk[0] == "r1000" {
			return fakeOutcomes(chunk[:10], nil), nil // too few outcomes
		}
		return fakeOutcomes(chunk, map[string]bool{"r3": true}), nil
	})
	if err != nil {
		t.Fatalf("a call that failed among calls that worked must not fail the command: %v", err)
	}
	if len(outs) != len(refs) {
		t.Fatalf("outcomes = %d", len(outs))
	}
	for i, o := range outs {
		wantErr := i == 3 || (i >= 500 && i < 1000) || i >= 1000
		if o.GetRef() != refs[i] || (o.GetError() != nil) != wantErr {
			t.Fatalf("outcome %d = %v, want ref %s with error %t", i, o, refs[i], wantErr)
		}
	}
	if e := outs[600].GetError(); e.GetReason() != api.ReasonServerUnavailable || e.GetHint() != "try again" {
		t.Errorf("outcome of a failed call = %v", e)
	}
	if e := outs[1100].GetError(); e.GetReason() != api.ReasonInternal {
		t.Errorf("outcome of a call with too few outcomes = %v", e)
	}

	_, err = callChunked(t.Context(), refs, func(context.Context, []string) ([]*chainmailv1.RefOutcome, error) {
		return nil, failed
	})
	if !errors.Is(err, failed) {
		t.Errorf("every call failed: err = %v, want the first call's", err)
	}
}

func TestAccountsProbeGolden(t *testing.T) {
	t.Parallel()
	for _, output := range []string{"table", "json"} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()
			h := daemontest.Start(t, cliMailConfig, daemontest.Options{
				NewStore: probingStores(map[string]int{"host": 4}),
				Now:      func() time.Time { return cliNow },
			})
			got := runCLI(t, Env{}, "--socket", h.Socket, "--output", output, "accounts", "probe", "host", "gmail")
			if got.code != api.ExitInternal || errorReason(t, got.stderr) != api.ReasonInternal {
				t.Fatalf("probe: %+v", got)
			}
			golden(t, "accounts_probe_"+output, got.stdout)
		})
	}
}

func TestAccountsProbeExitCodes(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, cliMailConfig, daemontest.Options{
		NewStore: probingStores(map[string]int{"host": 3, "gmail": 3}),
		Now:      func() time.Time { return cliNow },
	})
	all := runCLI(t, Env{}, "--socket", h.Socket, "--output", "json", "accounts", "probe")
	if all.code != 0 {
		t.Fatalf("probe every account: %+v", all)
	}
	var resp chainmailv1.ProbeLimitsResponse
	if err := protojson.Unmarshal([]byte(all.stdout), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.GetLimits()) != 2 {
		t.Errorf("limits = %v", resp.GetLimits())
	}
	for _, l := range resp.GetLimits() {
		if l.GetImapCap() != 3 || l.GetPoolSize() != 1 {
			t.Errorf("limit = %v, want cap 3 pool 1", l)
		}
	}
	unknown := runCLI(t, Env{}, "--socket", h.Socket, "accounts", "probe", "nope")
	if unknown.code != api.ExitNotFound {
		t.Errorf("unknown account: %+v", unknown)
	}
}

func TestSecretSetPrintsProbe(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{
		NewStore: probingStores(map[string]int{"host": 4}),
		Now:      func() time.Time { return cliNow },
	})
	table := runCLI(t, Env{Stdin: strings.NewReader("pw\n")}, "--socket", h.Socket, "--output", "table", "secret", "set", "accounts/host")
	if table.code != 0 || table.stdout != "stored secret accounts/host\nconnections: cap 4, pool 2\n" || table.stderr != "" {
		t.Errorf("table: %+v", table)
	}
	js := runCLI(t, Env{Stdin: strings.NewReader("pw\n")}, "--socket", h.Socket, "--output", "json", "secret", "set", "accounts/host")
	want := `{"limit":{"account":"host","imap_cap":4,"pool_size":2,"probed_at":"2026-10-29T12:00:00Z","error":null},"name":"accounts/host","set":true}` + "\n"
	if js.code != 0 || js.stdout != want {
		t.Errorf("json: %+v\nwant stdout %s", js, want)
	}
}

func TestSecretSetSurvivesProbeFailure(t *testing.T) {
	t.Parallel()
	h := daemontest.Start(t, twoAccounts, daemontest.Options{NewStore: offlineStores})
	table := runCLI(t, Env{Stdin: strings.NewReader("pw\n")}, "--socket", h.Socket, "--output", "table", "secret", "set", "accounts/host")
	if table.code != 0 || table.stdout != "stored secret accounts/host\n" ||
		table.stderr != "warning: connection probe failed: INTERNAL: probe not supported\n" {
		t.Errorf("table: %+v", table)
	}
	js := runCLI(t, Env{Stdin: strings.NewReader("pw\n")}, "--socket", h.Socket, "--output", "json", "secret", "set", "accounts/host")
	if js.code != 0 || js.stderr != "" || !strings.Contains(js.stdout, `"error":{"reason":"INTERNAL","message":"probe not supported"`) {
		t.Errorf("json: %+v", js)
	}
}

func TestSecretSetProbesOnlyAccounts(t *testing.T) {
	t.Parallel()
	cfg := twoAccounts + "\n[oauth_clients.g]\nclient_id = \"client-id\"\n"
	h := daemontest.Start(t, cfg, daemontest.Options{NewStore: offlineStores})
	got := runCLI(t, Env{Stdin: strings.NewReader("s\n")}, "--socket", h.Socket, "--output", "json", "secret", "set", "oauth_clients/g")
	if got.code != 0 || got.stdout != `{"name":"oauth_clients/g","set":true}`+"\n" {
		t.Errorf("secret set: %+v", got)
	}
	for _, r := range h.AuditRows(t) {
		if r.Action == "limits.probe" {
			t.Errorf("probed after storing a non-account secret: %+v", r)
		}
	}
}

// A probe call the daemon never answers comes back as the limit's error.
func TestProbeAccountCallFails(t *testing.T) {
	t.Parallel()
	c := client.New(filepath.Join(t.TempDir(), "none.sock"), "test")
	defer c.Close()
	l := probeAccount(t.Context(), c, "host")
	if l.GetAccount() != "host" || l.GetError().GetReason() != api.ReasonDaemonUnreachable {
		t.Errorf("limit = %v", l)
	}
}

// A pipe that stays open must not hold a command once its context ends,
// and no organize call may follow.
func TestOrganizeStdinStopsWhenContextEnds(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	// The daemon must have started, or the harness reports a cancelled
	// startup when this test, which never reaches it, ends first.
	if got := m.run(t, Env{}, "admin", "status"); got.code != 0 {
		t.Fatalf("admin status: %+v", got)
	}
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		_, _ = fmt.Fprintln(pw, m.invoice.String()) // returns once the command has read it
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	before := m.host.Calls("Archive")
	var out, errOut strings.Builder
	done := make(chan int, 1)
	go func() {
		env := Env{Stdin: pr, Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }}
		done <- Execute(ctx, []string{"--socket", m.socket, "archive", "-"}, env)
	}()
	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("the command never read stdin")
	}
	cancel()
	select {
	case code := <-done:
		if code == 0 || !strings.Contains(errOut.String(), "interrupted") {
			t.Errorf("exit %d, stderr %q, want an interrupted failure", code, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("archive - still reading stdin after its context ended")
	}
	if calls := m.host.Calls("Archive") - before; calls != 0 {
		t.Errorf("Archive calls = %d after an interrupted read, want 0", calls)
	}
}

// hasTerminalControls reports whether s holds a control character other
// than newline, or an invisible format character.
func hasTerminalControls(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x202e
	})
}

// A malformed ref on stdin comes back in its failed outcome. On a terminal
// it must not carry escape sequences; JSON keeps it as given.
func TestOrganizeTableCleansUntrustedText(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	bad := "bad\x1b[2J\x1b]0;pwned\x07\u202eref"
	table := m.run(t, Env{Stdin: strings.NewReader(bad + "\n")}, "--output", "table", "read", "-")
	if table.code != api.ExitUsage || hasTerminalControls(table.stdout) || !strings.Contains(table.stdout, "bad[2J]0;pwnedref: INVALID_ARGUMENT") {
		t.Errorf("table: exit %d, stdout %q", table.code, table.stdout)
	}
	js := m.run(t, Env{Stdin: strings.NewReader(bad + "\n")}, "--output", "json", "read", "-")
	var resp chainmailv1.SetFlagsResponse
	if err := protojson.Unmarshal([]byte(js.stdout), &resp); err != nil {
		t.Fatalf("stdout is not a SetFlagsResponse: %v\n%s", err, js.stdout)
	}
	if o := resp.GetOutcomes(); len(o) != 1 || o[0].GetRef() != bad {
		t.Errorf("json outcomes = %v, want the ref as given", o)
	}

	var b strings.Builder
	err := outcomesText(&b, "moved", []*chainmailv1.RefOutcome{{
		Ref:   "r\x1b[1m1",
		Error: &chainmailv1.ItemError{Reason: "STALE\x1b[0m_REF", Message: "gone\r\nforged: line\u009b"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "moved 0, failed 1\nr[1m1: STALE[0m_REF gone forged: line\n"; b.String() != want {
		t.Errorf("outcomesText = %q, want %q", b.String(), want)
	}
}

func TestProbeOutputCleansUntrustedText(t *testing.T) {
	t.Parallel()
	limits := &chainmailv1.ProbeLimitsResponse{Limits: []*chainmailv1.AccountLimit{
		{Account: "ok\x1b[31m", ImapCap: 4, PoolSize: 2},
		{Account: "bad\tone", Error: &chainmailv1.ItemError{Reason: "TIME\x1bOUT", Message: "slow\x07\nforged"}},
	}}
	var b strings.Builder
	if err := limitsTable(&b, time.UTC, limits); err != nil {
		t.Fatal(err)
	}
	want := "ACCOUNT  CAP  POOL  PROBED  ERROR\n" +
		"ok[31m   4    2             \n" +
		"bad one                     TIMEOUT: slow forged\n"
	if b.String() != want {
		t.Errorf("limitsTable =\n%q\nwant\n%q", b.String(), want)
	}

	var out, errOut strings.Builder
	l := &chainmailv1.AccountLimit{Account: "host", Error: &chainmailv1.ItemError{
		Reason: "INTER\x1bNAL", Message: "no\x1b[2J", Hint: "run\x1b]8;;http://x\x07 this",
	}}
	if err := reportProbe(Env{Stdout: &out, Stderr: &errOut}, &globals{output: "table"}, "logged in to host\n", nil, l); err != nil {
		t.Fatal(err)
	}
	if want := "warning: connection probe failed: INTERNAL: no[2J\n  hint: run]8;;http://x this\n"; errOut.String() != want {
		t.Errorf("warning = %q, want %q", errOut.String(), want)
	}
}

// folder create and rename print the name the server gave the folder,
// which later commands take: under Hostinger's "INBOX." prefix there.
func TestFolderCommandsPrintTheServersName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		output, create, rename string
	}{
		{"table", "created folder INBOX.chainmail-test-x on host\n", "renamed folder INBOX.chainmail-test-x to INBOX.Done.Here on host\n"},
		{"json", `{"name":"INBOX.chainmail-test-x"}` + "\n", `{"name":"INBOX.Done.Here"}` + "\n"},
	} {
		t.Run(tc.output, func(t *testing.T) {
			t.Parallel()
			m := startCLIMail(t)
			m.host.SetNamespace("INBOX.", ".")
			got := m.run(t, Env{}, "--output", tc.output, "folder", "create", "--account", "host", "chainmail-test-x")
			if got.code != 0 || got.stdout != tc.create {
				t.Errorf("folder create: exit %d, stdout %q, want %q; stderr %s", got.code, got.stdout, tc.create, got.stderr)
			}
			got = m.run(t, Env{}, "--output", tc.output, "folder", "rename", "--account", "host", "INBOX.chainmail-test-x", "Done/Here")
			if got.code != 0 || got.stdout != tc.rename {
				t.Errorf("folder rename: exit %d, stdout %q, want %q; stderr %s", got.code, got.stdout, tc.rename, got.stderr)
			}
		})
	}
}

// The help of the actions the policy gates says so.
func TestDestructiveHelpNamesThePolicy(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"expunge", "--help"}, {"folder", "delete", "--help"}} {
		got := runCLI(t, Env{}, args...)
		if got.code != 0 || !strings.Contains(got.stdout, "(disabled unless policy allows it)") {
			t.Errorf("%q: code %d, stdout %q, want the policy named", args, got.code, got.stdout)
		}
	}
}

// cliDestructiveConfig is cliMailConfig with expunge and folder delete
// allowed.
const cliDestructiveConfig = cliMailConfig + `
[policy]
allow_expunge = true
allow_folder_delete = true
`

// By default the policy refuses expunge and folder delete, and the CLI
// exits 3.
func TestDestructiveCommandsDeniedByDefault(t *testing.T) {
	t.Parallel()
	m := startCLIMail(t)
	for _, args := range [][]string{
		{"expunge", m.invoice.String()},
		{"folder", "delete", "--account", "host", "Archive", "--force"},
	} {
		got := m.run(t, Env{}, args...)
		if got.code != api.ExitDenied {
			t.Errorf("%q: exit %d, want %d; stderr %s", args, got.code, api.ExitDenied, got.stderr)
			continue
		}
		if r := errorReason(t, got.stderr); r != api.ReasonPolicyDenied {
			t.Errorf("%q: reason %s, want %s", args, r, api.ReasonPolicyDenied)
		}
	}
	if n := m.host.Calls("Expunge") + m.host.Calls("DeleteFolder"); n != 0 {
		t.Errorf("store called %d times for refused commands", n)
	}
}
