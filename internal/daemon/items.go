package daemon

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jamesainslie/chainmail/gen/chainmail/v1/chainmailv1connect"
	"github.com/jamesainslie/chainmail/internal/items"
)

// adminTimeout bounds creating the Admin items when the daemon starts.
const adminTimeout = 10 * time.Second

// itemsService implements chainmailv1connect.ItemsServiceHandler. Every
// call is audited, and no audit target, error message, hint or log line
// holds item text: only IDs, types, field names and counts. Error text
// reaches agents outside the untrusted wrapping, so it must never repeat
// what an agent copied from a message into an item.
type itemsService struct {
	// GetItem, LinkThread and UnlinkThread answer Unimplemented until the
	// link handlers land.
	chainmailv1connect.UnimplementedItemsServiceHandler
	s *Server
}

var _ chainmailv1connect.ItemsServiceHandler = (*itemsService)(nil)

// daemonActor is who the daemon's own item changes are recorded as: its
// uid and the client kind daemon.
func daemonActor() items.Actor {
	return items.Actor{UID: uint32(os.Getuid()), Client: daemonClient}
}

// ensureAdmin creates the two Admin items when they are missing. New runs
// it, so the items exist before any client connects. They are bootstrap
// rows: no event, no audit row. A second run changes nothing.
func (s *Server) ensureAdmin() error {
	ctx, cancel := context.WithTimeout(context.Background(), adminTimeout)
	defer cancel()
	if err := s.items.EnsureAdmin(ctx, daemonActor()); err != nil {
		return fmt.Errorf("daemon: creating the Admin items: %w", err)
	}
	return nil
}
