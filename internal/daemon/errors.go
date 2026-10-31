package daemon

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/jamesainslie/chainmail/internal/api"
	"github.com/jamesainslie/chainmail/internal/auth"
	"github.com/jamesainslie/chainmail/internal/config"
	"github.com/jamesainslie/chainmail/internal/mailstore"
	"github.com/jamesainslie/chainmail/internal/query"
	"github.com/jamesainslie/chainmail/internal/ref"
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
		required  *auth.RequiredError
		rejected  *auth.RejectedError
		badName   *secretNameError
		badArg    *argError
		denied    *deniedError
		syntaxErr *query.SyntaxError
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
	case errors.As(err, &badArg):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, badArg.hint, err)
	case errors.As(err, &denied):
		return api.New(connect.CodePermissionDenied, api.ReasonPolicyDenied, denied.hint, err)
	case errors.As(err, &syntaxErr):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, queryHint, err)
	case errors.Is(err, mailstore.ErrUnsupportedQuery):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "drop the operator, or search a Google account", err)
	// A write's error can carry the mail server's text, so the client sees
	// fixed text. The cause stays on the error for the daemon's logs.
	case errors.Is(err, mailstore.ErrUnsupported):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "this account does not support that action",
			&fixedTextError{msg: "the account does not support that action", err: err})
	case errors.Is(err, mailstore.ErrRefused):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "that folder cannot be changed that way",
			&fixedTextError{msg: "the mail server refused the change", err: err})
	case errors.Is(err, mailstore.ErrExists):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "a folder with that name exists",
			&fixedTextError{msg: "the folder already exists", err: err})
	case errors.Is(err, ref.ErrInvalid):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "refs come from search results; pass them unchanged", err)
	case errors.Is(err, ref.ErrInvalidCursor):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "pass next_cursor from the previous page of the same search unchanged", err)
	case isAny(err, invalidArgument):
		return api.New(connect.CodeInvalidArgument, api.ReasonInvalidArgument, "", err)
	case errors.Is(err, mailstore.ErrStaleRef):
		return api.New(connect.CodeNotFound, api.ReasonStaleRef, "search again", err)
	case errors.Is(err, mailstore.ErrNotFound):
		return api.New(connect.CodeNotFound, api.ReasonNotFound, "", err)
	case errors.Is(err, context.DeadlineExceeded):
		return api.New(connect.CodeDeadlineExceeded, api.ReasonTimeout, "", err)
	case errors.Is(err, context.Canceled):
		// The client gave up. TIMEOUT keeps the exit code contract without a new reason.
		return api.New(connect.CodeCanceled, api.ReasonTimeout, "", err)
	case errors.Is(err, auth.ErrTokenEndpoint), errors.Is(err, mailstore.ErrUnavailable):
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

// argError is a bad request argument, with a hint saying what to send instead.
type argError struct {
	msg  string
	hint string
}

func (e *argError) Error() string { return e.msg }

// deniedError is a call the policy refuses, with a hint naming the setting
// that allows it.
type deniedError struct {
	msg  string
	hint string
}

func (e *deniedError) Error() string { return e.msg }

// queryHint points at the query language reference.
const queryHint = "the query language is described in: chainmail search --help"
