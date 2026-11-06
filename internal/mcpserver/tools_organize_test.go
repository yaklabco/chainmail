package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/yaklabco/chainmail/internal/api"
)

func TestOrganizeRequests(t *testing.T) {
	t.Parallel()
	refs := []string{testRef, testRef2}
	tests := []struct {
		tool string
		args map[string]any
		proc string
		want proto.Message
	}{
		{"move", map[string]any{"refs": refs, "folder": "Projects"}, chainmailv1connect.OrganizeServiceMoveProcedure, &chainmailv1.MoveRequest{Refs: refs, Folder: "Projects"}},
		{"copy", map[string]any{"refs": refs, "folder": "Keep"}, chainmailv1connect.OrganizeServiceCopyProcedure, &chainmailv1.CopyRequest{Refs: refs, Folder: "Keep"}},
		{"archive", map[string]any{"refs": refs}, chainmailv1connect.OrganizeServiceArchiveProcedure, &chainmailv1.ArchiveRequest{Refs: refs}},
		{"trash", map[string]any{"refs": refs}, chainmailv1connect.OrganizeServiceTrashProcedure, &chainmailv1.TrashRequest{Refs: refs}},
		{
			"set_flags",
			map[string]any{"refs": refs, "add": []string{"seen", "$Work"}, "remove": []string{"Flagged"}},
			chainmailv1connect.OrganizeServiceSetFlagsProcedure,
			&chainmailv1.SetFlagsRequest{Refs: refs, Add: []string{`\Seen`, "$Work"}, Remove: []string{`\Flagged`}},
		},
		{
			"set_labels",
			map[string]any{"refs": refs, "add": []string{"todo"}, "remove": []string{`\Inbox`}},
			chainmailv1connect.OrganizeServiceSetLabelsProcedure,
			&chainmailv1.SetLabelsRequest{Refs: refs, Add: []string{"todo"}, Remove: []string{`\Inbox`}},
		},
		{"create_folder", map[string]any{"account": "work", "name": "Projects/2026"}, chainmailv1connect.OrganizeServiceCreateFolderProcedure, &chainmailv1.CreateFolderRequest{Account: "work", Name: "Projects/2026"}},
		{"rename_folder", map[string]any{"account": "work", "from": "Old", "to": "New"}, chainmailv1connect.OrganizeServiceRenameFolderProcedure, &chainmailv1.RenameFolderRequest{Account: "work", From: "Old", To: "New"}},
	}
	for _, tt := range tests {
		t.Run(tt.tool, func(t *testing.T) {
			t.Parallel()
			e := newTestEnv(t, &fakeDaemon{}, latest)
			e.call(t, tt.tool, tt.args)
			if got := e.fake.lastCall(t, tt.proc).Msg; !proto.Equal(got, tt.want) {
				t.Errorf("request = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOrganizeOutcomes(t *testing.T) {
	t.Parallel()
	stale := &chainmailv1.ItemError{Reason: api.ReasonStaleRef, Message: "gone", Hint: "search again"}
	tests := []struct {
		name          string
		outcomes      []*chainmailv1.RefOutcome
		wantErr       string
		wantSucceeded int
		wantFailed    int
	}{
		{name: "all moved", outcomes: []*chainmailv1.RefOutcome{{Ref: testRef, NewRef: testRef2}}, wantSucceeded: 1},
		{name: "one fails", outcomes: []*chainmailv1.RefOutcome{{Ref: testRef, NewRef: testRef2}, {Ref: testRef2, Error: stale}}, wantSucceeded: 1, wantFailed: 1},
		{name: "all fail", outcomes: []*chainmailv1.RefOutcome{{Ref: testRef2, Error: stale}}, wantErr: api.ReasonStaleRef},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeDaemon{Move: func(context.Context, *chainmailv1.MoveRequest) (*chainmailv1.MoveResponse, error) {
				return &chainmailv1.MoveResponse{Outcomes: tt.outcomes}, nil
			}}
			e := newTestEnv(t, f, latest)
			res := e.call(t, "move", map[string]any{"refs": []string{testRef, testRef2}, "folder": "Projects"})
			if tt.wantErr != "" {
				wantError(t, res, tt.wantErr)
				return
			}
			wantOK(t, res)
			var out outcomesOut
			decode(t, res, &out)
			if out.Succeeded != tt.wantSucceeded || out.Failed != tt.wantFailed || out.Outcomes[0].NewRef != testRef2 {
				t.Errorf("out = %+v", out)
			}
		})
	}
}

// gatePolicy is the capability state the fake ListAccounts reports.
type gatePolicy struct {
	mu                    sync.Mutex
	expunge, folderDelete bool
	down                  bool
}

func (p *gatePolicy) set(expunge, folderDelete, down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expunge, p.folderDelete, p.down = expunge, folderDelete, down
}

func (p *gatePolicy) listAccounts(context.Context, *chainmailv1.ListAccountsRequest) (*chainmailv1.ListAccountsResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("daemon restarting"))
	}
	return &chainmailv1.ListAccountsResponse{Accounts: []*chainmailv1.Account{
		{Name: "a", Capabilities: &chainmailv1.AccountCapabilities{AllowExpunge: p.expunge}},
		{Name: "b", Capabilities: &chainmailv1.AccountCapabilities{AllowFolderDelete: p.folderDelete}},
	}}, nil
}

func TestGatedToolsFollowPolicy(t *testing.T) {
	t.Parallel()
	p := &gatePolicy{}
	e := newTestEnv(t, &fakeDaemon{ListAccounts: p.listAccounts}, latest)
	steps := []struct {
		name                        string
		expunge, folderDelete, down bool
		want                        []string
	}{
		{name: "neither allowed", want: nil},
		{name: "expunge on account a", expunge: true, want: []string{"expunge"}},
		{name: "both, on different accounts", expunge: true, folderDelete: true, want: []string{"delete_folder", "expunge"}},
		{name: "folder delete only", folderDelete: true, want: []string{"delete_folder"}},
		{name: "daemon unreachable hides both", folderDelete: true, down: true, want: nil},
		{name: "back after a restart", expunge: true, want: []string{"expunge"}},
	}
	for _, st := range steps { // sequential: each step builds on the last
		p.set(st.expunge, st.folderDelete, st.down)
		e.srv.refreshGate(t.Context())
		var gated []string
		for _, n := range listToolNames(t, e.cs) {
			if slices.Contains(gatedTools, n) {
				gated = append(gated, n)
			}
		}
		if !slices.Equal(gated, st.want) {
			t.Errorf("%s: gated tools = %v, want %v", st.name, gated, st.want)
		}
	}
}

func TestGatedToolStillMeetsTheDaemonsPolicy(t *testing.T) {
	t.Parallel()
	p := &gatePolicy{expunge: true}
	f := &fakeDaemon{ListAccounts: p.listAccounts, Expunge: func(context.Context, *chainmailv1.ExpungeRequest) (*chainmailv1.ExpungeResponse, error) {
		return nil, api.New(connect.CodePermissionDenied, api.ReasonPolicyDenied, "ask James to allow expunge for this account", errors.New("expunge is disabled"))
	}}
	e := newTestEnv(t, f, latest)
	wantError(t, e.call(t, "expunge", map[string]any{"refs": []string{testRef}}), api.ReasonPolicyDenied)
	if got := e.fake.lastCall(t, chainmailv1connect.OrganizeServiceExpungeProcedure).Msg; !proto.Equal(got, &chainmailv1.ExpungeRequest{Refs: []string{testRef}}) {
		t.Errorf("request = %v", got)
	}
	p.set(false, true, false)
	e.srv.refreshGate(t.Context())
	e.call(t, "delete_folder", map[string]any{"account": "b", "name": "Old", "force": true})
	if got := e.fake.lastCall(t, chainmailv1connect.OrganizeServiceDeleteFolderProcedure).Msg; !proto.Equal(got, &chainmailv1.DeleteFolderRequest{Account: "b", Name: "Old", Force: true}) {
		t.Errorf("request = %v", got)
	}
}

// TestGateRechecksOnlyOnAStaleList pins the lazy refresh: tools/list asks the
// daemon again only once the last check is gateInterval old, and nothing
// asks it while the session is idle.
func TestGateRechecksOnlyOnAStaleList(t *testing.T) {
	t.Parallel()
	p := &gatePolicy{}
	var mu sync.Mutex
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	f := &fakeDaemon{ListAccounts: p.listAccounts}
	e := newTestEnv(t, f, latest, func(o *Options) { o.Now = clock })
	count := func() int {
		n := 0
		for _, c := range e.fake.Calls() {
			if c.Procedure == chainmailv1connect.AccountServiceListAccountsProcedure {
				n++
			}
		}
		return n
	}
	base := count()

	p.set(true, false, false)
	advance(30 * time.Second)
	if slices.Contains(listToolNames(t, e.cs), "expunge") {
		t.Error("expunge listed before the check went stale")
	}
	advance(30 * time.Second)
	if !slices.Contains(listToolNames(t, e.cs), "expunge") {
		t.Error("expunge not listed after a stale tools/list")
	}
	if got := count() - base; got != 1 {
		t.Errorf("ListAccounts calls after start = %d, want 1", got)
	}
}

// TestOrganizeAnnotations pins the spec section 4 title and hints of every
// organize tool, the gated two included.
func TestOrganizeAnnotations(t *testing.T) {
	t.Parallel()
	p := &gatePolicy{expunge: true, folderDelete: true}
	e := newTestEnv(t, &fakeDaemon{ListAccounts: p.listAccounts}, latest)
	res, err := e.cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	type ann struct {
		title                                    string
		readOnly, destructive, idempotent, world bool
	}
	want := map[string]ann{
		"move":          {"Move messages", false, false, true, true},
		"copy":          {"Copy messages", false, false, false, true},
		"archive":       {"Archive messages", false, false, true, true},
		"trash":         {"Move messages to Trash", false, false, true, true},
		"set_flags":     {"Set flags", false, false, true, true},
		"set_labels":    {"Set Gmail labels", false, false, true, true},
		"create_folder": {"Create a folder", false, false, true, true},
		"rename_folder": {"Rename a folder", false, false, false, true},
		"expunge":       {"Expunge messages", false, true, true, true},
		"delete_folder": {"Delete a folder", false, true, true, true},
	}
	seen := 0
	for _, tl := range res.Tools {
		w, ok := want[tl.Name]
		if !ok {
			continue
		}
		seen++
		a := tl.Annotations
		got := ann{tl.Title, a.ReadOnlyHint, *a.DestructiveHint, a.IdempotentHint, *a.OpenWorldHint}
		if got != w || a.Title != w.title {
			t.Errorf("%s: annotations = %+v (annotation title %q), want %+v", tl.Name, got, a.Title, w)
		}
	}
	if seen != len(want) {
		t.Errorf("listed %d of the %d organize tools", seen, len(want))
	}
}

// heldGate is a fake ListAccounts that answers in call order. The start-up
// check allows nothing. The second check allows expunge, but the daemon holds
// it until unblock. Every later check fails, as if the daemon were restarting.
type heldGate struct {
	calls     atomic.Int32
	held      chan struct{} // closed when the second check reaches the daemon
	release   chan struct{} // closed by unblock to answer the second check
	contended chan struct{} // closed when a tools/list refresh waits for another
	unblockMu sync.Once
	waitMu    sync.Once
}

func (h *heldGate) listAccounts(ctx context.Context, _ *chainmailv1.ListAccountsRequest) (*chainmailv1.ListAccountsResponse, error) {
	switch h.calls.Add(1) {
	case 1:
		return &chainmailv1.ListAccountsResponse{}, nil
	case 2:
		close(h.held)
		select {
		case <-h.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &chainmailv1.ListAccountsResponse{Accounts: []*chainmailv1.Account{
			{Name: "a", Capabilities: &chainmailv1.AccountCapabilities{AllowExpunge: true}},
		}}, nil
	default:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("daemon restarting"))
	}
}

func (h *heldGate) unblock() { h.unblockMu.Do(func() { close(h.release) }) }

// newHeldGateEnv starts a Server on a heldGate whose start-up check is
// already 60 seconds old, so the next tools/list asks the daemon again.
func newHeldGateEnv(t *testing.T) (*testEnv, *heldGate) {
	t.Helper()
	h := &heldGate{held: make(chan struct{}), release: make(chan struct{}), contended: make(chan struct{})}
	t.Cleanup(h.unblock)
	f := &fakeDaemon{ListAccounts: h.listAccounts}
	startFake(t, f)
	var mu sync.Mutex
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	s := New(Options{Socket: f.Socket, Version: "test", AttachmentsDir: dir, Now: func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}})
	s.gate.contended = func() { h.waitMu.Do(func() { close(h.contended) }) }
	cs := connectClient(t, s, latest) // Run's start-up check is done once the client is connected
	mu.Lock()
	now = now.Add(gateInterval)
	mu.Unlock()
	return &testEnv{fake: f, srv: s, cs: cs, dir: dir}, h
}

