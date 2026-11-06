package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

const loginInstructions = `Open this URL in a browser and approve access:

  %s

The browser will then fail to load a page on 127.0.0.1. That is expected.
Copy the full URL from the address bar, paste it here and press Enter.
The link expires at %s.
`

func newAuthCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	cmd := newGroup("auth", "Log in to OAuth2 accounts")
	cmd.AddCommand(&cobra.Command{
		Use:   "login <account>",
		Short: "Run the OAuth2 consent flow for an account",
		Args:  exactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			account := args[0]
			c := g.client(env)
			defer c.Close()
			begin, err := c.Admin.BeginAuthLogin(ctx, connect.NewRequest(&chainmailv1.BeginAuthLoginRequest{Account: account}))
			if err != nil {
				return err
			}
			expires := begin.Msg.GetExpiresAt().AsTime().Local().Format("15:04 MST")
			_, _ = fmt.Fprintf(env.Stderr, loginInstructions, begin.Msg.GetConsentUrl(), expires) // if stderr is gone the user can't see the URL; the login then expires unused
			line, err := readLine(ctx, env.Stdin)
			if err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("reading the redirect URL: %w", err)
			}
			redirect := strings.TrimSpace(line)
			if redirect == "" {
				return usageError{err: errors.New("no redirect URL was pasted"), hint: "run: chainmail auth login " + account + " again"}
			}
			finish := connect.NewRequest(&chainmailv1.FinishAuthLoginRequest{Account: account, RedirectUrl: redirect})
			if _, err := c.Admin.FinishAuthLogin(ctx, finish); err != nil {
				return err
			}
			// New credentials: measure the account's connection limit now.
			limit := probeAccount(ctx, c, account)
			return reportProbe(env, g, "logged in to "+account+"\n", map[string]any{"account": account, "logged_in": true}, limit)
		},
	})
	return cmd
}

// readLine reads one line from r, or gives up when ctx ends, so Ctrl-C at a
// prompt returns at once. The abandoned read ends with the process.
func readLine(ctx context.Context, r io.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		done <- result{line, err}
	}()
	select {
	case res := <-done:
		return res.line, res.err
	case <-ctx.Done():
		return "", fmt.Errorf("%w: %w", errInterrupted, ctx.Err())
	}
}
