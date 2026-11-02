package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/api"
)

const watchHelp = `Stream mail events from the daemon's event log: one JSON line per event,
or one human line per event on a terminal.

Where the stream starts:
  --group NAME   after the group's acknowledged event. Each line is
                 acknowledged once it is written, so a restarted watcher
                 misses nothing. A new group starts at the newest event.
                 Delivery is at least once.
  --since ID     after this event ID.
  neither        with events from now on.

Filters apply in the daemon. status and gap events pass a --kind filter
unless --kind names them. --in matches a folder or a Gmail label, and
inbox also matches \Inbox.

The watch reconnects by itself when the daemon goes away, or sends nothing
for 75 seconds, resuming from its group or from the last event or
heartbeat it saw. It stops with an error only when the daemon refuses the
request, such as a bad group name or an account that needs a login.
--once stops at the first heartbeat, which comes once every event already
in the log has been printed.

Subjects, names and addresses in new events come from the message and are
untrusted.`

// Watch timing. The daemon sends a heartbeat every 30 seconds.
const (
	// watchHeartbeatTimeout is how long a stream may stay silent before
	// the watch reconnects.
	watchHeartbeatTimeout = 75 * time.Second
	// watchBackoffStart and watchBackoffMax bound the wait before a reconnect.
	watchBackoffStart = time.Second
	watchBackoffMax   = 30 * time.Second
	// watchAckInterval and watchAckBatch bound how long, and for how many
	// lines, a group acknowledgement waits.
	watchAckInterval = time.Second
	watchAckBatch    = 100
	// watchFinalAckTimeout bounds the acknowledgement sent on exit.
	watchFinalAckTimeout = 5 * time.Second
)

// watchKinds are the event kinds --kind takes.
var watchKinds = []string{"new", "flags", "expunged", "status", "gap"}

// newWatchCmd builds chainmail watch and its status and groups subcommands.
func newWatchCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var (
		group     string
		since     int64
		accounts  []string
		kinds     []string
		in        string
		once      bool
		heartbeat bool
	)
	cmd := &cobra.Command{
		Use:   "watch [--group NAME | --since ID] [--account NAME]... [--kind KIND[,KIND]...] [--in FOLDER] [--once] [--heartbeat]",
		Short: "Stream mail events as they happen, one per line",
		Long:  watchHelp,
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := watchOptions{group: group, once: once, heartbeat: heartbeat, table: g.table()}
			if cmd.Flags().Changed("since") {
				switch {
				case group != "":
					return usageError{err: errors.New("--group and --since cannot be used together"), hint: helpHint(cmd)}
				case since < 0:
					return usageError{err: errors.New("--since must not be negative"), hint: helpHint(cmd)}
				}
				opts.since = &since
			}
			ks, err := parseKinds(kinds)
			if err != nil {
				return usageError{err: err, hint: helpHint(cmd)}
			}
			opts.filter = &chainmailv1.WatchFilter{Accounts: accounts, Kinds: ks, In: in}
			c := g.client(env)
			defer c.Close()
			return newWatcher(env, c.Watch, opts).run(ctx)
		},
	}
	cmd.Flags().StringVar(&group, "group", "", "resume after this group's acknowledged event, and acknowledge each line once written")
	cmd.Flags().Int64Var(&since, "since", 0, "resume after this event ID")
	cmd.Flags().StringArrayVar(&accounts, "account", nil, "only this account; repeat for more (default: all)")
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "only these kinds, comma separated: new, flags, expunged, status, gap (default: all)")
	cmd.Flags().StringVar(&in, "in", "", "only this folder or Gmail label, such as inbox (default: all)")
	cmd.Flags().BoolVar(&once, "once", false, "exit at the first heartbeat, once every event already in the log is printed")
	cmd.Flags().BoolVar(&heartbeat, "heartbeat", false, "print heartbeats too")
	cmd.AddCommand(newWatchStatusCmd(ctx, env, g), newWatchGroupsCmd(ctx, env, g))
	return cmd
}

// parseKinds checks --kind values against the event kinds.
func parseKinds(raw []string) ([]string, error) {
	var kinds []string
	for _, k := range raw {
		k = strings.TrimSpace(k)
		if !slices.Contains(watchKinds, k) {
			return nil, fmt.Errorf("unknown --kind %q: want new, flags, expunged, status or gap", k)
		}
		kinds = append(kinds, k)
	}
	return kinds, nil
}

