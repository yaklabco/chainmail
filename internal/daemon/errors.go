package daemon

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/vault"
)

var invalidArgument = []error{
	vault.ErrInvalidName, vault.ErrEmptyValue,
	auth.ErrNotOAuth, auth.ErrNoPendingLogin, auth.ErrLoginExpired,
	auth.ErrStateMismatch, auth.ErrConsentDenied, auth.ErrNoRefreshToken,
	errNULPassword,
}

// toConnectError maps a Go error to a Connect error with a stable reason.
func toConnectError(err error) *connect.Error {
	var (
		required *auth.RequiredError
		rejected *auth.RejectedError
		badName  *secretNameError
	)
	switch {
	case errors.As(err, &required):
		return api.New(connect.CodeUnauthenticated, api.ReasonAuthRequired, required.Hint, err)
	case errors.As(err, &rejected):
		return api.New(connect.CodeUnauthenticated, api.ReasonAuthRejected, rejected.Hint, err)
	case errors.Is(err, auth.ErrRejected):
		return api.New(connect.CodeUnauthenticated, api.ReasonAuthRejected, "", err)
	case errors.Is(err, config.ErrUnknownAccount):
		return api.New(connect.CodeNotFound, api.ReasonNotFound, "run: chainmail accounts list", err)
	case errors.Is(err, errInvalidAccountName):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument,
			"account names are 1 to 63 lowercase letters, digits and hyphens; run: chainmail accounts list", err)
	case errors.As(err, &badName):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, badName.hint, err)
	case isAny(err, invalidArgument):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "", err)
	case errors.Is(err, context.DeadlineExceeded):
		return api.New(connect.CodeDeadlineExceeded, api.ReasonTimeout, "", err)
	case errors.Is(err, context.Canceled):
		// The client gave up. TIMEOUT keeps the exit code contract without a new reason.
		return api.New(connect.CodeCanceled, api.ReasonTimeout, "", err)
	case errors.Is(err, auth.ErrTokenEndpoint):
		return api.New(connect.CodeUnavailable, api.ReasonServerUnavailable, "", err)
	default:
		return api.New(connect.CodeInternal, api.ReasonInternal, "", err)
	}
}

func isAny(err error, targets []error) bool {
	for _, target := range targets {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
