package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/client"
)

const (
	// maxCallRefs is the most refs one organize call takes. More are sent
	// in consecutive calls of this size.
	maxCallRefs = 500
	// maxCallsInFlight is how many organize calls one command runs at once.
	maxCallsInFlight = 4
)

// outcomeCall sends one organize call for refs and returns its outcomes.
type outcomeCall func(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.RefOutcome, error)

// batchCmd describes one batch organize command.
type batchCmd struct {
	use   string
	short string
	// verb starts the summary line, as in "archived 12, failed 1".
	verb string
	// check refuses a bad invocation before any call, such as a missing --to.
	check func(cmd *cobra.Command) error
	call  outcomeCall
	// wrap builds the call's response from the outcomes of every chunk, for
	// JSON output.
	wrap func([]*chainmailv1.RefOutcome) proto.Message
}

// newOrganizeCmds returns the commands that move, copy, flag, label and
// delete messages, and the folder group.
func newOrganizeCmds(ctx context.Context, env Env, g *globals) []*cobra.Command {
	var moveTo, copyTo string
	var flagsAdd, flagsRemove, labelsAdd, labelsRemove []string
	needTo := func(to *string) func(*cobra.Command) error {
		return func(cmd *cobra.Command) error {
			if *to == "" {
				return usageError{err: errors.New("--to is required"), hint: helpHint(cmd)}
			}
			return nil
		}
	}
	move := newBatchCmd(ctx, env, g, batchCmd{
		use: "move <ref>... --to <folder>", short: "Move messages to a folder", verb: "moved", check: needTo(&moveTo),
		call: func(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.RefOutcome, error) {
			resp, err := c.Organize.Move(ctx, connect.NewRequest(&chainmailv1.MoveRequest{Refs: refs, Folder: moveTo}))
			return outcomes(resp, err)
		},
		wrap: func(o []*chainmailv1.RefOutcome) proto.Message { return &chainmailv1.MoveResponse{Outcomes: o} },
	})
	move.Flags().StringVar(&moveTo, "to", "", "the destination folder, on each message's own account (required)")
	cp := newBatchCmd(ctx, env, g, batchCmd{
		use: "copy <ref>... --to <folder>", short: "Copy messages to a folder", verb: "copied", check: needTo(&copyTo),
		call: func(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.RefOutcome, error) {
			resp, err := c.Organize.Copy(ctx, connect.NewRequest(&chainmailv1.CopyRequest{Refs: refs, Folder: copyTo}))
			return outcomes(resp, err)
		},
		wrap: func(o []*chainmailv1.RefOutcome) proto.Message { return &chainmailv1.CopyResponse{Outcomes: o} },
	})
	cp.Flags().StringVar(&copyTo, "to", "", "the destination folder, on each message's own account (required)")
	flags := newBatchCmd(ctx, env, g, batchCmd{
		use: "flags <ref>... [--add <flag>]... [--remove <flag>]...", short: `Add and remove flags such as \Seen, \Flagged and keywords`, verb: "updated",
		call: setFlags(&flagsAdd, &flagsRemove),
		wrap: func(o []*chainmailv1.RefOutcome) proto.Message { return &chainmailv1.SetFlagsResponse{Outcomes: o} },
	})
	flags.Flags().StringArrayVar(&flagsAdd, "add", nil, "a flag to add (repeatable)")
	flags.Flags().StringArrayVar(&flagsRemove, "remove", nil, "a flag to remove (repeatable)")
	label := newBatchCmd(ctx, env, g, batchCmd{
		use: "label <ref>... [--add <label>]... [--remove <label>]...", short: "Add and remove Gmail labels", verb: "labelled",
		call: func(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.RefOutcome, error) {
			req := &chainmailv1.SetLabelsRequest{Refs: refs, Add: labelsAdd, Remove: labelsRemove}
			resp, err := c.Organize.SetLabels(ctx, connect.NewRequest(req))
			return outcomes(resp, err)
		},
		wrap: func(o []*chainmailv1.RefOutcome) proto.Message { return &chainmailv1.SetLabelsResponse{Outcomes: o} },
	})
	label.Flags().StringArrayVar(&labelsAdd, "add", nil, "a label to add (repeatable)")
	label.Flags().StringArrayVar(&labelsRemove, "remove", nil, "a label to remove (repeatable)")
	return []*cobra.Command{
		move, cp,
		newBatchCmd(ctx, env, g, batchCmd{
			use: "archive <ref>...", short: "Move messages out of the inbox to the account's archive", verb: "archived",
			call: func(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.RefOutcome, error) {
				resp, err := c.Organize.Archive(ctx, connect.NewRequest(&chainmailv1.ArchiveRequest{Refs: refs}))
				return outcomes(resp, err)
			},
			wrap: func(o []*chainmailv1.RefOutcome) proto.Message { return &chainmailv1.ArchiveResponse{Outcomes: o} },
		}),
		newBatchCmd(ctx, env, g, batchCmd{
			use: "trash <ref>...", short: "Move messages to Trash", verb: "trashed",
			call: func(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.RefOutcome, error) {
				resp, err := c.Organize.Trash(ctx, connect.NewRequest(&chainmailv1.TrashRequest{Refs: refs}))
				return outcomes(resp, err)
			},
			wrap: func(o []*chainmailv1.RefOutcome) proto.Message { return &chainmailv1.TrashResponse{Outcomes: o} },
		}),
		newBatchCmd(ctx, env, g, batchCmd{
			use: "expunge <ref>...", short: "Delete messages for good", verb: "expunged",
			call: func(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.RefOutcome, error) {
				resp, err := c.Organize.Expunge(ctx, connect.NewRequest(&chainmailv1.ExpungeRequest{Refs: refs}))
				return outcomes(resp, err)
			},
			wrap: func(o []*chainmailv1.RefOutcome) proto.Message { return &chainmailv1.ExpungeResponse{Outcomes: o} },
		}),
		flagCmd(ctx, env, g, "flag", "Star messages: add \\Flagged", "flagged", []string{`\Flagged`}, nil),
		flagCmd(ctx, env, g, "unflag", "Unstar messages: remove \\Flagged", "unflagged", nil, []string{`\Flagged`}),
		flagCmd(ctx, env, g, "read", "Mark messages read: add \\Seen", "marked read", []string{`\Seen`}, nil),
		flagCmd(ctx, env, g, "unread", "Mark messages unread: remove \\Seen", "marked unread", nil, []string{`\Seen`}),
		flags, label,
		newFolderCmd(ctx, env, g),
	}
}

