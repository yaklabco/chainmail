package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/items"
)

// itemAt is the time the item fixtures report.
var itemAt = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

// ut wraps s as untrusted text, as the daemon sends item and message text.
func ut(s string) *chainmailv1.Untrusted { return &chainmailv1.Untrusted{Untrusted: s} }

// fakeItems is an ItemsService for CLI tests. Each func answers its RPC;
// a nil func answers with an empty response. It records every request.
type fakeItems struct {
	chainmailv1connect.UnimplementedItemsServiceHandler

	create   func(*chainmailv1.CreateItemRequest) (*chainmailv1.CreateItemResponse, error)
	update   func(*chainmailv1.UpdateItemRequest) (*chainmailv1.UpdateItemResponse, error)
	del      func(*chainmailv1.DeleteItemRequest) (*chainmailv1.DeleteItemResponse, error)
	restore  func(*chainmailv1.RestoreItemRequest) (*chainmailv1.RestoreItemResponse, error)
	get      func(*chainmailv1.GetItemRequest) (*chainmailv1.GetItemResponse, error)
	list     func(*chainmailv1.ListItemsRequest) (*chainmailv1.ListItemsResponse, error)
	criteria func(*chainmailv1.EditCriteriaRequest) (*chainmailv1.EditCriteriaResponse, error)
	tick     func(*chainmailv1.TickCriterionRequest) (*chainmailv1.TickCriterionResponse, error)
	block    func(*chainmailv1.SetBlockerRequest) (*chainmailv1.SetBlockerResponse, error)
	link     func(*chainmailv1.LinkThreadRequest) (*chainmailv1.LinkThreadResponse, error)
	unlink   func(*chainmailv1.UnlinkThreadRequest) (*chainmailv1.UnlinkThreadResponse, error)

	mu   sync.Mutex
	reqs []proto.Message
}

func (f *fakeItems) record(m proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, m)
}

// requests returns every request so far, in order.
func (f *fakeItems) requests() []proto.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

// answerItems records req and answers it with fn, or with an empty
// response when fn is nil.
func answerItems[Req proto.Message, Resp any](f *fakeItems, req Req, fn func(Req) (*Resp, error)) (*connect.Response[Resp], error) {
	f.record(req)
	if fn == nil {
		return connect.NewResponse(new(Resp)), nil
	}
	resp, err := fn(req)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeItems) CreateItem(_ context.Context, req *connect.Request[chainmailv1.CreateItemRequest]) (*connect.Response[chainmailv1.CreateItemResponse], error) {
	return answerItems(f, req.Msg, f.create)
}

func (f *fakeItems) UpdateItem(_ context.Context, req *connect.Request[chainmailv1.UpdateItemRequest]) (*connect.Response[chainmailv1.UpdateItemResponse], error) {
	return answerItems(f, req.Msg, f.update)
}

func (f *fakeItems) DeleteItem(_ context.Context, req *connect.Request[chainmailv1.DeleteItemRequest]) (*connect.Response[chainmailv1.DeleteItemResponse], error) {
	return answerItems(f, req.Msg, f.del)
}

func (f *fakeItems) RestoreItem(_ context.Context, req *connect.Request[chainmailv1.RestoreItemRequest]) (*connect.Response[chainmailv1.RestoreItemResponse], error) {
	return answerItems(f, req.Msg, f.restore)
}

func (f *fakeItems) GetItem(_ context.Context, req *connect.Request[chainmailv1.GetItemRequest]) (*connect.Response[chainmailv1.GetItemResponse], error) {
	return answerItems(f, req.Msg, f.get)
}

func (f *fakeItems) ListItems(_ context.Context, req *connect.Request[chainmailv1.ListItemsRequest]) (*connect.Response[chainmailv1.ListItemsResponse], error) {
	return answerItems(f, req.Msg, f.list)
}

func (f *fakeItems) EditCriteria(_ context.Context, req *connect.Request[chainmailv1.EditCriteriaRequest]) (*connect.Response[chainmailv1.EditCriteriaResponse], error) {
	return answerItems(f, req.Msg, f.criteria)
}

func (f *fakeItems) TickCriterion(_ context.Context, req *connect.Request[chainmailv1.TickCriterionRequest]) (*connect.Response[chainmailv1.TickCriterionResponse], error) {
	return answerItems(f, req.Msg, f.tick)
}

func (f *fakeItems) SetBlocker(_ context.Context, req *connect.Request[chainmailv1.SetBlockerRequest]) (*connect.Response[chainmailv1.SetBlockerResponse], error) {
	return answerItems(f, req.Msg, f.block)
}

func (f *fakeItems) LinkThread(_ context.Context, req *connect.Request[chainmailv1.LinkThreadRequest]) (*connect.Response[chainmailv1.LinkThreadResponse], error) {
	return answerItems(f, req.Msg, f.link)
}

func (f *fakeItems) UnlinkThread(_ context.Context, req *connect.Request[chainmailv1.UnlinkThreadRequest]) (*connect.Response[chainmailv1.UnlinkThreadResponse], error) {
	return answerItems(f, req.Msg, f.unlink)
}

// startFakeItems serves f on a Unix socket until the test ends and returns the socket.
func startFakeItems(t *testing.T, f *fakeItems) string {
	t.Helper()
	path, handler := chainmailv1connect.NewItemsServiceHandler(f)
	return startFakeServer(t, path, handler)
}

