package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/yaklabco/chainmail/internal/setup"
	"github.com/yaklabco/chainmail/internal/setup/system"
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
	// StdoutTerminal says stdout is a terminal, which makes table the default output.
	StdoutTerminal bool
	// StderrTerminal says stderr is a terminal: help and errors are then
	// styled for a person instead of written as one line of JSON.
	StderrTerminal bool
	// TZ is the time zone tables and text show dates in. Nil means UTC.
	TZ *time.Location
	// Now reads the clock, for times relative to now such as audit --since.
	// Nil means time.Now.
	Now func() time.Time
	// After waits for a duration, as time.After does, for long-running
	// commands such as watch. Nil means time.After.
	After func(time.Duration) <-chan time.Time

	// The fields below serve chainmail bootstrap. Nil means the real
	// implementation, except Sudo: nil means sudo is not available, so a
	// test that forgets it never runs the real one.

	// GOOS is the operating system bootstrap sets up. "" means runtime.GOOS.
	GOOS string
	// Getuid returns the real user id of the process.
	Getuid func() int
	// Username returns the name of the user running chainmail.
	Username func() (string, error)
	// Executable returns the path of the running binary with every
	// symlink resolved.
	Executable func() (string, error)
	// Sudo runs "sudo <args...>" with stdin and stdout given and stderr
	// passed through to Env.Stderr.
	Sudo func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error
	// Resolver looks up the MX records that pick a provider.
	Resolver setup.Resolver
	// ReadLine reads one line from the terminal. Nil when stdin is not a
	// terminal.
	ReadLine func() (string, error)
	// SetupFiles, SetupAccounts and SetupService let tests fake the OS in
	// bootstrap's unprivileged phase and in bootstrap apply.
	SetupFiles    system.Files
	SetupAccounts system.Accounts
	SetupService  system.Service
	// SetupUnits reads systemd's unit facts and SetupCreds runs
	// systemd-creds, on NixOS only.
	SetupUnits system.Units
	SetupCreds system.Creds
	// AsDaemon runs the installed binary as the daemon user. NixOS uses
	// DaemonRunnerFor instead.
	AsDaemon func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error
	// DaemonRunnerFor builds the runner that runs binary as the daemon
	// user. On NixOS bootstrap apply runs the store binary the plan names
	// through it, never the layout's binary. Nil means daemonRunner.
	DaemonRunnerFor func(binary string) func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error
	// OpenTTY opens the controlling terminal.
	OpenTTY func() (io.ReadWriteCloser, error)
	// AccountHealth asks the daemon at socket about account. An error
	// means it could not be asked.
	AccountHealth func(ctx context.Context, socket, account string) (*setup.AccountHealth, error)
	// DaemonStatus asks the daemon at socket for its admin status, giving
	// up as quickly as AccountHealth. An error means it could not be asked.
	DaemonStatus func(ctx context.Context, socket string) (DaemonInfo, error)
	// Stat and EvalSymlinks read the filesystem when bootstrap tells
	// whether two socket paths name the same file. Nil means os.Stat and
	// filepath.EvalSymlinks.
	Stat         func(path string) (fs.FileInfo, error)
	EvalSymlinks func(path string) (string, error)
}

// DaemonInfo is what bootstrap needs from a daemon's admin status.
type DaemonInfo struct {
	// Dev says the daemon runs in dev mode.
	Dev bool
	// Version is the daemon's version.
	Version string
}

// DefaultEnv returns the real process environment.
func DefaultEnv() Env {
	env := Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Getenv: os.Getenv, PassShow: passShow,
		StdoutTerminal: term.IsTerminal(int(os.Stdout.Fd())), StderrTerminal: term.IsTerminal(int(os.Stderr.Fd())),
		TZ: time.Local, Now: time.Now, After: time.After,
		Getuid: os.Getuid, Username: currentUsername, Executable: resolvedExecutable,
		Sudo: realSudo, Resolver: net.DefaultResolver,
		SetupFiles: system.OSFiles{}, SetupAccounts: system.NewAccounts(runtime.GOOS, system.ExecRunner),
		SetupUnits: system.NewUnits(system.ExecRunner), SetupCreds: system.NewCreds(system.ExecPipeRunner),
		OpenTTY: openTTY, AccountHealth: daemonAccountHealth, DaemonStatus: daemonStatus,
	}
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		env.ReadPassword = func() ([]byte, error) { return readTerminalPassword(fd) }
		env.ReadLine = func() (string, error) { return readTerminalLine(os.Stdin) }
	}
	return env
}

// stat is e.Stat, or os.Stat when it is nil.
func (e Env) stat() func(string) (fs.FileInfo, error) {
	if e.Stat == nil {
		return os.Stat
	}
	return e.Stat
}

// evalSymlinks is e.EvalSymlinks, or filepath.EvalSymlinks when it is nil.
func (e Env) evalSymlinks() func(string) (string, error) {
	if e.EvalSymlinks == nil {
		return filepath.EvalSymlinks
	}
	return e.EvalSymlinks
}

// location is the time zone for dates in tables and text.
func (e Env) location() *time.Location {
	if e.TZ == nil {
		return time.UTC
	}
	return e.TZ
}

// now is the time from e.Now, or time.Now when it is nil.
func (e Env) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

// after is e.After(d), or time.After(d) when e.After is nil.
func (e Env) after(d time.Duration) <-chan time.Time {
	if e.After == nil {
		return time.After(d)
	}
	return e.After(d)
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

// errInterrupted is returned when a signal ends a prompt.
var errInterrupted = errors.New("interrupted")

// readTerminalPassword reads a password without echo. An interrupt or
// SIGTERM ends the prompt at once with the terminal restored, instead of
// being swallowed until the user presses Enter.
func readTerminalPassword(fd int) ([]byte, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, fmt.Errorf("reading terminal state: %w", err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	return readInterruptible(
		func() ([]byte, error) { return term.ReadPassword(fd) },
		sig,
		func() { _ = term.Restore(fd, state) }, // best effort: the process is about to exit
	)
}

// readInterruptible runs read, or returns errInterrupted after calling
// restore if a signal arrives first. The abandoned read ends with the process.
func readInterruptible(read func() ([]byte, error), sig <-chan os.Signal, restore func()) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := read()
		done <- result{b, err}
	}()
	select {
	case r := <-done:
		return r.b, r.err
	case <-sig:
		restore()
		return nil, errInterrupted
	}
}
