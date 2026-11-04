package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/watch"
)

const (
	// defaultWatchHeartbeat is how often an idle Subscribe stream sends a
	// heartbeat when Options.WatchHeartbeat is zero. The CLI reconnects
	// after 75 seconds without one.
	defaultWatchHeartbeat = 30 * time.Second
	// subscribePage is how many events a Subscribe stream reads at a time.
	subscribePage = 100
	// groupHint answers an invalid group name. Names starting with cache.
	// are the daemon's own (watch.IsCacheGroup).
	groupHint = "group names are lowercase letters, digits, dot, dash and underscore, and do not start with cache."
	// maxReadWaitSeconds is the longest ReadEvents wait.
	maxReadWaitSeconds = 60
	// defaultReadLimit is the ReadEvents page size when a request sets none.
	defaultReadLimit = 50
	// maxReadLimit is the largest ReadEvents page.
	maxReadLimit = 200
	// readWaitReserve is how long before its deadline a ReadEvents wait
	// ends, so the acknowledgement and the response still fit. When less
	// than twice that remains, the wait keeps half of what is left.
	readWaitReserve = time.Second
	// staleReadTries bounds the ReadEvents tries when other readers keep
	// recovering a stale group first. One retry is all a single recovery
	// race needs.
	staleReadTries = 3
	// settleTimeout bounds writing down a ReadEvents read that has already
	// happened, the group's acknowledgement, once the request's own
	// deadline no longer applies.
	settleTimeout = 5 * time.Second
)

var (
	errGroupName = &argError{msg: "invalid group name", hint: groupHint}
	errWatchKind = &argError{msg: "kinds must be new, flags, expunged, status or gap", hint: "leave kinds empty for every kind"}
	errReadWait  = &argError{msg: "wait_seconds must be 0 to 60", hint: "0 returns at once"}
	errReadLimit = &argError{msg: "limit must be 0 to 200", hint: "0 means 50"}
	errAckID     = &argError{msg: "id must be 0 or more", hint: "acknowledge an event ID or a heartbeat position"}
)

// kindNames maps the API's event kind names to the log's.
var kindNames = map[string]watch.Kind{
	string(watch.KindNew):      watch.KindNew,
	string(watch.KindFlags):    watch.KindFlags,
	string(watch.KindExpunged): watch.KindExpunged,
	string(watch.KindStatus):   watch.KindStatus,
	string(watch.KindGap):      watch.KindGap,
}

// watchService implements chainmailv1connect.WatchServiceHandler.
type watchService struct {
	s *Server
}

var _ chainmailv1connect.WatchServiceHandler = (*watchService)(nil)

// watchQuery is a validated Subscribe or ReadEvents request.
type watchQuery struct {
	group  string
	since  *int64
	filter watch.Filter
}

// parse validates a request's start and filter, and returns the audit
// entry that describes them, without its action. after names the request's
// ID field, since or cursor, in error messages. The entry records an
// invalid group or kind as invalidTarget and never the in value, which can
// be a label name. A cursor is recorded as since=<n>, since it means the
// same start. The first failure, in field order, is returned.
func (w *watchService) parse(after, group string, since *int64, f *chainmailv1.WatchFilter) (watchQuery, audit.Entry, error) {
	start, startErr := startTarget(after, group, since)
	names := slices.Compact(slices.Sorted(slices.Values(f.GetAccounts())))
	accounts, accountErr := w.filterAccounts(names)
	kinds, kindTarget, kindErr := watchKinds(f.GetKinds())
	in := "unset"
	if f.GetIn() != "" {
		in = "set"
	}
	entry := audit.Entry{Targets: []string{start, tally("accounts", len(names)), "kinds=" + kindTarget, "in=" + in}}
	if len(accounts) == 1 {
		entry.Account = accounts[0]
	}
	q := watchQuery{group: group, since: since, filter: watch.Filter{Accounts: accounts, Kinds: kinds, In: f.GetIn()}}
	return q, entry, cmp.Or(startErr, accountErr, kindErr)
}

