package daemon

import "github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"

// composeService sends mail and manages drafts. Calls not yet implemented
// answer CodeUnimplemented through the embedded handler.
type composeService struct {
	chainmailv1connect.UnimplementedComposeServiceHandler
	s *Server
}
