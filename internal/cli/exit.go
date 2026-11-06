package cli

import (
	"errors"
	"io"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/yaklabco/chainmail/internal/api"
)

// usageError marks a mistake in how chainmail was invoked.
type usageError struct {
	err  error
	hint string
}

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// exitError ends a command with a specific reason, often after it has
// already written its output.
type exitError struct {
	reason string
	msg    string
	hint   string
}

func (e *exitError) Error() string { return e.msg }

// helpHint points at the help for cmd.
func helpHint(cmd *cobra.Command) string {
	return "run: " + cmd.CommandPath() + " --help"
}

// exactArgs is cobra.ExactArgs, reported as a usage error.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return usageError{err: err, hint: helpHint(cmd)}
		}
		return nil
	}
}

// minArgs is cobra.MinimumNArgs, reported as a usage error.
func minArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.MinimumNArgs(n)(cmd, args); err != nil {
			return usageError{err: err, hint: helpHint(cmd)}
		}
		return nil
	}
}

// rangeArgs is cobra.RangeArgs, reported as a usage error.
func rangeArgs(lo, hi int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.RangeArgs(lo, hi)(cmd, args); err != nil {
			return usageError{err: err, hint: helpHint(cmd)}
		}
		return nil
	}
}

// classification is how a failure is reported: its exit code, reason, message and hint.
type classification struct {
	code    int
	reason  string
	message string
	hint    string
}

// classify maps err to its exit code and reason. New reasons are added here only.
func classify(err error) classification {
	var ue usageError
	if errors.As(err, &ue) {
		return classification{code: api.ExitUsage, reason: api.ReasonUsage, message: err.Error(), hint: ue.hint}
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return classification{code: api.ExitCode(ee.reason), reason: ee.reason, message: ee.msg, hint: ee.hint}
	}
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		d := api.Detail(cerr)
		return classification{code: api.ExitCode(d.GetReason()), reason: d.GetReason(), message: cerr.Message(), hint: d.GetHint()}
	}
	return classification{code: api.ExitInternal, reason: api.ReasonInternal, message: err.Error()}
}

type errorBody struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

type errorOutput struct {
	Error errorBody `json:"error"`
}

// reportError writes err to w as one line of JSON and returns its exit code.
func reportError(w io.Writer, err error) int {
	c := classify(err)
	body := errorBody{Reason: c.reason, Message: c.message, Hint: c.hint}
	_ = writeJSON(w, errorOutput{Error: body}) // stderr is the last resort; nothing else can report a failure here
	return c.code
}
