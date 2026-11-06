package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

// stubItems answers the ItemsService calls the client tests make from
// their requests, so a test can see each field arrive. It sends the client
// header of every call on headers, and fails a call once headers is full.
// idBytes, when set, makes the listed row's ID that long.
type stubItems struct {
	chainmailv1connect.UnimplementedItemsServiceHandler
	headers chan<- string
	idBytes int
}

// record sends req's client header on headers. A call past the buffer
// the test made for its calls fails at once instead of blocking on the
// send, so an unexpected extra call fails the test rather than hanging it.
func (s stubItems) record(req connect.AnyRequest) error {
	select {
	case s.headers <- req.Header().Get(api.HeaderClient):
		return nil
	default:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("stubItems: a call past the headers buffer"))
	}
}

func (s stubItems) CreateItem(_ context.Context, req *connect.Request[chainmailv1.CreateItemRequest]) (*connect.Response[chainmailv1.CreateItemResponse], error) {
	if err := s.record(req); err != nil {
		return nil, err
	}
	m := req.Msg
	crit := make([]*chainmailv1.Criterion, len(m.GetCriteria()))
	for i, text := range m.GetCriteria() {
		crit[i] = &chainmailv1.Criterion{Id: fmt.Sprintf("cr_%d", i), Position: int32(i), Text: &chainmailv1.Untrusted{Untrusted: text}}
	}
	return connect.NewResponse(&chainmailv1.CreateItemResponse{
		Item: &chainmailv1.WorkItem{
			Id: "it_new", Type: m.GetType(), Title: &chainmailv1.Untrusted{Untrusted: m.GetTitle()},
			State: "backlog", Area: m.GetArea(), EffectiveArea: m.GetArea(), EffectivePriority: "p2",
			Estimate: m.Estimate, ParentId: m.GetParentId(), Revision: 1, CreatedBy: "1000/test", UpdatedBy: "1000/test",
		},
		Criteria:  crit,
		Duplicate: m.GetIdempotencyKey() == "seen",
	}), nil
}

func (s stubItems) UpdateItem(_ context.Context, req *connect.Request[chainmailv1.UpdateItemRequest]) (*connect.Response[chainmailv1.UpdateItemResponse], error) {
	if err := s.record(req); err != nil {
		return nil, err
	}
	m := req.Msg
	// The title echoes the mask, so the test sees the paths arrive in order.
	return connect.NewResponse(&chainmailv1.UpdateItemResponse{Item: &chainmailv1.WorkItem{
		Id: m.GetId(), Title: &chainmailv1.Untrusted{Untrusted: strings.Join(m.GetUpdateMask().GetPaths(), ",")},
		Estimate: m.Estimate, Revision: 2,
	}}), nil
}

func (s stubItems) ListItems(_ context.Context, req *connect.Request[chainmailv1.ListItemsRequest]) (*connect.Response[chainmailv1.ListItemsResponse], error) {
	if err := s.record(req); err != nil {
		return nil, err
	}
	id := "it_" + req.Msg.GetUnder()
	if s.idBytes > 0 {
		id = strings.Repeat("x", s.idBytes)
	}
	return connect.NewResponse(&chainmailv1.ListItemsResponse{
		Items:         []*chainmailv1.WorkItemRow{{Id: id, Type: "task", Depth: 1, Ongoing: req.Msg.GetOngoing(), Revision: req.Msg.GetChangedSince() + 1}},
		NextPageToken: "dDox",
		Revision:      7,
	}), nil
}

func (s stubItems) LinkThread(_ context.Context, req *connect.Request[chainmailv1.LinkThreadRequest]) (*connect.Response[chainmailv1.LinkThreadResponse], error) {
	if err := s.record(req); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.LinkThreadResponse{
		Link: &chainmailv1.ThreadLink{
			Account: "work", ThreadKey: "0123456789abcdef", Note: &chainmailv1.Untrusted{Untrusted: req.Msg.GetNote()},
			State: "ok", Source: "cache", Messages: 2, Latest: &chainmailv1.MessageRow{Ref: req.Msg.GetRef()},
		},
		Created:  req.Msg.GetAnchor(),
		Revision: 3,
	}), nil
}

// serveItems serves h as the ItemsService on a new Unix socket and returns
// the socket path.
func serveItems(t *testing.T, h chainmailv1connect.ItemsServiceHandler) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewItemsServiceHandler(h))
	return serveUnix(t, &http.Server{Handler: mux})
}