// itemShowFixture is a task under the work Admin initiative with criteria, a
// blocker and two links, one of them on an account that is gone. Its
// description holds an escape sequence the table must not print.
func itemShowFixture() *chainmailv1.GetItemResponse {
	estimate := 1.5
	return &chainmailv1.GetItemResponse{
		Item: &chainmailv1.WorkItem{
			Id: "it_task1", Type: "task", Title: ut("Renew the domain"),
			Description: ut("Pay before the 20th.\n\nAsk about \x1b[31mbulk pricing.\n"),
			State:       "in-progress", Priority: "p1", EffectiveArea: "work", EffectivePriority: "p1",
			DueDate: "2026-11-20", Estimate: &estimate, ParentId: "it_admin_work", Revision: 42,
			CreatedAt: timestamppb.New(itemAt), UpdatedAt: timestamppb.New(itemAt.Add(time.Hour)),
			CreatedBy: "1000/mcp", UpdatedBy: "1000/cli",
		},
		Criteria: []*chainmailv1.Criterion{
			{Id: "cr_a", Text: ut("Invoice paid"), Ticked: true, Evidence: ut("receipt in cm1:work:INBOX:7:42"), TickedAt: timestamppb.New(itemAt), TickedBy: "1000/cli"},
			{Id: "cr_b", Position: 1, Text: ut("DNS checked"), Evidence: ut("")},
		},
		Ancestors: []*chainmailv1.WorkItemRow{
			{Id: "it_admin_work", Type: "initiative", Title: ut("Admin"), State: "backlog", EffectiveArea: "work", EffectivePriority: "p2", Ongoing: true, Children: 1, Revision: 1},
		},
		BlockedBy: []*chainmailv1.WorkItemRow{
			{Id: "it_task0", Type: "task", Title: ut("Find the registrar login"), State: "done", EffectiveArea: "work", EffectivePriority: "p2", ParentId: "it_admin_work", Revision: 40},
		},
		Threads: []*chainmailv1.ThreadLink{
			{
				Account: "work", ThreadKey: "a1b2c3d4", Note: ut("the renewal notice"), AnchorRef: "cm1:work:INBOX:7:40",
				State: "ok", Source: "cache", Messages: 3,
				Latest: &chainmailv1.MessageRow{
					Ref: "cm1:work:INBOX:7:42", Account: "work", Folder: "INBOX", Date: timestamppb.New(itemAt.Add(-2 * time.Hour)),
					From:    []*chainmailv1.Address{{Name: ut("Registrar"), Email: ut("billing@registrar.example")}},
					Subject: ut("Re: Domain renewal"),
				},
				CreatedAt: timestamppb.New(itemAt), CreatedBy: "1000/mcp",
			},
			{Account: "old", ThreadKey: "ffee", Note: ut(""), State: "unavailable", Reason: "NOT_FOUND", CreatedAt: timestamppb.New(itemAt), CreatedBy: "1000/cli"},
		},
		ThreadsTotal: 2,
	}
}

// itemCase is an item command, run with the arguments after "item" (or,
// for completion, the whole command line), and what a test expects of it.
type itemCase struct {
	name  string
	args  []string
	want  proto.Message // the one request it sends
	out   string        // what it prints with table output
	usage string        // text its usage error mentions
	cands []string      // the completion candidates it offers
}

// testItemRequests runs each case against a fresh fake with JSON output
// and checks that it sent exactly the case's request.
func testItemRequests(t *testing.T, cases []itemCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeItems{}
			args := append([]string{"--socket", startFakeItems(t, f), "--output", "json", "item"}, tt.args...)
			got := runCLI(t, Env{}, args...)
			if got.code != 0 {
				t.Fatalf("item %v: %+v", tt.args, got)
			}
			if reqs := f.requests(); !slices.EqualFunc(reqs, []proto.Message{tt.want}, proto.Equal) {
				t.Errorf("requests = %v, want %v", reqs, tt.want)
			}
		})
	}
}

// testItemUsage runs each case with no daemon and checks that it fails
// with a usage error mentioning the case's usage text.
func testItemUsage(t *testing.T, cases []itemCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, append([]string{"--socket", "/nonexistent.sock", "item"}, tt.args...)...)
			if got.code != api.ExitUsage || errorReason(t, got.stderr) != api.ReasonUsage {
				t.Fatalf("item %v: %+v, want a usage error", tt.args, got)
			}
			if !strings.Contains(got.stderr, tt.usage) {
				t.Errorf("stderr = %s, want it to mention %q", got.stderr, tt.usage)
			}
		})
	}
}

// testItemCompletion completes each case's arguments against the fake
// completionItems returns and checks the candidates' values, in order.
func testItemCompletion(t *testing.T, cases []itemCase) {
	t.Helper()
	env := sockEnv(startFakeItems(t, completionItems()))
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cands, _ := completeArgs(t, env, tt.args...)
			if got := values(cands); !slices.Equal(got, tt.cands) {
				t.Errorf("__complete %q: %q, want %q", tt.args, got, tt.cands)
			}
		})
	}
}

// completionItems answers ListItems with two live items, and a deleted
// one when deleted items are asked for, and GetItem with the show
// fixture plus a second link on the work account.
func completionItems() *fakeItems {
	return &fakeItems{
		list: func(r *chainmailv1.ListItemsRequest) (*chainmailv1.ListItemsResponse, error) {
			rows := []*chainmailv1.WorkItemRow{
				{Id: "it_admin_work", Type: "initiative", Title: ut("Admin"), Ongoing: true},
				{Id: "it_task1", Type: "task", Title: ut("Renew\x1b[2J the\ndomain")},
			}
			if r.GetIncludeDeleted() {
				rows = append(rows, &chainmailv1.WorkItemRow{Id: "it_gone", Type: "task", Title: ut("Old"), Deleted: true})
			}
			return &chainmailv1.ListItemsResponse{Items: rows}, nil
		},
		get: func(*chainmailv1.GetItemRequest) (*chainmailv1.GetItemResponse, error) {
			view := itemShowFixture()
			view.Threads = append(view.Threads, &chainmailv1.ThreadLink{Account: "work", ThreadKey: "b2c3d4e5", State: "empty"})
			return view, nil
		},
	}
}

