package daemon

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
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
	target, testErr := accountTarget(req.Msg.GetAccount())
	var resp *chainmailv1.TestAccountResponse
	if testErr == nil {
		resp, testErr = a.test(ctx, target)
	}
	entry := audit.Entry{Account: target, Action: "account.test"}
	if testErr == nil {
		for _, check := range []*chainmailv1.Check{resp.GetImap(), resp.GetSmtp()} {
			if check.GetOk() {
				continue
			}
			entry.Result = audit.ResultError
			// A rejection parked the account, so it is the reason worth keeping.
			if entry.Reason == "" || check.GetReason() == api.ReasonAuthRejected {
				entry.Reason = check.GetReason()
			}
		}
	}
	if err := a.s.record(ctx, c, entry, testErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// ProbeLimits measures how many IMAP connections each account allows, then
// resizes its pool. Accounts are probed concurrently, and a failed probe
// fails only its account.
func (a *accountService) ProbeLimits(ctx context.Context, req *connect.Request[chainmailv1.ProbeLimitsRequest]) (*connect.Response[chainmailv1.ProbeLimitsResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	accts, err := a.s.probeTargets(req.Msg.GetAccounts())
	if err != nil {
		return nil, a.s.record(ctx, c, audit.Entry{Action: actionProbe}, err)
	}
	resp, err := a.s.probeLimits(ctx, c, accts)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (a *accountService) test(ctx context.Context, name string) (*chainmailv1.TestAccountResponse, error) {
	acct, err := a.s.opts.Config.Account(name)
	if err != nil {
		return nil, err
	}
	// One test per account at a time: a caller that waited sees the
	// account parked by a rejection instead of logging in with the same
	// bad password again.
	unlock, err := a.s.accountLocks.lock(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	creds, err := a.s.auth.Credentials(ctx, name)
	if err != nil {
		return nil, err
	}
	resp := &chainmailv1.TestAccountResponse{Account: name}
	resp.Imap = a.check(ctx, name, creds, func(ctx context.Context) ([]string, error) {
		sess, err := imapstore.Dial(ctx, acct.IMAP, creds, imapstore.DialOptions{RootCAs: a.s.opts.RootCAs})
		if err != nil {
			return nil, err
		}
		caps := sess.Capabilities()
		if err := sess.Close(); err != nil {
			a.s.opts.Logger.Warn("closing IMAP test session", "account", name, "reason", apiReason(err))
		}
		return caps, nil
	})
	if resp.Imap.GetReason() == api.ReasonAuthRejected {
		// Providers such as Hostinger count IMAP and SMTP failures together
		// toward a lockout, so a rejected login is not tried again.
		resp.Smtp = &chainmailv1.Check{Reason: api.ReasonAuthRejected, Error: "not attempted: the IMAP server rejected the same credentials"}
		return resp, nil
	}
	resp.Smtp = a.check(ctx, name, creds, func(ctx context.Context) ([]string, error) {
		return nil, sender.Probe(ctx, acct.SMTP, creds, sender.DialOptions{RootCAs: a.s.opts.RootCAs})
	})
	return resp, nil
}

// check runs one connection test with creds. A rejection marks the account
// as needing a human, so chainmail stops retrying credentials that could
// lock it out, unless a human replaced them while the test ran.
// Server trouble and timeouts say nothing about the credentials and leave the
// auth state alone.
func (a *accountService) check(ctx context.Context, account string, creds auth.Credentials, run func(context.Context) ([]string, error)) *chainmailv1.Check {
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
		a.s.auth.Rejected(account, creds)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// A canceled request is reported as TIMEOUT, as toConnectError does.
		c.Reason = api.ReasonTimeout
	default:
		c.Reason = api.ReasonServerUnavailable
	}
	return c
}

// invalidTarget stands in for a client-supplied name that failed
// validation. Such a name can be any bytes of any size, and the audit log
// is append-only, so it never records one. Parentheses fit neither name
// pattern, so it cannot be mistaken for a real name, and encoding/json
// leaves them unescaped.
const invalidTarget = "(invalid)"

var errInvalidAccountName = errors.New("invalid account name")

// accountTarget returns account for the audit log, or invalidTarget and an
// error when it cannot be an account name. The error leaves the name out.
func accountTarget(account string) (string, error) {
	if config.ValidName(account) {
		return account, nil
	}
	return invalidTarget, errInvalidAccountName
}

// keyedLock is a set of mutexes, one per key, that waiters can give up on.
// Keys are configured account names, so the set stays small.
type keyedLock struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

// lock waits until key is free or ctx ends. The caller must call unlock
// exactly once.
func (l *keyedLock) lock(ctx context.Context, key string) (unlock func(), err error) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]chan struct{}{}
	}
	ch, ok := l.locks[key]
	if !ok {
		ch = make(chan struct{}, 1)
		l.locks[key] = ch
	}
	l.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