// startTarget is the audit target of a request's start, and the error when
// the start is not valid.
func startTarget(after, group string, since *int64) (string, error) {
	switch {
	case group != "" && !watch.ValidGroupName(group):
		return "group=" + invalidTarget, errGroupName
	case group != "" && since != nil:
		return "group=" + group, &argError{msg: "set at most one of group and " + after, hint: "a group resumes after its own acknowledged event"}
	case group != "":
		return "group=" + group, nil
	case since == nil:
		return "live", nil
	case *since < 0:
		return fmt.Sprintf("since=%d", *since), &argError{msg: after + " must be 0 or more", hint: "pass an event ID or a heartbeat position"}
	default:
		return fmt.Sprintf("since=%d", *since), nil
	}
}

// filterAccounts checks a filter's account names against the config as
// every other call does: an invalid name is INVALID_ARGUMENT and an unknown
// one NOT_FOUND. An empty list matches every account and needs no check.
func (w *watchService) filterAccounts(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	accts, err := w.s.resolveAccounts(names)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(accts))
	for i, a := range accts {
		out[i] = a.Name
	}
	return out, nil
}

// watchKinds checks kind names and returns them sorted without repeats,
// with their audit form, such as "flags,new". An unknown name is never
// recorded, since it can be any bytes.
func watchKinds(names []string) ([]watch.Kind, string, error) {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	kinds := make([]watch.Kind, 0, len(names))
	for _, n := range names {
		k, ok := kindNames[n]
		if !ok {
			return nil, invalidTarget, errWatchKind
		}
		kinds = append(kinds, k)
	}
	return kinds, strings.Join(names, ","), nil
}

// readStart is where a Subscribe or ReadEvents request reads from.
type readStart struct {
	// after is the ID to read after.
	after int64
	// gap is the trimmed gap to deliver before anything else, or nil.
	gap *watch.Event
	// stale says the group was acknowledged past the tail and must be
	// reset with Log.ResetGroup, which Ack could never do, once the caller
	// may move it.
	stale bool
	// from is the acknowledged ID a stale group was found at, which its
	// reset compares against.
	from int64
}

// start returns where a request reads from: after the group's acknowledged
// event, which creates the group at the tail when it is new, after the
// request's since or cursor, or after the tail for a live read.
//
// A client only ever learns IDs up to the tail, so a start past it means
// the daemon's state was reset under the client. Such a start is capped at
// the tail, and start also returns the trimmed gap that reports the loss,
// unless the filter drops it, as Log.Read reports a start below the oldest
// event. A group acknowledged past the tail belongs to a log that no longer
// exists, so it is marked stale. start changes nothing itself: the caller
// resets the group, to the tail start read or past it, never to a fresh
// tail, so an event published since is never acknowledged unread.
func (w *watchService) start(ctx context.Context, q watchQuery) (readStart, error) {
	var after int64
	switch {
	case q.group != "":
		acked, _, err := w.s.watchLog.Group(ctx, q.group)
		if err != nil {
			return readStart{}, err
		}
		after = acked
	case q.since != nil:
		after = *q.since
	}
	tail, err := w.s.watchLog.Tail(ctx)
	switch {
	case err != nil:
		return readStart{}, err
	case q.group == "" && q.since == nil:
		return readStart{after: tail}, nil // a live read
	case after <= tail:
		return readStart{after: after}, nil
	}
	if w.s.watchAfterTail != nil {
		w.s.watchAfterTail(ctx)
	}
	st := readStart{after: tail, stale: q.group != "", from: after}
	if gap := (watch.Event{At: w.s.opts.Now().UTC(), Kind: watch.KindGap, Reason: watch.GapTrimmed, NextID: tail + 1}); q.filter.Match(gap) {
		st.gap = &gap
	}
	return st, nil
}