// setFlags is the SetFlags call with the flags *add and *remove hold when it runs.
func setFlags(add, remove *[]string) outcomeCall {
	return func(ctx context.Context, c *client.Client, refs []string) ([]*chainmailv1.RefOutcome, error) {
		req := &chainmailv1.SetFlagsRequest{Refs: refs, Add: *add, Remove: *remove}
		resp, err := c.Organize.SetFlags(ctx, connect.NewRequest(req))
		return outcomes(resp, err)
	}
}

// flagCmd is a command that adds and removes fixed flags, such as read.
func flagCmd(ctx context.Context, env Env, g *globals, name, short, verb string, add, remove []string) *cobra.Command {
	return newBatchCmd(ctx, env, g, batchCmd{
		use: name + " <ref>...", short: short, verb: verb,
		call: setFlags(&add, &remove),
		wrap: func(o []*chainmailv1.RefOutcome) proto.Message { return &chainmailv1.SetFlagsResponse{Outcomes: o} },
	})
}

// newBatchCmd builds a batch organize command. Its refs are its arguments,
// or one per line on stdin when the only argument is -.
func newBatchCmd(ctx context.Context, env Env, g *globals, b batchCmd) *cobra.Command {
	return &cobra.Command{
		Use:   b.use,
		Short: b.short,
		Long:  b.short + ". Refs come from the arguments, or one per line on stdin when the only argument is -.",
		Args:  minArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if b.check != nil {
				if err := b.check(cmd); err != nil {
					return err
				}
			}
			refs, err := refArgs(cmd, env.Stdin, args)
			if err != nil {
				return err
			}
			c := g.client(env)
			defer c.Close()
			outs, err := callChunked(ctx, refs, func(ctx context.Context, chunk []string) ([]*chainmailv1.RefOutcome, error) {
				return b.call(ctx, c, chunk)
			})
			if err != nil {
				return err
			}
			if g.table() {
				err = outcomesText(env.Stdout, b.verb, outs)
			} else {
				err = writeProto(env.Stdout, b.wrap(outs))
			}
			if err != nil {
				return err
			}
			return outcomesResult(outs)
		},
	}
}

