package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/client"
)

func newAccountsCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("accounts", "List, test and probe configured accounts")
	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List accounts and whether they can log in",
			Args:  exactArgs(0),
			RunE: func(_ *cobra.Command, _ []string) error {
				c := g.client(env)
				defer c.Close()
				resp, err := c.Account.ListAccounts(ctx, connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
				if err != nil {
					return err
				}
				if g.table() {
					return accountsTable(env.Stdout, resp.Msg)
				}
				return writeProto(env.Stdout, resp.Msg)
			},
		},
		&cobra.Command{
			Use:   "test <account>",
			Short: "Log in to an account's IMAP and SMTP servers",
			Args:  exactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				c := g.client(env)
				defer c.Close()
				resp, err := c.Account.TestAccount(ctx, connect.NewRequest(&chainmailv1.TestAccountRequest{Account: args[0]}))
				if err != nil {
					return err
				}
				if g.table() {
					err = checksTable(env.Stdout, resp.Msg)
				} else {
					err = writeProto(env.Stdout, resp.Msg)
				}
				if err != nil {
					return err
				}
				return checkResult(resp.Msg)
			},
		},
		&cobra.Command{
			Use:   "probe [<account>]...",
			Short: "Measure how many IMAP connections accounts allow, and size their pools from it",
			Long: "Open IMAP connections to each account one at a time until the server refuses one, " +
				"then size the account's connection pool from the count. With no account, probe every account.",
			Args: cobra.ArbitraryArgs,
			RunE: func(_ *cobra.Command, args []string) error {
				c := g.client(env)
				defer c.Close()
				resp, err := c.Account.ProbeLimits(ctx, connect.NewRequest(&chainmailv1.ProbeLimitsRequest{Accounts: args}))
				if err != nil {
					return err
				}
				if g.table() {
					err = limitsTable(env.Stdout, env.location(), resp.Msg)
				} else {
					err = writeProto(env.Stdout, resp.Msg)
				}
				if err != nil {
					return err
				}
				return limitsResult(resp.Msg.GetLimits())
			},
		},
	)
	return cmd
}

// checkResult turns failed checks into an exit: 5 if a server rejected the
// credentials, otherwise the reason of the first failed check.
func checkResult(r *chainmailv1.TestAccountResponse) error {
	var reasons []string
	for _, c := range []*chainmailv1.Check{r.GetImap(), r.GetSmtp()} {
		if c.GetOk() {
			continue
		}
		// A failed check always carries a reason. If one ever does not, report
		// INTERNAL rather than an empty reason outside the contract.
		reason := c.GetReason()
		if reason == "" {
			reason = api.ReasonInternal
		}
		reasons = append(reasons, reason)
	}
	if len(reasons) == 0 {
		return nil
	}
	reason := reasons[0]
	if slices.Contains(reasons, api.ReasonAuthRejected) {
		reason = api.ReasonAuthRejected
	}
	return &exitError{reason: reason, msg: "account " + r.GetAccount() + " failed its connection test"}
}

// limitsResult fails a probe when any account's probe failed, with the
// first failure's reason.
func limitsResult(limits []*chainmailv1.AccountLimit) error {
	failed := 0
	var first *chainmailv1.ItemError
	for _, l := range limits {
		if e := l.GetError(); e != nil {
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
	return &exitError{reason: reason, msg: fmt.Sprintf("%d of %d probes failed", failed, len(limits)), hint: first.GetHint()}
}

// probeAccount asks the daemon to measure account's connection limit, as
// after new credentials are stored. A failed call comes back as the
// limit's error, so the caller can warn rather than fail.
func probeAccount(ctx context.Context, c *client.Client, account string) *chainmailv1.AccountLimit {
	resp, err := c.Account.ProbeLimits(ctx, connect.NewRequest(&chainmailv1.ProbeLimitsRequest{Accounts: []string{account}}))
	if err != nil {
		cl := classify(err)
		return &chainmailv1.AccountLimit{Account: account, Error: &chainmailv1.ItemError{Reason: cl.reason, Message: cl.message, Hint: cl.hint}}
	}
	for _, l := range resp.Msg.GetLimits() {
		if l.GetAccount() == account {
			return l
		}
	}
	return &chainmailv1.AccountLimit{Account: account, Error: &chainmailv1.ItemError{Reason: api.ReasonInternal, Message: "the daemon returned no probe result"}}
}

// reportProbe finishes a command that stored credentials for an account:
// on a terminal it prints done, then the probe's cap and pool or a warning
// on stderr; otherwise it writes fields as JSON with the probe result under
// "limit". A failed probe never fails the command.
func reportProbe(env Env, g *globals, done string, fields map[string]any, l *chainmailv1.AccountLimit) error {
	if !g.table() {
		raw, err := protoJSON.Marshal(l)
		if err != nil {
			return fmt.Errorf("encoding output: %w", err)
		}
		fields["limit"] = json.RawMessage(raw)
		return writeJSON(env.Stdout, fields)
	}
	if e := l.GetError(); e != nil {
		if err := writeString(env.Stdout, done); err != nil {
			return err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "warning: connection probe failed: %s: %s\n", termText(e.GetReason()), termText(e.GetMessage()))
		writeHint(&b, termText(e.GetHint()))
		// The command succeeded; a lost warning loses nothing else.
		_ = writeString(env.Stderr, b.String())
		return nil
	}
	return writeString(env.Stdout, fmt.Sprintf("%sconnections: cap %d, pool %d\n", done, l.GetImapCap(), l.GetPoolSize()))
}
