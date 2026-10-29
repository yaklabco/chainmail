package daemon

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
	"github.com/jamesainslie/chainmail/internal/audit"
	"github.com/jamesainslie/chainmail/internal/version"
)

type adminService struct {
	s *Server
}

func (a *adminService) SetSecret(ctx context.Context, req *connect.Request[chainmailv1.SetSecretRequest]) (*connect.Response[chainmailv1.SetSecretResponse], error) {
	name := req.Msg.GetName()
	setErr := a.s.opts.Vault.Set(name, req.Msg.GetValue())
	// Set can fail after the new value is already in use, when only the
	// directory sync fails, so the auth state is reset either way. Resetting
	// it for a value that was not stored costs nothing.
	a.s.auth.SecretChanged(name)
	if err := a.s.record(ctx, callerOf(ctx, req.Header()), audit.Entry{Action: "secret.set", Targets: []string{name}}, setErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.SetSecretResponse{}), nil
}

func (a *adminService) BeginAuthLogin(ctx context.Context, req *connect.Request[chainmailv1.BeginAuthLoginRequest]) (*connect.Response[chainmailv1.BeginAuthLoginResponse], error) {
	account := req.Msg.GetAccount()
	ch, loginErr := a.s.auth.BeginLogin(account)
	if err := a.s.record(ctx, callerOf(ctx, req.Header()), audit.Entry{Account: account, Action: "auth.login.begin"}, loginErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.BeginAuthLoginResponse{
		ConsentUrl: ch.URL,
		ExpiresAt:  timestamppb.New(ch.ExpiresAt),
	}), nil
}

func (a *adminService) FinishAuthLogin(ctx context.Context, req *connect.Request[chainmailv1.FinishAuthLoginRequest]) (*connect.Response[chainmailv1.FinishAuthLoginResponse], error) {
	account := req.Msg.GetAccount()
	loginErr := a.s.auth.FinishLogin(ctx, account, req.Msg.GetRedirectUrl())
	if err := a.s.record(ctx, callerOf(ctx, req.Header()), audit.Entry{Account: account, Action: "auth.login.finish"}, loginErr); err != nil {
		return nil, err
	}
	return connect.NewResponse(&chainmailv1.FinishAuthLoginResponse{}), nil
}

func (a *adminService) GetStatus(_ context.Context, _ *connect.Request[chainmailv1.GetStatusRequest]) (*connect.Response[chainmailv1.GetStatusResponse], error) {
	return connect.NewResponse(&chainmailv1.GetStatusResponse{
		Version:     version.Version,
		StartedAt:   timestamppb.New(a.s.started),
		DevMode:     a.s.opts.DevMode,
		SecretNames: a.s.opts.Vault.Names(),
	}), nil
}
