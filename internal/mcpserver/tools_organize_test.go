package mcpserver

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
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