func TestItemShowGolden(t *testing.T) {
	t.Parallel()
	f := &fakeItems{get: func(*chainmailv1.GetItemRequest) (*chainmailv1.GetItemResponse, error) { return itemShowFixture(), nil }}
	socket := startFakeItems(t, f)
	for _, tt := range []struct{ output, golden string }{
		{output: "json", golden: "item_show_json"},
		{output: "table", golden: "item_show_table"},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, "--socket", socket, "--output", tt.output, "item", "show", "it_task1")
			if got.code != 0 {
				t.Fatalf("item show: %+v", got)
			}
			golden(t, tt.golden, got.stdout)
		})
	}
}

// A show with no description, criteria, related items or links prints the
// fields only. A task with no parent says it is in the inbox, and a
// priority it does not set is the default.
func TestItemShowBareItem(t *testing.T) {
	t.Parallel()
	view := &chainmailv1.GetItemResponse{Item: &chainmailv1.WorkItem{
		Id: "it_task9", Type: "task", Title: ut("Sort me"), State: "backlog", Area: "personal",
		EffectiveArea: "personal", EffectivePriority: "p2", Revision: 3,
		CreatedAt: timestamppb.New(itemAt), UpdatedAt: timestamppb.New(itemAt), CreatedBy: "1000/cli", UpdatedBy: "1000/cli",
	}}
	var b strings.Builder
	if err := itemViewText(&b, time.UTC, view); err != nil {
		t.Fatal(err)
	}
	want := "id        it_task9\n" +
		"type      task\n" +
		"title     Sort me\n" +
		"state     backlog\n" +
		"area      personal\n" +
		"priority  p2 (default)\n" +
		"parent    - (inbox)\n" +
		"start     -\n" +
		"due       -\n" +
		"estimate  -\n" +
		"ongoing   false\n" +
		"deleted   -\n" +
		"revision  3\n" +
		"created   2026-11-06 09:00 by 1000/cli\n" +
		"updated   2026-11-06 09:00 by 1000/cli\n"
	if b.String() != want {
		t.Errorf("itemViewText =\n%q\nwant\n%q", b.String(), want)
	}
}

func TestItemShowRequestAndUsage(t *testing.T) {
	t.Parallel()
	testItemRequests(t, []itemCase{
		{name: "show", args: []string{"show", "it_task1"}, want: &chainmailv1.GetItemRequest{Id: "it_task1"}},
	})
	testItemUsage(t, []itemCase{
		{name: "show without an id", args: []string{"show"}, usage: "accepts 1 arg(s)"},
		{name: "show two", args: []string{"show", "a", "b"}, usage: "accepts 1 arg(s)"},
	})
}

// Daemon refusals keep their reason, and so their exit code.
func TestItemReportsDaemonErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		code   int
		reason string
	}{
		{name: "not found", err: api.New(connect.CodeNotFound, api.ReasonNotFound, "run: chainmail item list", errors.New("not found: item")), code: api.ExitNotFound, reason: api.ReasonNotFound},
		{name: "invalid", err: api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "", errors.New("bad id")), code: api.ExitUsage, reason: api.ReasonInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeItems{get: func(*chainmailv1.GetItemRequest) (*chainmailv1.GetItemResponse, error) { return nil, tt.err }}
			got := runCLI(t, Env{}, "--socket", startFakeItems(t, f), "item", "show", "it_nope")
			if got.code != tt.code || errorReason(t, got.stderr) != tt.reason {
				t.Errorf("item show: %+v, want exit %d %s", got, tt.code, tt.reason)
			}
		})
	}
}

func TestCompleteItemShow(t *testing.T) {
	t.Parallel()
	testItemCompletion(t, []itemCase{
		{name: "show", args: []string{"item", "show", ""}, cands: []string{"it_admin_work", "it_task1"}},
		{name: "show prefix", args: []string{"item", "show", "it_t"}, cands: []string{"it_task1"}},
		{name: "show takes one", args: []string{"item", "show", "it_task1", ""}},
	})
}

// An item ID completion reads one full page of live items, and describes
// each by its type and title, cleaned to one line.
func TestCompleteItemIDsReadAFullPage(t *testing.T) {
	t.Parallel()
	f := completionItems()
	cands, _ := completeArgs(t, sockEnv(startFakeItems(t, f)), "item", "show", "it_task")
	if want := []string{"it_task1\ttask · Renew [2J the domain"}; !slices.Equal(cands, want) {
		t.Errorf("candidates %q, want %q", cands, want)
	}
	want := []proto.Message{&chainmailv1.ListItemsRequest{Limit: itemCompletionLimit}}
	if reqs := f.requests(); !slices.EqualFunc(reqs, want, proto.Equal) {
		t.Errorf("requests = %v, want %v", reqs, want)
	}
}

// itemListFixture is a page of three rows in tree order: Admin, a task
// under it, and a deleted inbox task whose title holds an escape sequence
// and a line break.
func itemListFixture() *chainmailv1.ListItemsResponse {
	return &chainmailv1.ListItemsResponse{
		Items: []*chainmailv1.WorkItemRow{
			{Id: "it_admin_work", Type: "initiative", Title: ut("Admin"), State: "backlog", EffectiveArea: "work", EffectivePriority: "p2", Ongoing: true, Children: 1, Revision: 1},
			{
				Id: "it_task1", Type: "task", Title: ut("Renew the domain"), State: "in-progress", EffectiveArea: "work", EffectivePriority: "p1",
				DueDate: "2026-11-20", ParentId: "it_admin_work", Depth: 1, CriteriaTotal: 2, CriteriaTicked: 1, Links: 2, Revision: 42,
			},
			{Id: "it_task2", Type: "task", Title: ut("Call the \x1b[2Jplumber\nnow"), State: "backlog", EffectiveArea: "personal", EffectivePriority: "p2", Deleted: true, Revision: 57},
		},
		NextPageToken: "dDoz", Revision: 57,
	}
}