// listAsync sends a tools/list and reports its error when it returns.
func listAsync(t *testing.T, e *testEnv) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := e.cs.ListTools(t.Context(), nil)
		done <- err
	}()
	return done
}

// TestGateAppliesOverlappingChecksInOrder overlaps two stale tools/list
// calls. The first one's check allows expunge and is held at the daemon. A
// second check, if one is made, fails at once. An older answer must never
// replace a newer one, so either the second list waits and reuses the first
// check, or the later failure decides and expunge ends hidden.
func TestGateAppliesOverlappingChecksInOrder(t *testing.T) {
	t.Parallel()
	e, h := newHeldGateEnv(t)
	first := listAsync(t, e)
	<-h.held
	second := listAsync(t, e)
	select {
	case err := <-second: // it did not wait, so its own check has been applied
		if err != nil {
			t.Fatalf("second tools/list: %v", err)
		}
		second = nil
	case <-h.contended: // it is waiting for the held check
	}
	h.unblock()
	if err := <-first; err != nil {
		t.Fatalf("first tools/list: %v", err)
	}
	if second != nil {
		if err := <-second; err != nil {
			t.Fatalf("second tools/list: %v", err)
		}
	}
	asked := h.calls.Load() - 1 // checks made by the two overlapping lists
	listed := slices.Contains(listToolNames(t, e.cs), "expunge")
	switch {
	case asked == 1 && !listed:
		t.Error("the one overlapping check allowed expunge, but it is not listed")
	case asked == 2 && listed:
		t.Error("expunge listed after the newer check failed: the older allowed answer was applied last")
	case asked != 1 && asked != 2:
		t.Errorf("the overlapping lists asked the daemon %d times, want 1 or 2", asked)
	}
}

