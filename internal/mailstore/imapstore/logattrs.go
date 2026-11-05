package imapstore

import (
	"context"
	"errors"

	"github.com/emersion/go-imap/v2"

	"github.com/jamesainslie/chainmail/internal/mailstore"
)

// knownCodes are the response codes a log line may name. A code is an atom
// the server chooses, so only the ones the IMAP RFCs define are logged.
var knownCodes = map[imap.ResponseCode]bool{
	imap.ResponseCodeAlert: true, imap.ResponseCodeAlreadyExists: true,
	imap.ResponseCodeAuthenticationFailed: true, imap.ResponseCodeAuthorizationFailed: true,
	imap.ResponseCodeBadCharset: true, imap.ResponseCodeCannot: true, imap.ResponseCodeClientBug: true,
	imap.ResponseCodeContactAdmin: true, imap.ResponseCodeCorruption: true, imap.ResponseCodeExpired: true,
	imap.ResponseCodeHasChildren: true, imap.ResponseCodeInUse: true, imap.ResponseCodeLimit: true,
	imap.ResponseCodeNonExistent: true, imap.ResponseCodeNoPerm: true, imap.ResponseCodeOverQuota: true,
	imap.ResponseCodeParse: true, imap.ResponseCodePrivacyRequired: true, imap.ResponseCodeServerBug: true,
	imap.ResponseCodeTryCreate: true, imap.ResponseCodeUnavailable: true, imap.ResponseCodeUnknownCTE: true,
	imap.ResponseCodeTooMany: true, imap.ResponseCodeNoPrivate: true, imap.ResponseCodeTooBig: true,
}

// logAttrs returns attrs, then what a log line says about err: how it is
// classified and, for a server reply, its status and a known response code.
// The error's text is never logged: it can quote the server, which can echo
// anything it was sent, such as the password of a refused LOGIN.
func logAttrs(err error, attrs ...any) []any {
	attrs = append(attrs, "reason", logReason(err))
	var reply *imap.Error
	if errors.As(err, &reply) {
		attrs = append(attrs, "status", string(reply.Type))
		if knownCodes[reply.Code] {
			attrs = append(attrs, "code", string(reply.Code))
		}
	}
	return attrs
}

// logReason is err's class, from a fixed set.
func logReason(err error) string {
	var reply *imap.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, errCommandTimeout):
		return "command_timeout"
	case errors.Is(err, mailstore.ErrMalformed):
		return "malformed"
	case errors.Is(err, errConnLost):
		return "connection_lost"
	case errors.Is(err, mailstore.ErrStaleRef):
		return "stale_ref"
	case errors.Is(err, mailstore.ErrNotFound):
		return "not_found"
	case errors.Is(err, mailstore.ErrRefused), errors.As(err, &reply):
		return "refused"
	case errors.Is(err, mailstore.ErrUnavailable):
		return "unavailable"
	default:
		return "other"
	}
}