func TestItemListGolden(t *testing.T) {
	t.Parallel()
	f := &fakeItems{list: func(*chainmailv1.ListItemsRequest) (*chainmailv1.ListItemsResponse, error) {
		return itemListFixture(), nil
	}}
	socket := startFakeItems(t, f)
	for _, tt := range []struct{ output, golden string }{
		{output: "json", golden: "item_list_json"},
		{output: "table", golden: "item_list_table"},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()
			got := runCLI(t, Env{}, "--socket", socket, "--output", tt.output, "item", "list")
			if got.code != 0 {
				t.Fatalf("item list: %+v", got)
			}
			golden(t, tt.golden, got.stdout)
		})
	}
}

func TestItemListRequestsAndUsage(t *testing.T) {
	t.Parallel()
	testItemRequests(t, []itemCase{
		{name: "list", args: []string{"list"}, want: &chainmailv1.ListItemsRequest{}},
		{
			name: "list every flag",
			args: []string{
				"list", "--type", "task", "--state", "backlog", "--area", "work", "--under", "it_admin_work", "--inbox",
				"--ongoing", "--deleted", "--changed-since", "0", "--limit", "200", "--page-token", "dDoz",
			},
			want: &chainmailv1.ListItemsRequest{
				Type: "task", State: "backlog", Area: "work", Under: "it_admin_work", Inbox: true, Ongoing: proto.Bool(true),
				IncludeDeleted: true, ChangedSince: proto.Int64(0), Limit: 200, PageToken: "dDoz",
			},
		},
		{name: "list not ongoing", args: []string{"list", "--no-ongoing"}, want: &chainmailv1.ListItemsRequest{Ongoing: proto.Bool(false)}},
	})
	testItemUsage(t, []itemCase{
		{name: "list ongoing both ways", args: []string{"list", "--ongoing", "--no-ongoing"}, usage: "--ongoing and --no-ongoing"},
		{name: "list negative revision", args: []string{"list", "--changed-since", "-1"}, usage: "--changed-since must not be negative"},
		{name: "list argument", args: []string{"list", "extra"}, usage: "accepts 0 arg(s)"},
		{name: "list bad limit", args: []string{"list", "--limit", "many"}, usage: "--limit"},
	})
}

func TestCompleteItemList(t *testing.T) {
	t.Parallel()
	testItemCompletion(t, []itemCase{
		{name: "under", args: []string{"item", "list", "--under", "it_a"}, cands: []string{"it_admin_work"}},
		{name: "states", args: []string{"item", "list", "--state", ""}, cands: itemStates},
		{name: "type prefix", args: []string{"item", "list", "--type", "b"}, cands: []string{"bug"}},
		{name: "areas", args: []string{"item", "list", "--area", ""}, cands: itemAreas},
	})
}

// TestItemValuesMatchTheModel keeps the values help and completion offer
// in step with the item rules: each parses, in the model's order, and none
// is missing.
func TestItemValuesMatchTheModel(t *testing.T) {
	t.Parallel()
	wantTypes := []items.Type{
		items.TypeObjective, items.TypeKeyResult, items.TypeInitiative, items.TypeEpic, items.TypeFeature,
		items.TypeStory, items.TypeBug, items.TypeSupportTicket, items.TypeTask,
	}
	if len(itemTypes) != len(wantTypes) {
		t.Errorf("itemTypes has %d values, want %d", len(itemTypes), len(wantTypes))
	}
	for i, v := range itemTypes {
		if got, err := items.ParseType(v); err != nil || i >= len(wantTypes) || got != wantTypes[i] {
			t.Errorf("itemTypes[%d] = %q parses as %q, %v", i, v, got, err)
		}
	}
	wantStates := []string{string(items.StateBacklog), string(items.StateInProgress), string(items.StateInReview), string(items.StateDone)}
	if !slices.Equal(itemStates, wantStates) {
		t.Errorf("itemStates = %q, want %q", itemStates, wantStates)
	}
	if want := []string{string(items.AreaWork), string(items.AreaPersonal)}; !slices.Equal(itemAreas, want) {
		t.Errorf("itemAreas = %q, want %q", itemAreas, want)
	}
	wantPriorities := []string{string(items.PriorityP0), string(items.PriorityP1), string(items.PriorityP2), string(items.PriorityP3)}
	if !slices.Equal(itemPriorities, wantPriorities) {
		t.Errorf("itemPriorities = %q, want %q", itemPriorities, wantPriorities)
	}
	for _, v := range itemStates {
		if _, err := items.ParseState(v); err != nil {
			t.Errorf("state %q: %v", v, err)
		}
	}
	for _, v := range itemAreas {
		if _, err := items.ParseArea(v); err != nil {
			t.Errorf("area %q: %v", v, err)
		}
	}
	for _, v := range itemPriorities {
		if _, err := items.ParsePriority(v); err != nil {
			t.Errorf("priority %q: %v", v, err)
		}
	}
}

// testItemTable runs each case against the fake newFake returns, with
// table output, and checks that it printed exactly the case's out.
func testItemTable(t *testing.T, newFake func() *fakeItems, cases []itemCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"--socket", startFakeItems(t, newFake()), "--output", "table", "item"}, tt.args...)
			got := runCLI(t, Env{}, args...)
			if got.code != 0 {
				t.Fatalf("item %v: %+v", tt.args, got)
			}
			if got.stdout != tt.out {
				t.Errorf("stdout = %q, want %q", got.stdout, tt.out)
			}
		})
	}
}

