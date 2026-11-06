package mcpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

// unaryFn answers one unary RPC.
type unaryFn[Req, Resp any] func(context.Context, *Req) (*Resp, error)

// fakeCall is one RPC the fake daemon received.
type fakeCall struct {
	Procedure string
	Client    string        // the Chainmail-Client header
	Msg       proto.Message // the request; nil for UploadAttachment
}

// fakeDaemon serves the seven agent-facing services on a Unix socket. Each
// RPC answers with the func a test set, or CodeUnimplemented when it is nil.
// Set the funcs before startFake. To change an answer mid-test, close over
// state the test guards with a mutex or an atomic.
type fakeDaemon struct {
	Socket string

	ListAccounts unaryFn[chainmailv1.ListAccountsRequest, chainmailv1.ListAccountsResponse]
	TestAccount  unaryFn[chainmailv1.TestAccountRequest, chainmailv1.TestAccountResponse]
	ProbeLimits  unaryFn[chainmailv1.ProbeLimitsRequest, chainmailv1.ProbeLimitsResponse]

	ListFolders        unaryFn[chainmailv1.ListFoldersRequest, chainmailv1.ListFoldersResponse]
	Search             unaryFn[chainmailv1.SearchRequest, chainmailv1.SearchResponse]
	Get                unaryFn[chainmailv1.GetRequest, chainmailv1.GetResponse]
	GetThread          unaryFn[chainmailv1.GetThreadRequest, chainmailv1.GetThreadResponse]
	GetRaw             func(context.Context, *chainmailv1.GetRawRequest, *connect.ServerStream[chainmailv1.GetRawResponse]) error
	DownloadAttachment func(context.Context, *chainmailv1.DownloadAttachmentRequest, *connect.ServerStream[chainmailv1.DownloadAttachmentResponse]) error

	Send             unaryFn[chainmailv1.SendRequest, chainmailv1.SendResponse]
	SendRaw          unaryFn[chainmailv1.SendRawRequest, chainmailv1.SendRawResponse]
	Reply            unaryFn[chainmailv1.ReplyRequest, chainmailv1.ReplyResponse]
	Forward          unaryFn[chainmailv1.ForwardRequest, chainmailv1.ForwardResponse]
	CreateDraft      unaryFn[chainmailv1.CreateDraftRequest, chainmailv1.CreateDraftResponse]
	UpdateDraft      unaryFn[chainmailv1.UpdateDraftRequest, chainmailv1.UpdateDraftResponse]
	SendDraft        unaryFn[chainmailv1.SendDraftRequest, chainmailv1.SendDraftResponse]
	DeleteDraft      unaryFn[chainmailv1.DeleteDraftRequest, chainmailv1.DeleteDraftResponse]
	UploadAttachment func(context.Context, *connect.ClientStream[chainmailv1.UploadAttachmentRequest]) (*chainmailv1.UploadAttachmentResponse, error)

	Move         unaryFn[chainmailv1.MoveRequest, chainmailv1.MoveResponse]
	Copy         unaryFn[chainmailv1.CopyRequest, chainmailv1.CopyResponse]
	Archive      unaryFn[chainmailv1.ArchiveRequest, chainmailv1.ArchiveResponse]
	Trash        unaryFn[chainmailv1.TrashRequest, chainmailv1.TrashResponse]
	Expunge      unaryFn[chainmailv1.ExpungeRequest, chainmailv1.ExpungeResponse]
	SetFlags     unaryFn[chainmailv1.SetFlagsRequest, chainmailv1.SetFlagsResponse]
	SetLabels    unaryFn[chainmailv1.SetLabelsRequest, chainmailv1.SetLabelsResponse]
	CreateFolder unaryFn[chainmailv1.CreateFolderRequest, chainmailv1.CreateFolderResponse]
	RenameFolder unaryFn[chainmailv1.RenameFolderRequest, chainmailv1.RenameFolderResponse]
	DeleteFolder unaryFn[chainmailv1.DeleteFolderRequest, chainmailv1.DeleteFolderResponse]

	Subscribe      func(context.Context, *chainmailv1.SubscribeRequest, *connect.ServerStream[chainmailv1.SubscribeResponse]) error
	ReadEvents     unaryFn[chainmailv1.ReadEventsRequest, chainmailv1.ReadEventsResponse]
	Ack            unaryFn[chainmailv1.AckRequest, chainmailv1.AckResponse]
	GetWatchStatus unaryFn[chainmailv1.GetWatchStatusRequest, chainmailv1.GetWatchStatusResponse]
	ListGroups     unaryFn[chainmailv1.ListGroupsRequest, chainmailv1.ListGroupsResponse]
	DeleteGroup    unaryFn[chainmailv1.DeleteGroupRequest, chainmailv1.DeleteGroupResponse]
	GetCacheStatus unaryFn[chainmailv1.GetCacheStatusRequest, chainmailv1.GetCacheStatusResponse]
	RebuildCache   unaryFn[chainmailv1.RebuildCacheRequest, chainmailv1.RebuildCacheResponse]
	EvictCache     unaryFn[chainmailv1.EvictCacheRequest, chainmailv1.EvictCacheResponse]

	CreateItem    unaryFn[chainmailv1.CreateItemRequest, chainmailv1.CreateItemResponse]
	UpdateItem    unaryFn[chainmailv1.UpdateItemRequest, chainmailv1.UpdateItemResponse]
	DeleteItem    unaryFn[chainmailv1.DeleteItemRequest, chainmailv1.DeleteItemResponse]
	RestoreItem   unaryFn[chainmailv1.RestoreItemRequest, chainmailv1.RestoreItemResponse]
	GetItem       unaryFn[chainmailv1.GetItemRequest, chainmailv1.GetItemResponse]
	ListItems     unaryFn[chainmailv1.ListItemsRequest, chainmailv1.ListItemsResponse]
	EditCriteria  unaryFn[chainmailv1.EditCriteriaRequest, chainmailv1.EditCriteriaResponse]
	TickCriterion unaryFn[chainmailv1.TickCriterionRequest, chainmailv1.TickCriterionResponse]
	SetBlocker    unaryFn[chainmailv1.SetBlockerRequest, chainmailv1.SetBlockerResponse]
	LinkThread    unaryFn[chainmailv1.LinkThreadRequest, chainmailv1.LinkThreadResponse]
	UnlinkThread  unaryFn[chainmailv1.UnlinkThreadRequest, chainmailv1.UnlinkThreadResponse]

	mu    sync.Mutex
	calls []fakeCall
}

