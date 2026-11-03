package cli

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
)

// The keyInput modes of reply and forward.
const (
	modeReply                = "reply"
	modeReplyAll             = "reply-all"
	modeForward              = "forward"
	modeForwardNoAttachments = "forward-no-attachments"
)

const replyHelp = `Reply to a message, quoting it under the new text.

The reply goes to the original's Reply-To, or else its From. --all adds
the original's To and Cc, less this account's own address. --to, --cc and
--bcc add more recipients. The account is the one the message is on. The
body options are as for send, and a body is required.

Running the same command again within 7 days reports the first reply
instead of sending twice: without --idempotency-key, the key is derived
from the ref, the recipients, the body and files, and --all. To send
again on purpose, pass a new --idempotency-key.

--draft saves the reply to Drafts instead of sending it. --dry-run writes
it as MIME to stdout, as send --dry-run does. Policy holds apply as for
send.`

const forwardHelp = `Forward a message to new recipients, with the original's attachments
unless --no-attachments.

The new text, if any, goes above the forwarded original. The account is
the one the message is on. The body and attachment options are as for
send. At least one of --to, --cc and --bcc is required.

Running the same command again within 7 days reports the first forward
instead of sending twice: without --idempotency-key, the key is derived
from the ref, the recipients, the body and files, and --no-attachments. To send
again on purpose, pass a new --idempotency-key.

--draft saves the forward to Drafts instead of sending it. --dry-run
writes it as MIME to stdout, as send --dry-run does. Policy holds apply
as for send.`

// answerFlags are the flags reply and forward share.
type answerFlags struct {
	to, cc, bcc   []string
	content       contentFlags
	dryRun, draft bool
	key           string
}

// addAnswerFlags registers a's flags on cmd, with --to described by toHelp.
func addAnswerFlags(cmd *cobra.Command, a *answerFlags, toHelp string) {
	f := cmd.Flags()
	f.StringArrayVar(&a.to, "to", nil, toHelp)
	f.StringArrayVar(&a.cc, "cc", nil, "a copied recipient (repeatable)")
	f.StringArrayVar(&a.bcc, "bcc", nil, "a blind-copied recipient, kept out of the message itself (repeatable)")
	addContentFlags(cmd, &a.content)
	f.BoolVar(&a.dryRun, "dry-run", false, "write the message as MIME to stdout instead of sending it")
	f.BoolVar(&a.draft, "draft", false, "save the message to Drafts instead of sending it")
	f.StringVar(&a.key, "idempotency-key", "", "the key that stops a repeat from sending again (default: derived from the message)")
}

// answerReq is what a reply or forward call needs beyond its own flags.
type answerReq struct {
	ref     string
	content *chainmailv1.Content
	key     string
}

// answerCall sends one reply or forward and returns its response and result.
type answerCall func(ctx context.Context, c *client.Client, req answerReq) (proto.Message, *chainmailv1.ComposeResult, error)

// run checks the ref and flags, reads the body, uploads the files for the
// ref's account, derives the key unless one was given or the call is a dry
// run or draft, makes the call and prints its result. mode tells apart
// derived keys for the same ref.
func (a *answerFlags) run(ctx context.Context, env Env, g *globals, cmd *cobra.Command, arg, mode string, call answerCall) error {
	r, err := parseRef(cmd, arg)
	if err != nil {
		return err
	}
	if a.dryRun && a.draft {
		return a.content.usage("--dry-run and --draft cannot be used together")
	}
	if err := a.content.check(); err != nil {
		return err
	}
	jsonOut := explicitJSON(cmd, g)
	if a.dryRun {
		if err := checkDryRunOutput(env, jsonOut); err != nil {
			return err
		}
	}
	c := g.client(env)
	defer c.Close()
	a.content.account, a.content.stdin = r.Account, env.Stdin
	content, err := a.content.build(ctx, c)
	if err != nil {
		return err
	}
	req := answerReq{ref: r.String(), content: content, key: a.key}
	if req.key == "" && !a.dryRun && !a.draft {
		req.key = deriveKey(keyInput{
			account: r.Account, to: a.to, cc: a.cc, bcc: a.bcc,
			content: a.content.digest, ref: req.ref, mode: mode,
		})
	}
	resp, res, err := call(ctx, c, req)
	if err != nil {
		return err
	}
	return writeComposeResult(env, g, jsonOut, resp, res)
}

func newReplyCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var (
		a   answerFlags
		all bool
	)
	cmd := &cobra.Command{
		Use:   "reply <ref>",
		Short: "Reply to a message, quoting it",
		Long:  replyHelp,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !a.content.hasBody() {
				return usageError{err: errors.New("give a body: --body, --body-file or --html-file"), hint: helpHint(cmd)}
			}
			mode := modeReply
			if all {
				mode = modeReplyAll
			}
			return a.run(ctx, env, g, cmd, args[0], mode, func(ctx context.Context, c *client.Client, req answerReq) (proto.Message, *chainmailv1.ComposeResult, error) {
				resp, err := c.Compose.Reply(ctx, connect.NewRequest(&chainmailv1.ReplyRequest{
					Ref: req.ref, All: all, Content: req.content, To: a.to, Cc: a.cc, Bcc: a.bcc,
					DryRun: a.dryRun, Draft: a.draft, IdempotencyKey: req.key,
				}))
				if err != nil {
					return nil, nil, err
				}
				return resp.Msg, resp.Msg.GetResult(), nil
			})
		},
	}
	addAnswerFlags(cmd, &a, `another recipient, added to the ones the reply goes to (repeatable)`)
	cmd.Flags().BoolVar(&all, "all", false, "reply to everyone: add the original's To and Cc, less this account's own address")
	return cmd
}

func newForwardCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var (
		a             answerFlags
		noAttachments bool
	)
	cmd := &cobra.Command{
		Use:   "forward <ref> --to <address>",
		Short: "Forward a message, with its attachments",
		Long:  forwardHelp,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(a.to)+len(a.cc)+len(a.bcc) == 0 {
				return usageError{err: errors.New("give at least one recipient: --to, --cc or --bcc"), hint: helpHint(cmd)}
			}
			mode := modeForward
			if noAttachments {
				mode = modeForwardNoAttachments
			}
			return a.run(ctx, env, g, cmd, args[0], mode, func(ctx context.Context, c *client.Client, req answerReq) (proto.Message, *chainmailv1.ComposeResult, error) {
				resp, err := c.Compose.Forward(ctx, connect.NewRequest(&chainmailv1.ForwardRequest{
					Ref: req.ref, To: a.to, Cc: a.cc, Bcc: a.bcc, Content: req.content, NoAttachments: noAttachments,
					DryRun: a.dryRun, Draft: a.draft, IdempotencyKey: req.key,
				}))
				if err != nil {
					return nil, nil, err
				}
				return resp.Msg, resp.Msg.GetResult(), nil
			})
		},
	}
	addAnswerFlags(cmd, &a, `a recipient, "Name <a@b>" or "a@b" (repeatable)`)
	cmd.Flags().BoolVar(&noAttachments, "no-attachments", false, "leave out the original's attachments")
	return cmd
}
