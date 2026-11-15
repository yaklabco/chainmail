package setuptest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Exit codes the fake CLI uses, as internal/api defines them.
const (
	exitInternal    = 1
	exitAuth        = 5
	exitUnavailable = 7
)

// Auth states the fake daemon reports, as protojson spells them.
const (
	AuthStateOK       = "AUTH_STATE_OK"
	AuthStateRequired = "AUTH_STATE_REQUIRED"
)

// ExitError is a failed run of the CLI as the daemon user, carrying its
// exit status the way *exec.ExitError does.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d: %s", e.Code, e.Msg) }

// ExitCode returns the exit status.
func (e *ExitError) ExitCode() int { return e.Code }

// DaemonCall is one run of the CLI as the daemon user.
type DaemonCall struct {
	Args   []string  // the whole argument list
	Stdin  []byte    // what secret set read from stdin; nil for other commands
	In     io.Reader // the stdin given
	Out    io.Writer // the stdout given
	Socket string    // the --socket value, "" when there was none
}

// Daemon fakes the chainmail CLI run as the daemon user against a running
// daemon: its Run method has the signature of setup.Applier.AsDaemon. It
// answers admin status, accounts list, accounts test, secret set and auth
// login with the JSON the real CLI prints.
type Daemon struct {
	// Service, when set, decides whether the daemon answers and what start
	// time admin status reports. A daemon whose service is not running is
	// unreachable. Nil means always running, started at the zero time.
	Service *Service
	// Socket, when set, is the --socket every call must pass.
	Socket string
	// Journal, when set, gets "daemon <command words>" for each call, with
	// the --socket and --output flags left out.
	Journal *Journal
	// StatusErrs are returned by the next admin status calls, in order,
	// before it answers, as a stale socket would fail.
	StatusErrs []error
	// TestExit is the exit status of accounts test; 0 passes.
	TestExit int
	// LoginErr makes auth login fail.
	LoginErr error

	mu      sync.Mutex
	states  map[string]string
	secrets map[string][]byte
	calls   []DaemonCall
}

// SetAuthState sets what accounts list reports as account's auth_state.
func (d *Daemon) SetAuthState(account, state string) *Daemon {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.states == nil {
		d.states = map[string]string{}
	}
	d.states[account] = state
	return d
}

// SetSecret stores a secret as if secret set had run.
func (d *Daemon) SetSecret(name string, value []byte) *Daemon {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.store(name, value)
	return d
}

// Secret returns the stored secret called name.
func (d *Daemon) Secret(name string) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.secrets[name]
	return bytes.Clone(v), ok
}

// Calls lists the runs so far.
func (d *Daemon) Calls() []DaemonCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

// Run runs the fake CLI with args.
func (d *Daemon) Run(_ context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	words, socket := commandWords(args)
	call := DaemonCall{Args: slices.Clone(args), In: stdin, Out: stdout, Socket: socket}
	cmd := strings.Join(words, " ")
	if len(words) >= 2 && words[0] == "secret" && words[1] == "set" && stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
		call.Stdin = data
	}
	d.calls = append(d.calls, call)
	d.Journal.Add("daemon " + cmd)

	if d.Socket != "" && socket != d.Socket {
		return &ExitError{Code: exitUnavailable, Msg: fmt.Sprintf("dial %q: no such file or directory", socket)}
	}
	if !d.reachable() {
		return &ExitError{Code: exitUnavailable, Msg: "dial " + socket + ": connect: connection refused"}
	}
	switch {
	case cmd == "admin status":
		return d.status(stdout)
	case cmd == "accounts list":
		return d.list(stdout)
	case len(words) == 3 && words[0] == "secret" && words[1] == "set":
		value := bytes.TrimRight(call.Stdin, "\r\n")
		if len(value) == 0 {
			return &ExitError{Code: 2, Msg: "secret is empty"}
		}
		d.store(words[2], value)
		return writeJSON(stdout, map[string]any{"name": words[2], "set": true})
	case len(words) == 3 && words[0] == "accounts" && words[1] == "test":
		return d.test(stdout, words[2])
	case len(words) == 3 && words[0] == "auth" && words[1] == "login":
		if d.LoginErr != nil {
			return d.LoginErr
		}
		d.setState(words[2], AuthStateOK)
		return nil
	default:
		return &ExitError{Code: 2, Msg: "unknown command " + cmd}
	}
}

// commandWords drops the --socket and --output flags from args and
// returns the rest and the --socket value.
func commandWords(args []string) (words []string, socket string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case (a == "--socket" || a == "--output") && i+1 < len(args):
			if a == "--socket" {
				socket = args[i+1]
			}
			i++
		case strings.HasPrefix(a, "--socket="):
			socket = strings.TrimPrefix(a, "--socket=")
		case strings.HasPrefix(a, "--output="):
		default:
			words = append(words, a)
		}
	}
	return words, socket
}

// reachable reports whether the daemon answers. The caller holds mu.
func (d *Daemon) reachable() bool {
	if d.Service == nil {
		return true
	}
	running, _ := d.Service.Running()
	return running
}

// status answers admin status. The caller holds mu.
func (d *Daemon) status(w io.Writer) error {
	if len(d.StatusErrs) > 0 {
		err := d.StatusErrs[0]
		d.StatusErrs = d.StatusErrs[1:]
		return err
	}
	var started time.Time
	if d.Service != nil {
		_, started = d.Service.Running()
	}
	names := slices.Sorted(maps.Keys(d.secrets))
	if names == nil {
		names = []string{}
	}
	return writeJSON(w, map[string]any{
		"version":      "test",
		"started_at":   started.UTC().Format(time.RFC3339Nano),
		"dev_mode":     false,
		"secret_names": names,
		"sync":         []any{},
		"cache":        []any{},
	})
}

// list answers accounts list. The caller holds mu.
func (d *Daemon) list(w io.Writer) error {
	accounts := []map[string]any{}
	for _, name := range slices.Sorted(maps.Keys(d.states)) {
		accounts = append(accounts, map[string]any{"name": name, "auth_state": d.states[name]})
	}
	return writeJSON(w, map[string]any{"accounts": accounts})
}

// test answers accounts test. The caller holds mu.
func (d *Daemon) test(w io.Writer, account string) error {
	ok := d.TestExit == 0
	check := map[string]any{"ok": ok}
	if !ok {
		reason := "SERVER_UNAVAILABLE"
		if d.TestExit == exitAuth {
			reason = "AUTH_REJECTED"
		}
		check["reason"] = reason
	}
	if err := writeJSON(w, map[string]any{"account": account, "imap": check, "smtp": check}); err != nil {
		return err
	}
	if !ok {
		return &ExitError{Code: d.TestExit, Msg: "account " + account + " failed its connection test"}
	}
	return nil
}

// store keeps a secret. The caller holds mu.
func (d *Daemon) store(name string, value []byte) {
	if d.secrets == nil {
		d.secrets = map[string][]byte{}
	}
	d.secrets[name] = bytes.Clone(value)
}

// setState records an auth state. The caller holds mu.
func (d *Daemon) setState(account, state string) {
	if d.states == nil {
		d.states = map[string]string{}
	}
	d.states[account] = state
}

// writeJSON writes v as one line of JSON. A nil w discards it.
func writeJSON(w io.Writer, v any) error {
	if w == nil {
		return nil
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return &ExitError{Code: exitInternal, Msg: "writing output: " + err.Error()}
	}
	return nil
}
