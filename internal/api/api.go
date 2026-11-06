// Package api holds the contract shared by the daemon and its clients: error
// reasons, CLI exit codes and request headers. Agents depend on these values,
// so never change or reuse one.
package api

import (
	"errors"

	"connectrpc.com/connect"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
)

// HeaderClient names the kind of client, such as "cli" or "mcp", for the audit log.
const HeaderClient = "Chainmail-Client"

// Error reasons.
const (
	ReasonUsage              = "USAGE"
	ReasonInvalidArgument    = "INVALID_ARGUMENT"
	ReasonPolicyDenied       = "POLICY_DENIED"
	ReasonHeld               = "HELD"
	ReasonAuthRequired       = "AUTH_REQUIRED"
	ReasonAuthRejected       = "AUTH_REJECTED"
	ReasonNotFound           = "NOT_FOUND"
	ReasonStaleRef           = "STALE_REF"
	ReasonServerUnavailable  = "SERVER_UNAVAILABLE"
	ReasonTimeout            = "TIMEOUT"
	ReasonSendOutcomeUnknown = "SEND_OUTCOME_UNKNOWN"
	ReasonTOTPInvalid        = "TOTP_INVALID"
	ReasonTOTPLocked         = "TOTP_LOCKED"
	ReasonForbidden          = "FORBIDDEN"
	ReasonDaemonUnreachable  = "DAEMON_UNREACHABLE"
	ReasonInternal           = "INTERNAL"
	// ReasonMalformedResponse means the mail server sent a response
	// chainmail cannot read, such as a message in a form its parser does
	// not know. Asking again gets the same response.
	ReasonMalformedResponse = "MALFORMED_RESPONSE"
	// ReasonCacheNotReady means a search needs the local cache, which
	// cannot answer completely yet for the account, such as item: while
	// the cache is backfilling or stale. Trying again later can succeed.
	ReasonCacheNotReady = "CACHE_NOT_READY"
)

// CLI exit codes.
const (
	ExitOK          = 0
	ExitInternal    = 1
	ExitUsage       = 2
	ExitDenied      = 3
	ExitHeld        = 4
	ExitAuth        = 5
	ExitNotFound    = 6
	ExitUnavailable = 7
	ExitSendUnknown = 8
	ExitTOTP        = 9
)

// ExitCode maps an error reason to the CLI exit code.
func ExitCode(reason string) int {
	switch reason {
	case ReasonUsage, ReasonInvalidArgument:
		return ExitUsage
	case ReasonPolicyDenied, ReasonForbidden:
		return ExitDenied
	case ReasonHeld:
		return ExitHeld
	case ReasonAuthRequired, ReasonAuthRejected:
		return ExitAuth
	case ReasonNotFound, ReasonStaleRef:
		return ExitNotFound
	case ReasonServerUnavailable, ReasonTimeout, ReasonDaemonUnreachable, ReasonCacheNotReady:
		return ExitUnavailable
	case ReasonSendOutcomeUnknown:
		return ExitSendUnknown
	case ReasonTOTPInvalid, ReasonTOTPLocked:
		return ExitTOTP
	default:
		// INTERNAL, any reason not listed, and MALFORMED_RESPONSE, which
		// is not exit 7 because trying again later gets the same response.
		return ExitInternal
	}
}

// New builds a Connect error carrying an ErrorDetail.
func New(code connect.Code, reason, hint string, err error) *connect.Error {
	cerr := connect.NewError(code, err)
	// NewErrorDetail only fails for messages that can't be marshaled, and a
	// generated message always can. Without the detail, Detail falls back to the code.
	if detail, derr := connect.NewErrorDetail(&chainmailv1.ErrorDetail{Reason: reason, Hint: hint}); derr == nil {
		cerr.AddDetail(detail)
	}
	return cerr
}

// Detail extracts the ErrorDetail from err. When there is none, as when the
// daemon couldn't be reached or refused the socket peer, it derives one from the Connect code.
func Detail(err error) *chainmailv1.ErrorDetail {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return &chainmailv1.ErrorDetail{Reason: ReasonInternal}
	}
	for _, d := range cerr.Details() {
		v, verr := d.Value()
		if verr != nil {
			continue
		}
		if ed, ok := v.(*chainmailv1.ErrorDetail); ok {
			return ed
		}
	}
	switch cerr.Code() {
	case connect.CodePermissionDenied:
		return &chainmailv1.ErrorDetail{Reason: ReasonForbidden, Hint: "your user may not use this daemon socket; join its client group"}
	case connect.CodeUnavailable:
		return &chainmailv1.ErrorDetail{Reason: ReasonDaemonUnreachable, Hint: "is the chainmail daemon running? check --socket or CHAINMAIL_SOCKET"}
	case connect.CodeDeadlineExceeded:
		return &chainmailv1.ErrorDetail{Reason: ReasonTimeout}
	default:
		return &chainmailv1.ErrorDetail{Reason: ReasonInternal}
	}
}
