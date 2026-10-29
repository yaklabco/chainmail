package daemon

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/audit"
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
	setErr := a.s.checkSecretName(name)
	if setErr == nil {
		setErr = a.s.opts.Vault.Set(name, value)
		// A failed final directory sync still leaves the new value live, so an
		// error audit row for secret.set does not prove the old value is in use.
		if setErr == nil || a.s.secretIs(name, value) {
			a.s.auth.SecretChanged(name)
		}
	}
	if err := a.s.record(ctx, c, audit.Entry{Action: "secret.set", Targets: []string{name}}, setErr); err != nil {
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

// checkSecretName allows the password of a configured password account and
// the secret of a configured OAuth2 client. Refresh tokens are refused:
// only a login writes one, after the provider issued it.
func (s *Server) checkSecretName(name string) error {
	kind, rest, _ := strings.Cut(name, "/")
	cfg := s.opts.Config
	switch kind {
	case "accounts":
		if acct, err := cfg.Account(rest); err == nil && acct.Auth == provider.AuthPassword {
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
	account := req.Msg.GetAccount()
	ch, loginErr := a.s.auth.BeginLogin(account)
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
	account := req.Msg.GetAccount()
	loginErr := a.s.auth.FinishLogin(ctx, account, req.Msg.GetRedirectUrl())
	if err := a.s.record(ctx, c, audit.Entry{Account: account, Action: "auth.login.finish"}, loginErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.FinishAuthLoginResponse{}), nil
}

// GetStatus reports the daemon's version, start time, mode and secret names.
// It is read-only and returns no secret values, so it is deliberately not
// audited: status polling would otherwise flood the audit log.
func (a *adminService) GetStatus(_ context.Context, _ *connect.Request[chainmailv1.GetStatusRequest]) (*connect.Response[chainmailv1.GetStatusResponse], error) {
	return connect.NewResponse(&chainmailv1.GetStatusResponse{
		Version:     version.Version,
		StartedAt:   timestamppb.New(a.s.started),
		DevMode:     a.s.opts.DevMode,
		SecretNames: a.s.opts.Vault.Names(),
	}), nil
}
