package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
)

// groupHint is how a user gets access to the daemon socket.
const groupHint = "this user must be in the chainmail-clients group: log in again after being added, or start the agent under sg chainmail-clients"

// internalMessage is all the agent sees of an error that carries no
// approved public text. The stderr log names its kind (see errorKind).
const internalMessage = "internal error in chainmail mcp; see its stderr log"

// errorBody is a failure as the agent sees it: in error results and in per-item errors.
type errorBody struct {
	Reason  string `json:"reason" jsonschema:"a stable reason code such as NOT_FOUND, POLICY_DENIED or INVALID_ARGUMENT"`
	Message string `json:"message" jsonschema:"what went wrong"`
	Hint    string `json:"hint,omitempty" jsonschema:"what to do next"`
}

// toolError is a failed tool call. Handlers return it as their error. The
// SDK makes it an isError result with Error() as the text, and
// structuredErrors adds {"error": body} as its structured content.
//
// The body reaches the agent verbatim, outside the untrusted markers, so a
// toolError built in this package must hold only approved text: fixed
// strings and values from the agent's own input. Never embed text derived
// from a message (attachment names, subjects, addresses) or an error's
// Error(); pass such an error to errorResult instead.
type toolError struct{ body errorBody }

func (e *toolError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.body.Reason, e.body.Message)
	if e.body.Hint != "" {
		fmt.Fprintf(&b, "\nhint: %s", e.body.Hint)
	}
	return b.String()
}

// errorResult maps an error to the tool error the agent sees. Error text is
// deny by default: only approved public text reaches the agent.
//
//   - A *toolError passes through unchanged. Its text was written by this
//     package (see toolError and invalidArg).
//   - A Connect error that carries the daemon's api.ErrorDetail keeps its
//     message, reason and hint: the daemon chose that text for clients. A
//     FORBIDDEN refusal also names the socket and the group.
//   - A Connect Unavailable error without a detail is DAEMON_UNREACHABLE,
//     naming the socket and the group.
//   - Every other error, Connect or not, gets fixed text for its reason:
//     TIMEOUT for a deadline or a cancellation, FORBIDDEN for a detail-less
//     refusal, and otherwise INTERNAL with internalMessage. The stderr log
//     gets only errorKind, never the error's text: that can hold a secret,
//     a path or a name taken from mail.
//
// Handlers pass any error they get, from the daemon or from the filesystem,
// straight to errorResult. They never format one into a toolError or invalidArg.
func (s *Server) errorResult(err error) error {
	var te *toolError
	if errors.As(err, &te) {
		return te
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		if d, ok := daemonDetail(cerr); ok {
			body := errorBody{Reason: d.GetReason(), Message: cerr.Message(), Hint: d.GetHint()}
			if body.Reason == api.ReasonForbidden {
				body.Hint = s.forbiddenHint()
			}
			return &toolError{body: body}
		}
		if cerr.Code() == connect.CodeUnavailable {
			s.opts.Logger.Warn("the daemon is unreachable", "socket", s.opts.Socket, "kind", errorKind(err))
			return &toolError{body: errorBody{
				Reason:  api.ReasonDaemonUnreachable,
				Message: "cannot reach the chainmail daemon at " + s.opts.Socket,
				Hint:    "check that the daemon runs (systemctl status chainmail); " + groupHint,
			}}
		}
	}
	body := s.fallbackBody(err)
	if body.Reason == api.ReasonInternal {
		s.opts.Logger.Error("tool call failed", "kind", errorKind(err))
	} else {
		s.opts.Logger.Warn("tool call failed", "reason", body.Reason, "kind", errorKind(err))
	}
	return &toolError{body: body}
}

