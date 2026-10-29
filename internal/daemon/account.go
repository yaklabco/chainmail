package daemon

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/mailstore/imapstore"
	"github.com/jamesainslie/chainmail/internal/sender"
)

type accountService struct {
	s *Server
}

func (a *accountService) ListAccounts(ctx context.Context, req *connect.Request[chainmailv1.ListAccountsRequest]) (*connect.Response[chainmailv1.ListAccountsResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	resp, listErr := a.list()
	if err := a.s.record(ctx, c, audit.Entry{Action: "accounts.list"}, listErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (a *accountService) list() (*chainmailv1.ListAccountsResponse, error) {
	cfg := a.s.opts.Config
	resp := &chainmailv1.ListAccountsResponse{}
	for _, name := range cfg.AccountNames() {
		acct, err := cfg.Account(name)
		if err != nil {
			return nil, err
		}
		st, err := a.s.auth.State(name)
		if err != nil {
			return nil, err
		}
		pb := &chainmailv1.Account{
			Name:       name,
			Provider:   acct.Provider.Name,
			Address:    acct.Address,
			AuthMethod: string(acct.Auth),
			AuthState:  chainmailv1.AuthState_AUTH_STATE_OK,
		}
		if st.Required {
			pb.AuthState = chainmailv1.AuthState_AUTH_STATE_REQUIRED
			pb.AuthDetail = st.Detail
		}
		resp.Accounts = append(resp.Accounts, pb)
	}
	return resp, nil
}

func (a *accountService) TestAccount(ctx context.Context, req *connect.Request[chainmailv1.TestAccountRequest]) (*connect.Response[chainmailv1.TestAccountResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetAccount()
	resp, testErr := a.test(ctx, name)
	entry := audit.Entry{Account: name, Action: "account.test"}
	if testErr == nil {
		for _, check := range []*chainmailv1.Check{resp.GetImap(), resp.GetSmtp()} {
			if check.GetOk() {
				continue
			}
			entry.Result = audit.ResultError
			// A rejection parked the account, so it is the reason worth keeping.
			if entry.Reason != api.ReasonAuthRejected {
				entry.Reason = check.GetReason()
			}
		}
	}
	if err := a.s.record(ctx, c, entry, testErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (a *accountService) test(ctx context.Context, name string) (*chainmailv1.TestAccountResponse, error) {
	acct, err := a.s.opts.Config.Account(name)
	if err != nil {
		return nil, err
	}
	creds, err := a.s.auth.Credentials(ctx, name)
	if err != nil {
		return nil, err
	}
	resp := &chainmailv1.TestAccountResponse{Account: name}
	resp.Imap = a.check(ctx, name, func(ctx context.Context) ([]string, error) {
		sess, err := imapstore.Dial(ctx, acct.IMAP, creds, imapstore.DialOptions{RootCAs: a.s.opts.RootCAs})
		if err != nil {
			return nil, err
		}
		caps := sess.Capabilities()
		if err := sess.Close(); err != nil {
			a.s.opts.Logger.Warn("closing IMAP test session", "account", name, "err", err)
		}
		return caps, nil
	})
	resp.Smtp = a.check(ctx, name, func(ctx context.Context) ([]string, error) {
		return nil, sender.Probe(ctx, acct.SMTP, creds, sender.DialOptions{RootCAs: a.s.opts.RootCAs})
	})
	return resp, nil
}

// check runs one connection test. A rejection marks the account as needing a
// human, so chainmail stops retrying credentials that could lock it out.
// Server trouble and timeouts say nothing about the credentials and leave the
// auth state alone.
func (a *accountService) check(ctx context.Context, account string, run func(context.Context) ([]string, error)) *chainmailv1.Check {
	start := a.s.opts.Now()
	caps, err := run(ctx)
	c := &chainmailv1.Check{
		Ok:           err == nil,
		Capabilities: caps,
		LatencyMs:    a.s.opts.Now().Sub(start).Milliseconds(),
	}
	if err == nil {
		return c
	}
	c.Error = err.Error()
	switch {
	case errors.Is(err, auth.ErrRejected):
		c.Reason = api.ReasonAuthRejected
		a.s.auth.Rejected(account)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// A canceled request is reported as TIMEOUT, as toConnectError does.
		c.Reason = api.ReasonTimeout
	default:
		c.Reason = api.ReasonServerUnavailable
	}
	return c
}