// TestGateListStopsWaitingWhenItsContextEnds pins that a stale tools/list
// waiting for a held check gives up when its context ends: it skips the
// check, asks the daemon nothing, and the list goes on.
func TestGateListStopsWaitingWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	e, h := newHeldGateEnv(t)
	first := listAsync(t, e)
	<-h.held
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	nextRan := make(chan struct{})
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		close(nextRan)
		return nil, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.srv.gateOnList(next)(ctx, "tools/list", nil)
	}()
	select {
	case <-h.contended:
		cancel()
	case <-done:
	}
	<-done
	select {
	case <-nextRan:
	default:
		t.Error("tools/list did not go on after its context ended")
	}
	if got := h.calls.Load(); got != 2 {
		t.Errorf("daemon asked %d times, want 2 (the start-up check and the held one)", got)
	}
	h.unblock()
	if err := <-first; err != nil {
		t.Fatalf("first tools/list: %v", err)
	}
}

// TestGateLogsOnlyTheErrorKind fails ListAccounts with a sentinel in the
// error text, at start and on a stale tools/list. The stderr log names the
// failure's kind and never its text, which can carry a backend's
// diagnostics or a secret.
func TestGateLogsOnlyTheErrorKind(t *testing.T) {
	t.Parallel()
	for name, failure := range map[string]error{
		"connect error":              connect.NewError(connect.CodeUnavailable, errors.New("backend said "+sentinel)),
		"detail-less internal error": connect.NewError(connect.CodeInternal, errors.New("transport said "+sentinel)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			var logs syncBuffer
			f := &fakeDaemon{ListAccounts: func(context.Context, *chainmailv1.ListAccountsRequest) (*chainmailv1.ListAccountsResponse, error) {
				return nil, failure
			}}
			e := newTestEnv(t, f, latest, func(o *Options) {
				o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
				o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
			})
			start := logs.String()
			if !strings.Contains(start, "kind=") || !strings.Contains(start, "connect") {
				t.Errorf("start-up log does not name the failure's kind: %q", start)
			}
			mu.Lock()
			now = now.Add(gateInterval)
			mu.Unlock()
			listToolNames(t, e.cs)
			calls := 0
			for _, c := range e.fake.Calls() {
				if c.Procedure == chainmailv1connect.AccountServiceListAccountsProcedure {
					calls++
				}
			}
			if calls != 2 {
				t.Fatalf("ListAccounts called %d times, want 2 (start-up and the stale list)", calls)
			}
			all := logs.String()
			if strings.Count(all, "kind=") < 2 {
				t.Errorf("the stale refresh did not log the failure's kind: %q", all)
			}
			if strings.Contains(all, sentinel) {
				t.Errorf("the error's text reached the stderr log: %q", all)
			}
		})
	}
}

