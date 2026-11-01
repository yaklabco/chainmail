package mcpserver

import "github.com/modelcontextprotocol/go-sdk/mcp"

func boolPtr(b bool) *bool { return &b }

// hints are a tool's four annotations. Every tool sets all four explicitly,
// because some clients read a missing hint as its spec default.
type hints struct{ readOnly, destructive, idempotent, openWorld bool }

// addTool registers a typed tool with a title and all four annotations.
func addTool[In, Out any](s *Server, name, title, description string, h hints, fn mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Annotations: &mcp.ToolAnnotations{
			Title:           title,
			ReadOnlyHint:    h.readOnly,
			DestructiveHint: boolPtr(h.destructive),
			IdempotentHint:  h.idempotent,
			OpenWorldHint:   boolPtr(h.openWorld),
		},
	}, fn)
}
