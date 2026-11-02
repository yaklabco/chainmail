package daemon

import (
	"time"

	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
)

// defaultWatchHeartbeat is how often an idle Subscribe stream sends a
// heartbeat when Options.WatchHeartbeat is zero. The CLI reconnects after
// 75 seconds without one.
const defaultWatchHeartbeat = 30 * time.Second

// watchService implements chainmailv1connect.WatchServiceHandler.
type watchService struct {
	chainmailv1connect.UnimplementedWatchServiceHandler
	s *Server
}

var _ chainmailv1connect.WatchServiceHandler = (*watchService)(nil)
