package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/client"
	"github.com/jamesainslie/chainmail/internal/ref"
)

// modeDraftSend is the keyInput mode of draft send.
const modeDraftSend = "draft-send"

const draftCreateHelp = `Save a new message to the account's Drafts folder without sending it.

It takes send's account, recipient, subject, body and attachment options
(not --draft, --dry-run or --idempotency-key); --account is required. The
ref it prints is what draft update, draft send and draft delete take.

Examples:
  chainmail draft create --account work --to ann@example.com --subject "Q3" --body-file q3.md
  chainmail draft send "$(chainmail draft create --account work --to ann@example.com --subject Hi --body Hi --output json | jq -r .draft.ref)"`

const draftUpdateHelp = `Replace a draft with a new version: the new version is saved and the old
one deleted. The new version has a new ref, which is printed; the old ref
is gone.

It takes send's recipient, subject, body and attachment options, without
--account (the draft's account is used) and without --draft, --dry-run or
--idempotency-key. The whole message is replaced, so give every recipient, the
subject and the body again. A reply saved as a draft stays a reply.

Example:
  chainmail draft update cm1:work:Drafts:7:12 --to ann@example.com --subject "Q3" --body-file q3.md`

const draftSendHelp = `Send a draft, then remove it from Drafts.

Running the same command again within 7 days reports the first send
instead of sending twice: without --idempotency-key, the key is derived
from the draft's ref.

Example:
  chainmail draft send cm1:work:Drafts:7:12`

// messageFlags are the flags of a whole new message: the account,
// recipients, subject and content. send, draft create and draft update
// share them.
type messageFlags struct {
	spec    chainmailv1.ComposeSpec
	content contentFlags
}

// addMessageFlags registers m's flags on cmd, with --account described by
// accountHelp. An empty accountHelp leaves --account out, for a command
// that takes the account from a ref and sets m.spec.Account itself.
func addMessageFlags(cmd *cobra.Command, m *messageFlags, accountHelp string) {
	f := cmd.Flags()
	if accountHelp != "" {
		f.StringVar(&m.spec.Account, "account", "", accountHelp)
	}
	f.StringArrayVar(&m.spec.To, "to", nil, `a recipient, "Name <a@b>" or "a@b" (repeatable)`)
	f.StringArrayVar(&m.spec.Cc, "cc", nil, "a copied recipient (repeatable)")
	f.StringArrayVar(&m.spec.Bcc, "bcc", nil, "a blind-copied recipient, kept out of the message itself (repeatable)")
	f.StringVar(&m.spec.Subject, "subject", "", "the subject line (required)")
	addContentFlags(cmd, &m.content)
}

// check refuses a message with no account, recipient, subject or body, or
// with bad content flags, without reading anything.
func (m *messageFlags) check() error {
	switch {
	case m.spec.GetAccount() == "":
		return m.content.usage("--account is required")
	case len(m.spec.GetTo())+len(m.spec.GetCc())+len(m.spec.GetBcc()) == 0:
		return m.content.usage("give at least one recipient: --to, --cc or --bcc")
	case !m.content.cmd.Flags().Changed("subject"):
		return m.content.usage("--subject is required")
	case !m.content.hasBody():
		return m.content.usage("give a body: --body, --body-file or --html-file")
	}
	return m.content.check()
}

// build reads the body, uploads the files for the spec's account and
// returns the spec with its Content set. --body - reads stdin.
func (m *messageFlags) build(ctx context.Context, c *client.Client, stdin io.Reader) (*chainmailv1.ComposeSpec, error) {
	m.content.account, m.content.stdin = m.spec.GetAccount(), stdin
	body, err := m.content.build(ctx, c)
	if err != nil {
		return nil, err
	}
	m.spec.Content = body
	return &m.spec, nil
}

// parseRef reads a ref argument. One that is not a chainmail ref is a
// usage error, before the daemon is asked.
func parseRef(cmd *cobra.Command, s string) (ref.Ref, error) {
	r, err := ref.Parse(s)
	if err != nil {
		return ref.Ref{}, usageError{err: err, hint: "refs come from search results and the draft commands; pass them unchanged, or " + helpHint(cmd)}
	}
	return r, nil
}

