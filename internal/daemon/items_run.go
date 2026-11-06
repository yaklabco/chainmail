package daemon

import (
	"context"
	"database/sql"

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