// watchOptions are chainmail watch's flags, checked.
type watchOptions struct {
	group     string
	since     *int64
	filter    *chainmailv1.WatchFilter
	once      bool
	heartbeat bool
	table     bool
}

// watcher is one chainmail watch run. Only the command's goroutine uses it.
type watcher struct {
	env   Env
	watch chainmailv1connect.WatchServiceClient
	opts  watchOptions
	// pos is the highest event ID or heartbeat position seen, or --since
	// before any. hasPos says there is one.
	pos    int64
	hasPos bool
	// acked is the highest position the group has acknowledged in this
	// run, and unacked counts the lines printed since. ackDue fires when
	// the current batch is due, and is nil when no batch is waiting.
	acked   int64
	unacked int
	ackDue  <-chan time.Time
}

func newWatcher(env Env, svc chainmailv1connect.WatchServiceClient, opts watchOptions) *watcher {
	w := &watcher{env: env, watch: svc, opts: opts}
	if opts.since != nil {
		w.pos, w.hasPos = *opts.since, true
	}
	return w
}

var (
	// errWatchOnce ends a --once watch at its first heartbeat.
	errWatchOnce = errors.New("first heartbeat")
	// errStreamEnded is a stream the daemon ended without an error, as it
	// does when it shuts down.
	errStreamEnded = errors.New("the daemon ended the event stream")
	// errHeartbeatTimeout is a stream silent for watchHeartbeatTimeout.
	errHeartbeatTimeout = errors.New("no heartbeat from the daemon for 75s")
)

// stopError ends a watch without a reconnect: writing its output failed.
type stopError struct{ err error }

func (e stopError) Error() string { return e.err.Error() }
func (e stopError) Unwrap() error { return e.err }

// run streams until --once sees its first heartbeat or a signal ends ctx.
// A stream that fails, ends, or stays silent for watchHeartbeatTimeout is
// reopened after a backoff, from the group or the last position seen.
// Only a refusal the daemon would repeat, or a failure to write output,
// ends the watch with an error. A group's pending acknowledgement goes out
// before each reconnect and on exit.
func (w *watcher) run(ctx context.Context) error {
	defer w.finalAck(ctx)
	attempt := 0
	for {
		got, err := w.session(ctx)
		if signalled(ctx) {
			return nil // a signal ended the watch, as it ends mcp
		}
		var stop stopError
		switch {
		case errors.Is(err, errWatchOnce):
			return nil
		case errors.As(err, &stop):
			return stop.err
		case watchFatal(err):
			return err
		}
		if got {
			attempt = 0
		}
		w.flushAck(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-w.env.after(watchBackoff(attempt)):
		}
		attempt++
	}
}

// watchFatal reports whether the daemon refused the stream in a way a
// retry would repeat: a bad request, a refused peer or policy, an account
// that needs a login, or an unknown account. Everything else, an
// unreachable daemon included, is retried.
func watchFatal(err error) bool {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return false
	}
	switch api.ExitCode(api.Detail(cerr).GetReason()) {
	case api.ExitUsage, api.ExitDenied, api.ExitAuth, api.ExitNotFound:
		return true
	default:
		return false
	}
}

// watchBackoff is the wait before reconnect attempt n (0-based): 1s,
// doubling, capped at watchBackoffMax.
func watchBackoff(n int) time.Duration {
	d := watchBackoffStart
	for range n {
		d *= 2
		if d >= watchBackoffMax {
			return watchBackoffMax
		}
	}
	return d
}

// signalled reports whether ctx has ended, as a signal ends it.
func signalled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// request is the SubscribeRequest for the next stream: the group, else
// since the last position seen, else live.
func (w *watcher) request() *chainmailv1.SubscribeRequest {
	req := &chainmailv1.SubscribeRequest{Group: w.opts.group, Filter: w.opts.filter}
	if w.opts.group == "" && w.hasPos {
		req.Since = proto.Int64(w.pos)
	}
	return req
}

