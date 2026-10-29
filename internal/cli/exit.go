package cli

import (
	"errors"
	"io"

	"github.com/spf13/cobra"
)

// Exit codes and reasons. They are a contract with agents: never renumber or rename them.
const (
	exitOK       = 0
	exitInternal = 1
	exitUsage    = 2

	reasonUsage    = "USAGE"
	reasonInternal = "INTERNAL"
)

// usageError marks a mistake in how chainmail was invoked.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// exactArgs is cobra.ExactArgs, reported as a usage error.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

func exitCode(err error) int {
	var ue usageError
	if errors.As(err, &ue) {
		return exitUsage
	}
	return exitInternal
}

type errorBody struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

type errorOutput struct {
	Error errorBody `json:"error"`
}

// writeError reports err on w as one line of JSON.
func writeError(w io.Writer, err error) {
	body := errorBody{Reason: reasonInternal, Message: err.Error()}
	var ue usageError
	if errors.As(err, &ue) {
		body.Reason = reasonUsage
	}
	_ = writeJSON(w, errorOutput{Error: body}) // stderr is the last resort; nothing else can report a failure here
}
