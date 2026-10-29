package daemon

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	chainmailv1 "github.com/jamesainslie/chainmail/gen/chainmail/v1"
)

type accountService struct {
	s *Server
}

func (a *accountService) ListAccounts(context.Context, *connect.Request[chainmailv1.ListAccountsRequest]) (*connect.Response[chainmailv1.ListAccountsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ListAccounts arrives in the next task"))
}

func (a *accountService) TestAccount(context.Context, *connect.Request[chainmailv1.TestAccountRequest]) (*connect.Response[chainmailv1.TestAccountResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("TestAccount arrives in the next task"))
}
