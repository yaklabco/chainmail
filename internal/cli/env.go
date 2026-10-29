package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/term"
)

// Env is everything the CLI reads or writes outside its arguments. Tests replace it.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv reads an environment variable.
	Getenv func(string) string
	// ReadPassword reads a line from the terminal without echo. Nil means
	// stdin is not a terminal, so secrets are read from stdin instead.
	ReadPassword func() ([]byte, error)
	// PassShow returns the output of `pass show path`.
	PassShow func(ctx context.Context, path string) ([]byte, error)
}

// DefaultEnv returns the real process environment.
func DefaultEnv() Env {
	env := Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, PassShow: passShow}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		env.ReadPassword = func() ([]byte, error) { return term.ReadPassword(fd) }
	}
	return env
}

// passShow reads a pass entry through a pipe. gpg asks for a passphrase
// through pinentry on the terminal, not on pass's stderr.
func passShow(ctx context.Context, path string) ([]byte, error) {
	return runCapture(ctx, "pass", "show", "--", path)
}

// runCapture runs a command and returns its stdout. Its stderr is captured
// into the error rather than passed through, so chainmail's own stderr stays
// one line of JSON.
func runCapture(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.Join(strings.Fields(stderr.String()), " ")
		if msg == "" {
			return nil, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return out, nil
}

// errNoPass is returned when Env has no way to run pass.
var errNoPass = errors.New("reading from pass is not available")