// watchLogErr is the error for a failed event log call. The client sees
// fixed text with the reason of the cause, such as TIMEOUT for a
// cancellation. The database's text stays in the daemon's log.
func (s *Server) watchLogErr(err error) error {
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.opts.Logger.Error("event log call failed", "err", err)
	}
	return &fixedTextError{msg: "the event log failed", err: err}
}

// Subscribe implements chainmailv1connect.WatchServiceHandler. It records
// watch.subscribe before it sends anything, so a stream that runs for days
// is audited when it starts, and a failed audit write fails the call. It
// then sends the events after its start, a heartbeat each time it has
// caught up, and a heartbeat every Options.WatchHeartbeat while nothing
// arrives.
//
// Unlike the MessageService streams it has no streamTimeout. A watcher
// holds one stream open for as long as it runs, and the heartbeats prove
// it alive. It ends when the client goes away or the daemon shuts down,
// either of which cancels ctx, and that is a normal end. A group stream
// acknowledges nothing here: the client acknowledges what it has written,
// so a crash between send and write loses no event.
func (w *watchService) Subscribe(ctx context.Context, req *connect.Request[chainmailv1.SubscribeRequest], stream *connect.ServerStream[chainmailv1.SubscribeResponse]) error {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return err
	}
	m := req.Msg
	q, entry, parseErr := w.parse("since", m.GetGroup(), m.Since, m.GetFilter())
	entry.Action = "watch.subscribe"
	if err := w.s.record(ctx, c, entry, messageErr(parseErr)); err != nil {
		return err
	}
	st, err := w.start(ctx, q)
	if err != nil {
		return w.streamEnd(ctx, err)
	}
	if st.gap != nil {
		if w.sendItem(stream, &chainmailv1.SubscribeResponse{Item: &chainmailv1.SubscribeResponse_Event{Event: eventToProto(*st.gap)}}) != nil {
			return toConnectError(clientGone())
		}
	}
	pos := st.after
	if st.stale {
		// Only once the gap is sent may a stale group be recovered: a
		// stream that ends first leaves it stale, so the next connection
		// gets the gap again. The client's Acks could never lower it, so
		// the stream recovers it itself.
		if pos, err = w.recoverGroup(ctx, q.group, st); err != nil {
			return w.streamEnd(ctx, err)
		}
	}
	return w.send(ctx, q, pos, stream)
}

// recoverGroup resets a stale group from the acknowledged ID start found
// to the tail it captured, and returns where the stream resumes: that
// tail, or, when another reader recovered or acknowledged the group first,
// the group's acknowledged ID now, so nothing is lowered or replayed.
func (w *watchService) recoverGroup(ctx context.Context, group string, st readStart) (int64, error) {
	applied, err := w.s.watchLog.ResetGroup(ctx, group, st.from, st.after)
	if err != nil || applied {
		return st.after, err
	}
	acked, _, err := w.s.watchLog.Group(ctx, group)
	return acked, err
}

