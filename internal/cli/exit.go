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
type usageError struct {
	err  error
	hint string
}

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// exactArgs is cobra.ExactArgs, reported as a usage error.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return usageError{err: err}
		}
		return nil
	}
}

// classification is how a failure is reported: its exit code, reason and hint.
type classification struct {
	code   int
	reason string
	hint   string
}

// classify maps err to its exit code and reason. New reasons are added here only.
func classify(err error) classification {
	var ue usageError
	if errors.As(err, &ue) {
		return classification{code: exitUsage, reason: reasonUsage, hint: ue.hint}
	}
	return classification{code: exitInternal, reason: reasonInternal}
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
	body := errorBody{Reason: c.reason, Message: err.Error(), Hint: c.hint}
	_ = writeJSON(w, errorOutput{Error: body}) // stderr is the last resort; nothing else can report a failure here
	return c.code
}