// TestItemsReachesTheDaemon checks that the Items client calls
// ItemsService methods over the socket, names the client kind, and carries
// the field mask, optional fields and nested messages both ways.
func TestItemsReachesTheDaemon(t *testing.T) {
	t.Parallel()
	headers := make(chan string, 4)
	c := New(serveItems(t, stubItems{headers: headers}), "test")
	defer c.Close()

	estimate := 2.5
	created, err := c.Items.CreateItem(t.Context(), connect.NewRequest(&chainmailv1.CreateItemRequest{
		Type: "task", Title: "file the receipt", Area: "work", Estimate: &estimate,
		Criteria: []string{"receipt saved", "expense filed"}, IdempotencyKey: "seen",
	}))
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	it := created.Msg.GetItem()
	if it.GetId() != "it_new" || it.GetType() != "task" || it.GetTitle().GetUntrusted() != "file the receipt" || it.GetEffectivePriority() != "p2" {
		t.Errorf("created item = %v, want it_new, a task titled as sent, p2", it)
	}
	if it.Estimate == nil || it.GetEstimate() != 2.5 {
		t.Errorf("estimate = %v, want 2.5", it.Estimate)
	}
	if crit := created.Msg.GetCriteria(); len(crit) != 2 || crit[1].GetText().GetUntrusted() != "expense filed" || crit[1].GetPosition() != 1 {
		t.Errorf("criteria = %v, want two, in order", crit)
	}
	if !created.Msg.GetDuplicate() {
		t.Error("duplicate = false, want true for the key the stub has seen")
	}

	updated, err := c.Items.UpdateItem(t.Context(), connect.NewRequest(&chainmailv1.UpdateItemRequest{
		Id: "it_new", UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title", "estimate", "parent_id"}}, Title: "x",
	}))
	if err != nil {
		t.Fatalf("UpdateItem: %v", err)
	}
	if got := updated.Msg.GetItem().GetTitle().GetUntrusted(); got != "title,estimate,parent_id" {
		t.Errorf("mask paths at the daemon = %q, want title,estimate,parent_id", got)
	}
	if updated.Msg.GetItem().Estimate != nil {
		t.Errorf("estimate = %v, want unset, as sent", updated.Msg.GetItem().Estimate)
	}

	ongoing, since := true, int64(41)
	listed, err := c.Items.ListItems(t.Context(), connect.NewRequest(&chainmailv1.ListItemsRequest{Under: "admin_work", Ongoing: &ongoing, ChangedSince: &since, Limit: 5}))
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	rows := listed.Msg.GetItems()
	if len(rows) != 1 || rows[0].GetId() != "it_admin_work" || !rows[0].GetOngoing() || rows[0].GetRevision() != 42 {
		t.Errorf("rows = %v, want it_admin_work, ongoing, revision 42", rows)
	}
	if listed.Msg.GetNextPageToken() != "dDox" || listed.Msg.GetRevision() != 7 {
		t.Errorf("page = %q at %d, want dDox at 7", listed.Msg.GetNextPageToken(), listed.Msg.GetRevision())
	}

	linked, err := c.Items.LinkThread(t.Context(), connect.NewRequest(&chainmailv1.LinkThreadRequest{ItemId: "it_new", Ref: "cm1:work:INBOX:1:2", Anchor: true, Note: "the receipt"}))
	if err != nil {
		t.Fatalf("LinkThread: %v", err)
	}
	link := linked.Msg.GetLink()
	if link.GetLatest().GetRef() != "cm1:work:INBOX:1:2" || link.GetNote().GetUntrusted() != "the receipt" || link.GetState() != "ok" || !linked.Msg.GetCreated() {
		t.Errorf("link = %v, created %t; want the ref and note back, state ok, created", link, linked.Msg.GetCreated())
	}

	for range 4 {
		if got := <-headers; got != "test" {
			t.Errorf("%s = %q, want test", api.HeaderClient, got)
		}
	}

	// The stub leaves DeleteItem to the generated handler, which proves the
	// call reached ItemsService and not some other route.
	if _, err := c.Items.DeleteItem(t.Context(), connect.NewRequest(&chainmailv1.DeleteItemRequest{Id: "it_new"})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("DeleteItem: code %v (%v), want %v", connect.CodeOf(err), err, connect.CodeUnimplemented)
	}
}

// TestItemsReadLimit checks that the Items client reads a response as large
// as the daemon's 16 MiB cap and refuses one past maxMessageBytes.
func TestItemsReadLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		id      int
		refused bool
	}{
		{name: "16 MiB is read", id: 16 << 20},
		{name: "18 MiB is refused", id: 18 << 20, refused: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := make(chan string, 1)
			c := New(serveItems(t, stubItems{headers: headers, idBytes: tt.id}), "test")
			defer c.Close()
			_, err := c.Items.ListItems(t.Context(), connect.NewRequest(&chainmailv1.ListItemsRequest{}))
			switch {
			case !tt.refused && err != nil:
				t.Errorf("ListItems: %v, want success", err)
			case tt.refused && connect.CodeOf(err) != connect.CodeResourceExhausted:
				t.Errorf("ListItems: code %v (%v), want %v", connect.CodeOf(err), err, connect.CodeResourceExhausted)
			}
		})
	}
}

// TestStubItemsRefusesAnExtraCall checks that a call past the stub's
// headers buffer fails at once instead of blocking the test on the send.
func TestStubItemsRefusesAnExtraCall(t *testing.T) {
	t.Parallel()
	headers := make(chan string, 1)
	headers <- "earlier" // the buffer is full: the next call is one too many
	c := New(serveItems(t, stubItems{headers: headers}), "test")
	defer c.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err := c.Items.ListItems(ctx, connect.NewRequest(&chainmailv1.ListItemsRequest{}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("ListItems past the buffer: code %v (%v), want %v", connect.CodeOf(err), err, connect.CodeFailedPrecondition)
	}
}
