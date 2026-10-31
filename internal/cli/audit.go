package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

// newAuditCmd builds chainmail audit, which reads the daemon's audit log.
func newAuditCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var (
		since    time.Duration
		account  string
		action   string
		limit    int32
		beforeID int64
	)
	cmd := &cobra.Command{
		Use:   "audit [--since 24h] [--account NAME] [--action NAME] [--limit N] [--before-id N]",
		Short: "Show the daemon's audit log, newest first",
		Long: "Show the daemon's audit log, newest first. Rows hold no message content, recipients or secrets.\n\n" +
			"A page ends with next_before_id; pass it as --before-id for the older rows. It is 0 on the last page.",
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if since < 0 {
				return usageError{err: errors.New("--since must not be negative"), hint: helpHint(cmd)}
			}
			req := &chainmailv1.ReadAuditRequest{
				Since:    timestamppb.New(time.Now().Add(-since)),
				Account:  account,
				Action:   action,
				Limit:    limit,
				BeforeId: beforeID,
			}
			c := g.client(env)
			defer c.Close()
			resp, err := c.Admin.ReadAudit(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			if g.table() {
				return auditTable(env.Stdout, env.location(), resp.Msg)
			}
			return writeProto(env.Stdout, resp.Msg)
		},
	}
	cmd.Flags().DurationVar(&since, "since", 24*time.Hour, "how far back to look, as a Go duration such as 2h or 30m")
	cmd.Flags().StringVar(&account, "account", "", "only this account (default: all)")
	cmd.Flags().StringVar(&action, "action", "", "only this action, such as send or expunge (default: all)")
	cmd.Flags().Int32Var(&limit, "limit", 0, "rows per page, up to 1000 (default: the daemon's 100)")
	cmd.Flags().Int64Var(&beforeID, "before-id", 0, "only rows older than this id, from next_before_id")
	return cmd
}

// auditTable prints one line per entry. Every value came from a client at
// some point, so each is cleaned for the terminal.
func auditTable(w io.Writer, loc *time.Location, r *chainmailv1.ReadAuditResponse) error {
	tw := newTable(w)
	_, _ = fmt.Fprintln(tw, "AT\tACTION\tACCOUNT\tDECISION\tRESULT\tREASON\tTARGETS")
	for _, e := range r.GetEntries() {
		at := ""
		if ts := e.GetAt(); ts != nil {
			at = ts.AsTime().In(loc).Format("2006-01-02 15:04:05")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			at, termText(e.GetAction()), termText(e.GetAccount()), termText(e.GetDecision()),
			termText(e.GetResult()), termText(e.GetReason()), termText(strings.Join(e.GetTargets(), ",")))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if next := r.GetNextBeforeId(); next != 0 {
		return writeString(w, fmt.Sprintf("older rows: pass --before-id %d\n", next))
	}
	return nil
}