// TestFolderResultsWrapTheServersName: create_folder and rename_folder
// return the name the server uses, which a NAMESPACE prefix the server
// chose can extend. It reaches the agent only as untrusted data, and the
// summaries are fixed text.
func TestFolderResultsWrapTheServersName(t *testing.T) {
	t.Parallel()
	serverSide := "INBOX." + readHostile + ".Projects"
	f := &fakeDaemon{
		CreateFolder: func(context.Context, *chainmailv1.CreateFolderRequest) (*chainmailv1.CreateFolderResponse, error) {
			return &chainmailv1.CreateFolderResponse{Name: serverSide}, nil
		},
		RenameFolder: func(context.Context, *chainmailv1.RenameFolderRequest) (*chainmailv1.RenameFolderResponse, error) {
			return &chainmailv1.RenameFolderResponse{Name: serverSide}, nil
		},
	}
	e := newTestEnv(t, f, latest)
	for tool, args := range map[string]map[string]any{
		"create_folder": {"account": "work", "name": "Projects"},
		"rename_folder": {"account": "work", "from": "Old", "to": "Projects"},
	} {
		t.Run(tool, func(t *testing.T) {
			res := e.call(t, tool, args)
			wantOK(t, res)
			var out struct {
				Account string    `json:"account"`
				Name    untrusted `json:"name"`
			}
			decode(t, res, &out)
			if out.Account != "work" || out.Name.Untrusted != serverSide {
				t.Errorf("out = %+v", out)
			}
			if n := readWrappedOnly(t, res.StructuredContent, readHostile); n != 1 {
				t.Errorf("%d enveloped hostile names, want 1", n)
			}
			readInsideMarkers(t, textOf(res), readHostile)
		})
	}
}