// session runs one Subscribe stream until it fails or ends, stays silent
// for watchHeartbeatTimeout, --once sees a heartbeat, or ctx ends. It
// reports whether any message arrived, so the caller can reset its backoff.
func (w *watcher) session(ctx context.Context) (bool, error) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := w.watch.Subscribe(sctx, connect.NewRequest(w.request()))
	if err != nil {
		return false, err
	}
	msgs := make(chan *chainmailv1.SubscribeResponse)
	ended := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		for stream.Receive() {
			select {
			case msgs <- stream.Msg():
			case <-sctx.Done():
				return
			}
		}
		ended <- stream.Err()
	})
	defer func() {
		cancel() // ends a Receive still waiting on the daemon
		wg.Wait()
		_ = stream.Close() // the stream has ended or been cancelled; its outcome is returned below
	}()
	got := false
	quiet := w.env.after(watchHeartbeatTimeout)
	for {
		select {
		case <-ctx.Done():
			return got, ctx.Err()
		case err := <-ended:
			if err == nil {
				err = errStreamEnded
			}
			return got, err
		case <-quiet:
			return got, errHeartbeatTimeout
		case <-w.ackDue:
			w.flushAck(ctx)
		case m := <-msgs:
			got = true
			if err := w.handle(ctx, m); err != nil {
				return got, err
			}
			quiet = w.env.after(watchHeartbeatTimeout)
		}
	}
}

// handle prints one stream message, moves the watch's position, and
// queues a group acknowledgement for it.
func (w *watcher) handle(ctx context.Context, m *chainmailv1.SubscribeResponse) error {
	if e := m.GetEvent(); e != nil {
		if err := w.printEvent(e); err != nil {
			return stopError{err}
		}
		if id := e.GetId(); id > 0 { // a trimmed gap has no ID
			w.seen(id)
		}
		w.queueAck(ctx, true)
		return nil
	}
	hb := m.GetHeartbeat()
	if hb == nil {
		return nil // an item this version does not know
	}
	if w.opts.heartbeat {
		if err := w.printHeartbeat(m); err != nil {
			return stopError{err}
		}
	}
	w.seen(hb.GetPosition())
	w.queueAck(ctx, false)
	if w.opts.once {
		return errWatchOnce
	}
	return nil
}

// seen raises the position the watch resumes after.
func (w *watcher) seen(pos int64) {
	if w.hasPos && pos <= w.pos {
		return
	}
	w.pos, w.hasPos = pos, true
}

// queueAck notes that a group may acknowledge the current position. line
// says a line was printed for it: 100 lines flush at once, and otherwise
// the first waiting line or heartbeat starts the batch's one-second timer.
func (w *watcher) queueAck(ctx context.Context, line bool) {
	if w.opts.group == "" || !w.hasPos || w.pos <= w.acked {
		return
	}
	if line {
		w.unacked++
	}
	if w.unacked >= watchAckBatch {
		w.flushAck(ctx)
		return
	}
	if w.ackDue == nil {
		w.ackDue = w.env.after(watchAckInterval)
	}
}

// flushAck acknowledges the current position for the group, if it is
// higher than the last acknowledgement. A failed Ack is dropped: the next
// batch covers it, and a reconnect resumes the group from what the daemon
// has, so delivery stays at least once.
func (w *watcher) flushAck(ctx context.Context) {
	w.ackDue = nil
	if w.opts.group == "" || !w.hasPos || w.pos <= w.acked {
		return
	}
	w.unacked = 0
	id := w.pos
	if _, err := w.watch.Ack(ctx, connect.NewRequest(&chainmailv1.AckRequest{Group: w.opts.group, Id: id})); err != nil {
		return
	}
	w.acked = id
}

// finalAck acknowledges what the watch printed since its last batch. It
// runs as the watch exits, after a signal too, so it gets a context of its
// own with a short deadline.
func (w *watcher) finalAck(ctx context.Context) {
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), watchFinalAckTimeout)
	defer cancel()
	w.flushAck(actx)
}

// printEvent writes one event: a JSON line, or one human line in a table.
func (w *watcher) printEvent(e *chainmailv1.WatchEvent) error {
	if !w.opts.table {
		return writeProto(w.env.Stdout, e)
	}
	return writeString(w.env.Stdout, eventLine(w.clock(e.GetAt()), e))
}