// descriptionFile writes a description file holding "line one\nline two\n"
// for the test and returns its path.
func descriptionFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "description.md")
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestItemCreateRequestsAndUsage(t *testing.T) {
	t.Parallel()
	desc := descriptionFile(t)
	testItemRequests(t, []itemCase{
		{name: "create", args: []string{"create", "task", "Renew the domain"}, want: &chainmailv1.CreateItemRequest{Type: "task", Title: "Renew the domain"}},
		{
			name: "create every flag",
			args: []string{
				"create", "task", "Renew the domain", "--description", "Pay it", "--parent", "it_admin_work", "--area", "work",
				"--priority", "p1", "--state", "in-progress", "--start", "2026-11-07", "--due", "2026-11-20", "--estimate", "1.5",
				"--criterion", "Invoice paid", "--criterion", "DNS checked", "--idempotency-key", "triage:42",
			},
			want: &chainmailv1.CreateItemRequest{
				Type: "task", Title: "Renew the domain", Description: "Pay it", ParentId: "it_admin_work", Area: "work",
				Priority: "p1", State: "in-progress", StartDate: "2026-11-07", DueDate: "2026-11-20", Estimate: proto.Float64(1.5),
				Criteria: []string{"Invoice paid", "DNS checked"}, IdempotencyKey: "triage:42",
			},
		},
		{name: "create zero estimate", args: []string{"create", "story", "Spike", "--estimate", "0"}, want: &chainmailv1.CreateItemRequest{Type: "story", Title: "Spike", Estimate: proto.Float64(0)}},
		{name: "create description file", args: []string{"create", "feature", "Search", "--description-file", desc}, want: &chainmailv1.CreateItemRequest{Type: "feature", Title: "Search", Description: "line one\nline two\n"}},
	})
	testItemUsage(t, []itemCase{
		{name: "create without a title", args: []string{"create", "task"}, usage: "accepts 2 arg(s)"},
		{name: "create with both descriptions", args: []string{"create", "task", "T", "--description", "a", "--description-file", desc}, usage: "--description and --description-file"},
		{name: "create with a missing file", args: []string{"create", "task", "T", "--description-file", "/nonexistent/x.md"}, usage: "no such file"},
		{name: "create with a bad estimate", args: []string{"create", "task", "T", "--estimate", "soon"}, usage: "--estimate"},
	})
}

func TestItemCreateTable(t *testing.T) {
	t.Parallel()
	newFake := func() *fakeItems {
		return &fakeItems{create: func(r *chainmailv1.CreateItemRequest) (*chainmailv1.CreateItemResponse, error) {
			item := &chainmailv1.WorkItem{Id: "it_task1", Type: r.GetType(), Revision: 43}
			return &chainmailv1.CreateItemResponse{Item: item, Duplicate: r.GetIdempotencyKey() != ""}, nil
		}}
	}
	testItemTable(t, newFake, []itemCase{
		{name: "create", args: []string{"create", "task", "Renew the domain"}, out: "created task it_task1\n"},
		{
			name: "create again", args: []string{"create", "task", "Renew the domain", "--idempotency-key", "k1"},
			out: "already created task it_task1 by an earlier call with this idempotency key\n",
		},
	})
}

func TestCompleteItemCreate(t *testing.T) {
	t.Parallel()
	testItemCompletion(t, []itemCase{
		{name: "types", args: []string{"item", "create", ""}, cands: itemTypes},
		{name: "type prefix", args: []string{"item", "create", "s"}, cands: []string{"story", "support-ticket"}},
		{name: "title", args: []string{"item", "create", "task", ""}},
		{name: "areas", args: []string{"item", "create", "task", "T", "--area", ""}, cands: itemAreas},
		{name: "priorities", args: []string{"item", "create", "task", "T", "--priority", ""}, cands: itemPriorities},
		{name: "parent", args: []string{"item", "create", "task", "T", "--parent", ""}, cands: []string{"it_admin_work", "it_task1"}},
	})
}