// draftExpungers are the draft tools that expunge the old draft for good
// while policy allows expunge, and move it to Trash otherwise.
var draftExpungers = []string{"delete_draft", "send_draft", "update_draft"}

// destructiveHints returns each listed tool's destructiveHint.
func destructiveHints(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]bool, len(res.Tools))
	for _, tl := range res.Tools {
		if tl.Annotations == nil || tl.Annotations.DestructiveHint == nil {
			t.Fatalf("%s: no destructiveHint", tl.Name)
		}
		out[tl.Name] = *tl.Annotations.DestructiveHint
	}
	return out
}

// TestDraftToolsFollowTheExpungePolicy: update_draft, send_draft and
// delete_draft are destructive exactly while some account allows expunge,
// since the daemon then expunges the old draft rather than move it to
// Trash. The hint changes with the same check that lists expunge, on a
// stale tools/list, and the tools keep their handlers.
func TestDraftToolsFollowTheExpungePolicy(t *testing.T) {
	t.Parallel()
	p := &gatePolicy{}
	var mu sync.Mutex
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	advance := func() { mu.Lock(); now = now.Add(gateInterval); mu.Unlock() }
	f := &fakeDaemon{ListAccounts: p.listAccounts, DeleteDraft: func(context.Context, *chainmailv1.DeleteDraftRequest) (*chainmailv1.DeleteDraftResponse, error) {
		return &chainmailv1.DeleteDraftResponse{}, nil
	}}
	e := newTestEnv(t, f, latest, func(o *Options) { o.Now = clock })
	steps := []struct {
		name                  string
		expunge, folderDelete bool
	}{
		{name: "expunge off"},
		{name: "expunge on", expunge: true},
		{name: "folder delete alone does not count", folderDelete: true},
		{name: "expunge on again", expunge: true, folderDelete: true},
		{name: "expunge off again"},
	}
	for _, st := range steps { // sequential: each step builds on the last
		if st.name != steps[0].name {
			p.set(st.expunge, st.folderDelete, false)
			advance() // the next tools/list finds the check stale
		}
		hints := destructiveHints(t, e.cs)
		if _, listed := hints["expunge"]; listed != st.expunge {
			t.Errorf("%s: expunge listed = %t, want %t", st.name, listed, st.expunge)
		}
		for _, name := range draftExpungers {
			got, ok := hints[name]
			if !ok || got != st.expunge {
				t.Errorf("%s: %s destructiveHint = %t (listed %t), want %t", st.name, name, got, ok, st.expunge)
			}
		}
		for _, name := range []string{"send_email", "reply", "forward", "create_draft"} {
			if hints[name] {
				t.Errorf("%s: %s is destructive", st.name, name)
			}
		}
		wantOK(t, e.call(t, "delete_draft", map[string]any{"ref": testRef}))
		if got := e.fake.lastCall(t, chainmailv1connect.ComposeServiceDeleteDraftProcedure).Msg; !proto.Equal(got, &chainmailv1.DeleteDraftRequest{Ref: testRef}) {
			t.Errorf("%s: delete_draft request = %v", st.name, got)
		}
	}
}