func (f *fakeDaemon) record(proc, client string, msg proto.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{Procedure: proc, Client: client, Msg: msg})
}

// Calls returns every call so far, in order.
func (f *fakeDaemon) Calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

// lastCall returns the latest call to proc, or fails the test.
func (f *fakeDaemon) lastCall(t *testing.T, proc string) fakeCall {
	t.Helper()
	calls := f.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Procedure == proc {
			return calls[i]
		}
	}
	t.Fatalf("no call to %s among %d calls", proc, len(calls))
	return fakeCall{}
}

func notFaked(proc string) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New(proc+" is not faked"))
}

func fakeUnary[Req, Resp any](ctx context.Context, f *fakeDaemon, proc string, fn unaryFn[Req, Resp], req *connect.Request[Req]) (*connect.Response[Resp], error) {
	msg, _ := any(req.Msg).(proto.Message)
	f.record(proc, req.Header().Get(api.HeaderClient), msg)
	if fn == nil {
		return nil, notFaked(proc)
	}
	resp, err := fn(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

type (
	fakeAccountSvc  struct{ f *fakeDaemon }
	fakeMessageSvc  struct{ f *fakeDaemon }
	fakeComposeSvc  struct{ f *fakeDaemon }
	fakeOrganizeSvc struct{ f *fakeDaemon }
	fakeWatchSvc    struct{ f *fakeDaemon }
	fakeCacheSvc    struct{ f *fakeDaemon }
	fakeItemsSvc    struct{ f *fakeDaemon }
)

func (a fakeAccountSvc) ListAccounts(ctx context.Context, req *connect.Request[chainmailv1.ListAccountsRequest]) (*connect.Response[chainmailv1.ListAccountsResponse], error) {
	return fakeUnary(ctx, a.f, chainmailv1connect.AccountServiceListAccountsProcedure, a.f.ListAccounts, req)
}

func (a fakeAccountSvc) TestAccount(ctx context.Context, req *connect.Request[chainmailv1.TestAccountRequest]) (*connect.Response[chainmailv1.TestAccountResponse], error) {
	return fakeUnary(ctx, a.f, chainmailv1connect.AccountServiceTestAccountProcedure, a.f.TestAccount, req)
}

func (a fakeAccountSvc) ProbeLimits(ctx context.Context, req *connect.Request[chainmailv1.ProbeLimitsRequest]) (*connect.Response[chainmailv1.ProbeLimitsResponse], error) {
	return fakeUnary(ctx, a.f, chainmailv1connect.AccountServiceProbeLimitsProcedure, a.f.ProbeLimits, req)
}

func (m fakeMessageSvc) ListFolders(ctx context.Context, req *connect.Request[chainmailv1.ListFoldersRequest]) (*connect.Response[chainmailv1.ListFoldersResponse], error) {
	return fakeUnary(ctx, m.f, chainmailv1connect.MessageServiceListFoldersProcedure, m.f.ListFolders, req)
}

func (m fakeMessageSvc) Search(ctx context.Context, req *connect.Request[chainmailv1.SearchRequest]) (*connect.Response[chainmailv1.SearchResponse], error) {
	return fakeUnary(ctx, m.f, chainmailv1connect.MessageServiceSearchProcedure, m.f.Search, req)
}

func (m fakeMessageSvc) Get(ctx context.Context, req *connect.Request[chainmailv1.GetRequest]) (*connect.Response[chainmailv1.GetResponse], error) {
	return fakeUnary(ctx, m.f, chainmailv1connect.MessageServiceGetProcedure, m.f.Get, req)
}

func (m fakeMessageSvc) GetThread(ctx context.Context, req *connect.Request[chainmailv1.GetThreadRequest]) (*connect.Response[chainmailv1.GetThreadResponse], error) {
	return fakeUnary(ctx, m.f, chainmailv1connect.MessageServiceGetThreadProcedure, m.f.GetThread, req)
}

func (m fakeMessageSvc) GetRaw(ctx context.Context, req *connect.Request[chainmailv1.GetRawRequest], stream *connect.ServerStream[chainmailv1.GetRawResponse]) error {
	const proc = chainmailv1connect.MessageServiceGetRawProcedure
	m.f.record(proc, req.Header().Get(api.HeaderClient), req.Msg)
	if m.f.GetRaw == nil {
		return notFaked(proc)
	}
	return m.f.GetRaw(ctx, req.Msg, stream)
}

func (m fakeMessageSvc) DownloadAttachment(ctx context.Context, req *connect.Request[chainmailv1.DownloadAttachmentRequest], stream *connect.ServerStream[chainmailv1.DownloadAttachmentResponse]) error {
	const proc = chainmailv1connect.MessageServiceDownloadAttachmentProcedure
	m.f.record(proc, req.Header().Get(api.HeaderClient), req.Msg)
	if m.f.DownloadAttachment == nil {
		return notFaked(proc)
	}
	return m.f.DownloadAttachment(ctx, req.Msg, stream)
}

func (c fakeComposeSvc) Send(ctx context.Context, req *connect.Request[chainmailv1.SendRequest]) (*connect.Response[chainmailv1.SendResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.ComposeServiceSendProcedure, c.f.Send, req)
}

func (c fakeComposeSvc) SendRaw(ctx context.Context, req *connect.Request[chainmailv1.SendRawRequest]) (*connect.Response[chainmailv1.SendRawResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.ComposeServiceSendRawProcedure, c.f.SendRaw, req)
}

func (c fakeComposeSvc) Reply(ctx context.Context, req *connect.Request[chainmailv1.ReplyRequest]) (*connect.Response[chainmailv1.ReplyResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.ComposeServiceReplyProcedure, c.f.Reply, req)
}

func (c fakeComposeSvc) Forward(ctx context.Context, req *connect.Request[chainmailv1.ForwardRequest]) (*connect.Response[chainmailv1.ForwardResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.ComposeServiceForwardProcedure, c.f.Forward, req)
}

func (c fakeComposeSvc) CreateDraft(ctx context.Context, req *connect.Request[chainmailv1.CreateDraftRequest]) (*connect.Response[chainmailv1.CreateDraftResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.ComposeServiceCreateDraftProcedure, c.f.CreateDraft, req)
}

func (c fakeComposeSvc) UpdateDraft(ctx context.Context, req *connect.Request[chainmailv1.UpdateDraftRequest]) (*connect.Response[chainmailv1.UpdateDraftResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.ComposeServiceUpdateDraftProcedure, c.f.UpdateDraft, req)
}

func (c fakeComposeSvc) SendDraft(ctx context.Context, req *connect.Request[chainmailv1.SendDraftRequest]) (*connect.Response[chainmailv1.SendDraftResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.ComposeServiceSendDraftProcedure, c.f.SendDraft, req)
}

func (c fakeComposeSvc) DeleteDraft(ctx context.Context, req *connect.Request[chainmailv1.DeleteDraftRequest]) (*connect.Response[chainmailv1.DeleteDraftResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.ComposeServiceDeleteDraftProcedure, c.f.DeleteDraft, req)
}

func (c fakeComposeSvc) UploadAttachment(ctx context.Context, stream *connect.ClientStream[chainmailv1.UploadAttachmentRequest]) (*connect.Response[chainmailv1.UploadAttachmentResponse], error) {
	const proc = chainmailv1connect.ComposeServiceUploadAttachmentProcedure
	c.f.record(proc, stream.RequestHeader().Get(api.HeaderClient), nil)
	if c.f.UploadAttachment == nil {
		return nil, notFaked(proc)
	}
	resp, err := c.f.UploadAttachment(ctx, stream)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (o fakeOrganizeSvc) Move(ctx context.Context, req *connect.Request[chainmailv1.MoveRequest]) (*connect.Response[chainmailv1.MoveResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceMoveProcedure, o.f.Move, req)
}

func (o fakeOrganizeSvc) Copy(ctx context.Context, req *connect.Request[chainmailv1.CopyRequest]) (*connect.Response[chainmailv1.CopyResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceCopyProcedure, o.f.Copy, req)
}

func (o fakeOrganizeSvc) Archive(ctx context.Context, req *connect.Request[chainmailv1.ArchiveRequest]) (*connect.Response[chainmailv1.ArchiveResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceArchiveProcedure, o.f.Archive, req)
}

func (o fakeOrganizeSvc) Trash(ctx context.Context, req *connect.Request[chainmailv1.TrashRequest]) (*connect.Response[chainmailv1.TrashResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceTrashProcedure, o.f.Trash, req)
}

func (o fakeOrganizeSvc) Expunge(ctx context.Context, req *connect.Request[chainmailv1.ExpungeRequest]) (*connect.Response[chainmailv1.ExpungeResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceExpungeProcedure, o.f.Expunge, req)
}

func (o fakeOrganizeSvc) SetFlags(ctx context.Context, req *connect.Request[chainmailv1.SetFlagsRequest]) (*connect.Response[chainmailv1.SetFlagsResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceSetFlagsProcedure, o.f.SetFlags, req)
}

func (o fakeOrganizeSvc) SetLabels(ctx context.Context, req *connect.Request[chainmailv1.SetLabelsRequest]) (*connect.Response[chainmailv1.SetLabelsResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceSetLabelsProcedure, o.f.SetLabels, req)
}

func (o fakeOrganizeSvc) CreateFolder(ctx context.Context, req *connect.Request[chainmailv1.CreateFolderRequest]) (*connect.Response[chainmailv1.CreateFolderResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceCreateFolderProcedure, o.f.CreateFolder, req)
}

func (o fakeOrganizeSvc) RenameFolder(ctx context.Context, req *connect.Request[chainmailv1.RenameFolderRequest]) (*connect.Response[chainmailv1.RenameFolderResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceRenameFolderProcedure, o.f.RenameFolder, req)
}

func (o fakeOrganizeSvc) DeleteFolder(ctx context.Context, req *connect.Request[chainmailv1.DeleteFolderRequest]) (*connect.Response[chainmailv1.DeleteFolderResponse], error) {
	return fakeUnary(ctx, o.f, chainmailv1connect.OrganizeServiceDeleteFolderProcedure, o.f.DeleteFolder, req)
}

func (w fakeWatchSvc) Subscribe(ctx context.Context, req *connect.Request[chainmailv1.SubscribeRequest], stream *connect.ServerStream[chainmailv1.SubscribeResponse]) error {
	const proc = chainmailv1connect.WatchServiceSubscribeProcedure
	w.f.record(proc, req.Header().Get(api.HeaderClient), req.Msg)
	if w.f.Subscribe == nil {
		return notFaked(proc)
	}
	return w.f.Subscribe(ctx, req.Msg, stream)
}

func (w fakeWatchSvc) ReadEvents(ctx context.Context, req *connect.Request[chainmailv1.ReadEventsRequest]) (*connect.Response[chainmailv1.ReadEventsResponse], error) {
	return fakeUnary(ctx, w.f, chainmailv1connect.WatchServiceReadEventsProcedure, w.f.ReadEvents, req)
}

func (w fakeWatchSvc) Ack(ctx context.Context, req *connect.Request[chainmailv1.AckRequest]) (*connect.Response[chainmailv1.AckResponse], error) {
	return fakeUnary(ctx, w.f, chainmailv1connect.WatchServiceAckProcedure, w.f.Ack, req)
}

func (w fakeWatchSvc) GetWatchStatus(ctx context.Context, req *connect.Request[chainmailv1.GetWatchStatusRequest]) (*connect.Response[chainmailv1.GetWatchStatusResponse], error) {
	return fakeUnary(ctx, w.f, chainmailv1connect.WatchServiceGetWatchStatusProcedure, w.f.GetWatchStatus, req)
}

func (w fakeWatchSvc) ListGroups(ctx context.Context, req *connect.Request[chainmailv1.ListGroupsRequest]) (*connect.Response[chainmailv1.ListGroupsResponse], error) {
	return fakeUnary(ctx, w.f, chainmailv1connect.WatchServiceListGroupsProcedure, w.f.ListGroups, req)
}

func (w fakeWatchSvc) DeleteGroup(ctx context.Context, req *connect.Request[chainmailv1.DeleteGroupRequest]) (*connect.Response[chainmailv1.DeleteGroupResponse], error) {
	return fakeUnary(ctx, w.f, chainmailv1connect.WatchServiceDeleteGroupProcedure, w.f.DeleteGroup, req)
}

func (c fakeCacheSvc) GetCacheStatus(ctx context.Context, req *connect.Request[chainmailv1.GetCacheStatusRequest]) (*connect.Response[chainmailv1.GetCacheStatusResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.CacheServiceGetCacheStatusProcedure, c.f.GetCacheStatus, req)
}

func (c fakeCacheSvc) RebuildCache(ctx context.Context, req *connect.Request[chainmailv1.RebuildCacheRequest]) (*connect.Response[chainmailv1.RebuildCacheResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.CacheServiceRebuildCacheProcedure, c.f.RebuildCache, req)
}

func (c fakeCacheSvc) EvictCache(ctx context.Context, req *connect.Request[chainmailv1.EvictCacheRequest]) (*connect.Response[chainmailv1.EvictCacheResponse], error) {
	return fakeUnary(ctx, c.f, chainmailv1connect.CacheServiceEvictCacheProcedure, c.f.EvictCache, req)
}

func (i fakeItemsSvc) CreateItem(ctx context.Context, req *connect.Request[chainmailv1.CreateItemRequest]) (*connect.Response[chainmailv1.CreateItemResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceCreateItemProcedure, i.f.CreateItem, req)
}

func (i fakeItemsSvc) UpdateItem(ctx context.Context, req *connect.Request[chainmailv1.UpdateItemRequest]) (*connect.Response[chainmailv1.UpdateItemResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceUpdateItemProcedure, i.f.UpdateItem, req)
}

func (i fakeItemsSvc) DeleteItem(ctx context.Context, req *connect.Request[chainmailv1.DeleteItemRequest]) (*connect.Response[chainmailv1.DeleteItemResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceDeleteItemProcedure, i.f.DeleteItem, req)
}

func (i fakeItemsSvc) RestoreItem(ctx context.Context, req *connect.Request[chainmailv1.RestoreItemRequest]) (*connect.Response[chainmailv1.RestoreItemResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceRestoreItemProcedure, i.f.RestoreItem, req)
}

func (i fakeItemsSvc) GetItem(ctx context.Context, req *connect.Request[chainmailv1.GetItemRequest]) (*connect.Response[chainmailv1.GetItemResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceGetItemProcedure, i.f.GetItem, req)
}

func (i fakeItemsSvc) ListItems(ctx context.Context, req *connect.Request[chainmailv1.ListItemsRequest]) (*connect.Response[chainmailv1.ListItemsResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceListItemsProcedure, i.f.ListItems, req)
}

func (i fakeItemsSvc) EditCriteria(ctx context.Context, req *connect.Request[chainmailv1.EditCriteriaRequest]) (*connect.Response[chainmailv1.EditCriteriaResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceEditCriteriaProcedure, i.f.EditCriteria, req)
}

func (i fakeItemsSvc) TickCriterion(ctx context.Context, req *connect.Request[chainmailv1.TickCriterionRequest]) (*connect.Response[chainmailv1.TickCriterionResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceTickCriterionProcedure, i.f.TickCriterion, req)
}

func (i fakeItemsSvc) SetBlocker(ctx context.Context, req *connect.Request[chainmailv1.SetBlockerRequest]) (*connect.Response[chainmailv1.SetBlockerResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceSetBlockerProcedure, i.f.SetBlocker, req)
}

func (i fakeItemsSvc) LinkThread(ctx context.Context, req *connect.Request[chainmailv1.LinkThreadRequest]) (*connect.Response[chainmailv1.LinkThreadResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceLinkThreadProcedure, i.f.LinkThread, req)
}

func (i fakeItemsSvc) UnlinkThread(ctx context.Context, req *connect.Request[chainmailv1.UnlinkThreadRequest]) (*connect.Response[chainmailv1.UnlinkThreadResponse], error) {
	return fakeUnary(ctx, i.f, chainmailv1connect.ItemsServiceUnlinkThreadProcedure, i.f.UnlinkThread, req)
}

// startFake serves f until the test ends and sets f.Socket. The socket
// lives under a short os.MkdirTemp path: Unix socket paths stop at 108 bytes.
func startFake(t *testing.T, f *fakeDaemon) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mcpf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f.Socket = filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", f.Socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(chainmailv1connect.NewAccountServiceHandler(fakeAccountSvc{f: f}))
	mux.Handle(chainmailv1connect.NewMessageServiceHandler(fakeMessageSvc{f: f}))
	mux.Handle(chainmailv1connect.NewComposeServiceHandler(fakeComposeSvc{f: f}))
	mux.Handle(chainmailv1connect.NewOrganizeServiceHandler(fakeOrganizeSvc{f: f}))
	mux.Handle(chainmailv1connect.NewWatchServiceHandler(fakeWatchSvc{f: f}))
	mux.Handle(chainmailv1connect.NewCacheServiceHandler(fakeCacheSvc{f: f}))
	mux.Handle(chainmailv1connect.NewItemsServiceHandler(fakeItemsSvc{f: f}))
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: protocols, ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Close() // Serve's return value below reports the outcome
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("fake daemon: %v", err)
		}
	})
}
