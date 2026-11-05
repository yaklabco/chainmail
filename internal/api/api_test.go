package api

import (
	"errors"
	"testing"

	"connectrpc.com/connect"
)

func TestExitCode(t *testing.T) {
	t.Parallel()
	tests := map[string]int{
		ReasonUsage:              ExitUsage,
		ReasonInvalidArgument:    ExitUsage,
		ReasonPolicyDenied:       ExitDenied,
		ReasonForbidden:          ExitDenied,
		ReasonHeld:               ExitHeld,
		ReasonAuthRequired:       ExitAuth,
		ReasonAuthRejected:       ExitAuth,
		ReasonNotFound:           ExitNotFound,
		ReasonStaleRef:           ExitNotFound,
		ReasonServerUnavailable:  ExitUnavailable,
		ReasonMalformedResponse:  ExitInternal,
		ReasonTimeout:            ExitUnavailable,
		ReasonDaemonUnreachable:  ExitUnavailable,
		ReasonSendOutcomeUnknown: ExitSendUnknown,
		ReasonTOTPInvalid:        ExitTOTP,
		ReasonTOTPLocked:         ExitTOTP,
		ReasonInternal:           ExitInternal,
		"SOMETHING_NEW":          ExitInternal,
	}
	for reason, want := range tests {
		if got := ExitCode(reason); got != want {
			t.Errorf("ExitCode(%s) = %d, want %d", reason, got, want)
		}
	}
}

func TestDetailRoundTrip(t *testing.T) {
	t.Parallel()
	err := New(connect.CodeUnauthenticated, ReasonAuthRequired, "run: chainmail auth login work", errors.New("boom"))
	d := Detail(err)
	if d.GetReason() != ReasonAuthRequired || d.GetHint() != "run: chainmail auth login work" {
		t.Errorf("Detail = %+v", d)
	}
}

func TestDetailWithoutErrorDetail(t *testing.T) {
	t.Parallel()
	tests := map[connect.Code]string{
		connect.CodePermissionDenied: ReasonForbidden,
		connect.CodeUnavailable:      ReasonDaemonUnreachable,
		connect.CodeDeadlineExceeded: ReasonTimeout,
		connect.CodeInternal:         ReasonInternal,
	}
	for code, want := range tests {
		if got := Detail(connect.NewError(code, errors.New("x"))).GetReason(); got != want {
			t.Errorf("code %v: reason = %s, want %s", code, got, want)
		}
	}
	if got := Detail(errors.New("plain")).GetReason(); got != ReasonInternal {
		t.Errorf("plain error reason = %s", got)
	}
}