// refArgs returns the command's refs: args, or the lines of stdin when args
// is just -. Blank lines and the space around a ref are dropped.
func refArgs(cmd *cobra.Command, stdin io.Reader, args []string) ([]string, error) {
	if !slices.Contains(args, "-") {
		return args, nil
	}
	if len(args) != 1 {
		return nil, usageError{err: errors.New("- reads refs from stdin and cannot be given with other refs"), hint: helpHint(cmd)}
	}
	var refs []string
	sc := bufio.NewScanner(stdin)
	for sc.Scan() {
		if r := strings.TrimSpace(sc.Text()); r != "" {
			refs = append(refs, r)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading refs from stdin: %w", err)
	}
	if len(refs) == 0 {
		return nil, usageError{err: errors.New("no refs on stdin"), hint: "pipe refs in one per line, for example: chainmail search ... -o json | jq -r '.rows[].ref'"}
	}
	return refs, nil
}

// callChunked sends refs in calls of at most maxCallRefs, up to
// maxCallsInFlight at once, and returns the outcomes in the order of refs.
// When every call fails it returns the first call's error. When only some
// fail, each ref of a failed call gets that error as its outcome, so the
// outcomes of the calls that worked are never lost.
func callChunked(ctx context.Context, refs []string, call func(context.Context, []string) ([]*chainmailv1.RefOutcome, error)) ([]*chainmailv1.RefOutcome, error) {
	chunks := slices.Collect(slices.Chunk(refs, maxCallRefs))
	results := make([][]*chainmailv1.RefOutcome, len(chunks))
	errs := make([]error, len(chunks))
	sem := make(chan struct{}, maxCallsInFlight)
	var wg sync.WaitGroup
	for i, chunk := range chunks {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			outs, err := call(ctx, chunk)
			if err == nil && len(outs) != len(chunk) {
				err = fmt.Errorf("the daemon answered %d of %d refs", len(outs), len(chunk))
			}
			results[i], errs[i] = outs, err
		})
	}
	wg.Wait()
	if !slices.ContainsFunc(errs, func(err error) bool { return err == nil }) {
		return nil, errs[0]
	}
	out := make([]*chainmailv1.RefOutcome, 0, len(refs))
	for i, chunk := range chunks {
		if errs[i] == nil {
			out = append(out, results[i]...)
			continue
		}
		c := classify(errs[i])
		for _, r := range chunk {
			out = append(out, &chainmailv1.RefOutcome{Ref: r, Error: &chainmailv1.ItemError{Reason: c.reason, Message: c.message, Hint: c.hint}})
		}
	}
	return out, nil
}

// outcomesText prints how many refs worked and failed, then one line per failure.
func outcomesText(w io.Writer, verb string, outs []*chainmailv1.RefOutcome) error {
	var b strings.Builder
	failed := 0
	for _, o := range outs {
		if o.GetError() != nil {
			failed++
		}
	}
	fmt.Fprintf(&b, "%s %d, failed %d\n", verb, len(outs)-failed, failed)
	for _, o := range outs {
		if e := o.GetError(); e != nil {
			fmt.Fprintf(&b, "%s: %s %s\n", o.GetRef(), e.GetReason(), e.GetMessage())
		}
	}
	return writeString(w, b.String())
}