// fallbackBody is the fixed public text for an error with no approved text of its own.
func (s *Server) fallbackBody(err error) errorBody {
	var cerr *connect.Error
	isConnect := errors.As(err, &cerr)
	switch {
	case errors.Is(err, context.Canceled) || isConnect && cerr.Code() == connect.CodeCanceled:
		return errorBody{Reason: api.ReasonTimeout, Message: "the call was cancelled before the chainmail daemon answered"}
	case errors.Is(err, context.DeadlineExceeded) || isConnect && cerr.Code() == connect.CodeDeadlineExceeded:
		return errorBody{Reason: api.ReasonTimeout, Message: "the chainmail daemon did not answer in time", Hint: "retry the call"}
	case isConnect && cerr.Code() == connect.CodePermissionDenied:
		return errorBody{Reason: api.ReasonForbidden, Message: "the chainmail daemon refused this user", Hint: s.forbiddenHint()}
	default:
		return errorBody{Reason: api.ReasonInternal, Message: internalMessage, Hint: "this is a fault in chainmail, not in the request; tell James"}
	}
}

// forbiddenHint tells a refused user how to get access to the socket.
func (s *Server) forbiddenHint() string {
	return "the daemon at " + s.opts.Socket + " refused this user; " + groupHint
}

// daemonDetail returns the api.ErrorDetail the daemon attached to cerr, if
// any. Unlike api.Detail it does not derive one from the Connect code.
func daemonDetail(cerr *connect.Error) (*chainmailv1.ErrorDetail, bool) {
	for _, d := range cerr.Details() {
		v, err := d.Value()
		if err != nil {
			continue
		}
		if ed, ok := v.(*chainmailv1.ErrorDetail); ok && ed.GetReason() != "" {
			return ed, true
		}
	}
	return nil, false
}

// invalidArg is a tool error for input the schema accepts but chainmail does
// not. Its text reaches the agent verbatim, outside the untrusted markers,
// so format only fixed text and values from the agent's own input. Never
// embed text derived from a message (attachment names, subjects, addresses)
// or an error's Error(): pass such an error to errorResult instead.
func invalidArg(format string, args ...any) error {
	return &toolError{body: errorBody{Reason: api.ReasonInvalidArgument, Message: fmt.Sprintf(format, args...)}}
}

// itemError converts a per-item daemon error. Nil stays nil.
func itemError(e *chainmailv1.ItemError) *errorBody {
	if e == nil {
		return nil
	}
	return &errorBody{Reason: e.GetReason(), Message: e.GetMessage(), Hint: e.GetHint()}
}

// structuredErrors gives every error result of tools/call the structured
// content {"error": {...}}. The SDK cannot: it sets no structured content
// on a handler error and overwrites what a handler sets. Errors the SDK
// raised itself, such as arguments that fail the input schema, become
// INVALID_ARGUMENT.
func structuredErrors(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		r, ok := res.(*mcp.CallToolResult)
		if err != nil || !ok || r == nil || !r.IsError || r.StructuredContent != nil {
			return res, err
		}
		var te *toolError
		if !errors.As(r.GetError(), &te) {
			// The SDK's own error, such as a schema failure: give it a reason too.
			te = &toolError{body: errorBody{Reason: api.ReasonInvalidArgument, Message: textOf(r)}}
			r.Content = []mcp.Content{&mcp.TextContent{Text: te.Error()}}
		}
		r.StructuredContent = map[string]errorBody{"error": te.body}
		return r, nil
	}
}

// textOf joins a result's text content.
func textOf(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// errorKind describes err for the stderr log without its text, which can
// hold a secret, a backend's diagnostics, a path or a name taken from mail.
// It names the Connect code, the filesystem operation and the errno, or a
// context ending, and falls back to err's type.
func errorKind(err error) string {
	var parts []string
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		parts = append(parts, "connect "+cerr.Code().String())
	}
	var perr *fs.PathError
	if errors.As(err, &perr) {
		parts = append(parts, "fs "+perr.Op)
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		parts = append(parts, "errno "+errno.Error())
	}
	if len(parts) == 0 {
		switch {
		case errors.Is(err, context.Canceled):
			parts = append(parts, "canceled")
		case errors.Is(err, context.DeadlineExceeded):
			parts = append(parts, "deadline exceeded")
		default:
			parts = append(parts, fmt.Sprintf("%T", err))
		}
	}
	return strings.Join(parts, ", ")
}
