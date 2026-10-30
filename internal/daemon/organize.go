package daemon

import "github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"

// organizeService moves, flags, labels and deletes messages, and manages
// folders. Calls not yet implemented answer CodeUnimplemented through the
// embedded handler.
type organizeService struct {
	chainmailv1connect.UnimplementedOrganizeServiceHandler
	s *Server
}