// outcomesResult fails a batch with any failed ref, with the first failure's reason.
func outcomesResult(outs []*chainmailv1.RefOutcome) error {
	failed := 0
	var first *chainmailv1.ItemError
	for _, o := range outs {
		if e := o.GetError(); e != nil {
			failed++
			if first == nil {
				first = e
			}
		}
	}
	if first == nil {
		return nil
	}
	reason := first.GetReason()
	if reason == "" {
		reason = api.ReasonInternal
	}
	return &exitError{reason: reason, msg: fmt.Sprintf("%d of %d refs failed", failed, len(outs)), hint: first.GetHint()}
}

// newFolderCmd builds folder create, rename and delete.
func newFolderCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("folder", "Create, rename and delete folders")
	var account string
	var force bool
	cmd.PersistentFlags().StringVar(&account, "account", "", "the account the folder is on (required)")
	// run checks --account, makes the call and prints done on a terminal,
	// or the call's response as JSON.
	run := func(cmd *cobra.Command, done string, call func(*client.Client) (proto.Message, error)) error {
		if account == "" {
			return usageError{err: errors.New("--account is required"), hint: helpHint(cmd)}
		}
		c := g.client(env)
		defer c.Close()
		resp, err := call(c)
		if err != nil {
			return err
		}
		if g.table() {
			return writeString(env.Stdout, done+" on "+account+"\n")
		}
		return writeProto(env.Stdout, resp)
	}
	del := &cobra.Command{
		Use:   "delete <name> --account <account> [--force]",
		Short: "Delete a folder; INBOX and role folders are refused",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, "deleted folder "+args[0], func(c *client.Client) (proto.Message, error) {
				req := &chainmailv1.DeleteFolderRequest{Account: account, Name: args[0], Force: force}
				resp, err := c.Organize.DeleteFolder(ctx, connect.NewRequest(req))
				if err != nil {
					return nil, err
				}
				return resp.Msg, nil
			})
		},
	}
	del.Flags().BoolVar(&force, "force", false, "delete the folder even when it holds mail, and the mail with it")
	cmd.AddCommand(
		&cobra.Command{
			Use:   "create <name> --account <account>",
			Short: "Create a folder, and its parents when missing",
			Args:  exactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return run(cmd, "created folder "+args[0], func(c *client.Client) (proto.Message, error) {
					req := &chainmailv1.CreateFolderRequest{Account: account, Name: args[0]}
					resp, err := c.Organize.CreateFolder(ctx, connect.NewRequest(req))
					if err != nil {
						return nil, err
					}
					return resp.Msg, nil
				})
			},
		},
		&cobra.Command{
			Use:   "rename <from> <to> --account <account>",
			Short: "Rename a folder",
			Args:  exactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				return run(cmd, "renamed folder "+args[0]+" to "+args[1], func(c *client.Client) (proto.Message, error) {
					req := &chainmailv1.RenameFolderRequest{Account: account, From: args[0], To: args[1]}
					resp, err := c.Organize.RenameFolder(ctx, connect.NewRequest(req))
					if err != nil {
						return nil, err
					}
					return resp.Msg, nil
				})
			},
		},
		del,
	)
	return cmd
}

// outcomesMsg is a batch organize response.
type outcomesMsg interface {
	GetOutcomes() []*chainmailv1.RefOutcome
}

// outcomes returns a batch call's outcomes, or its error.
func outcomes[T any, PT interface {
	*T
	outcomesMsg
}](resp *connect.Response[T], err error) ([]*chainmailv1.RefOutcome, error) {
	if err != nil {
		return nil, err
	}
	return PT(resp.Msg).GetOutcomes(), nil
}
