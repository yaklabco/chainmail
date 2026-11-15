package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/setup"
	"github.com/yaklabco/chainmail/internal/setup/setuptest"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// bootSecret is the app password the bootstrap tests type.
const bootSecret = "app-password-1234"

// bootBinary is the invoking user's chainmail binary on the fake hosts.
var bootBinary = []byte("chainmail test binary")

// bootStart is the fake clock's time when a bootstrap test starts.
var bootStart = time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)

// fakeResolver answers MX lookups from a map. A domain not in it is not
// found.
type fakeResolver map[string][]*net.MX

func (r fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	mx, ok := r[name]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return mx, nil
}

// bootRig runs chainmail bootstrap as alice against the fakes of one host.
// Its sudo runs bootstrap apply in-process as root against the same fakes.
type bootRig struct {
	t      *testing.T
	goos   string
	l      setup.Layout
	host   setuptest.Host
	clock  *setuptest.Clock
	svc    *setuptest.Service
	daemon *setuptest.Daemon
	tty    *setuptest.TTY
	exe    string
	uid    int

	// Set before run.
	lines    []string // what ReadLine returns, in order
	terminal bool     // stdin and stdout are terminals
	stdin    string   // stdin when it is not a terminal
	getenv   map[string]string
	resolver fakeResolver
	health   func(ctx context.Context, socket, account string) (*setup.AccountHealth, error)
	status   func(ctx context.Context, socket string) (DaemonInfo, error)
	// fsRoot stands in for "/" when bootstrap asks whether two socket
	// paths name the same file. It starts empty.
	fsRoot string

	// Recorded by run.
	linesLeft     []string
	sudoArgs      [][]string
	sudoStdin     [][]byte
	passwordReads int
	healthAsked   []string
	statusAsked   []string
	stderr        bytes.Buffer
}

// newBootRig is a fresh host for goos with alice's binary in her home.
func newBootRig(t *testing.T, goos string) *bootRig {
	t.Helper()
	h := setuptest.FreshHost(goos)
	clock := setuptest.NewClock(bootStart)
	svc := &setuptest.Service{Clock: clock}
	l := setup.LayoutFor(setup.Platform(goos))
	alice, ok := h.Accounts.User("alice")
	if !ok {
		t.Fatal("the fresh host has no alice")
	}
	home := alice.Home
	r := &bootRig{
		t: t, goos: goos, l: l, host: h, clock: clock, svc: svc,
		daemon: &setuptest.Daemon{Service: svc, Socket: l.Socket},
		tty:    setuptest.NewTTY("\n"),
		exe:    home + "/bin/chainmail",
		uid:    int(alice.UID),
		getenv: map[string]string{},
		fsRoot: realTempDir(t),
	}
	h.Files.Dir(home, alice.UID, alice.GID, 0o755).
		Dir(home+"/bin", alice.UID, alice.GID, 0o755).
		File(r.exe, alice.UID, alice.GID, 0o755, bootBinary)
	r.health = func(context.Context, string, string) (*setup.AccountHealth, error) {
		return nil, errors.New("dial unix: no such file or directory")
	}
	r.status = func(context.Context, string) (DaemonInfo, error) {
		return DaemonInfo{}, errors.New("dial unix: no such file or directory")
	}
	return r
}

// under is host path p inside fsRoot, joined as written. filepath.Join
// would clean ".." away before the symlinks ahead of it are resolved.
func (r *bootRig) under(p string) string { return r.fsRoot + "/" + p }

// env is the invoking user's environment.
func (r *bootRig) env() Env {
	env := Env{
		Stdin:      strings.NewReader(r.stdin),
		Stderr:     &r.stderr,
		Getenv:     func(k string) string { return r.getenv[k] },
		GOOS:       r.goos,
		Getuid:     func() int { return r.uid },
		Username:   func() (string, error) { return "alice", nil },
		Executable: func() (string, error) { return r.exe, nil },
		Sudo:       r.sudo,
		Resolver:   r.resolver,
		SetupFiles: r.host.Files, SetupAccounts: r.host.Accounts, SetupService: r.svc,
		AsDaemon: r.daemon.Run,
		OpenTTY:  func() (io.ReadWriteCloser, error) { return r.tty, nil },
		AccountHealth: func(ctx context.Context, socket, account string) (*setup.AccountHealth, error) {
			r.healthAsked = append(r.healthAsked, socket+" "+account)
			return r.health(ctx, socket, account)
		},
		DaemonStatus: func(ctx context.Context, socket string) (DaemonInfo, error) {
			r.statusAsked = append(r.statusAsked, socket)
			return r.status(ctx, socket)
		},
		Now:   r.clock.Now,
		After: r.after,
		Stat:  func(p string) (os.FileInfo, error) { return os.Stat(r.under(p)) },
		EvalSymlinks: func(p string) (string, error) {
			got, err := filepath.EvalSymlinks(r.under(p))
			if err != nil {
				return "", err
			}
			rel, err := filepath.Rel(r.fsRoot, got)
			if err != nil || strings.HasPrefix(rel, "..") {
				return "", fmt.Errorf("%s leaves the test root", p)
			}
			return filepath.Join("/", rel), nil
		},
	}
	if r.terminal {
		env.StdoutTerminal = true
		env.ReadLine = func() (string, error) {
			if len(r.linesLeft) == 0 {
				return "", io.EOF
			}
			line := r.linesLeft[0]
			r.linesLeft = r.linesLeft[1:]
			return line, nil
		}
		env.ReadPassword = func() ([]byte, error) {
			r.passwordReads++
			return []byte(bootSecret), nil
		}
	}
	return env
}

// after advances the fake clock instead of waiting.
func (r *bootRig) after(d time.Duration) <-chan time.Time {
	r.clock.Advance(d)
	ch := make(chan time.Time, 1)
	ch <- r.clock.Now()
	return ch
}

// sudo checks the command, then runs bootstrap apply in-process as root,
// with the environment sudo leaves.
func (r *bootRig) sudo(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	r.sudoArgs = append(r.sudoArgs, slices.Clone(args))
	if want := []string{r.exe, "bootstrap", "apply"}; !slices.Equal(args, want) {
		r.t.Errorf("sudo args = %q, want %q", args, want)
		return &setuptest.ExitError{Code: 1, Msg: "wrong sudo command"}
	}
	payload, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading the payload: %w", err)
	}
	r.sudoStdin = append(r.sudoStdin, payload)
	root := r.env()
	root.Getuid = func() int { return 0 }
	root.Stdin, root.Stdout = bytes.NewReader(payload), stdout
	root.ReadLine, root.ReadPassword, root.StdoutTerminal = nil, nil, false
	root.Getenv = func(k string) string {
		switch k {
		case "SUDO_UID":
			return strconv.Itoa(r.uid)
		case "SUDO_USER":
			return "alice"
		}
		return ""
	}
	if code := Execute(ctx, []string{"bootstrap", "apply"}, root); code != 0 {
		return &setuptest.ExitError{Code: code, Msg: "bootstrap apply failed"}
	}
	return nil
}

