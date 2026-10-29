package cli

import (
	"context"
	"slices"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
)

func newAccountsCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("accounts", "List and test configured accounts")
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
