package cli

import (
	"io"
	"os"
)

// Env is everything the CLI reads or writes outside its arguments. Tests replace it.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Getenv reads an environment variable.
	Getenv func(string) string
}

// DefaultEnv returns the real process environment.
func DefaultEnv() Env {
	return Env{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv}
}