// run runs chainmail bootstrap with args.
func (r *bootRig) run(args ...string) cliResult {
	r.t.Helper()
	r.stderr.Reset()
	r.linesLeft = slices.Clone(r.lines)
	var out bytes.Buffer
	env := r.env()
	env.Stdout = &out
	code := Execute(r.t.Context(), append([]string{"bootstrap"}, args...), env)
	return cliResult{stdout: out.String(), stderr: r.stderr.String(), code: code}
}

// runApply runs bootstrap apply directly, as uid with getenv and stdin.
func (r *bootRig) runApply(uid int, getenv map[string]string, stdin io.Reader) cliResult {
	r.t.Helper()
	r.stderr.Reset()
	var out bytes.Buffer
	env := r.env()
	env.Getuid = func() int { return uid }
	env.Getenv = func(k string) string { return getenv[k] }
	env.Stdin, env.Stdout = stdin, &out
	code := Execute(r.t.Context(), []string{"bootstrap", "apply"}, env)
	return cliResult{stdout: out.String(), stderr: r.stderr.String(), code: code}
}

// noChanges fails when anything on the host changed.
func (r *bootRig) noChanges() {
	r.t.Helper()
	if w := r.host.Files.Writes(); len(w) > 0 {
		r.t.Errorf("files written: %q", w)
	}
	if c := r.host.Accounts.Changes(); len(c) > 0 {
		r.t.Errorf("accounts changed: %q", c)
	}
	if c := r.svc.Calls(); len(c) > 0 {
		r.t.Errorf("service calls: %q", c)
	}
}

// wantError checks a failed run's exit code and that its error mentions
// each of substrs, in the message or the hint.
func wantError(t *testing.T, got cliResult, code int, substrs ...string) {
	t.Helper()
	if got.code != code {
		t.Fatalf("exit %d, want %d; stderr %s", got.code, code, got.stderr)
	}
	for _, s := range substrs {
		if !strings.Contains(got.stderr, s) {
			t.Errorf("stderr lacks %q:\n%s", s, got.stderr)
		}
	}
}

func TestBootstrapRefusesRoot(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.uid = 0
	got := r.run("alice@gmail.com", "--yes")
	wantError(t, got, api.ExitUsage, "run bootstrap as the user who will use chainmail")
	if len(r.sudoArgs) > 0 {
		t.Error("sudo was called")
	}
	r.noChanges()
}

func TestBootstrapRefusesNixOS(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.host.Files.File("/etc/NIXOS", 0, 0, 0o644, nil)
	got := r.run("alice@gmail.com", "--yes")
	wantError(t, got, api.ExitUsage, "https://chainmail.sh/docs/operations/deploy#nixos")
	if len(r.sudoArgs) > 0 {
		t.Error("sudo was called")
	}
}

