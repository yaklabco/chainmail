package cli

import (
	"context"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/jamesainslie/chainmail/internal/mcpserver"
	"github.com/jamesainslie/chainmail/internal/version"
)

const mcpHelp = `Serve chainmail's tools to an AI agent over the Model Context Protocol.

The agent's MCP client starts this command and speaks MCP on stdin and
stdout. Diagnostics go to stderr. Calls go to the daemon at --socket, then
$CHAINMAIL_SOCKET, then the default socket, and the audit log records them
with client kind mcp. The command starts and lists its tools even when the
daemon is down; calls then fail with DAEMON_UNREACHABLE.

Register it with Claude Code:
  claude mcp add --scope user chainmail -- /run/current-system/sw/bin/chainmail mcp`

func newMCPCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve chainmail's tools to an AI agent over MCP on stdio",
		Long:  mcpHelp,
		Args:  exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			dir, err := mcpserver.DefaultAttachmentsDir(env.Getenv)
			if err != nil {
				return usageError{err: err, hint: "set HOME or XDG_CACHE_HOME to an absolute path"}
			}
			logger := slog.New(slog.NewTextHandler(env.Stderr, nil))
			s := mcpserver.New(mcpserver.Options{
				Socket:         g.socketPath(env),
				Version:        version.Version,
				AttachmentsDir: dir,
				Logger:         logger,
			})
			runErr := s.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(env.Stdin), Writer: nopWriteCloser{env.Stdout}})
			if cause := ctx.Err(); cause != nil {
				logger.Info("session ended by a signal", "cause", cause)
				return nil
			}
			return runErr
		},
	}
}

// nopWriteCloser lets the MCP transport close stdout without closing it.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
