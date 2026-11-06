package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/cache"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/items"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/ref"
)

const (
	// maxViewRows caps GetItem's children and its threads.
	maxViewRows = 50
	// linkResolvers bounds the links resolved at once, so a view with many
	// links holds no more than a few of an account's pooled connections.
	linkResolvers = 4
	// The states of a resolved link.
	linkOK          = "ok"
	linkEmpty       = "empty"
	linkUnavailable = "unavailable"
)

// The order of resolved links in a view: links with mail, then empty
// ones, then the unavailable.
const (
	rankOK = iota
	rankEmpty
	rankUnavailable
)

var (
	errItemDeleted = &argError{msg: "the item is deleted", hint: "restore it first: chainmail item restore <id>"}
	errThreadKey   = &argError{msg: "invalid thread key", hint: "thread keys are lowercase hex, as chainmail item show prints them"}
)

// threadKeyRE matches a thread key: Gmail's X-GM-THRID in lowercase hex,
// or mailstore.ThreadKey's 16 hex digits.
var threadKeyRE = regexp.MustCompile(`^[0-9a-f]{1,16}$`)

// GetItem implements chainmailv1connect.ItemsServiceHandler. It returns the
// item, deleted or not, with its criteria, ancestors, children, blockers
// and linked threads. It is audited as item.get with the item and, when it
// succeeds, its type.
func (i *itemsService) GetItem(ctx context.Context, req *connect.Request[chainmailv1.GetItemRequest]) (*connect.Response[chainmailv1.GetItemResponse], error) {
	return itemCall(ctx, i.s, req, "item.get", func(ctx context.Context, _ caller, entry *audit.Entry) (*chainmailv1.GetItemResponse, error) {
		id := req.Msg.GetId()
		target, err := itemTarget("item", id)
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		resp, err := i.s.itemView(ctx, id)
		if err != nil {
			return nil, err
		}
		entry.Targets = append(entry.Targets, "type="+resp.GetItem().GetType())
		return resp, nil
	})
}