func TestItemUpdateRequestsAndUsage(t *testing.T) {
	t.Parallel()
	desc := descriptionFile(t)
	mask := func(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
	testItemRequests(t, []itemCase{
		{
			name: "update in mask order",
			args: []string{"update", "it_task1", "--due", "2026-12-01", "--state", "done", "--title", "Renewed"},
			want: &chainmailv1.UpdateItemRequest{Id: "it_task1", UpdateMask: mask("title", "state", "due_date"), Title: "Renewed", State: "done", DueDate: "2026-12-01"},
		},
		{name: "update type", args: []string{"update", "it_task1", "--type", "story"}, want: &chainmailv1.UpdateItemRequest{Id: "it_task1", UpdateMask: mask("type"), Type: "story"}},
		{
			name: "update clear and set",
			args: []string{"update", "it_task1", "--clear", "due,area", "--estimate", "2"},
			want: &chainmailv1.UpdateItemRequest{Id: "it_task1", UpdateMask: mask("area", "due_date", "estimate"), Estimate: proto.Float64(2)},
		},
		{
			name: "update clear twice",
			args: []string{"update", "it_task1", "--clear", "estimate", "--clear", "description,estimate"},
			want: &chainmailv1.UpdateItemRequest{Id: "it_task1", UpdateMask: mask("description", "estimate")},
		},
		{
			name: "update every clear",
			args: []string{"update", "it_task1", "--clear", "area,priority,start,due,estimate,description"},
			want: &chainmailv1.UpdateItemRequest{Id: "it_task1", UpdateMask: mask("description", "area", "priority", "start_date", "due_date", "estimate")},
		},
		{
			name: "update description file",
			args: []string{"update", "it_task1", "--description-file", desc, "--priority", "p0", "--start", "2026-11-08"},
			want: &chainmailv1.UpdateItemRequest{
				Id: "it_task1", UpdateMask: mask("description", "priority", "start_date"),
				Description: "line one\nline two\n", Priority: "p0", StartDate: "2026-11-08",
			},
		},
		{name: "update empty description", args: []string{"update", "it_task1", "--description", ""}, want: &chainmailv1.UpdateItemRequest{Id: "it_task1", UpdateMask: mask("description")}},
		{name: "move", args: []string{"move", "it_task1", "it_feat1"}, want: &chainmailv1.UpdateItemRequest{Id: "it_task1", UpdateMask: mask("parent_id"), ParentId: "it_feat1"}},
		{name: "move to root", args: []string{"move", "it_task1", "--root"}, want: &chainmailv1.UpdateItemRequest{Id: "it_task1", UpdateMask: mask("parent_id")}},
	})
	testItemUsage(t, []itemCase{
		{name: "update nothing", args: []string{"update", "it_task1"}, usage: "nothing to change"},
		{name: "update unknown clear", args: []string{"update", "it_task1", "--clear", "title"}, usage: "unknown --clear"},
		{name: "update set and clear", args: []string{"update", "it_task1", "--clear", "area", "--area", "work"}, usage: "--clear area cannot be used with the flag that sets it"},
		{name: "update description and clear", args: []string{"update", "it_task1", "--clear", "description", "--description", "x"}, usage: "--clear description cannot be used"},
		{name: "update file and clear", args: []string{"update", "it_task1", "--clear", "description", "--description-file", desc}, usage: "--clear description cannot be used"},
		{name: "move without a parent", args: []string{"move", "it_task1"}, usage: "give a parent ID or --root"},
		{name: "move with both", args: []string{"move", "it_task1", "it_feat1", "--root"}, usage: "--root and a parent ID"},
		{name: "move three", args: []string{"move", "a", "b", "c"}, usage: "accepts between 1 and 2 arg(s)"},
	})
}

func TestItemUpdateTable(t *testing.T) {
	t.Parallel()
	newFake := func() *fakeItems {
		return &fakeItems{update: func(r *chainmailv1.UpdateItemRequest) (*chainmailv1.UpdateItemResponse, error) {
			return &chainmailv1.UpdateItemResponse{Item: &chainmailv1.WorkItem{Id: r.GetId(), Type: "task", ParentId: r.GetParentId(), Revision: 44}}, nil
		}}
	}
	testItemTable(t, newFake, []itemCase{
		{name: "update", args: []string{"update", "it_task1", "--state", "done"}, out: "updated task it_task1, revision 44\n"},
		{name: "move", args: []string{"move", "it_task1", "it_feat1"}, out: "moved it_task1 under it_feat1, revision 44\n"},
		{name: "move to root", args: []string{"move", "it_task1", "--root"}, out: "moved it_task1 to the top of the tree, revision 44\n"},
	})
}

func TestCompleteItemUpdate(t *testing.T) {
	t.Parallel()
	testItemCompletion(t, []itemCase{
		{name: "update item", args: []string{"item", "update", ""}, cands: []string{"it_admin_work", "it_task1"}},
		{name: "priorities", args: []string{"item", "update", "it_task1", "--priority", ""}, cands: itemPriorities},
		{name: "clear", args: []string{"item", "update", "it_task1", "--clear", ""}, cands: itemClearFields},
		{name: "second clear", args: []string{"item", "update", "it_task1", "--clear", "due,e"}, cands: []string{"due,estimate"}},
		{name: "move parent leaves out the item", args: []string{"item", "move", "it_task1", ""}, cands: []string{"it_admin_work"}},
		{name: "move takes two", args: []string{"item", "move", "it_task1", "it_admin_work", ""}},
	})
}

func TestItemDeleteAndRestore(t *testing.T) {
	t.Parallel()
	testItemRequests(t, []itemCase{
		{name: "delete", args: []string{"delete", "it_task1"}, want: &chainmailv1.DeleteItemRequest{Id: "it_task1"}},
		{name: "delete cascade", args: []string{"delete", "it_feat1", "--cascade"}, want: &chainmailv1.DeleteItemRequest{Id: "it_feat1", Cascade: true}},
		{name: "restore", args: []string{"restore", "it_task1"}, want: &chainmailv1.RestoreItemRequest{Id: "it_task1"}},
		{name: "restore cascade", args: []string{"restore", "it_feat1", "--cascade"}, want: &chainmailv1.RestoreItemRequest{Id: "it_feat1", Cascade: true}},
	})
	testItemUsage(t, []itemCase{
		{name: "delete two", args: []string{"delete", "a", "b"}, usage: "accepts 1 arg(s)"},
		{name: "restore none", args: []string{"restore"}, usage: "accepts 1 arg(s)"},
	})
	newFake := func() *fakeItems {
		return &fakeItems{
			del: func(*chainmailv1.DeleteItemRequest) (*chainmailv1.DeleteItemResponse, error) {
				return &chainmailv1.DeleteItemResponse{Ids: []string{"it_feat1", "it_task1"}, Revision: 46}, nil
			},
			restore: func(*chainmailv1.RestoreItemRequest) (*chainmailv1.RestoreItemResponse, error) {
				return &chainmailv1.RestoreItemResponse{Ids: []string{"it_feat1", "it_task1"}, Revision: 47}, nil
			},
		}
	}
	testItemTable(t, newFake, []itemCase{
		{name: "delete table", args: []string{"delete", "it_feat1", "--cascade"}, out: "deleted it_feat1 it_task1, revision 46\n"},
		{name: "restore table", args: []string{"restore", "it_feat1", "--cascade"}, out: "restored it_feat1 it_task1, revision 47\n"},
	})
	testItemCompletion(t, []itemCase{
		{name: "delete offers live items", args: []string{"item", "delete", ""}, cands: []string{"it_admin_work", "it_task1"}},
		{name: "restore offers deleted items", args: []string{"item", "restore", ""}, cands: []string{"it_gone"}},
	})
}

func TestItemCriteria(t *testing.T) {
	t.Parallel()
	testItemRequests(t, []itemCase{
		{name: "add", args: []string{"criteria", "add", "it_task1", "Invoice paid", "DNS checked"}, want: &chainmailv1.EditCriteriaRequest{ItemId: "it_task1", Add: []string{"Invoice paid", "DNS checked"}}},
		{
			name: "edit",
			args: []string{"criteria", "edit", "it_task1", "cr_a", "Every invoice paid"},
			want: &chainmailv1.EditCriteriaRequest{ItemId: "it_task1", Edit: []*chainmailv1.CriterionText{{Id: "cr_a", Text: "Every invoice paid"}}},
		},
		{name: "remove", args: []string{"criteria", "remove", "it_task1", "cr_a", "cr_b"}, want: &chainmailv1.EditCriteriaRequest{ItemId: "it_task1", Remove: []string{"cr_a", "cr_b"}}},
		{name: "order", args: []string{"criteria", "order", "it_task1", "cr_b", "cr_a"}, want: &chainmailv1.EditCriteriaRequest{ItemId: "it_task1", Order: []string{"cr_b", "cr_a"}}},
		{
			name: "tick",
			args: []string{"criteria", "tick", "it_task1", "cr_a", "--evidence", "receipt"},
			want: &chainmailv1.TickCriterionRequest{ItemId: "it_task1", CriterionId: "cr_a", Ticked: true, Evidence: "receipt"},
		},
		{name: "untick", args: []string{"criteria", "untick", "it_task1", "cr_a"}, want: &chainmailv1.TickCriterionRequest{ItemId: "it_task1", CriterionId: "cr_a"}},
	})
	testItemUsage(t, []itemCase{
		{name: "bare criteria", args: []string{"criteria"}, usage: "no command given"},
		{name: "add without text", args: []string{"criteria", "add", "it_task1"}, usage: "requires at least 2 arg(s)"},
		{name: "edit without text", args: []string{"criteria", "edit", "it_task1", "cr_a"}, usage: "accepts 3 arg(s)"},
		{name: "remove without a criterion", args: []string{"criteria", "remove", "it_task1"}, usage: "requires at least 2 arg(s)"},
		{name: "tick without evidence", args: []string{"criteria", "tick", "it_task1", "cr_a"}, usage: "--evidence is required"},
		{name: "tick with blank evidence", args: []string{"criteria", "tick", "it_task1", "cr_a", "--evidence", "  "}, usage: "--evidence is required"},
		{name: "untick takes no evidence", args: []string{"criteria", "untick", "it_task1", "cr_a", "--evidence", "x"}, usage: "unknown flag: --evidence"},
	})
	newFake := func() *fakeItems {
		return &fakeItems{
			criteria: func(*chainmailv1.EditCriteriaRequest) (*chainmailv1.EditCriteriaResponse, error) {
				return &chainmailv1.EditCriteriaResponse{Criteria: []*chainmailv1.Criterion{
					{Id: "cr_a", Text: ut("Invoice paid"), Ticked: true, Evidence: ut("receipt")},
					{Id: "cr_b", Position: 1, Text: ut("DNS\x1b[2J checked\nnow")},
				}, Revision: 48}, nil
			},
			tick: func(r *chainmailv1.TickCriterionRequest) (*chainmailv1.TickCriterionResponse, error) {
				return &chainmailv1.TickCriterionResponse{Criterion: &chainmailv1.Criterion{Id: r.GetCriterionId(), Ticked: r.GetTicked()}, Revision: 49}, nil
			},
		}
	}
	testItemTable(t, newFake, []itemCase{
		{
			name: "criteria table", args: []string{"criteria", "add", "it_task1", "DNS checked"},
			out: "criteria: 1 of 2 ticked\n  [x] cr_a Invoice paid\n      evidence: receipt\n  [ ] cr_b DNS[2J checked now\nrevision 48\n",
		},
		{name: "tick table", args: []string{"criteria", "tick", "it_task1", "cr_b", "--evidence", "dig output"}, out: "ticked cr_b on it_task1, revision 49\n"},
		{name: "untick table", args: []string{"criteria", "untick", "it_task1", "cr_a"}, out: "unticked cr_a on it_task1, revision 49\n"},
	})
	testItemCompletion(t, []itemCase{
		{name: "add item", args: []string{"item", "criteria", "add", ""}, cands: []string{"it_admin_work", "it_task1"}},
		{name: "add text", args: []string{"item", "criteria", "add", "it_task1", ""}},
		{name: "tick criterion", args: []string{"item", "criteria", "tick", "it_task1", ""}, cands: []string{"cr_a", "cr_b"}},
		{name: "remove leaves out typed", args: []string{"item", "criteria", "remove", "it_task1", "cr_a", ""}, cands: []string{"cr_b"}},
		{name: "order criterion", args: []string{"item", "criteria", "order", "it_task1", "cr_"}, cands: []string{"cr_a", "cr_b"}},
		{name: "edit text", args: []string{"item", "criteria", "edit", "it_task1", "cr_a", ""}},
	})
}

// A criterion's description is its tick box and untrusted text.
func TestCompleteCriterionDescription(t *testing.T) {
	t.Parallel()
	env := sockEnv(startFakeItems(t, completionItems()))
	cands, _ := completeArgs(t, env, "item", "criteria", "untick", "it_task1", "cr_a")
	if want := []string{"cr_a\t[x] Invoice paid"}; !slices.Equal(cands, want) {
		t.Errorf("candidates %q, want %q", cands, want)
	}
}

func TestItemBlockAndLinks(t *testing.T) {
	t.Parallel()
	testItemRequests(t, []itemCase{
		{name: "block", args: []string{"block", "it_task1", "it_task0"}, want: &chainmailv1.SetBlockerRequest{ItemId: "it_task1", BlockedById: "it_task0", Linked: true}},
		{name: "unblock", args: []string{"block", "it_task1", "it_task0", "--remove"}, want: &chainmailv1.SetBlockerRequest{ItemId: "it_task1", BlockedById: "it_task0"}},
		{name: "link", args: []string{"link", "it_task1", "cm1:work:INBOX:7:42"}, want: &chainmailv1.LinkThreadRequest{ItemId: "it_task1", Ref: "cm1:work:INBOX:7:42"}},
		{
			name: "link anchor and note",
			args: []string{"link", "it_task1", "cm1:work:INBOX:7:42", "--anchor", "--note", "the renewal notice"},
			want: &chainmailv1.LinkThreadRequest{ItemId: "it_task1", Ref: "cm1:work:INBOX:7:42", Anchor: true, Note: "the renewal notice"},
		},
		{name: "unlink", args: []string{"unlink", "it_task1", "work", "a1b2c3d4"}, want: &chainmailv1.UnlinkThreadRequest{ItemId: "it_task1", Account: "work", ThreadKey: "a1b2c3d4"}},
	})
	testItemUsage(t, []itemCase{
		{name: "block one", args: []string{"block", "it_task1"}, usage: "accepts 2 arg(s)"},
		{name: "link a bad ref", args: []string{"link", "it_task1", "nope"}, usage: "refs come from search results"},
		{name: "link without a ref", args: []string{"link", "it_task1"}, usage: "accepts 2 arg(s)"},
		{name: "unlink without a key", args: []string{"unlink", "it_task1", "work"}, usage: "accepts 3 arg(s)"},
	})
	newFake := func() *fakeItems {
		return &fakeItems{
			block: func(r *chainmailv1.SetBlockerRequest) (*chainmailv1.SetBlockerResponse, error) {
				if r.GetBlockedById() == "it_same" {
					return &chainmailv1.SetBlockerResponse{Revision: 50}, nil
				}
				return &chainmailv1.SetBlockerResponse{Changed: true, Revision: 51}, nil
			},
			link: func(r *chainmailv1.LinkThreadRequest) (*chainmailv1.LinkThreadResponse, error) {
				if r.GetAnchor() {
					link := &chainmailv1.ThreadLink{Account: "work", ThreadKey: "a1b2c3d4", State: "unavailable", Reason: "SERVER_UNAVAILABLE"}
					return &chainmailv1.LinkThreadResponse{Link: link, Revision: 52}, nil
				}
				link := &chainmailv1.ThreadLink{Account: "work", ThreadKey: "a1b2c3d4", State: "ok", Messages: 3}
				return &chainmailv1.LinkThreadResponse{Link: link, Created: true, Revision: 53}, nil
			},
			unlink: func(*chainmailv1.UnlinkThreadRequest) (*chainmailv1.UnlinkThreadResponse, error) {
				return &chainmailv1.UnlinkThreadResponse{Revision: 54}, nil
			},
		}
	}
	testItemTable(t, newFake, []itemCase{
		{name: "block table", args: []string{"block", "it_task1", "it_task0"}, out: "it_task1 is now blocked by it_task0, revision 51\n"},
		{name: "block again", args: []string{"block", "it_task1", "it_same"}, out: "it_task1 was already blocked by it_same\n"},
		{name: "unblock table", args: []string{"block", "it_task1", "it_task0", "--remove"}, out: "it_task1 is no longer blocked by it_task0, revision 51\n"},
		{name: "unblock again", args: []string{"block", "it_task1", "it_same", "--remove"}, out: "it_task1 was not blocked by it_same\n"},
		{name: "link table", args: []string{"link", "it_task1", "cm1:work:INBOX:7:42"}, out: "linked work thread a1b2c3d4 to it_task1: ok, 3 messages, revision 53\n"},
		{
			name: "relink unavailable", args: []string{"link", "it_task1", "cm1:work:INBOX:7:42", "--anchor"},
			out: "updated link work thread a1b2c3d4 to it_task1: unavailable (SERVER_UNAVAILABLE), 0 messages, revision 52\n",
		},
		{name: "unlink table", args: []string{"unlink", "it_task1", "work", "a1b2c3d4"}, out: "unlinked work thread a1b2c3d4 from it_task1, revision 54\n"},
	})
	testItemCompletion(t, []itemCase{
		{name: "block blocker leaves out the item", args: []string{"item", "block", "it_task1", ""}, cands: []string{"it_admin_work"}},
		{name: "link item", args: []string{"item", "link", ""}, cands: []string{"it_admin_work", "it_task1"}},
		{name: "unlink accounts", args: []string{"item", "unlink", "it_task1", ""}, cands: []string{"old", "work"}},
		{name: "unlink threads", args: []string{"item", "unlink", "it_task1", "work", ""}, cands: []string{"a1b2c3d4", "b2c3d4e5"}},
		{name: "unlink nothing after the key", args: []string{"item", "unlink", "it_task1", "work", "a1b2c3d4", ""}},
	})
}

func TestCompleteWatchItem(t *testing.T) {
	t.Parallel()
	testItemCompletion(t, []itemCase{
		{name: "item", args: []string{"watch", "--item", ""}, cands: []string{"it_admin_work", "it_task1"}},
		{name: "kinds", args: []string{"watch", "--kind", "new,item"}, cands: []string{"new,item", "new,item_mail"}},
	})
}
