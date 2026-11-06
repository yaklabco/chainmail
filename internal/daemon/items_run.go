package daemon

import (
	"context"
	"database/sql"
	"time"

	"github.com/jamesainslie/chainmail/internal/items"
	"github.com/jamesainslie/chainmail/internal/watch"
)

// itemMail is the sync engine's OnNew hook. Inside the batch's transaction
// it turns the batch's new messages on threads linked to live items into
// item_mail events, from_self against the account's own address. It uses
// only tx: state.db has one connection, which tx holds. The account comes
// from the config, in memory; one the config no longer has is an error,
// which rolls the batch back.
func (s *Server) itemMail(ctx context.Context, tx *sql.Tx, account string, news []watch.NewMail) ([]watch.Event, error) {
	acct, err := s.opts.Config.Account(account)
	if err != nil {
		return nil, err
	}
	return items.MailArrived(ctx, tx, items.MailAccount{Name: acct.Name, Address: acct.Address}, news, s.opts.Now().UTC())
}

// runItemsTrim trims the item tables every interval until ctx ends, once on
// entry too, as the event log's Run does: item_mail_seen rows older than
// the watch retention, and idempotency rows past items.RequestRetention. A
// failed trim is logged and retried at the next tick; one cut short by ctx
// is not logged. The ticks come from s.itemsTrimTick when a test sets it.
func (s *Server) runItemsTrim(ctx context.Context, interval, retention time.Duration) {
	tick := s.itemsTrimTick
	if tick == nil {
		if interval <= 0 {
			interval = time.Hour
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		if _, err := s.items.Trim(ctx, s.opts.Now().Add(-retention)); err != nil && ctx.Err() == nil {
			s.opts.Logger.ErrorContext(ctx, "trimming the item tables failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

var _ itemResolver = (*items.Store)(nil)
