package daemon

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
)

const (
	defaultSearchLimit = 50
	maxSearchLimit     = 500
	maxGetRefs         = 50
	// callTimeout is the deadline of a unary MessageService call whose
	// client set none.
	callTimeout = 30 * time.Second
	// streamTimeout is the same for streams, which carry whole attachments.
	streamTimeout = 10 * time.Minute
	// maxResponseBytes caps a unary response. Larger payloads travel on a stream.
	maxResponseBytes = 16 << 20
	// chunkBytes is the size of one streamed data message.
	chunkBytes = 64 << 10
	// sourceServer says the mail servers answered. cache.SourceCache and
	// cache.SourceMixed are the other answers a response can report.
	sourceServer = "server"
)

// messageService implements chainmailv1connect.MessageServiceHandler.
type messageService struct {
	s *Server
}

var _ chainmailv1connect.MessageServiceHandler = (*messageService)(nil)

// withDeadline gives ctx a deadline of d unless the client already set one.
func withDeadline(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// resolveAccounts resolves account names, sorted without repeats: the ones
// given, or every configured one. An invalid name is INVALID_ARGUMENT and
// an unknown one NOT_FOUND.
func (s *Server) resolveAccounts(names []string) ([]config.ResolvedAccount, error) {
	cfg := s.opts.Config
	if len(names) == 0 {
		names = cfg.AccountNames()
	}
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	out := make([]config.ResolvedAccount, 0, len(names))
	for _, name := range names {
		if !config.ValidName(name) {
			return nil, errInvalidAccountName
		}
		acct, err := cfg.Account(name)
		if err != nil {
			return nil, err
		}
		out = append(out, acct)
	}
	return out, nil
}

func (m *messageService) store(acct config.ResolvedAccount) mailstore.Store {
	return m.s.stores.get(acct)
}

// singleAccount is the audit account: set when exactly one is in scope.
func singleAccount(accts []config.ResolvedAccount) string {
	if len(accts) == 1 {
		return accts[0].Name
	}
	return ""
}

// tally is an audit target that counts something, such as "rows=3".
func tally(name string, n int) string { return fmt.Sprintf("%s=%d", name, n) }

// partialSource describes a source that failed, for a response's partial
// list. Its message is messageErr's fixed text.
func partialSource(account, folder string, err error) *chainmailv1.PartialSource {
	cerr := toConnectError(messageErr(err))
	d := api.Detail(cerr)
	return &chainmailv1.PartialSource{Account: account, Folder: folder, Reason: d.GetReason(), Message: cerr.Message(), Hint: d.GetHint()}
}

// severity orders reasons for a call whose every source failed: what needs
// a human first, then a caller's mistake, then what may pass on its own.
var severity = map[string]int{
	api.ReasonInternal:          10,
	api.ReasonAuthRejected:      9,
	api.ReasonAuthRequired:      8,
	api.ReasonInvalidArgument:   7,
	api.ReasonMalformedResponse: 6,
	api.ReasonNotFound:          5,
	api.ReasonStaleRef:          4,
	api.ReasonServerUnavailable: 3,
	api.ReasonCacheNotReady:     2,
	api.ReasonTimeout:           1,
}

// mostSevere returns the error whose reason ranks highest.
func mostSevere(errs []error) error {
	var worst error
	rank := -1
	for _, err := range errs {
		if r := severity[api.Detail(toConnectError(err)).GetReason()]; r > rank {
			worst, rank = err, r
		}
	}
	return worst
}

// ListFolders implements chainmailv1connect.MessageServiceHandler.
func (m *messageService) ListFolders(ctx context.Context, req *connect.Request[chainmailv1.ListFoldersRequest]) (*connect.Response[chainmailv1.ListFoldersResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	resp, entry, listErr := m.listFolders(ctx, req.Msg.GetAccounts())
	if err := m.s.record(ctx, c, entry, messageErr(listErr)); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (m *messageService) listFolders(ctx context.Context, names []string) (*chainmailv1.ListFoldersResponse, audit.Entry, error) {
	entry := audit.Entry{Action: "folders.list"}
	accts, err := m.s.resolveAccounts(names)
	if err != nil {
		return nil, entry, err
	}
	entry.Account = singleAccount(accts)
	type result struct {
		folders []mailstore.Folder
		err     error
	}
	results := make([]result, len(accts))
	var wg sync.WaitGroup
	for i, a := range accts {
		wg.Go(func() {
			folders, err := m.store(a).ListFolders(ctx)
			results[i] = result{folders, err}
		})
	}
	wg.Wait()
	resp := &chainmailv1.ListFoldersResponse{}
	var failures []error
	listed := 0
	for i, a := range accts {
		if err := results[i].err; err != nil {
			resp.Partial = append(resp.Partial, partialSource(a.Name, "", err))
			failures = append(failures, err)
			continue
		}
		af := &chainmailv1.AccountFolders{Account: a.Name}
		for _, f := range results[i].folders {
			if !f.Selectable {
				continue
			}
			af.Folders = append(af.Folders, &chainmailv1.Folder{Name: f.Name, Role: string(f.Role), Messages: f.Messages, Unseen: f.Unseen})
			listed++
		}
		resp.Accounts = append(resp.Accounts, af)
	}
	entry.Targets = []string{tally("accounts", len(accts)), tally("folders", listed)}
	if len(resp.Accounts) == 0 && len(failures) > 0 {
		return nil, entry, mostSevere(failures)
	}
	return resp, entry, nil
}
