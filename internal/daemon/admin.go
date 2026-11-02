package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/provider"
	"github.com/jamesainslie/chainmail/internal/vault"
	"github.com/jamesainslie/chainmail/internal/version"
)

type adminService struct {
	s *Server
}

func (a *adminService) SetSecret(ctx context.Context, req *connect.Request[chainmailv1.SetSecretRequest]) (*connect.Response[chainmailv1.SetSecretResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	name, value := req.Msg.GetName(), req.Msg.GetValue()
	target := name
	if !vault.ValidName(name) {
		target = invalidTarget
	}
	setErr := a.s.checkSecret(name, value)
	if setErr == nil {
		setErr = a.s.opts.Vault.Set(name, value)
		// A failed final directory sync still leaves the new value live, so an
		// error audit row for secret.set does not prove the old value is in use.
		if setErr == nil || a.s.secretIs(name, value) {
			a.s.auth.SecretChanged(name)
		}
	}
	if err := a.s.record(ctx, c, audit.Entry{Action: "secret.set", Targets: []string{target}}, setErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.SetSecretResponse{}), nil
}

// secretNameHint names the secrets SetSecret accepts.
const secretNameHint = "secret names are accounts/<account> for a password account, or oauth_clients/<client>, " +
	"as named in the config; run: chainmail accounts list"

// secretNameError refuses a secret name the config does not use. It
// matches vault.ErrInvalidName. Names are not secrets, so the message
// quotes the name, cut short.
type secretNameError struct {
	name string
	hint string
}

func (e *secretNameError) Error() string {
	return fmt.Sprintf("%v: %.128q is not a secret this config uses", vault.ErrInvalidName, e.name)
}

func (e *secretNameError) Unwrap() error { return vault.ErrInvalidName }

// errNULPassword never includes the value.
var errNULPassword = errors.New("the password contains a NUL byte, which no login can send")

// checkSecret allows the password of a configured password account and the
// secret of a configured OAuth2 client. Refresh tokens are refused: only a
// login writes one, after the provider issued it. A password may not hold a
// NUL byte, which SASL PLAIN uses as its separator.
func (s *Server) checkSecret(name string, value []byte) error {
	kind, rest, _ := strings.Cut(name, "/")
	cfg := s.opts.Config
	switch kind {
	case "accounts":
		if acct, err := cfg.Account(rest); err == nil && acct.Auth == provider.AuthPassword {
			if bytes.IndexByte(value, 0) >= 0 {
				return errNULPassword
			}
			return nil
		}
	case "oauth_clients":
		if _, ok := cfg.OAuthClients[rest]; ok {
			return nil
		}
	case "tokens":
		return &secretNameError{name: name, hint: "refresh tokens are stored by logging in; run: chainmail auth login <account>"}
	}
	return &secretNameError{name: name, hint: secretNameHint}
}

// secretIs reports whether the vault now serves value under name. Set can
// fail after the new value is already in use, when only the directory sync
// fails, and the auth state must then be reset as if it had succeeded. Any
// other failure leaves the old value, and rejected credentials stay parked.
func (s *Server) secretIs(name string, value []byte) bool {
	cur, err := s.opts.Vault.Get(name)
	return err == nil && len(value) > 0 && bytes.Equal(cur, value)
}

func (a *adminService) BeginAuthLogin(ctx context.Context, req *connect.Request[chainmailv1.BeginAuthLoginRequest]) (*connect.Response[chainmailv1.BeginAuthLoginResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	account, loginErr := accountTarget(req.Msg.GetAccount())
	var ch auth.Challenge
	if loginErr == nil {
		ch, loginErr = a.s.auth.BeginLogin(account)
	}
	if err := a.s.record(ctx, c, audit.Entry{Account: account, Action: "auth.login.begin"}, loginErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.BeginAuthLoginResponse{
		ConsentUrl: ch.URL,
		ExpiresAt:  timestamppb.New(ch.ExpiresAt),
	}), nil
}

func (a *adminService) FinishAuthLogin(ctx context.Context, req *connect.Request[chainmailv1.FinishAuthLoginRequest]) (*connect.Response[chainmailv1.FinishAuthLoginResponse], error) {
	c, err := callerOf(ctx, req.Header())
	if err != nil {
		return nil, err
	}
	account, loginErr := accountTarget(req.Msg.GetAccount())
	if loginErr == nil {
		loginErr = a.s.auth.FinishLogin(ctx, account, req.Msg.GetRedirectUrl())
	}
	if err := a.s.record(ctx, c, audit.Entry{Account: account, Action: "auth.login.finish"}, loginErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.FinishAuthLoginResponse{}), nil
}

// GetStatus reports the daemon's version, start time, mode, secret names
// and each synced folder's state. It is read-only and returns no secret
// values or message content, so it is deliberately not audited: status
// polling would otherwise flood the audit log.
func (a *adminService) GetStatus(ctx context.Context, _ *connect.Request[chainmailv1.GetStatusRequest]) (*connect.Response[chainmailv1.GetStatusResponse], error) {
	rows, err := a.s.watchState.Folders(ctx)
	if err != nil {
		a.s.opts.Logger.Error("sync state read failed", "err", err)
		return nil, toConnectError(&fixedTextError{msg: "the sync state could not be read", err: err})
	}
	folders := make([]*chainmailv1.FolderSyncStatus, 0, len(rows))
	for _, r := range rows {
		folders = append(folders, folderStatusProto(r))
	}
	return connect.NewResponse(&chainmailv1.GetStatusResponse{
		Version:     version.Version,
		StartedAt:   timestamppb.New(a.s.started),
		DevMode:     a.s.opts.DevMode,
		SecretNames: a.s.opts.Vault.Names(),
		Sync:        folders,
	}), nil
}

const (
	// defaultAuditLimit is the page size when a ReadAudit request sets none.
	defaultAuditLimit = 100
	// maxAuditLimit is the largest ReadAudit page.
	maxAuditLimit = 1000
	// defaultAuditWindow is how far back ReadAudit looks without a since.
	defaultAuditWindow = 24 * time.Hour
)

var (
	errAuditLimit = &argError{msg: "limit must be 0 to 1000", hint: "0 means 100"}
	// errAuditAccount has fixed text: the name is client-supplied bytes of any size.
	errAuditAccount = &argError{msg: "unknown account", hint: "leave account empty for all accounts; run: chainmail accounts list"}
	errAuditSince   = &argError{msg: "since is not a valid time", hint: "send a time between the years 1 and 9999"}
)

// ReadAudit returns audit rows newest first, paged by before_id. Like
// GetStatus it is read-only and deliberately not audited: reading the log
// would otherwise add to it on every poll. Audit rows hold no content,
// recipients or secrets, so any peer the Authorizer allows may read them.
func (a *adminService) ReadAudit(ctx context.Context, req *connect.Request[chainmailv1.ReadAuditRequest]) (*connect.Response[chainmailv1.ReadAuditResponse], error) {
	q, err := a.s.auditQuery(req.Msg)
	if err != nil {
		return nil, toConnectError(err)
	}
	rows, next, err := a.s.auditReader.Read(ctx, q)
	if err != nil {
		a.s.opts.Logger.Error("audit read failed", "err", err)
		return nil, toConnectError(&fixedTextError{msg: "the audit log could not be read", err: err})
	}
	resp := &chainmailv1.ReadAuditResponse{
		Entries:      make([]*chainmailv1.AuditEntry, 0, len(rows)),
		NextBeforeId: next,
	}
	for _, r := range rows {
		resp.Entries = append(resp.Entries, &chainmailv1.AuditEntry{
			Id:        r.ID,
			At:        timestamppb.New(r.At),
			CallerUid: r.CallerUID,
			Client:    r.Client,
			Account:   r.Account,
			Action:    r.Action,
			Targets:   r.Targets,
			Decision:  r.Decision,
			Result:    string(r.Result),
			Reason:    r.Reason,
		})
	}
	return connect.NewResponse(resp), nil
}

// auditQuery validates a ReadAudit request and fills its defaults.
func (s *Server) auditQuery(req *chainmailv1.ReadAuditRequest) (audit.Query, error) {
	limit := int(req.GetLimit())
	switch {
	case limit < 0 || limit > maxAuditLimit:
		return audit.Query{}, errAuditLimit
	case limit == 0:
		limit = defaultAuditLimit
	}
	if account := req.GetAccount(); account != "" {
		if _, ok := s.opts.Config.Accounts[account]; !ok {
			return audit.Query{}, errAuditAccount
		}
	}
	since := s.opts.Now().Add(-defaultAuditWindow)
	if ts := req.GetSince(); ts != nil {
		if ts.CheckValid() != nil {
			return audit.Query{}, errAuditSince
		}
		since = ts.AsTime()
	}
	return audit.Query{
		Since:    since,
		Account:  req.GetAccount(),
		Action:   req.GetAction(),
		Limit:    limit,
		BeforeID: req.GetBeforeId(),
	}, nil
}
