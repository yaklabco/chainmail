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
		ReasonCacheNotReady:      ExitUnavailable,
		ReasonSendOutcomeUnknown: ExitSendUnknown,
		ReasonTOTPInvalid:        ExitTOTP,
		ReasonTOTPLocked:         ExitTOTP,
		ReasonNotOrganizer:       ExitDenied,
		ReasonCalendarReadOnly:   ExitDenied,
		ReasonNoCalendarStore:    ExitUsage,
		ReasonEventConflict:      ExitUnavailable,
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

// TestCacheNotReadyIsStable pins the new reason's text, which agents match
// on, and its exit code: trying again later can succeed, as with
// SERVER_UNAVAILABLE.
func TestCacheNotReadyIsStable(t *testing.T) {
	t.Parallel()
	if ReasonCacheNotReady != "CACHE_NOT_READY" {
		t.Errorf("ReasonCacheNotReady = %q, want CACHE_NOT_READY", ReasonCacheNotReady)
	}
	if got := ExitCode("CACHE_NOT_READY"); got != 7 {
		t.Errorf("ExitCode(CACHE_NOT_READY) = %d, want 7", got)
	}
}

// TestCalendarReasonsAreStable pins the calendar reasons' text, which
// agents match on, and their exit codes. A caller who is not the organizer
// or writes to a read-only calendar is refused (3); an account without a
// calendar store is a usage mistake (2); an event changed since it was read
// can succeed after a fresh read (7).
func TestCalendarReasonsAreStable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		got, want string
		exit      int
	}{
		{ReasonNotOrganizer, "NOT_ORGANIZER", 3},
		{ReasonCalendarReadOnly, "CALENDAR_READ_ONLY", 3},
		{ReasonNoCalendarStore, "NO_CALENDAR_STORE", 2},
		{ReasonEventConflict, "EVENT_CONFLICT", 7},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("reason = %q, want %q", tc.got, tc.want)
		}
		if got := ExitCode(tc.want); got != tc.exit {
			t.Errorf("ExitCode(%s) = %d, want %d", tc.want, got, tc.exit)
		}
	}
}

// TestCalendarReasonsKeepTheirDetail checks that each calendar reason
// survives a round trip with the Connect code it is sent with. A
// PermissionDenied without a detail reads as FORBIDDEN, so the detail must
// win.
func TestCalendarReasonsKeepTheirDetail(t *testing.T) {
	t.Parallel()
	tests := map[string]connect.Code{
		ReasonNotOrganizer:     connect.CodeFailedPrecondition,
		ReasonCalendarReadOnly: connect.CodePermissionDenied,
		ReasonNoCalendarStore:  connect.CodeFailedPrecondition,
		ReasonEventConflict:    connect.CodeAborted,
	}
	for reason, code := range tests {
		err := New(code, reason, "hint", errors.New("x"))
		if err.Code() != code {
			t.Errorf("%s: code = %v, want %v", reason, err.Code(), code)
		}
		if got := Detail(err).GetReason(); got != reason {
			t.Errorf("%s: Detail reason = %s", reason, got)
		}
	}
}