// printHeartbeat writes a heartbeat: the whole SubscribeResponse as a JSON
// line, so it reads {"heartbeat":{...}}, or one human line in a table.
func (w *watcher) printHeartbeat(m *chainmailv1.SubscribeResponse) error {
	if !w.opts.table {
		return writeProto(w.env.Stdout, m)
	}
	hb := m.GetHeartbeat()
	return writeString(w.env.Stdout, fmt.Sprintf("%s heartbeat position %d\n", w.clock(hb.GetAt()), hb.GetPosition()))
}

// clock is the time of day a line shows, in the display zone: ts, or now
// when ts is unset, as on a trimmed gap.
func (w *watcher) clock(ts *timestamppb.Timestamp) string {
	at := w.env.now()
	if ts != nil {
		at = ts.AsTime()
	}
	return at.In(w.env.location()).Format(time.TimeOnly)
}

// eventLine is one event as a terminal line: the time, kind, account and
// folder, then what happened. Subjects and folder names come from mail
// servers, so every value is cleaned for the terminal.
func eventLine(clock string, e *chainmailv1.WatchEvent) string {
	var what string
	switch e.GetKind() {
	case "new":
		what = clip(termText(e.GetMessage().GetSubject().GetUntrusted()), 60)
	case "flags":
		what = e.GetRef() + " " + strings.Join(slices.Concat(e.GetFlags(), e.GetLabels()), ",")
	case "status":
		what = e.GetState() + " " + e.GetDetail()
	case "gap":
		what = fmt.Sprintf("%s, next id %d", e.GetReason(), e.GetNextId())
	default:
		what = e.GetRef()
	}
	line := fmt.Sprintf("%s %-8s %s %s", clock, termText(e.GetKind()),
		orDash(termText(e.GetAccount())), orDash(termText(e.GetFolder())))
	if what = termText(what); what != "" {
		line += " " + what
	}
	return line + "\n"
}

func newWatchStatusCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show each synced folder's mode, state, last sync and last error",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Watch.GetWatchStatus(ctx, connect.NewRequest(&chainmailv1.GetWatchStatusRequest{}))
			if err != nil {
				return err
			}
			if g.table() {
				return watchStatusTable(env.Stdout, env.location(), resp.Msg)
			}
			return writeProto(env.Stdout, resp.Msg)
		},
	}
}

func newWatchGroupsCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "groups",
		Short: "List watch groups with their acknowledged event, lag and last use",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Watch.ListGroups(ctx, connect.NewRequest(&chainmailv1.ListGroupsRequest{}))
			if err != nil {
				return err
			}
			if g.table() {
				return watchGroupsTable(env.Stdout, env.location(), resp.Msg)
			}
			return writeProto(env.Stdout, resp.Msg)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a watch group, so its next use starts at the newest event",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c := g.client(env)
			defer c.Close()
			resp, err := c.Watch.DeleteGroup(ctx, connect.NewRequest(&chainmailv1.DeleteGroupRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			if resp.Msg.GetDeleted() {
				return writeString(env.Stdout, "deleted group "+termText(args[0])+"\n")
			}
			return writeString(env.Stdout, "no group named "+termText(args[0])+"\n")
		},
	})
	return cmd
}

// orDash shows an empty cell as "-", so a table never ends a line with padding.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// watchStatusTable prints one line per synced folder. Folder names come
// from mail servers, so every value is cleaned for the terminal.
func watchStatusTable(w io.Writer, loc *time.Location, r *chainmailv1.GetWatchStatusResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tFOLDER\tMODE\tSTATE\tCONDSTORE\tLAST SYNC\tLAST SWEEP\tERROR")
	for _, f := range r.GetFolders() {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%t\t%s\t%s\t%s\n",
			termText(f.GetAccount()), termText(f.GetFolder()), orDash(termText(f.GetMode())), orDash(termText(f.GetState())),
			f.GetCondstore(), orDash(shortDate(f.GetLastSyncAt(), loc)), orDash(shortDate(f.GetLastSweepAt(), loc)),
			orDash(termText(f.GetLastError())))
	}
	return tw.Flush()
}

// watchGroupsTable prints one line per group.
func watchGroupsTable(w io.Writer, loc *time.Location, r *chainmailv1.ListGroupsResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "NAME\tACKED\tLAG\tLAST SEEN")
	for _, gr := range r.GetGroups() {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n",
			termText(gr.GetName()), gr.GetAckedId(), gr.GetLag(), orDash(shortDate(gr.GetLastSeenAt(), loc)))
	}
	return tw.Flush()
}