// send streams the events after pos that match q's filter, with
// heartbeats, until ctx ends or the client goes away. A group stream
// refreshes its group's last use before a heartbeat once a heartbeat
// interval has passed since the last refresh, or since start stamped it:
// the client acknowledges only positions that move, so a quiet group would
// otherwise look unused and be pruned by Log.Trim while its stream is
// open, and the caught-up heartbeat after each event needs no write.
func (w *watchService) send(ctx context.Context, q watchQuery, pos int64, stream *connect.ServerStream[chainmailv1.SubscribeResponse]) error {
	touched := w.s.opts.Now()
	for {
		page, err := w.s.watchLog.Read(ctx, watch.Query{After: pos, Filter: q.filter, Limit: subscribePage})
		if err != nil {
			return w.streamEnd(ctx, err)
		}
		for _, e := range page.Events {
			if w.sendItem(stream, &chainmailv1.SubscribeResponse{Item: &chainmailv1.SubscribeResponse_Event{Event: eventToProto(e)}}) != nil {
				return toConnectError(clientGone())
			}
		}
		if page.Next != pos {
			// The read examined events, so more may follow at once.
			pos = page.Next
			continue
		}
		if now := w.s.opts.Now(); q.group != "" && now.Sub(touched) >= w.s.opts.WatchHeartbeat {
			if err := w.s.watchLog.TouchGroup(ctx, q.group); err != nil {
				return w.streamEnd(ctx, err)
			}
			touched = now
		}
		hb := &chainmailv1.Heartbeat{At: timestamppb.New(w.s.opts.Now()), Position: pos}
		if w.sendItem(stream, &chainmailv1.SubscribeResponse{Item: &chainmailv1.SubscribeResponse_Heartbeat{Heartbeat: hb}}) != nil {
			return toConnectError(clientGone())
		}
		if err := w.wait(ctx, pos, w.s.opts.WatchHeartbeat); err != nil {
			return w.streamEnd(ctx, err)
		}
	}
}

// wait blocks until an event after pos exists, d passes, or ctx ends. It
// returns nil when an event exists or d passed, and an error when ctx
// ended or the log failed.
func (w *watchService) wait(ctx context.Context, pos int64, d time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	err := w.s.watchLog.Wait(waitCtx, pos)
	if ctx.Err() == nil && waitCtx.Err() != nil {
		return nil // d passed with nothing new
	}
	return err
}

// streamEnd is what Subscribe returns after err: nil once ctx has ended,
// since the client or the daemon closed the stream, else err as fixed text.
func (w *watchService) streamEnd(ctx context.Context, err error) error {
	if ctx.Err() == nil {
		return toConnectError(w.s.watchLogErr(err))
	}
	return nil // err is only the stream's own end
}