func TestBootstrapDryRun(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.terminal = true
	got := r.run("alice@gmail.com", "--dry-run")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{"create group chainmail-clients", "create user chainmail", "/etc/chainmail/config.toml", "test account alice"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("plan lacks %q:\n%s", want, got.stdout)
		}
	}
	if len(r.sudoArgs) > 0 {
		t.Error("dry run called sudo")
	}
	if r.passwordReads > 0 {
		t.Error("dry run asked for the password")
	}
	r.noChanges()

	// JSON lists the same steps.
	got = r.run("alice@gmail.com", "--dry-run", "--output", "json")
	var plan struct {
		DryRun  bool `json:"dry_run"`
		Actions []struct {
			Kind string `json:"kind"`
			Note string `json:"note"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &plan); err != nil || !plan.DryRun || len(plan.Actions) == 0 {
		t.Fatalf("dry run JSON = %s (%v)", got.stdout, err)
	}
	if plan.Actions[0].Kind != string(setup.ActCreateGroup) {
		t.Errorf("first action = %+v", plan.Actions[0])
	}
	r.noChanges()
}

func TestBootstrapFreshPassword(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.terminal = true
	r.lines = []string{"1", "y"} // the backup choice, then the confirmation
	got := r.run("alice@gmail.com")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(r.sudoArgs) != 1 {
		t.Fatalf("sudo calls = %d, want 1", len(r.sudoArgs))
	}
	if len(r.linesLeft) != 0 {
		t.Errorf("unread answers: %q", r.linesLeft)
	}
	if r.passwordReads != 1 {
		t.Errorf("password reads = %d, want 1", r.passwordReads)
	}
	for _, want := range []string{
		"https://myaccount.google.com/apppasswords", // where to make the app password
		"Vault key backup",                          // the backup prompt
		"Use provider",                              // must not appear: gmail is not decided by MX
	} {
		has := strings.Contains(got.stderr, want)
		if want == "Use provider" {
			if has {
				t.Errorf("stderr asks to confirm gmail:\n%s", got.stderr)
			}
			continue
		}
		if !has {
			t.Errorf("stderr lacks %q:\n%s", want, got.stderr)
		}
	}
	v, ok := r.daemon.Secret("accounts/alice")
	checkSecret(t, "accounts/alice", v, ok, bootSecret)
	for _, want := range []string{
		"alice", "gmail", "running", "ok",
		"newgrp chainmail-clients", "chainmail accounts list",
		"An agent that can run sudo without a password can get around all of this. Run agents from an account that cannot.",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, got.stdout)
		}
	}
}

func TestBootstrapFreshPasswordJSON(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.terminal = true
	r.lines = []string{"1", "y"}
	got := r.run("alice@gmail.com", "--output", "json")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	dec := json.NewDecoder(strings.NewReader(got.stdout))
	dec.DisallowUnknownFields()
	var res setup.Result
	if err := dec.Decode(&res); err != nil {
		t.Fatalf("stdout is not a Result: %v\n%s", err, got.stdout)
	}
	if dec.More() {
		t.Errorf("stdout holds more than one JSON object:\n%s", got.stdout)
	}
	if res.Account != "alice" || res.Provider != "gmail" || !res.ServiceRunning || res.Test != "ok" || res.KeyShown {
		t.Errorf("result = %+v", res)
	}
	if res.Paths["config"] != r.l.Config || len(res.NextSteps) == 0 {
		t.Errorf("result paths and steps = %+v", res)
	}
}

func TestBootstrapNoTerminalNeedsYes(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.stdin = bootSecret + "\n"
	got := r.run("alice@gmail.com")
	wantError(t, got, api.ExitUsage, "--yes")
	if len(r.sudoArgs) > 0 {
		t.Error("sudo was called without a confirmation")
	}
	r.noChanges()

	got = r.run("alice@gmail.com", "--yes")
	if got.code != 0 {
		t.Fatalf("with --yes: exit %d: %s", got.code, got.stderr)
	}
	v, ok := r.daemon.Secret("accounts/alice")
	checkSecret(t, "accounts/alice", v, ok, bootSecret)
	var res setup.Result
	if err := json.Unmarshal([]byte(got.stdout), &res); err != nil || res.Test != "ok" {
		t.Errorf("stdout = %s (%v)", got.stdout, err)
	}
}

func TestBootstrapKeyBackupShowNeedsTerminal(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.terminal = true
	r.lines = []string{"y"}
	// stdin is a terminal, stdout is not.
	r.stderr.Reset()
	r.linesLeft = slices.Clone(r.lines)
	var out bytes.Buffer
	e := r.env()
	e.Stdout, e.StdoutTerminal = &out, false
	code := Execute(t.Context(), []string{"bootstrap", "alice@gmail.com", "--key-backup", "show"}, e)
	got := cliResult{stdout: out.String(), stderr: r.stderr.String(), code: code}
	wantError(t, got, api.ExitUsage, "--key-backup show needs a terminal; captured output would hold the key")
	if len(r.sudoArgs) > 0 {
		t.Error("sudo was called")
	}
	if r.passwordReads > 0 {
		t.Error("the password was asked before the refusal")
	}
	r.noChanges()

	// Choosing to show the key at the prompt is refused the same way.
	r.lines = []string{"2", "y"}
	r.linesLeft = slices.Clone(r.lines)
	r.stderr.Reset()
	out.Reset()
	code = Execute(t.Context(), []string{"bootstrap", "alice@gmail.com"}, e)
	got = cliResult{stdout: out.String(), stderr: r.stderr.String(), code: code}
	wantError(t, got, api.ExitUsage, "--key-backup show needs a terminal")
	if len(r.sudoArgs) > 0 {
		t.Error("sudo was called")
	}
}

func TestBootstrapKeyShownOnTTY(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.terminal = true
	r.lines = []string{"y"}
	got := r.run("alice@gmail.com", "--key-backup", "show")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	key, _, _ := r.host.Files.Content(r.l.VaultKey)
	secretLine := ""
	for line := range strings.Lines(string(key)) {
		if strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			secretLine = strings.TrimSpace(line)
		}
	}
	if secretLine == "" {
		t.Fatalf("no key written: the key file holds %d bytes and no AGE-SECRET-KEY- line", len(key))
	}
	if !strings.Contains(r.tty.Output(), secretLine) {
		t.Error("the key was not shown on the terminal")
	}
	for name, s := range map[string]string{"stdout": got.stdout, "stderr": got.stderr} {
		if strings.Contains(s, secretLine) {
			t.Errorf("the key is on %s", name)
		}
	}
	for _, p := range r.sudoStdin {
		if bytes.Contains(p, []byte(secretLine)) {
			t.Error("the key is in the sudo payload")
		}
	}
	if !strings.Contains(got.stdout, "shown") {
		t.Errorf("summary does not say the key was shown:\n%s", got.stdout)
	}
}

func TestBootstrapMXConfirm(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.terminal = true
	r.resolver = fakeResolver{"example.org": {{Host: "aspmx.l.google.com.", Pref: 1}}}
	r.lines = []string{"n"}
	got := r.run("alice@example.org", "--oauth-client-id", "id.apps.googleusercontent.com")
	wantError(t, got, api.ExitUsage, "Use provider google-workspace for example.org? [Y/n]", "--provider")
	if len(r.sudoArgs) > 0 {
		t.Error("sudo was called")
	}
	r.noChanges()

	// Yes goes on to the plan.
	r.lines = []string{"y"}
	got = r.run("alice@example.org", "--oauth-client-id", "id.apps.googleusercontent.com", "--dry-run")
	if got.code != 0 {
		t.Fatalf("after yes: exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "oauth_clients/google") {
		t.Errorf("plan lacks the OAuth client secret:\n%s", got.stdout)
	}
}

func TestBootstrapSkipsPasswordWhenHealthy(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.terminal = true
	r.lines = []string{"1", "y"}
	if got := r.run("alice@gmail.com"); got.code != 0 {
		t.Fatalf("first run: exit %d: %s", got.code, got.stderr)
	}
	if r.passwordReads != 1 {
		t.Fatalf("first run password reads = %d", r.passwordReads)
	}

	// The daemon reports the account healthy: no password.
	r.health = func(context.Context, string, string) (*setup.AccountHealth, error) {
		return &setup.AccountHealth{Exists: true, SecretStored: true, AuthOK: true}, nil
	}
	r.healthAsked = nil
	r.lines = []string{"y"}
	got := r.run("alice@gmail.com")
	if got.code != 0 {
		t.Fatalf("healthy rerun: exit %d: %s", got.code, got.stderr)
	}
	if r.passwordReads != 1 {
		t.Errorf("healthy rerun asked for the password")
	}
	if want := []string{r.l.Socket + " alice"}; !slices.Equal(r.healthAsked, want) {
		t.Errorf("health asked = %q, want %q", r.healthAsked, want)
	}
	if strings.Contains(got.stderr, "Vault key backup") {
		t.Error("a rerun with a key asked about the backup")
	}

	// The daemon cannot be asked: the password is asked again.
	r.health = func(context.Context, string, string) (*setup.AccountHealth, error) {
		return nil, errors.New("permission denied")
	}
	r.lines = []string{"y"}
	got = r.run("alice@gmail.com")
	if got.code != 0 {
		t.Fatalf("unreachable rerun: exit %d: %s", got.code, got.stderr)
	}
	if r.passwordReads != 2 {
		t.Errorf("password reads = %d, want 2", r.passwordReads)
	}
}

func TestBootstrapWarnsOtherSocket(t *testing.T) {
	t.Parallel()
	const dev = "/run/user/1000/chainmail-dev.sock"
	r := newBootRig(t, "linux")
	r.stdin = bootSecret
	r.getenv["CHAINMAIL_SOCKET"] = dev
	got := r.run("alice@gmail.com", "--yes")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{dev, r.l.Socket, "CHAINMAIL_SOCKET=" + r.l.Socket} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, got.stderr)
		}
	}

	// cli.toml naming another socket gets the line to change.
	r2 := newBootRig(t, "linux")
	r2.stdin = bootSecret
	dir := t.TempDir()
	r2.getenv["XDG_CONFIG_HOME"] = dir
	path := filepath.Join(dir, "chainmail", "cli.toml")
	writeTestFile(t, path, "socket = \""+dev+"\"\n")
	got = r2.run("alice@gmail.com", "--yes")
	if got.code != 0 {
		t.Fatalf("cli.toml: exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{dev, path, `socket = "` + r2.l.Socket + `"`} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, got.stderr)
		}
	}
	if data, _ := os.ReadFile(path); string(data) != "socket = \""+dev+"\"\n" {
		t.Errorf("cli.toml changed: %q", data)
	}
}

// otherSocket is a socket CHAINMAIL_SOCKET or cli.toml selects in place of
// the layout's.
const otherSocket = "/srv/other/chainmail.sock"

// selectOther makes r's user select otherSocket through how ("env" or
// "cli.toml").
func selectOther(t *testing.T, r *bootRig, how string) {
	t.Helper()
	switch how {
	case "env":
		r.getenv["CHAINMAIL_SOCKET"] = otherSocket
	case "cli.toml":
		dir := t.TempDir()
		r.getenv["XDG_CONFIG_HOME"] = dir
		writeTestFile(t, filepath.Join(dir, "chainmail", "cli.toml"), "socket = \""+otherSocket+"\"\n")
	default:
		t.Fatalf("unknown selection %q", how)
	}
}

// A production daemon answering at another selected socket is a different
// installation: bootstrap refuses before changing anything.
func TestBootstrapRefusesOtherProductionDaemon(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"env", "cli.toml"} {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s dry-run=%v", how, dryRun), func(t *testing.T) {
				t.Parallel()
				r := newBootRig(t, "linux")
				r.stdin = bootSecret
				selectOther(t, r, how)
				r.status = func(context.Context, string) (DaemonInfo, error) {
					return DaemonInfo{Dev: false, Version: "0.9.0"}, nil
				}
				args := []string{"alice@gmail.com", "--yes"}
				if dryRun {
					args = append(args, "--dry-run")
				}
				got := r.run(args...)
				wantError(t, got, api.ExitUsage, otherSocket, "production")
				if !slices.Equal(r.statusAsked, []string{otherSocket}) {
					t.Errorf("status asked of %q, want only %s", r.statusAsked, otherSocket)
				}
				if len(r.sudoArgs) > 0 {
					t.Error("sudo was called")
				}
				if r.passwordReads > 0 {
					t.Error("the password was asked for")
				}
				r.noChanges()
			})
		}
	}
}

// A dev daemon at another selected socket only gets today's warning.
func TestBootstrapOtherDevDaemonWarns(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"env", "cli.toml"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			r := newBootRig(t, "linux")
			r.stdin = bootSecret
			selectOther(t, r, how)
			r.status = func(context.Context, string) (DaemonInfo, error) {
				return DaemonInfo{Dev: true, Version: "0.9.0"}, nil
			}
			got := r.run("alice@gmail.com", "--yes")
			if got.code != 0 {
				t.Fatalf("exit %d: %s", got.code, got.stderr)
			}
			if !slices.Equal(r.statusAsked, []string{otherSocket}) {
				t.Errorf("status asked of %q, want only %s", r.statusAsked, otherSocket)
			}
			if !strings.Contains(got.stderr, "warning: ") || !strings.Contains(got.stderr, otherSocket) {
				t.Errorf("no warning about %s:\n%s", otherSocket, got.stderr)
			}
			if len(r.sudoArgs) != 1 {
				t.Errorf("sudo called %d times, want once", len(r.sudoArgs))
			}
		})
	}
}

// An unreachable socket selected in place of the layout's only gets
// today's warning.
func TestBootstrapOtherSocketUnreachableWarns(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"env", "cli.toml"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			r := newBootRig(t, "linux")
			r.stdin = bootSecret
			selectOther(t, r, how)
			got := r.run("alice@gmail.com", "--yes")
			if got.code != 0 {
				t.Fatalf("exit %d: %s", got.code, got.stderr)
			}
			if !slices.Equal(r.statusAsked, []string{otherSocket}) {
				t.Errorf("status asked of %q, want only %s", r.statusAsked, otherSocket)
			}
			if !strings.Contains(got.stderr, "warning: ") || !strings.Contains(got.stderr, otherSocket) {
				t.Errorf("no warning about %s:\n%s", otherSocket, got.stderr)
			}
		})
	}
}

// The --socket flag selects the socket this run's client would dial, as
// it does for every other command, so a daemon answering there is checked
// the same way: production refuses, dev and silence go ahead.
func TestBootstrapSocketFlagSelectsDaemon(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status func(context.Context, string) (DaemonInfo, error)
		refuse bool
	}{
		{
			name:   "production",
			status: func(context.Context, string) (DaemonInfo, error) { return DaemonInfo{Version: "0.9.0"}, nil },
			refuse: true,
		},
		{
			name:   "dev",
			status: func(context.Context, string) (DaemonInfo, error) { return DaemonInfo{Dev: true, Version: "0.9.0"}, nil },
		},
		{
			name: "unreachable",
			status: func(context.Context, string) (DaemonInfo, error) {
				return DaemonInfo{}, errors.New("dial unix: no such file or directory")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newBootRig(t, "linux")
			r.stdin = bootSecret
			r.status = tt.status
			got := r.run("--socket", otherSocket, "alice@gmail.com", "--yes")
			if !slices.Equal(r.statusAsked, []string{otherSocket}) {
				t.Errorf("status asked of %q, want only %s", r.statusAsked, otherSocket)
			}
			if tt.refuse {
				wantError(t, got, api.ExitUsage, otherSocket, "--socket", "production")
				if len(r.sudoArgs) > 0 {
					t.Error("sudo was called")
				}
				r.noChanges()
				return
			}
			if got.code != 0 {
				t.Fatalf("exit %d: %s", got.code, got.stderr)
			}
			if len(r.sudoArgs) != 1 {
				t.Errorf("sudo called %d times, want once", len(r.sudoArgs))
			}
		})
	}
}

// On macOS the built-in default socket is not the layout's, so with
// nothing selected the client dials the default: a production daemon
// answering there is another installation.
func TestBootstrapMacDefaultSocketDaemon(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "darwin")
	r.stdin = bootSecret
	if r.l.Socket == config.DefaultSocket {
		t.Fatalf("the macOS layout socket is the default %s", config.DefaultSocket)
	}
	r.status = func(context.Context, string) (DaemonInfo, error) { return DaemonInfo{Version: "0.9.0"}, nil }
	got := r.run("alice@icloud.com", "--yes")
	wantError(t, got, api.ExitUsage, config.DefaultSocket, "production")
	if !slices.Equal(r.statusAsked, []string{config.DefaultSocket}) {
		t.Errorf("status asked of %q, want only %s", r.statusAsked, config.DefaultSocket)
	}
	if len(r.sudoArgs) > 0 {
		t.Error("sudo was called")
	}
	r.noChanges()

	// Nothing answering at the default is no reason to stop.
	r2 := newBootRig(t, "darwin")
	r2.stdin = bootSecret
	if got := r2.run("alice@icloud.com", "--yes"); got.code != 0 {
		t.Fatalf("nothing at the default: exit %d: %s", got.code, got.stderr)
	}
	if !slices.Equal(r2.statusAsked, []string{config.DefaultSocket}) {
		t.Errorf("status asked of %q, want only %s", r2.statusAsked, config.DefaultSocket)
	}
}

// realTempDir is a temporary directory with every symlink in its path
// resolved.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// aliasRun makes root hold /run/chainmail, with /var/run a symlink to
// /run as on most Linux hosts, and the socket file itself when socket is
// set.
func aliasRun(t *testing.T, root string, socket bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "run", "chainmail"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "var"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../run", filepath.Join(root, "var", "run")); err != nil {
		t.Fatal(err)
	}
	if socket {
		writeTestFile(t, filepath.Join(root, "run", "chainmail", "chainmail.sock"), "")
	}
}

// redirectSocket names /srv/other/chainmail/chainmail.sock once
// redirectRun has run: the kernel follows /run/redirect before "..".
// Cleaned, it would read as the layout socket.
const redirectSocket = "/run/redirect/../chainmail/chainmail.sock"

// redirectRun makes root hold /run/redirect, a symlink to /srv/other/run,
// and a socket file at /srv/other/chainmail/chainmail.sock.
func redirectRun(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "srv", "other", "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "srv", "other", "chainmail", "chainmail.sock"), "")
	if err := os.MkdirAll(filepath.Join(root, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../srv/other/run", filepath.Join(root, "run", "redirect")); err != nil {
		t.Fatal(err)
	}
}

// sameSocket tells two names for one socket file from two sockets.
func TestSameSocket(t *testing.T) {
	t.Parallel()
	root := realTempDir(t)
	aliasRun(t, root, true)
	writeTestFile(t, filepath.Join(root, "run", "chainmail", "other.sock"), "")
	redirectRun(t, root)
	if err := os.Symlink("../run/chainmail", filepath.Join(root, "srv", "link")); err != nil {
		t.Fatal(err)
	}
	// Joined as written: filepath.Join would clean "..".
	at := func(p string) string { return root + p }
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"a symlink before .. reaching another file", at(redirectSocket), at("/run/chainmail/chainmail.sock"), false},
		{"a symlink before .. reaching another directory", at("/run/redirect/../chainmail/new.sock"), at("/run/chainmail/new.sock"), false},
		{"a symlink before .. reaching the same file", at("/srv/link/../chainmail/chainmail.sock"), at("/run/chainmail/chainmail.sock"), true},
		{"a symlink before .. reaching the same directory", at("/srv/link/../chainmail/new.sock"), at("/run/chainmail/new.sock"), true},
		{"an unresolvable path that cleans to a resolvable one", at("/none/../run/chainmail/new.sock"), at("/run/chainmail/new.sock"), false},
		{"one file through a symlinked directory", at("/var/run/chainmail/chainmail.sock"), at("/run/chainmail/chainmail.sock"), true},
		{"a missing file through a symlinked directory", at("/var/run/chainmail/new.sock"), at("/run/chainmail/new.sock"), true},
		{"two files in one directory", at("/run/chainmail/other.sock"), at("/run/chainmail/chainmail.sock"), false},
		{"a missing file next to an existing one", at("/run/chainmail/new.sock"), at("/run/chainmail/chainmail.sock"), false},
		{"unresolvable paths that clean to one", root + "/none/./x/../chainmail.sock", at("/none/chainmail.sock"), true},
		{"unresolvable paths that differ", at("/none/a.sock"), at("/none/b.sock"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sameSocket(Env{}, tt.a, tt.b); got != tt.want {
				t.Errorf("sameSocket(%s, %s) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// A socket selected through another name for the layout's socket, such as
// /var/run for /run, is the installation bootstrap sets up, not another
// one: its daemon is not asked about and bootstrap goes ahead.
func TestBootstrapAliasedLayoutSocket(t *testing.T) {
	t.Parallel()
	const alias = "/var/run/chainmail/chainmail.sock"
	for _, socketExists := range []bool{true, false} {
		for _, how := range []string{"env", "flag"} {
			t.Run(fmt.Sprintf("%s socket=%v", how, socketExists), func(t *testing.T) {
				t.Parallel()
				r := newBootRig(t, "linux")
				r.stdin = bootSecret
				aliasRun(t, r.fsRoot, socketExists)
				r.status = func(context.Context, string) (DaemonInfo, error) { return DaemonInfo{Version: "0.9.0"}, nil }
				args := []string{"alice@gmail.com", "--yes"}
				if how == "env" {
					r.getenv["CHAINMAIL_SOCKET"] = alias
				} else {
					args = append([]string{"--socket", alias}, args...)
				}
				got := r.run(args...)
				if got.code != 0 {
					t.Fatalf("exit %d: %s", got.code, got.stderr)
				}
				if len(r.statusAsked) > 0 {
					t.Errorf("status asked of %q, want none", r.statusAsked)
				}
				if how == "env" && strings.Contains(got.stderr, "still reach that daemon") {
					t.Errorf("an alias of the layout socket was warned about as another daemon:\n%s", got.stderr)
				}
			})
		}
	}
}

// A different socket is still asked about when the layout's socket exists.
func TestBootstrapDifferentSocketStillProbed(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.stdin = bootSecret
	aliasRun(t, r.fsRoot, true)
	writeTestFile(t, filepath.Join(r.fsRoot, otherSocket), "")
	r.getenv["CHAINMAIL_SOCKET"] = otherSocket
	r.status = func(context.Context, string) (DaemonInfo, error) { return DaemonInfo{Version: "0.9.0"}, nil }
	got := r.run("alice@gmail.com", "--yes")
	wantError(t, got, api.ExitUsage, otherSocket, "production")
	if !slices.Equal(r.statusAsked, []string{otherSocket}) {
		t.Errorf("status asked of %q, want only %s", r.statusAsked, otherSocket)
	}
	r.noChanges()
}

// A selection whose symlink comes before "..", such as
// /run/redirect/../chainmail/chainmail.sock, names the socket the kernel
// reaches, not the one its cleaned spelling suggests. With both sockets
// present it is asked about, and a production answer refuses.
func TestBootstrapSymlinkBeforeDotDotProbed(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"env", "flag"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			r := newBootRig(t, "linux")
			r.stdin = bootSecret
			aliasRun(t, r.fsRoot, true)
			redirectRun(t, r.fsRoot)
			r.status = func(context.Context, string) (DaemonInfo, error) { return DaemonInfo{Version: "0.9.0"}, nil }
			args := []string{"alice@gmail.com", "--yes"}
			if how == "env" {
				r.getenv["CHAINMAIL_SOCKET"] = redirectSocket
			} else {
				args = append([]string{"--socket", redirectSocket}, args...)
			}
			got := r.run(args...)
			wantError(t, got, api.ExitUsage, redirectSocket, "production")
			if !slices.Equal(r.statusAsked, []string{redirectSocket}) {
				t.Errorf("status asked of %q, want only %s", r.statusAsked, redirectSocket)
			}
			if len(r.sudoArgs) > 0 {
				t.Error("sudo was called")
			}
			r.noChanges()
		})
	}
}

// With no other socket selected, no other daemon is asked about.
func TestBootstrapAsksNoOtherDaemon(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.stdin = bootSecret
	r.getenv["CHAINMAIL_SOCKET"] = r.l.Socket
	if got := r.run("alice@gmail.com", "--yes"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(r.statusAsked) > 0 {
		t.Errorf("status asked of %q, want none", r.statusAsked)
	}
}

// daemonStatus reads dev mode and version from admin status.
func TestDaemonStatus(t *testing.T) {
	t.Parallel()
	socket := startFakeAdmin(t, &fakeAdmin{status: &chainmailv1.GetStatusResponse{Version: "1.2.3", DevMode: true}})
	got, err := daemonStatus(t.Context(), socket)
	if err != nil || got != (DaemonInfo{Dev: true, Version: "1.2.3"}) {
		t.Fatalf("daemonStatus = %+v, %v", got, err)
	}
	if _, err := daemonStatus(t.Context(), filepath.Join(t.TempDir(), "none.sock")); err == nil {
		t.Fatal("daemonStatus of a missing socket succeeded")
	}
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapMacCLIConfig(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "darwin")
	r.stdin = bootSecret
	dir := t.TempDir()
	r.getenv["XDG_CONFIG_HOME"] = dir
	path := filepath.Join(dir, "chainmail", "cli.toml")
	got := r.run("alice@icloud.com", "--yes")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cli.toml: %v", err)
	}
	if want := "socket = \"/var/db/chainmail/chainmail.sock\"\n"; string(data) != want {
		t.Errorf("cli.toml = %q, want %q", data, want)
	}

	// An existing cli.toml is left alone.
	r2 := newBootRig(t, "darwin")
	r2.stdin = bootSecret
	dir2 := t.TempDir()
	r2.getenv["XDG_CONFIG_HOME"] = dir2
	path2 := filepath.Join(dir2, "chainmail", "cli.toml")
	writeTestFile(t, path2, "output = \"json\"\n")
	got = r2.run("alice@icloud.com", "--yes")
	if got.code != 0 {
		t.Fatalf("existing: exit %d: %s", got.code, got.stderr)
	}
	if data, _ := os.ReadFile(path2); string(data) != "output = \"json\"\n" {
		t.Errorf("existing cli.toml changed: %q", data)
	}
	if !strings.Contains(got.stderr, `socket = "/var/db/chainmail/chainmail.sock"`) {
		t.Errorf("no line to add for the existing cli.toml:\n%s", got.stderr)
	}

	// A failed bootstrap writes no cli.toml.
	r3 := newBootRig(t, "darwin")
	dir3 := t.TempDir()
	r3.getenv["XDG_CONFIG_HOME"] = dir3
	if got := r3.run("alice@icloud.com", "--yes"); got.code == 0 {
		t.Fatal("bootstrap with an empty secret succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir3, "chainmail", "cli.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cli.toml after a failure: %v", err)
	}
}

func TestBootstrapApplyRefusesNonRoot(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	payload := `{"plan":{},"request":{}}`
	got := r.runApply(1000, map[string]string{"SUDO_UID": "1000", "SUDO_USER": "alice"}, strings.NewReader(payload))
	wantError(t, got, api.ExitUsage, "root")
	for _, sudoUID := range []string{"", "0", "abc", "-1", "1000x"} {
		got := r.runApply(0, map[string]string{"SUDO_UID": sudoUID, "SUDO_USER": "alice"}, strings.NewReader(payload))
		wantError(t, got, api.ExitUsage, "SUDO_UID")
	}
	got = r.runApply(0, map[string]string{"SUDO_UID": "1000"}, strings.NewReader(payload))
	wantError(t, got, api.ExitUsage, "SUDO_USER")
	r.noChanges()
}

func TestBootstrapApplyRefusesBigPayload(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	env := map[string]string{"SUDO_UID": "1000", "SUDO_USER": "alice"}
	big := `{"plan":{},"request":{},"secret":"` + strings.Repeat("A", 1<<20) + `"}`
	got := r.runApply(0, env, strings.NewReader(big))
	wantError(t, got, api.ExitUsage, "1 MiB")

	for name, payload := range map[string]string{
		"not json":      "plan",
		"unknown field": `{"plan":{},"request":{},"extra":1}`,
		"two objects":   `{"plan":{},"request":{}} {}`,
	} {
		got := r.runApply(0, env, strings.NewReader(payload))
		if got.code != api.ExitUsage {
			t.Errorf("%s: exit %d, want 2; stderr %s", name, got.code, got.stderr)
		}
	}
	r.noChanges()
}

func TestBootstrapApplyRefusesAlteredPlan(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.stdin = bootSecret
	// Run the real flow once, then send its payload again, altered.
	got := r.run("alice@gmail.com", "--yes")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	payload := r.sudoStdin[0]
	var p map[string]json.RawMessage
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatal(err)
	}
	p["plan"] = json.RawMessage(strings.Replace(string(p["plan"]), `"InvokingUser":"alice"`, `"InvokingUser":"mallory"`, 1))
	altered, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	res := r.runApply(0, map[string]string{"SUDO_UID": strconv.Itoa(r.uid), "SUDO_USER": "alice"}, bytes.NewReader(altered))
	if res.code != api.ExitUsage {
		t.Errorf("altered plan: exit %d, want 2; stderr %s", res.code, res.stderr)
	}
}

func TestBootstrapExitCodePassesThrough(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.stdin = bootSecret
	r.daemon.TestExit = api.ExitAuth
	got := r.run("alice@gmail.com", "--yes")
	if got.code != api.ExitAuth {
		t.Fatalf("exit %d, want 5; stderr %s", got.code, got.stderr)
	}
	// bootstrap apply reported the error; bootstrap does not repeat it.
	if n := strings.Count(got.stderr, `"error"`); n != 1 {
		t.Errorf("stderr has %d error lines, want 1:\n%s", n, got.stderr)
	}
	if !strings.Contains(got.stderr, "AUTH_REJECTED") || !strings.Contains(got.stderr, "enter the new password") {
		t.Errorf("stderr = %s", got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestBootstrapSudoFailure(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.stdin = bootSecret
	e := r.env()
	var out bytes.Buffer
	e.Stdout = &out
	e.Sudo = func(context.Context, io.Reader, io.Writer, ...string) error {
		return errors.New(`exec: "sudo": executable file not found in $PATH`)
	}
	code := Execute(t.Context(), []string{"bootstrap", "alice@gmail.com", "--yes"}, e)
	if code != api.ExitInternal {
		t.Errorf("exit %d, want 1; stderr %s", code, r.stderr.String())
	}
	if strings.Contains(r.stderr.String(), bootSecret) {
		t.Error("the secret is in the error")
	}
}

func TestBootstrapSecretNotInArgv(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.terminal = true
	r.lines = []string{"1", "y"}
	if got := r.run("alice@gmail.com"); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(r.sudoArgs) != 1 {
		t.Fatalf("sudo calls = %d", len(r.sudoArgs))
	}
	checkArgv(t, "sudo", r.sudoArgs[0], bootSecret)
	for _, c := range r.daemon.Calls() {
		checkArgv(t, "a daemon-user command", c.Args, bootSecret)
	}
	// It travels on stdin only, and reached the vault.
	v, ok := r.daemon.Secret("accounts/alice")
	checkSecret(t, "accounts/alice", v, ok, bootSecret)
	if strings.Contains(r.stderr.String(), bootSecret) {
		t.Error("the secret is on stderr")
	}
}

func TestBootstrapHelp(t *testing.T) {
	t.Parallel()
	got := runCLI(t, Env{}, "bootstrap", "--help")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, flag := range []string{"--name", "--provider", "--oauth-client-id", "--oauth-client ", "--from-pass", "--key-backup", "--dry-run", "--yes"} {
		if !strings.Contains(got.stdout, flag) {
			t.Errorf("help lacks %s:\n%s", flag, got.stdout)
		}
	}
	if strings.Contains(got.stdout, "Available Commands") || strings.Contains(got.stdout, "\n  apply") {
		t.Errorf("help lists bootstrap apply:\n%s", got.stdout)
	}
	root := runCLI(t, Env{}, "--help")
	if !strings.Contains(root.stdout, "bootstrap") {
		t.Errorf("root help lacks bootstrap:\n%s", root.stdout)
	}
}

func TestAccountHealthFrom(t *testing.T) {
	t.Parallel()
	list := &chainmailv1.ListAccountsResponse{Accounts: []*chainmailv1.Account{
		{Name: "alice", AuthMethod: "password", AuthState: chainmailv1.AuthState_AUTH_STATE_OK},
		{Name: "work", AuthMethod: "oauth2", AuthState: chainmailv1.AuthState_AUTH_STATE_REQUIRED},
		{Name: "team", AuthMethod: "oauth2", AuthState: chainmailv1.AuthState_AUTH_STATE_OK},
	}}
	status := &chainmailv1.GetStatusResponse{SecretNames: []string{"accounts/alice", "oauth_clients/google", "tokens/team"}}
	tests := []struct {
		account string
		want    setup.AccountHealth
	}{
		{"alice", setup.AccountHealth{Exists: true, SecretStored: true, AuthOK: true}},
		{"work", setup.AccountHealth{Exists: true}},
		{"team", setup.AccountHealth{Exists: true, SecretStored: true, AuthOK: true}},
		{"nobody", setup.AccountHealth{}},
	}
	for _, tt := range tests {
		if got := accountHealthFrom(list, status, tt.account); *got != tt.want {
			t.Errorf("%s: %+v, want %+v", tt.account, *got, tt.want)
		}
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		err    error
		code   int
		reason string
		hint   string
	}{
		{"refusal", &setup.Refusal{Msg: "no", Hint: "h1"}, api.ExitUsage, api.ReasonUsage, "h1"},
		{"wrapped refusal", fmt.Errorf("plan: %w", &setup.Refusal{Msg: "no", Hint: "h2"}), api.ExitUsage, api.ReasonUsage, "h2"},
		{"auth", &setup.AuthError{Msg: "rejected", Hint: "h3"}, api.ExitAuth, api.ReasonAuthRejected, "h3"},
		{"not ready", setup.ErrNotReady, api.ExitInternal, api.ReasonInternal, ""},
	}
	for _, tt := range tests {
		c := classify(tt.err)
		if c.code != tt.code || c.reason != tt.reason || c.hint != tt.hint {
			t.Errorf("%s: classify = %+v", tt.name, c)
		}
	}
}

func TestBootstrapOAuthNoTerminal(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.stdin = "client-secret-5678\n"
	got := r.run("alice@example.org", "--provider", "google-workspace",
		"--oauth-client-id", "id.apps.googleusercontent.com", "--yes")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	var res setup.Result
	if err := json.Unmarshal([]byte(got.stdout), &res); err != nil {
		t.Fatalf("stdout = %s (%v)", got.stdout, err)
	}
	if res.Test != "skipped" || !slices.Contains(res.NextSteps, "chainmail auth login alice") {
		t.Errorf("result = %+v", res)
	}
	v, ok := r.daemon.Secret("oauth_clients/google")
	checkSecret(t, "oauth_clients/google", v, ok, "client-secret-5678")
	for _, c := range r.daemon.Calls() {
		if slices.Contains(c.Args, "login") {
			t.Errorf("auth login ran without a terminal: %q", c.Args)
		}
	}
}

func TestAskYesNo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		answers []string
		def     bool
		want    bool
		asked   int
	}{
		{"yes", []string{"y"}, false, true, 1},
		{"YES", []string{" YES "}, false, true, 1},
		{"no", []string{"n"}, true, false, 1},
		{"empty takes the default", []string{""}, true, true, 1},
		{"empty takes a no default", []string{""}, false, false, 1},
		{"end of input is no", nil, true, false, 1},
		{"asks again", []string{"maybe", "y"}, false, true, 2},
		{"gives up", []string{"a", "b", "c", "y"}, true, false, 3},
	}
	for _, tt := range tests {
		left := slices.Clone(tt.answers)
		var stderr bytes.Buffer
		env := Env{Stderr: &stderr, ReadLine: func() (string, error) {
			if len(left) == 0 {
				return "", io.EOF
			}
			a := left[0]
			left = left[1:]
			return a, nil
		}}
		got, err := askYesNo(env, "Sure? ", tt.def)
		if err != nil || got != tt.want {
			t.Errorf("%s: %v, %v; want %v", tt.name, got, err, tt.want)
		}
		if n := strings.Count(stderr.String(), "Sure? "); n != tt.asked {
			t.Errorf("%s: asked %d times, want %d", tt.name, n, tt.asked)
		}
	}
}

func TestReadTerminalLine(t *testing.T) {
	t.Parallel()
	r := strings.NewReader("one\r\ntwo\nthree")
	for _, want := range []string{"one", "two", "three"} {
		got, err := readTerminalLine(r)
		if err != nil || got != want {
			t.Errorf("got %q, %v; want %q", got, err, want)
		}
	}
	if got, err := readTerminalLine(r); got != "" || !errors.Is(err, io.EOF) {
		t.Errorf("at the end: %q, %v", got, err)
	}
	// Nothing after the newline is read, so a later password prompt on
	// the same terminal gets it.
	r = strings.NewReader("1\nsecret\n")
	if _, err := readTerminalLine(r); err != nil {
		t.Fatal(err)
	}
	if r.Len() != len("secret\n") {
		t.Errorf("read past the newline: %d bytes left", r.Len())
	}
}

func TestLineReaderFeedsAskBackup(t *testing.T) {
	t.Parallel()
	answers := []string{"3", "2"}
	lr := &lineReader{read: func() (string, error) {
		if len(answers) == 0 {
			return "", io.EOF
		}
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}}
	got, err := setup.AskBackup(lr, io.Discard)
	if err != nil || got != setup.BackupShow {
		t.Errorf("AskBackup = %q, %v", got, err)
	}
}

func TestDaemonRunnerNeedsTheUser(t *testing.T) {
	t.Parallel()
	l := setup.LayoutFor(setup.Linux)
	accounts := setuptest.NewAccounts("linux")
	run := daemonRunner(l, accounts)
	err := run(t.Context(), nil, io.Discard, "admin", "status")
	if err == nil || !strings.Contains(err.Error(), "chainmail does not exist") {
		t.Errorf("no user: %v", err)
	}
	accounts.AddUser(system.User{Name: "chainmail", UID: 990, GID: 990, Shell: "/usr/sbin/nologin"})
	err = run(t.Context(), nil, io.Discard, "admin", "status")
	if err == nil || !strings.Contains(err.Error(), "group chainmail-clients does not exist") {
		t.Errorf("no group: %v", err)
	}
}

// A saved table preference does not apply when stdout is redirected: the
// spec promises one JSON object whenever stdout is not a terminal.
func TestBootstrapJSONWhenStdoutRedirectedDespiteTablePreference(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.stdin = bootSecret + "\n"
	r.getenv["CHAINMAIL_OUTPUT"] = "table"
	got := r.run("alice@gmail.com", "--yes")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	var res setup.Result
	if err := json.Unmarshal([]byte(got.stdout), &res); err != nil {
		t.Fatalf("stdout is not one JSON object (%v): %s", err, got.stdout)
	}
	if res.Account != "alice" {
		t.Errorf("result account = %q, want alice", res.Account)
	}

	// The dry run follows the same rule.
	r2 := newBootRig(t, "linux")
	r2.getenv["CHAINMAIL_OUTPUT"] = "table"
	got = r2.run("alice@gmail.com", "--dry-run")
	if got.code != 0 {
		t.Fatalf("dry run: exit %d: %s", got.code, got.stderr)
	}
	var plan dryRunOutput
	if err := json.Unmarshal([]byte(got.stdout), &plan); err != nil || !plan.DryRun || len(plan.Actions) == 0 {
		t.Fatalf("dry run stdout is not the JSON plan (%v): %s", err, got.stdout)
	}
	r2.noChanges()
}

// Showing the key needs both halves: a terminal stdout with stdin that is
// not a terminal is refused too.
func TestBootstrapKeyBackupShowNeedsTerminalStdin(t *testing.T) {
	t.Parallel()
	r := newBootRig(t, "linux")
	r.stdin = bootSecret + "\n"
	e := r.env()
	e.StdoutTerminal = true // stdin stays a pipe: no ReadLine, no ReadPassword
	var out bytes.Buffer
	e.Stdout = &out
	code := Execute(t.Context(), []string{"bootstrap", "alice@gmail.com", "--yes", "--key-backup", "show"}, e)
	got := cliResult{stdout: out.String(), stderr: r.stderr.String(), code: code}
	wantError(t, got, api.ExitUsage, "--key-backup show needs a terminal; captured output would hold the key")
	if len(r.sudoArgs) > 0 {
		t.Error("sudo was called")
	}
	r.noChanges()
}

// checkSecret checks a secret the fake daemon stored (got, and ok for
// whether it is there) against want. A failure gives presence and
// lengths, never either value.
func checkSecret(tb testing.TB, name string, got []byte, ok bool, want string) {
	tb.Helper()
	switch {
	case !ok:
		tb.Errorf("no secret stored as %s", name)
	case string(got) != want:
		tb.Errorf("secret %s holds %d bytes that differ from the %d bytes given", name, len(got), len(want))
	}
}

// checkArgv fails for each argument of what's argv that holds secret,
// giving its position, never the argument.
func checkArgv(tb testing.TB, what string, args []string, secret string) {
	tb.Helper()
	for i, a := range args {
		if strings.Contains(a, secret) {
			tb.Errorf("the secret is in %s's argv at position %d of %d", what, i, len(args))
		}
	}
}