// draftSaved is the text output for a saved draft.
func draftSaved(d *chainmailv1.Draft) string {
	if d.GetRef() == "" {
		// The server stored the draft without saying where.
		return fmt.Sprintf("draft saved; search the Drafts folder for Message-ID <%s>\n", termText(d.GetMessageId()))
	}
	return fmt.Sprintf("draft saved %s\n", termText(d.GetRef()))
}

// writeDraft prints a saved draft: its ref on a terminal, else resp as JSON.
func writeDraft(env Env, g *globals, resp proto.Message, d *chainmailv1.Draft) error {
	if !g.table() {
		return writeProto(env.Stdout, resp)
	}
	if d == nil {
		return errors.New("the daemon returned no draft")
	}
	return writeString(env.Stdout, draftSaved(d))
}

// newDraftCmd is the draft group: create, update, send and delete.
func newDraftCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("draft", "Save, change, send and delete drafts")
	cmd.AddCommand(
		newDraftCreateCmd(ctx, env, g),
		newDraftUpdateCmd(ctx, env, g),
		newDraftSendCmd(ctx, env, g),
		newDraftDeleteCmd(ctx, env, g),
	)
	return cmd
}

func newDraftCreateCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var m messageFlags
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Save a new message to Drafts",
		Long:  draftCreateHelp,
		Args:  exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			if err := m.check(); err != nil {
				return err
			}
			c := g.client(env)
			defer c.Close()
			spec, err := m.build(ctx, c, env.Stdin)
			if err != nil {
				return err
			}
			resp, err := c.Compose.CreateDraft(ctx, connect.NewRequest(&chainmailv1.CreateDraftRequest{Spec: spec}))
			if err != nil {
				return err
			}
			return writeDraft(env, g, resp.Msg, resp.Msg.GetDraft())
		},
	}
	addMessageFlags(cmd, &m, "the account whose Drafts folder keeps the draft (required)")
	return cmd
}

func newDraftUpdateCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var m messageFlags
	cmd := &cobra.Command{
		Use:   "update <ref>",
		Short: "Replace a draft with a new version",
		Long:  draftUpdateHelp,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRef(cmd, args[0])
			if err != nil {
				return err
			}
			m.spec.Account = r.Account
			if err := m.check(); err != nil {
				return err
			}
			c := g.client(env)
			defer c.Close()
			spec, err := m.build(ctx, c, env.Stdin)
			if err != nil {
				return err
			}
			resp, err := c.Compose.UpdateDraft(ctx, connect.NewRequest(&chainmailv1.UpdateDraftRequest{Ref: r.String(), Spec: spec}))
			if err != nil {
				return err
			}
			return writeDraft(env, g, resp.Msg, resp.Msg.GetDraft())
		},
	}
	addMessageFlags(cmd, &m, "")
	return cmd
}

func newDraftSendCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var key string
	cmd := &cobra.Command{
		Use:   "send <ref>",
		Short: "Send a draft and remove it from Drafts",
		Long:  draftSendHelp,
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRef(cmd, args[0])
			if err != nil {
				return err
			}
			if key == "" {
				key = deriveKey(keyInput{account: r.Account, ref: r.String(), mode: modeDraftSend})
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Compose.SendDraft(ctx, connect.NewRequest(&chainmailv1.SendDraftRequest{Ref: r.String(), IdempotencyKey: key}))
			if err != nil {
				return err
			}
			return writeComposeResult(env, g, explicitJSON(cmd, g), resp.Msg, resp.Msg.GetResult())
		},
	}
	cmd.Flags().StringVar(&key, "idempotency-key", "", "the key that stops a repeat from sending again (default: derived from the ref)")
	return cmd
}

func newDraftDeleteCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <ref>",
		Short: "Delete a draft for good",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := parseRef(cmd, args[0])
			if err != nil {
				return err
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Compose.DeleteDraft(ctx, connect.NewRequest(&chainmailv1.DeleteDraftRequest{Ref: r.String()}))
			if err != nil {
				return err
			}
			if !g.table() {
				return writeProto(env.Stdout, resp.Msg)
			}
			return writeString(env.Stdout, "draft deleted "+r.String()+"\n")
		},
	}
}
