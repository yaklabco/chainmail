package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
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
	// groupHint answers an invalid group name.
	groupHint = "group names are lowercase letters, digits, dot, dash and underscore"
)

var (
	errGroupName = &argError{msg: "invalid group name", hint: groupHint}
	errWatchKind = &argError{msg: "kinds must be new, flags, expunged, status or gap", hint: "leave kinds empty for every kind"}
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
	chainmailv1connect.UnimplementedWatchServiceHandler
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
	accts, err := (&messageService{s: w.s}).accounts(names)
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

// start returns the ID a request reads after: the group's acknowledged
// event, which creates the group at the tail when it is new, the request's
// since or cursor, or the tail for a live read.
func (w *watchService) start(ctx context.Context, q watchQuery) (int64, error) {
	switch {
	case q.group != "":
		acked, _, err := w.s.watchLog.Group(ctx, q.group)
		return acked, err
	case q.since != nil:
		return *q.since, nil
	default:
		return w.s.watchLog.Tail(ctx)
	}
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
	pos, err := w.start(ctx, q)
	if err != nil {
		return w.streamEnd(ctx, err)
	}
	return w.send(ctx, q.filter, pos, stream)
}

// send streams the events after pos that match f, with heartbeats, until
// ctx ends or the client goes away.
func (w *watchService) send(ctx context.Context, f watch.Filter, pos int64, stream *connect.ServerStream[chainmailv1.SubscribeResponse]) error {
	for {
		page, err := w.s.watchLog.Read(ctx, watch.Query{After: pos, Filter: f, Limit: subscribePage})
		if err != nil {
			return w.streamEnd(ctx, err)
		}
		for _, e := range page.Events {
			if stream.Send(&chainmailv1.SubscribeResponse{Item: &chainmailv1.SubscribeResponse_Event{Event: eventToProto(e)}}) != nil {
				return toConnectError(clientGone())
			}
		}
		if page.Next != pos {
			// The read examined events, so more may follow at once.
			pos = page.Next
			continue
		}
		hb := &chainmailv1.Heartbeat{At: timestamppb.New(w.s.opts.Now()), Position: pos}
		if stream.Send(&chainmailv1.SubscribeResponse{Item: &chainmailv1.SubscribeResponse_Heartbeat{Heartbeat: hb}}) != nil {
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
