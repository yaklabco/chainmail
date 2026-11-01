package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
)

// groupHint is how a user gets access to the daemon socket.
const groupHint = "this user must be in the chainmail-clients group: log in again after being added, or start the agent under sg chainmail-clients"

// errorBody is a failure as the agent sees it: in error results and in per-item errors.
type errorBody struct {
	Reason  string `json:"reason" jsonschema:"a stable reason code such as NOT_FOUND, POLICY_DENIED or INVALID_ARGUMENT"`
	Message string `json:"message" jsonschema:"what went wrong"`
	Hint    string `json:"hint,omitempty" jsonschema:"what to do next"`
}

// toolError is a failed tool call. Handlers return it as their error. The
// SDK makes it an isError result with Error() as the text, and
// structuredErrors adds {"error": body} as its structured content.
type toolError struct{ body errorBody }

func (e *toolError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.body.Reason, e.body.Message)
	if e.body.Hint != "" {
		fmt.Fprintf(&b, "\nhint: %s", e.body.Hint)
	}
	return b.String()
}

// errorResult maps an error to the tool error the agent sees. A *toolError
// passes through. A Connect error keeps the daemon's reason and hint, with
// the socket and group named when the daemon cannot be reached or refused us.
func (s *Server) errorResult(err error) error {
	var te *toolError
	if errors.As(err, &te) {
		return te
	}
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return &toolError{body: errorBody{Reason: api.ReasonInternal, Message: err.Error()}}
	}
	d := api.Detail(cerr)
	body := errorBody{Reason: d.GetReason(), Message: cerr.Message(), Hint: d.GetHint()}
	switch body.Reason {
	case api.ReasonDaemonUnreachable:
		body.Message = fmt.Sprintf("cannot reach the chainmail daemon at %s: %s", s.opts.Socket, cerr.Message())
		body.Hint = "check that the daemon runs (systemctl status chainmail); " + groupHint
	case api.ReasonForbidden:
		body.Hint = "the daemon at " + s.opts.Socket + " refused this user; " + groupHint
	}
	return &toolError{body: body}
}

// invalidArg is a tool error for input the schema accepts but chainmail does not.
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
