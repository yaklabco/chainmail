package cli

import (
	"context"
	"io"
	"log/slog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/jamesainslie/chainmail/internal/api"
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
			runErr := s.Run(ctx, &mcp.IOTransport{Reader: closingReader(env.Stdin), Writer: nopWriteCloser{env.Stdout}})
			// Cause is nil while ctx runs, and once it ends names the signal
			// (signal.NotifyContext). A session error that raced the signal
			// is not dropped: Run logged its kind, never its text, unless it
			// was the cancellation itself.
			if cause := context.Cause(ctx); cause != nil {
				logger.Info("session ended by a signal", "cause", cause)
				return nil
			}
			if runErr != nil {
				// Run already logged the error's kind. Its text can hold what
				// the client sent, so it is not reported again here.
				return &exitError{reason: api.ReasonInternal, msg: "the mcp session ended with an error; its kind is in the warning above", hint: "check the MCP client's configuration and restart it"}
			}
			return nil
		},
	}
}

// closingReader is stdin for the MCP transport. When the session ends the
// SDK closes it, which closes stdin itself when it can be closed, such as
// os.Stdin or a pipe, so the SDK's reader goroutine is not left blocked in
// Read. A blocking os.Stdin may still hold that Read until the process
// exits, which follows Execute at once.
func closingReader(r io.Reader) io.ReadCloser {
	if rc, ok := r.(io.ReadCloser); ok {
		return rc
	}
	return io.NopCloser(r)
}

// nopWriteCloser lets the MCP transport close stdout without closing it.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