// itemView builds GetItem's answer from the items store: the item, its
// criteria, its ancestors from the root, up to maxViewRows live children
// with their total, its live blockers both ways, and how many threads are
// linked. Every store read is its own short query, done before any mail
// is read.
func (s *Server) itemView(ctx context.Context, id string) (*chainmailv1.GetItemResponse, error) {
	it, err := s.items.Get(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	crit, err := s.items.Criteria(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	ancestors, err := s.items.Ancestors(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	children, total, err := s.items.Children(ctx, id, maxViewRows)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	blockedBy, blocks, err := s.items.Blockers(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	links, err := s.items.Links(ctx, id)
	if err != nil {
		return nil, s.itemsErr(err)
	}
	return &chainmailv1.GetItemResponse{
		Item: workItemProto(it), Criteria: criteriaProto(crit), Ancestors: workItemRows(ancestors),
		Children: workItemRows(children), ChildrenTotal: int32(total),
		BlockedBy: workItemRows(blockedBy), Blocks: workItemRows(blocks),
		Threads:      s.linkViews(ctx, links[:min(len(links), maxViewRows)]),
		ThreadsTotal: int32(len(links)),
	}, nil
}

// LinkThread implements chainmailv1connect.ItemsServiceHandler. It links
// the thread of the message at ref to a live item, or on a thread already
// linked sets the anchor when asked and the note when one is sent, and
// returns the link resolved as GetItem resolves it. It is audited as
// item.link with the item, the account and the thread key; the link's
// account is the entry's account.
func (i *itemsService) LinkThread(ctx context.Context, req *connect.Request[chainmailv1.LinkThreadRequest]) (*connect.Response[chainmailv1.LinkThreadResponse], error) {
	return itemCall(ctx, i.s, req, "item.link", func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.LinkThreadResponse, error) {
		target, err := itemTarget("item", req.Msg.GetItemId())
		entry.Targets = []string{target}
		if err != nil {
			return nil, err
		}
		resp, err := i.s.linkThread(ctx, actorOf(c), req.Msg, entry)
		return resp, messageErr(err)
	})
}

// linkThread runs LinkThread for a valid item ID. The item is checked
// before any mail is read; the entry gains the account and the thread key
// as they become known.
func (s *Server) linkThread(ctx context.Context, a items.Actor, m *chainmailv1.LinkThreadRequest, entry *audit.Entry) (*chainmailv1.LinkThreadResponse, error) {
	it, err := s.items.Get(ctx, m.GetItemId())
	if err != nil {
		return nil, s.itemsErr(err)
	}
	if !it.DeletedAt.IsZero() {
		return nil, errItemDeleted
	}
	r, err := ref.Parse(m.GetRef())
	if err != nil {
		return nil, err
	}
	acct, err := s.opts.Config.Account(r.Account)
	if err != nil {
		return nil, err
	}
	entry.Account = acct.Name
	entry.Targets = append(entry.Targets, "account="+acct.Name)
	target, err := s.linkTarget(ctx, acct, r)
	if err != nil {
		return nil, err
	}
	entry.Targets = append(entry.Targets, "thread="+target.ThreadKey)
	link, created, revision, err := s.items.LinkThread(ctx, a, it.ID, target, m.GetAnchor(), m.GetNote())
	if err != nil {
		return nil, s.itemsErr(err)
	}
	return &chainmailv1.LinkThreadResponse{Link: s.resolveLink(ctx, link).link, Created: created, Revision: revision}, nil
}

// linkTarget reads what a link stores of the message at r: its thread key,
// its thread's root Message-ID and its own Message-ID. The cache answers
// when the account has a worker and holds the row, or off Google the row
// of the message a stale r was recovered to. Otherwise the message is read
// as get reads it, through the cache view or the server, so a ref no read
// can recover fails with STALE_REF.
func (s *Server) linkTarget(ctx context.Context, acct config.ResolvedAccount, r ref.Ref) (items.LinkTarget, error) {
	if s.cacheAccount(acct.Name) != nil {
		if row, ok := s.cachedLinkRow(ctx, acct, r); ok {
			return items.LinkTarget{
				Account: acct.Name, ThreadKey: row.ThreadID, MsgID: row.MessageID,
				RootMsgID: mailstore.RootMessageID(row.References, row.InReplyTo, row.MessageID),
			}, nil
		}
	}
	res, _ := (&messageService{s: s}).reader(acct).FetchMessagesFrom(ctx, []ref.Ref{r})
	if res[0].Err != nil {
		return items.LinkTarget{}, res[0].Err
	}
	msg := res[0].Message
	return items.LinkTarget{
		Account: acct.Name, ThreadKey: msg.Summary.ThreadID, MsgID: msg.MessageID,
		RootMsgID: mailstore.RootMessageID(msg.References, msg.InReplyTo, msg.MessageID),
	}, nil
}

// cachedLinkRow reads r's cached row, or off Google, when the cache has
// none, the row recoveredRow finds. The bool is false when the cache
// cannot say; a cache failure is noted and logged and leaves the read to
// linkTarget's fallback.
func (s *Server) cachedLinkRow(ctx context.Context, acct config.ResolvedAccount, r ref.Ref) (cache.Row, bool) {
	row, ok, err := s.cache.Row(ctx, r)
	if err == nil && !ok && !acct.Provider.Gmail {
		row, ok, err = s.recoveredRow(ctx, acct, r)
	}
	if err != nil {
		s.noteCacheErr(err)
		s.opts.Logger.Warn("reading a message's cached row for a link failed", "account", acct.Name, "err", err)
		return cache.Row{}, false
	}
	return row, ok
}

// recoveredRow is the cached row of the message r was recovered to by the
// cache view's ref recovery rule (cache.Store.RecoverRef): none or several
// matches find nothing.
func (s *Server) recoveredRow(ctx context.Context, acct config.ResolvedAccount, r ref.Ref) (cache.Row, bool, error) {
	moved, ok, err := s.cache.RecoverRef(ctx, acct.Name, r)
	if err != nil || !ok {
		return cache.Row{}, false, err
	}
	return s.cache.Row(ctx, moved)
}

// UnlinkThread implements chainmailv1connect.ItemsServiceHandler. It
// removes a link; an account no longer in the config can still be named.
// It is audited as item.unlink with the item, the account and the thread
// key, and the account as the entry's account when it is a valid name.
func (i *itemsService) UnlinkThread(ctx context.Context, req *connect.Request[chainmailv1.UnlinkThreadRequest]) (*connect.Response[chainmailv1.UnlinkThreadResponse], error) {
	return itemCall(ctx, i.s, req, "item.unlink", func(ctx context.Context, c caller, entry *audit.Entry) (*chainmailv1.UnlinkThreadResponse, error) {
		m := req.Msg
		itemT, itemErr := itemTarget("item", m.GetItemId())
		account, accountErr := accountTarget(m.GetAccount())
		threadT, threadErr := threadTarget(m.GetThreadKey())
		entry.Targets = []string{itemT, "account=" + account, threadT}
		if accountErr == nil {
			entry.Account = account
		}
		if err := cmp.Or(itemErr, accountErr, threadErr); err != nil {
			return nil, err
		}
		revision, err := i.s.items.UnlinkThread(ctx, actorOf(c), m.GetItemId(), account, m.GetThreadKey())
		if err != nil {
			return nil, i.s.itemsErr(err)
		}
		return &chainmailv1.UnlinkThreadResponse{Revision: revision}, nil
	})
}

// threadTarget is the audit target thread=key, and the error when key
// cannot be a thread key.
func threadTarget(key string) (string, error) {
	if !threadKeyRE.MatchString(key) {
		return "thread=" + invalidTarget, errThreadKey
	}
	return "thread=" + key, nil
}

// linkMember is one message of a linked thread: its summary and its
// Message-ID.
type linkMember struct {
	sum   mailstore.Summary
	msgID string
}

// resolvedLink is a link as a view shows it, with what orders it there.
type resolvedLink struct {
	link   *chainmailv1.ThreadLink
	latest mailstore.Summary // the newest member, when rank is rankOK
	rank   int
}

// linkViews resolves links and orders them for the item view: links with
// mail by their latest message, newest first, then the empty ones, then
// the unavailable, each group keeping the order given.
func (s *Server) linkViews(ctx context.Context, links []items.Link) []*chainmailv1.ThreadLink {
	resolved := s.resolveLinks(ctx, links)
	slices.SortStableFunc(resolved, func(a, b resolvedLink) int {
		switch {
		case a.rank != b.rank:
			return cmp.Compare(a.rank, b.rank)
		case a.rank != rankOK:
			return 0
		case later(a.latest, b.latest):
			return -1
		case later(b.latest, a.latest):
			return 1
		default:
			return 0
		}
	})
	out := make([]*chainmailv1.ThreadLink, len(resolved))
	for i, r := range resolved {
		out[i] = r.link
	}
	return out
}

// resolveLinks resolves each link, linkResolvers at a time, and returns
// them in the order given.
func (s *Server) resolveLinks(ctx context.Context, links []items.Link) []resolvedLink {
	out := make([]resolvedLink, len(links))
	sem := make(chan struct{}, linkResolvers)
	var wg sync.WaitGroup
	for i, l := range links {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = s.resolveLink(ctx, l)
		})
	}
	wg.Wait()
	return out
}

// resolveLink finds a link's thread as it is now, wherever its messages
// live (linkMembers). A thread with no message left is empty and kept. A
// failure, an account gone from the config included, makes the link
// unavailable.
func (s *Server) resolveLink(ctx context.Context, l items.Link) resolvedLink {
	out := &chainmailv1.ThreadLink{
		Account: l.Account, ThreadKey: l.ThreadKey, Note: untrusted(l.Note),
		CreatedAt: timestamp(l.CreatedAt), CreatedBy: l.CreatedBy,
	}
	acct, err := s.opts.Config.Account(l.Account)
	if err != nil {
		return s.markUnavailable(out, err)
	}
	members, source, err := s.linkMembers(ctx, acct, l)
	if err != nil {
		return s.markUnavailable(out, err)
	}
	out.Source, out.Messages = source, int32(len(members))
	if len(members) == 0 {
		out.State = linkEmpty
		return resolvedLink{link: out, rank: rankEmpty}
	}
	slices.SortStableFunc(members, func(a, b linkMember) int {
		switch {
		case later(a.sum, b.sum):
			return -1
		case later(b.sum, a.sum):
			return 1
		default:
			return 0
		}
	})
	out.State, out.Latest = linkOK, rowFromSummary(members[0].sum, false, len(members))
	if l.AnchorMsgID != "" {
		if k := slices.IndexFunc(members, func(mb linkMember) bool { return mb.msgID == l.AnchorMsgID }); k >= 0 {
			out.AnchorRef = members[k].sum.Ref.String()
		}
	}
	return resolvedLink{link: out, latest: members[0].sum, rank: rankOK}
}

// markUnavailable marks a link unavailable with err's API reason. An
// account no longer configured is a routine state that every read of the
// item meets again, so it is logged at Debug; any other failure is a
// warning. The reason is logged; err's text, which can carry a mail
// server's words, is not.
func (s *Server) markUnavailable(out *chainmailv1.ThreadLink, err error) resolvedLink {
	reason := apiReason(err)
	if errors.Is(err, config.ErrUnknownAccount) {
		s.opts.Logger.Debug("a linked thread's account is no longer configured", "account", out.GetAccount(), "reason", reason)
	} else {
		s.opts.Logger.Warn("resolving a linked thread failed", "account", out.GetAccount(), "reason", reason)
	}
	out.State, out.Reason = linkUnavailable, reason
	return resolvedLink{link: out, rank: rankUnavailable}
}

// linkMembers returns a link's messages in the account's default search
// scope and where they came from: the cache when it can answer, else the
// mail server.
func (s *Server) linkMembers(ctx context.Context, acct config.ResolvedAccount, l items.Link) ([]linkMember, string, error) {
	st := s.stores.get(acct)
	if members, ok := s.cacheMembers(ctx, acct, st, l); ok {
		return members, cache.SourceCache, nil
	}
	members, err := serverMembers(ctx, st, l)
	return members, cache.SourceServer, err
}

// cacheMembers answers a link from the cache as GetThread's cached thread
// does: only when the account has a cache worker and every folder of its
// default search scope is fresh, and then with the cached rows of the
// thread key in those folders. The bool is false when the server must
// answer. A folder list the server cannot give leaves it to the server,
// which reports the failure; a cache failure is noted and logged.
func (s *Server) cacheMembers(ctx context.Context, acct config.ResolvedAccount, st mailstore.Store, l items.Link) ([]linkMember, bool) {
	ca := s.cacheAccount(acct.Name)
	sc, ok := st.(mailstore.Scoper)
	if ca == nil || !ok {
		return nil, false
	}
	folders, err := sc.SearchFolders(ctx, "")
	if err != nil {
		return nil, false
	}
	failed := func(err error) ([]linkMember, bool) {
		s.noteCacheErr(err)
		s.opts.Logger.Warn("reading a linked thread from the cache failed", "account", acct.Name, "err", err)
		return nil, false
	}
	fresh, _, _, err := ca.Freshness(ctx, folders)
	if err != nil {
		return failed(err)
	}
	if !fresh {
		return nil, false
	}
	rows, err := s.cache.ThreadRows(ctx, acct.Name, l.ThreadKey)
	if err != nil {
		return failed(err)
	}
	var out []linkMember
	for _, r := range rows {
		if slices.Contains(folders, r.Folder) {
			out = append(out, linkMember{sum: r.Summary(), msgID: r.MessageID})
		}
	}
	return out, true
}

// serverMembers finds a link's messages on the mail server: the thread's
// refs by key and root (mailstore.ThreadFinder), then their headers, one
// fetch per folder, for the dates, the latest row and the Message-IDs. A
// message the server sends in a form chainmail cannot read fails the link
// with the fetch's *mailstore.UnreadableError, so the link is unavailable
// rather than shown with a short count, a wrong latest row or as empty.
func serverMembers(ctx context.Context, st mailstore.Store, l items.Link) ([]linkMember, error) {
	tf, canFind := st.(mailstore.ThreadFinder)
	hf, canFetch := st.(mailstore.HeaderFetcher)
	if !canFind || !canFetch {
		return nil, fmt.Errorf("%w: the account's store cannot find threads", mailstore.ErrUnsupported)
	}
	refs, err := tf.FindThread(ctx, l.ThreadKey, l.RootMsgID)
	if err != nil {
		return nil, err
	}
	var order []mailstore.Source
	uids := map[mailstore.Source][]uint32{}
	for _, r := range refs {
		src := mailstore.Source{Folder: r.Folder, UIDValidity: r.UIDValidity}
		if _, seen := uids[src]; !seen {
			order = append(order, src)
		}
		uids[src] = append(uids[src], r.UID)
	}
	var out []linkMember
	for _, src := range order {
		hs, err := hf.FetchHeaders(ctx, src, uids[src])
		if err != nil {
			return nil, err
		}
		for _, h := range hs {
			out = append(out, linkMember{sum: h.Summary, msgID: h.MessageID})
		}
	}
	return out, nil
}
