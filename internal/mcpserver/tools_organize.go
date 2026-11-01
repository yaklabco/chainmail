package mcpserver

import "context"

// registerOrganizeTools adds the organize and folder tools. Task 11 fills it in.
func (s *Server) registerOrganizeTools() {}

// refreshGate lists expunge and delete_folder when policy allows them on
// some account. Task 11 fills it in.
func (s *Server) refreshGate(context.Context) {}

// gateStale reports whether the last gate check is older than
// gateInterval. Task 11 fills it in.
func (s *Server) gateStale() bool { return false }
