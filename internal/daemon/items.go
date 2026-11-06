package daemon

import (
	"context"
	"errors"
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

// itemListHint points at the list of items, where item IDs come from.
const itemListHint = "run: chainmail item list"

// actorOf is who a caller's item change is recorded as: its uid and client
// kind, as the audit log records them.
func actorOf(c caller) items.Actor {
	return items.Actor{UID: c.uid, Client: c.client}
}

// itemsErr is the error to report for a failed items store call. The
// store's refusals pass unchanged: their text is fixed and never holds
// item text. Anything else reads as fixed text and keeps its cause, so its
// reason still maps (TIMEOUT for a deadline, else INTERNAL) and the cause
// reaches the daemon's log.
func (s *Server) itemsErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*items.InvalidError](err); ok {
		return err
	}
	if errors.Is(err, items.ErrNotFound) || errors.Is(err, items.ErrKeyMismatch) {
		return err
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		s.opts.Logger.Error("an items store call failed", "err", err)
	}
	return &fixedTextError{msg: "the item store failed", err: err}
}