// ReadEvents implements chainmailv1connect.WatchServiceHandler. It returns
// one page, waiting up to wait_seconds for the first event, and records
// watch.read with the number of events returned. A client cancelling the
// call ends the wait.
//
// A group read acknowledges the page's Next, so events the filter dropped
// move the group too, or recovers a stale group to it, but only once the
// audit row is written: a read that could not be audited returns an error
// and leaves the group where it was. If the acknowledgement itself fails,
// the call fails without its events and the next read returns the same
// page, so delivery is at least once. If another reader recovered a stale
// group first, the page, which starts with a gap that reader already
// reported, is dropped and the read runs again as a normal read. Each try
// is audited, so that rare race leaves one extra watch.read row.
func (w *watchService) ReadEvents(ctx context.Context, req *connect.Request[chainmailv1.ReadEventsRequest]) (*connect.Response[chainmailv1.ReadEventsResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	m := req.Msg
	q, entry, parseErr := w.parse("cursor", m.GetGroup(), m.Cursor, m.GetFilter())
	entry.Action = "watch.read"
	wait, limit, boundsErr := readBounds(m.GetWaitSeconds(), m.GetLimit(), w.s.watchWaitUnit)
	argErr := cmp.Or(parseErr, boundsErr)
	for range staleReadTries {
		resp, done, err := w.readOnce(ctx, c, q, entry, argErr, wait, limit)
		if done || err != nil {
			return resp, err
		}
	}
	return nil, toConnectError(w.s.watchLogErr(fmt.Errorf("group %s kept changing while it was recovered", q.group)))
}

// readOnce is one try of ReadEvents: the read, its audit row, then the
// group's acknowledgement. It reports done false, with no response, when a
// stale group's recovery did not apply because another reader recovered
// or acknowledged it first.
func (w *watchService) readOnce(ctx context.Context, c caller, q watchQuery, entry audit.Entry, argErr error, wait time.Duration, limit int) (*connect.Response[chainmailv1.ReadEventsResponse], bool, error) {
	var (
		page watch.Page
		st   readStart
	)
	readErr := argErr
	if readErr == nil {
		page, st, readErr = w.read(ctx, q, wait, limit)
	}
	entry.Targets = append(slices.Clone(entry.Targets), tally("events", len(page.Events)))
	if err := w.s.record(ctx, c, entry, messageErr(readErr)); err != nil {
		return nil, true, err
	}
	if q.group != "" {
		moved, err := w.ack(ctx, q.group, st, page.Next)
		if err != nil || !moved {
			return nil, err != nil, err
		}
	}
	resp := &chainmailv1.ReadEventsResponse{Events: make([]*chainmailv1.WatchEvent, 0, len(page.Events)), NextCursor: page.Next}
	for _, e := range page.Events {
		resp.Events = append(resp.Events, eventToProto(e))
	}
	return connect.NewResponse(resp), true, nil
}

// readBounds checks a ReadEvents wait and limit and fills the default limit.
func readBounds(waitSeconds, limit int32, unit time.Duration) (time.Duration, int, error) {
	switch {
	case waitSeconds < 0 || waitSeconds > maxReadWaitSeconds:
		return 0, 0, errReadWait
	case limit < 0 || limit > maxReadLimit:
		return 0, 0, errReadLimit
	case limit == 0:
		limit = defaultReadLimit
	}
	return time.Duration(waitSeconds) * unit, int(limit), nil
}

// read runs a ReadEvents query: one page after the start and, when it is
// empty and wait is positive, the first events that arrive within wait.
// The wait ends before the request's deadline when that comes first, so a
// client that set a short deadline gets what there is, not TIMEOUT. It
// does not move a group: ReadEvents does, once the read is audited, and
// read reports whether the group is stale (see start).
func (w *watchService) read(ctx context.Context, q watchQuery, wait time.Duration, limit int) (watch.Page, readStart, error) {
	ctx, cancel := withDeadline(ctx, wait+callTimeout)
	defer cancel()
	st, err := w.start(ctx, q)
	if err != nil {
		return watch.Page{}, readStart{}, w.s.watchLogErr(err)
	}
	after, gap := st.after, st.gap
	if gap != nil {
		// The gap counts toward the limit, as one from Log.Read does.
		limit--
	}
	query := watch.Query{After: after, Filter: q.filter, Limit: limit}
	page := watch.Page{Next: after}
	if limit > 0 {
		if page, err = w.s.watchLog.Read(ctx, query); err != nil {
			return watch.Page{}, readStart{}, w.s.watchLogErr(err)
		}
	}
	if gap != nil {
		page.Events = append([]watch.Event{*gap}, page.Events...)
	}
	// A stale group's read returns at once even if its filter dropped the
	// gap: its recovery is news, and a read that loses the recovery race is
	// tried again, which must not wait twice.
	if wait = waitBudget(ctx, wait); len(page.Events) == 0 && wait > 0 && !st.stale {
		if page, err = w.waitRead(ctx, query, page, wait); err != nil {
			return watch.Page{}, readStart{}, err
		}
	}
	return page, st, nil
}

// waitRead waits up to wait for the first events after page, reading on
// past events the filter drops. It returns page as it stands when wait
// passes, and an error when ctx is cancelled first. ctx's deadline passing
// is no error: read gives the wait a budget that ends before it, but a
// late wakeup on a loaded machine can find both passed, and that is still
// a quiet wait, which contract C8 says returns what it has.
func (w *watchService) waitRead(ctx context.Context, query watch.Query, page watch.Page, wait time.Duration) (watch.Page, error) {
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for len(page.Events) == 0 {
		waitErr := w.s.watchLog.Wait(waitCtx, page.Next)
		if w.s.watchAfterWait != nil {
			w.s.watchAfterWait(ctx)
		}
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			return watch.Page{}, fmt.Errorf("waiting for events: %w", ctx.Err())
		case waitCtx.Err() != nil:
			return page, nil // the wait, or the deadline, passed with nothing new
		case waitErr != nil:
			return watch.Page{}, w.s.watchLogErr(waitErr)
		}
		query.After = page.Next
		next, err := w.s.watchLog.Read(ctx, query)
		if err != nil {
			return watch.Page{}, w.s.watchLogErr(err)
		}
		page = next
	}
	return page, nil
}