// TestGateKeepsItsAnswerWhenTheListIsCancelled: a stale tools/list whose
// own context ends while its check is at the daemon learns nothing about
// policy, so the gated tools stay as they were and the check time is not
// stamped, and the next tools/list asks again. A check the daemon itself
// fails still hides the gated tools and counts as a check.
func TestGateKeepsItsAnswerWhenTheListIsCancelled(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cancelList bool
		wantListed bool
		wantStale  bool
	}{
		"the list is cancelled": {cancelList: true, wantListed: true, wantStale: true},
		"the daemon fails":      {wantListed: false, wantStale: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			reached := make(chan struct{})
			f := &fakeDaemon{ListAccounts: func(ctx context.Context, req *chainmailv1.ListAccountsRequest) (*chainmailv1.ListAccountsResponse, error) {
				if calls.Add(1) != 2 {
					return allowAll(ctx, req)
				}
				close(reached)
				if !tc.cancelList {
					return nil, connect.NewError(connect.CodeUnavailable, errors.New("daemon restarting"))
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			var mu sync.Mutex
			now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			e := newTestEnv(t, f, latest, func(o *Options) {
				o.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
			})
			mu.Lock()
			now = now.Add(gateInterval)
			mu.Unlock()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return nil, nil }
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = e.srv.gateOnList(next)(ctx, "tools/list", nil)
			}()
			<-reached
			if tc.cancelList {
				cancel()
			}
			<-done
			e.srv.gate.mu.Lock()
			expunge, folders := e.srv.gate.listed[toolExpunge], e.srv.gate.listed[toolDeleteFolder]
			e.srv.gate.mu.Unlock()
			if expunge != tc.wantListed || folders != tc.wantListed {
				t.Errorf("expunge listed %t, delete_folder listed %t; want both %t", expunge, folders, tc.wantListed)
			}
			if stale := e.srv.gateStale(); stale != tc.wantStale {
				t.Errorf("check stale after the list = %t, want %t", stale, tc.wantStale)
			}
			hints := destructiveHints(t, e.cs)
			if tc.wantStale && calls.Load() != 3 {
				t.Errorf("ListAccounts called %d times, want 3: the next tools/list did not ask again", calls.Load())
			}
			if _, ok := hints[toolExpunge]; ok != tc.wantListed {
				t.Errorf("expunge in tools/list = %t, want %t", ok, tc.wantListed)
			}
			for _, n := range draftExpungers {
				if hints[n] != tc.wantListed {
					t.Errorf("%s destructiveHint = %t, want %t", n, hints[n], tc.wantListed)
				}
			}
		})
	}
}