// waitBudget is how long a read may wait for events: wait, cut short to
// end readWaitReserve before ctx's deadline, or half the time left when
// less than twice the reserve remains. It is never negative.
func waitBudget(ctx context.Context, wait time.Duration) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return wait
	}
	left := time.Until(deadline)
	return max(0, min(wait, left-min(readWaitReserve, left/2)))
}

// Ack implements chainmailv1connect.WatchServiceHandler. It raises a
// group's acknowledged event, creating the group when it is new, and never
// lowers it. Like GetStatus it is deliberately not audited: it changes no
// mail and no policy state, only where the group's next read starts, and a
// watcher acknowledges every second, which would flood the audit log.
func (w *watchService) Ack(ctx context.Context, req *connect.Request[chainmailv1.AckRequest]) (*connect.Response[chainmailv1.AckResponse], error) {
	group, id := req.Msg.GetGroup(), req.Msg.GetId()
	switch {
	case !watch.ValidGroupName(group):
		return nil, toConnectError(errGroupName)
	case id < 0:
		return nil, toConnectError(errAckID)
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	if err := w.s.watchLog.Ack(ctx, group, id); err != nil {
		return nil, toConnectError(w.s.watchLogErr(err))
	}
	return connect.NewResponse(&chainmailv1.AckResponse{}), nil
}

// GetWatchStatus implements chainmailv1connect.WatchServiceHandler. It
// reports each synced folder's status, ordered by account then folder.
// Like GetStatus it is read-only, returns no message content, and is
// deliberately not audited: polling watch status would otherwise flood the
// audit log.
func (w *watchService) GetWatchStatus(ctx context.Context, _ *connect.Request[chainmailv1.GetWatchStatusRequest]) (*connect.Response[chainmailv1.GetWatchStatusResponse], error) {
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	rows, err := w.s.watchState.Folders(ctx)
	if err != nil {
		return nil, toConnectError(w.s.watchLogErr(err))
	}
	resp := &chainmailv1.GetWatchStatusResponse{Folders: make([]*chainmailv1.FolderSyncStatus, 0, len(rows))}
	for _, r := range rows {
		resp.Folders = append(resp.Folders, folderStatusProto(r))
	}
	return connect.NewResponse(resp), nil
}

// ListGroups implements chainmailv1connect.WatchServiceHandler. It records
// watch.groups.list with the number of groups.
func (w *watchService) ListGroups(ctx context.Context, req *connect.Request[chainmailv1.ListGroupsRequest]) (*connect.Response[chainmailv1.ListGroupsResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	groups, listErr := w.s.watchLog.Groups(ctx)
	if listErr != nil {
		listErr = w.s.watchLogErr(listErr)
	}
	// The cache's groups belong to the daemon: they are left out of the
	// list and of its audited count.
	groups = slices.DeleteFunc(groups, func(g watch.GroupInfo) bool { return watch.IsCacheGroup(g.Name) })
	entry := audit.Entry{Action: "watch.groups.list", Targets: []string{tally("groups", len(groups))}}
	if err := w.s.record(ctx, c, entry, listErr); err != nil {
		return nil, err
	}
	resp := &chainmailv1.ListGroupsResponse{Groups: make([]*chainmailv1.GroupStatus, 0, len(groups))}
	for _, g := range groups {
		resp.Groups = append(resp.Groups, groupProto(g))
	}
	return connect.NewResponse(resp), nil
}

// DeleteGroup implements chainmailv1connect.WatchServiceHandler. It records
// watch.groups.delete with the group's name, or invalidTarget for a name
// that is not valid. Deleting an unknown group succeeds with deleted false.
func (w *watchService) DeleteGroup(ctx context.Context, req *connect.Request[chainmailv1.DeleteGroupRequest]) (*connect.Response[chainmailv1.DeleteGroupResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	ctx, cancel := withDeadline(ctx, callTimeout)
	defer cancel()
	name := req.Msg.GetName()
	var deleted bool
	target, delErr := "group="+invalidTarget, error(errGroupName)
	if watch.ValidGroupName(name) {
		target = "group=" + name
		if deleted, delErr = w.s.watchLog.DeleteGroup(ctx, name); delErr != nil {
			delErr = w.s.watchLogErr(delErr)
		}
	}
	if err := w.s.record(ctx, c, audit.Entry{Action: "watch.groups.delete", Targets: []string{target}}, delErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.DeleteGroupResponse{Deleted: deleted}), nil
}

// ack moves a group past a page ReadEvents has read and audited: an Ack to
// id, or for a stale group a reset from the acknowledged ID start found to
// id. It reports false when the reset did not apply because another reader
// recovered or acknowledged the group first. It runs under settle, so a
// read whose quiet wait outlived the request's deadline is still
// acknowledged, and one whose client went away is not. A failure is the
// client's error, with fixed text.
func (w *watchService) ack(ctx context.Context, group string, st readStart, id int64) (bool, error) {
	moved := true
	err := w.s.settle(ctx, func(ctx context.Context) error {
		if st.stale {
			var err error
			moved, err = w.s.watchLog.ResetGroup(ctx, group, st.from, id)
			return err
		}
		return w.s.watchLog.Ack(ctx, group, id)
	})
	if err != nil {
		return false, toConnectError(w.s.watchLogErr(err))
	}
	return moved, nil
}

// sendItem sends one Subscribe item. Server.watchSendHook, set only by
// tests, can fail the send first.
func (w *watchService) sendItem(stream *connect.ServerStream[chainmailv1.SubscribeResponse], item *chainmailv1.SubscribeResponse) error {
	if hook := w.s.watchSendHook; hook != nil {
		if err := hook(item); err != nil {
			return err
		}
	}
	return stream.Send(item)
}

// settle runs write, which writes down a group read that has already
// happened: its acknowledgement or a stale group's reset.
//
// The request's deadline does not stop it, since a quiet wait may end at
// or just past the deadline and must still be acknowledged (contract C8).
// settleTimeout bounds it instead. A request that ended with
// context.Canceled, because the client went away, is never written down:
// that is checked synchronously just before write, and a cancellation
// during write ends it too. A daemon shutdown ends it as well, through
// the server's shutdown channel, which still reports a shutdown after
// the request's own context has ended with DeadlineExceeded.
//
// One window remains, by design: a disconnect after write and before the
// response reaches the client loses that page, since a group read
// acknowledges what it returns before it responds. Under a disconnect a
// group read is at most once.
func (s *Server) settle(ctx context.Context, write func(context.Context) error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("settling a read: %w", ctx.Err())
	}
	select {
	case <-s.shutdown:
		return fmt.Errorf("settling a read during shutdown: %w", context.Canceled)
	default:
	}
	settled, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	stopRequest := afterJoined(ctx, func() {
		if errors.Is(ctx.Err(), context.Canceled) {
			cancel()
		}
	})
	defer stopRequest()
	if s.shutdown != nil {
		// A shutdown ends settlement, joined.
		finished := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			select {
			case <-s.shutdown:
				cancel()
			case <-finished:
			}
		})
		defer func() {
			close(finished)
			wg.Wait()
		}()
	}
	return write(settled)
}

// afterJoined runs f once ctx is done, as context.AfterFunc does, and
// returns a stop that prevents f or, when f has started, waits for it to
// return, so f never outlives the caller.
func afterJoined(ctx context.Context, f func()) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		f()
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}
