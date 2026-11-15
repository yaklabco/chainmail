package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"time"

	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// Readiness: how long Apply waits for a started daemon to answer, how
// often it asks, and how many log lines it shows when the daemon does not.
const (
	readinessTimeout = 15 * time.Second
	readinessPoll    = 250 * time.Millisecond
	logTailLines     = 20
)

// Result values for Result.Test.
const (
	testOK      = "ok"
	testSkipped = "skipped"
	testFailed  = "failed"
)

// The auth states the CLI prints for an account, as protojson spells them.
const authStateRequired = "AUTH_STATE_REQUIRED"

// changedHint is the hint on a refusal that a rerun fixes.
const changedHint = "run chainmail bootstrap again"

// newPasswordHint is the hint for a password the provider rejects.
const newPasswordHint = "run bootstrap again and enter the new password"

// exitAuthRejected is the CLI's exit status for rejected credentials,
// api.ExitAuth.
const exitAuthRejected = 5

// Applier applies a plan as root. Every field but Resolver is required,
// except that Stderr may be nil.
type Applier struct {
	Layout   Layout
	Files    system.Files
	Accounts system.Accounts
	Service  system.Service
	// AsDaemon runs the installed binary as the daemon user. Apply always
	// passes --socket, since the command sees none of the caller's
	// settings. It must look the daemon user up when it is called: a
	// fresh install creates that user part way through Apply.
	AsDaemon func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error
	// Executable is the path of the running binary, copied to l.Binary. It
	// must not be a symlink.
	Executable string
	// OpenTTY opens the controlling terminal, or fails when there is none.
	OpenTTY func() (io.ReadWriteCloser, error)
	Now     func() time.Time
	Sleep   func(time.Duration)
	Stderr  io.Writer // progress lines
	// Resolver is not consulted: the request carries the detection the
	// user confirmed, and Build checks it against the presets.
	Resolver Resolver
}

// Input is what bootstrap apply reads from bootstrap.
type Input struct {
	Plan    Plan
	Request Request // to rebuild the plan under the lock
	Secret  []byte  // empty when the plan needs none
	// Health is the Current.Health the plan was built with. The rebuild
	// under the lock uses it too, so the two plans compare like with like.
	Health *AccountHealth
	// SudoUID and SudoUser name the user who ran sudo, from SUDO_UID and
	// SUDO_USER, never from the plan. Plan.InvokingUser must be that user.
	SudoUID  uint32
	SudoUser string
}

// Result is what bootstrap apply reports back.
type Result struct {
	Account        string            `json:"account"`
	Provider       string            `json:"provider"`
	Paths          map[string]string `json:"paths"`
	ServiceRunning bool              `json:"service_running"`
	Test           string            `json:"test"` // "ok", "skipped", "failed"
	KeyShown       bool              `json:"key_shown"`
	Warnings       []string          `json:"warnings,omitempty"`
	NextSteps      []string          `json:"next_steps"`
}

// AuthError is a credential problem. The CLI reports it as
// api.ReasonAuthRejected (exit 5) with Hint as the hint.
type AuthError struct {
	Msg  string
	Hint string
}

func (e *AuthError) Error() string { return e.Msg }

// ErrNotReady is returned when the daemon did not answer in 15 seconds.
var ErrNotReady = errors.New("daemon did not become ready")

// Apply validates, locks, re-inspects, rebuilds, applies, waits, stores
// the secret, tests and returns the result.
//
// Nothing is written before the plan is validated and the user who ran
// sudo is checked. The only write before the lock is the config
// directory, which holds the lock file, when it does not exist yet. Under
// the lock Apply inspects the installation again, rebuilds the plan and
// refuses unless it is the same, so a plan can only make the changes this
// binary would plan itself.
//
// A failed account test still returns the Result, with Test "failed".
func (a *Applier) Apply(ctx context.Context, in Input) (Result, error) {
	l := a.Layout
	if err := Validate(l, in.Plan); err != nil {
		return Result{}, err
	}
	if in.Plan.NeedSecret && len(in.Secret) == 0 {
		return Result{}, refuse(validateHint, "the plan stores the secret %s, but none was given", in.Plan.SecretName)
	}
	if err := a.checkSudoUser(ctx, in); err != nil {
		return Result{}, err
	}
	binary, err := a.readExecutable(in.Request.ExecSHA256)
	if err != nil {
		return Result{}, err
	}

	if err := a.ensureConfigDir(); err != nil {
		return Result{}, err
	}
	unlock, err := a.Files.Lock(l.Lock)
	if err != nil {
		return Result{}, fmt.Errorf("lock %s: %w", l.Lock, err)
	}
	defer func() { _ = unlock() }() // the process exits soon after; the kernel drops the lock anyway

	cur, err := Inspect(ctx, l, a.Files, a.Accounts, in.SudoUser)
	if err != nil {
		return Result{}, err
	}
	cur.Health = in.Health
	rebuilt, err := Build(l, cur, in.Request)
	if err != nil {
		return Result{}, err
	}
	if !Same(rebuilt, in.Plan) {
		return Result{}, refuse(changedHint, "the installation changed while bootstrap was running; run it again")
	}

	r := &applyRun{a: a, l: l, in: in, plan: in.Plan, binary: binary, res: newResult(l, in.Plan)}
	if r.pre, err = a.Service.State(ctx); err != nil {
		return Result{}, fmt.Errorf("read the state of %s: %w", l.ServiceLabel, err)
	}
	if err := r.checkRejected(ctx); err != nil {
		return Result{}, err
	}
	return r.run(ctx)
}

// checkSudoUser refuses unless the user who ran sudo exists with
// SUDO_UID's uid and is the plan's invoking user.
func (a *Applier) checkSudoUser(ctx context.Context, in Input) error {
	hint := "run chainmail bootstrap as yourself; it runs bootstrap apply through sudo"
	if in.SudoUser == "" || in.SudoUser == "root" || in.SudoUID == 0 {
		return refuse(hint, "SUDO_UID and SUDO_USER must name the user who ran sudo, not root")
	}
	u, ok, err := a.Accounts.LookupUser(ctx, in.SudoUser)
	if err != nil {
		return fmt.Errorf("look up %s: %w", in.SudoUser, err)
	}
	if !ok || u.UID != in.SudoUID {
		return refuse(hint, "SUDO_UID %d is not the uid of SUDO_USER %q", in.SudoUID, in.SudoUser)
	}
	if in.Plan.InvokingUser != in.SudoUser {
		return refuse(hint, "the plan was made for invoking user %q, but %q ran sudo", in.Plan.InvokingUser, in.SudoUser)
	}
	return nil
}

// readExecutable reads the running binary and refuses it unless its
// SHA-256 is the one bootstrap planned with.
func (a *Applier) readExecutable(want string) ([]byte, error) {
	data, err := a.Files.ReadFile(a.Executable)
	if err != nil {
		return nil, fmt.Errorf("read the running binary: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return nil, refuse(changedHint, "the running binary %s changed while bootstrap was running", a.Executable)
	}
	return data, nil
}

// ensureConfigDir makes the config directory, which holds the lock file,
// when it does not exist. An existing one is left as it is for the trust
// check under the lock.
func (a *Applier) ensureConfigDir() error {
	e, _ := a.Layout.Entry("config-dir")
	info, err := a.Files.Lstat(e.Path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", e.Path, err)
	}
	if info.Exists {
		return nil
	}
	if err := a.ensureAncestors(e.Path); err != nil {
		return err
	}
	if err := a.Files.EnsureDir(e.Path, 0, 0, e.Mode); err != nil {
		return fmt.Errorf("create %s: %w", e.Path, err)
	}
	return nil
}

// ensureAncestors creates the missing directories above path, from the
// top down, owned by root and the root group with mode 0755. macOS has no
// /private/var/lib, for example.
func (a *Applier) ensureAncestors(path string) error {
	var missing []string
	for dir := filepath.Dir(path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		info, err := a.Files.Lstat(dir)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", dir, err)
		}
		if info.Exists {
			break
		}
		missing = append(missing, dir)
	}
	for _, dir := range slices.Backward(missing) {
		if err := a.Files.EnsureDir(dir, 0, 0, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// newResult is the Result before anything ran.
func newResult(l Layout, p Plan) Result {
	paths := map[string]string{
		"binary":    l.Binary,
		"config":    l.Config,
		"vault-key": l.VaultKey,
		"state-dir": l.StateDir,
		"vault":     l.Vault,
		"socket":    l.Socket,
		"service":   l.Service,
	}
	if l.LogFile != "" {
		paths["log"] = l.LogFile
	}
	steps := []string{"log out and back in, or run: newgrp " + l.ClientGroup, "chainmail accounts list"}
	return Result{
		Account:   p.Account.Name,
		Provider:  p.Account.Provider,
		Paths:     paths,
		Test:      testSkipped,
		NextSteps: append(steps, p.NextSteps...),
	}
}

// applyRun is one Apply after the plan passed the checks under the lock.
type applyRun struct {
	a      *Applier
	l      Layout
	in     Input
	plan   Plan
	binary []byte
	res    Result

	pre            system.ServiceState // before Apply changed anything
	serviceWritten bool                // the service definition was written
	ready          bool                // a start in this run answered
	secretStored   bool
	skipTest       bool // the OAuth login could not run, so a test would fail
}

// run applies the plan's actions in order, then the steps every run
// ends with.
func (r *applyRun) run(ctx context.Context) (Result, error) {
	for i, act := range r.plan.Actions {
		r.progress(i, act.Note)
		if err := r.step(ctx, act); err != nil {
			var ae *AuthError
			if errors.As(err, &ae) {
				r.finish(ctx)
				return r.res, err
			}
			var ref *Refusal
			if errors.As(err, &ref) || errors.Is(err, ErrNotReady) {
				return Result{}, err
			}
			return Result{}, fmt.Errorf("step %d, %s: %w", i+1, act.Note, err)
		}
	}
	if err := r.ensureRunning(ctx); err != nil {
		return Result{}, err
	}
	if err := r.storeSecret(ctx); err != nil {
		return Result{}, err
	}
	r.finish(ctx)
	return r.res, nil
}

// progress prints the step being run.
func (r *applyRun) progress(i int, note string) {
	if r.a.Stderr != nil {
		_, _ = fmt.Fprintf(r.a.Stderr, "[%d/%d] %s\n", i+1, len(r.plan.Actions), note) // progress only; a lost line changes nothing
	}
}

// step runs one action.
func (r *applyRun) step(ctx context.Context, act Action) error {
	switch act.Kind {
	case ActCreateGroup:
		_, err := r.a.Accounts.CreateGroup(ctx, r.l.ClientGroup)
		return err
	case ActCreateUser:
		home := r.l.StateDir
		if r.l.Platform == Darwin {
			home = "/var/empty"
		}
		_, err := r.a.Accounts.CreateDaemonUser(ctx, r.l.DaemonUser, r.l.ClientGroup, home)
		return err
	case ActAddMember:
		return r.a.Accounts.AddMember(ctx, r.l.ClientGroup, act.Name)
	case ActEnsureDir:
		return r.ensureDir(ctx, act.Entry)
	case ActInstallBin:
		return r.writeFile(ctx, "binary", r.binary)
	case ActCreateKey:
		return r.createKey(ctx)
	case ActWriteConfig:
		return r.writeFile(ctx, "config", act.Data)
	case ActWriteService:
		if err := r.writeFile(ctx, "service", act.Data); err != nil {
			return err
		}
		r.serviceWritten = true
		return nil
	case ActActivate:
		return r.activate(ctx)
	case ActStoreSecret:
		if err := r.ensureRunning(ctx); err != nil {
			return err
		}
		return r.storeSecret(ctx)
	case ActAuthLogin:
		if err := r.ensureRunning(ctx); err != nil {
			return err
		}
		return r.login(ctx, act.Name)
	case ActTestAccount:
		if err := r.ensureRunning(ctx); err != nil {
			return err
		}
		if err := r.storeSecret(ctx); err != nil {
			return err
		}
		return r.test(ctx, act.Name)
	default:
		return fmt.Errorf("unknown step %q", act.Kind)
	}
}

// ids resolves an entry's owner and group to ids. Root and the root
// group are 0 on both platforms.
func (r *applyRun) ids(ctx context.Context, e Entry) (uid, gid uint32, err error) {
	if e.Owner == OwnerDaemon {
		u, ok, err := r.a.Accounts.LookupUser(ctx, r.l.DaemonUser)
		if err != nil {
			return 0, 0, fmt.Errorf("look up %s: %w", r.l.DaemonUser, err)
		}
		if !ok {
			return 0, 0, fmt.Errorf("user %s does not exist", r.l.DaemonUser)
		}
		uid = u.UID
	}
	if e.Group != GroupRoot {
		name := groupName(r.l, e.Group)
		g, ok, err := r.a.Accounts.LookupGroup(ctx, name)
		if err != nil {
			return 0, 0, fmt.Errorf("look up group %s: %w", name, err)
		}
		if !ok {
			return 0, 0, fmt.Errorf("group %s does not exist", name)
		}
		gid = g.GID
	}
	return uid, gid, nil
}

// entry returns the layout entry called name, with its ids.
func (r *applyRun) entry(ctx context.Context, name string) (e Entry, uid, gid uint32, err error) {
	e, ok := r.l.Entry(name)
	if !ok {
		return Entry{}, 0, 0, fmt.Errorf("no layout entry %q", name)
	}
	uid, gid, err = r.ids(ctx, e)
	return e, uid, gid, err
}

// ensureDir makes the directory entry called name, and any missing
// directory above it, and sets its owner and mode.
func (r *applyRun) ensureDir(ctx context.Context, name string) error {
	e, uid, gid, err := r.entry(ctx, name)
	if err != nil {
		return err
	}
	if err := r.a.ensureAncestors(e.Path); err != nil {
		return err
	}
	return r.a.Files.EnsureDir(e.Path, uid, gid, e.Mode)
}

// writeFile replaces the file entry called name with data.
func (r *applyRun) writeFile(ctx context.Context, name string, data []byte) error {
	e, uid, gid, err := r.entry(ctx, name)
	if err != nil {
		return err
	}
	if err := r.a.ensureAncestors(e.Path); err != nil {
		return err
	}
	return r.a.Files.WriteFile(e.Path, data, uid, gid, e.Mode)
}

// createKey makes the vault key, which must not exist, and shows it when
// the user asked for that.
func (r *applyRun) createKey(ctx context.Context) error {
	e, uid, gid, err := r.entry(ctx, "vault-key")
	if err != nil {
		return err
	}
	key, err := GenerateKey(r.a.Now())
	if err != nil {
		return err
	}
	defer clear(key)
	if err := r.a.Files.CreateExclusive(e.Path, key, uid, gid, e.Mode); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return refuse(changedHint,
				"the vault key %s appeared while bootstrap was running; another bootstrap made it, so this one stops", e.Path)
		}
		return err
	}
	if r.plan.Backup == BackupShow {
		r.showKey(key)
	}
	return nil
}

// showKey shows key on the terminal. Any problem is a warning: the key is
// already in place, and stopping now would leave a half-done install.
func (r *applyRun) showKey(key []byte) {
	tty, err := r.a.OpenTTY()
	if err != nil {
		r.warn("could not open the terminal to show the key; it was not shown")
		return
	}
	defer func() { _ = tty.Close() }() // nothing was buffered; the key is already out
	err = ShowKey(tty, key)
	switch {
	case err == nil:
		r.res.KeyShown = true
	case errors.Is(err, io.EOF):
		r.res.KeyShown = true
		r.warn("the key was shown, but the terminal closed before Enter, so saving it was not confirmed")
	default:
		r.warn("could not show the key on the terminal: " + err.Error())
	}
}

// activate runs the activate-service step: load or start the service,
// restart it when it was running before, and wait for it to answer.
func (r *applyRun) activate(ctx context.Context) error {
	start := r.a.Now()
	if err := r.a.Service.Activate(ctx, r.serviceWritten); err != nil {
		return err
	}
	// systemd's enable --now leaves a running unit alone, so a running
	// service would keep the old binary or config.
	if r.pre.Running {
		if err := r.a.Service.Restart(ctx); err != nil {
			return err
		}
	}
	return r.waitReady(ctx, start)
}

// ensureRunning starts a service that was stopped before Apply and that
// no step of this run started.
func (r *applyRun) ensureRunning(ctx context.Context) error {
	if r.ready || r.pre.Running {
		return nil
	}
	start := r.a.Now()
	if err := r.a.Service.Activate(ctx, false); err != nil {
		return fmt.Errorf("start %s: %w", r.l.ServiceLabel, err)
	}
	return r.waitReady(ctx, start)
}

// waitReady polls admin status until the daemon reports a start later
// than start, for at most readinessTimeout. A stale socket left by an
// old daemon fails to connect or reports an older start, so it is never
// taken for the new one.
func (r *applyRun) waitReady(ctx context.Context, start time.Time) error {
	deadline := r.a.Now().Add(readinessTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for the daemon: %w", err)
		}
		st, err := r.status(ctx)
		if err == nil && st.StartedAt.After(start) {
			r.ready = true
			return nil
		}
		if !r.a.Now().Before(deadline) {
			break
		}
		r.a.Sleep(readinessPoll)
	}
	logs, err := r.a.Service.Logs(ctx, logTailLines)
	if err != nil {
		return fmt.Errorf("%w within %v; reading its log failed too: %w", ErrNotReady, readinessTimeout, err)
	}
	return fmt.Errorf("%w within %v; the last %d log lines:\n%s", ErrNotReady, readinessTimeout, logTailLines, bytes.TrimRight(logs, "\n"))
}

// daemonStatus is the part of admin status's JSON Apply reads.
type daemonStatus struct {
	StartedAt   time.Time `json:"started_at"`
	SecretNames []string  `json:"secret_names"`
}

// daemonAccounts is the part of accounts list's JSON Apply reads.
type daemonAccounts struct {
	Accounts []daemonAccount `json:"accounts"`
}

// daemonAccount is one account in accounts list's JSON.
type daemonAccount struct {
	Name      string `json:"name"`
	AuthState string `json:"auth_state"`
}

// status runs admin status as the daemon user.
func (r *applyRun) status(ctx context.Context) (daemonStatus, error) {
	var st daemonStatus
	err := r.daemonJSON(ctx, &st, "admin", "status")
	return st, err
}

// daemonJSON runs a CLI command as the daemon user with JSON output and
// decodes it into v.
func (r *applyRun) daemonJSON(ctx context.Context, v any, args ...string) error {
	var out bytes.Buffer
	if err := r.asDaemon(ctx, nil, &out, append(args, "--output", "json")...); err != nil {
		return err
	}
	if err := json.NewDecoder(&out).Decode(v); err != nil {
		return fmt.Errorf("decode the output of %v: %w", args, err)
	}
	return nil
}

// asDaemon runs the installed binary as the daemon user with --socket.
func (r *applyRun) asDaemon(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	return r.a.AsDaemon(ctx, stdin, stdout, append(args, "--socket", r.l.Socket)...)
}

// checkRejected looks, before a restart, for an account the provider
// already rejected: the daemon keeps that only in memory, and a restart
// would make it try the bad password again. Without a new secret Apply
// stops there.
func (r *applyRun) checkRejected(ctx context.Context) error {
	if !r.plan.ServiceChanged || !r.pre.Running || len(r.in.Secret) > 0 || !r.rejected(ctx) {
		return nil
	}
	return &AuthError{
		Msg: fmt.Sprintf("the provider rejected the stored credentials of account %s; "+
			"bootstrap did not restart the daemon, so it does not try them again", r.plan.Account.Name),
		Hint: newPasswordHint,
	}
}

// rejected reports whether the running daemon has the plan's secret
// stored and still says the account needs auth. A daemon that cannot be
// asked says nothing, so that is not a rejection. Only a password can be
// rejected this way: an OAuth account that needs auth needs a new login,
// which a restart does not hurt and a new client secret does not fix.
func (r *applyRun) rejected(ctx context.Context) bool {
	if r.plan.Account.Auth != provider.AuthPassword {
		return false
	}
	var list daemonAccounts
	if err := r.daemonJSON(ctx, &list, "accounts", "list"); err != nil {
		return false
	}
	st, err := r.status(ctx)
	if err != nil {
		return false
	}
	name := r.plan.Account.Name
	required := slices.ContainsFunc(list.Accounts, func(acc daemonAccount) bool {
		return acc.Name == name && acc.AuthState == authStateRequired
	})
	return required && slices.Contains(st.SecretNames, r.plan.SecretName)
}

// storeSecret stores the secret the user gave, once.
func (r *applyRun) storeSecret(ctx context.Context) error {
	if r.secretStored || len(r.in.Secret) == 0 {
		return nil
	}
	if err := r.ensureRunning(ctx); err != nil {
		return err
	}
	stdin := bytes.NewReader(r.in.Secret)
	if err := r.asDaemon(ctx, stdin, io.Discard, "secret", "set", r.plan.SecretName, "--output", "json"); err != nil {
		return fmt.Errorf("store the secret %s: %w", r.plan.SecretName, err)
	}
	r.secretStored = true
	return nil
}

// login runs the OAuth login for account on the terminal.
func (r *applyRun) login(ctx context.Context, account string) error {
	tty, err := r.a.OpenTTY()
	if err != nil {
		r.warn("could not open the terminal for the OAuth login of account " + account)
		r.res.NextSteps = append(r.res.NextSteps, "chainmail auth login "+account)
		r.skipTest = true
		return nil
	}
	defer func() { _ = tty.Close() }() // the login already finished
	if err := r.asDaemon(ctx, tty, tty, "auth", "login", account); err != nil {
		return &AuthError{
			Msg:  fmt.Sprintf("the OAuth login for account %s did not finish: %v", account, err),
			Hint: "run: chainmail auth login " + account,
		}
	}
	return nil
}

// test runs the account test. Rejected credentials are an AuthError;
// any other failure is a warning, since the install itself is done.
func (r *applyRun) test(ctx context.Context, account string) error {
	if r.skipTest {
		return nil
	}
	err := r.asDaemon(ctx, nil, io.Discard, "accounts", "test", account, "--output", "json")
	if err == nil {
		r.res.Test = testOK
		return nil
	}
	r.res.Test = testFailed
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) && exit.ExitCode() == exitAuthRejected {
		hint := newPasswordHint
		if r.plan.Account.Auth == provider.AuthOAuth2 {
			hint = "run: chainmail auth login " + account
		}
		return &AuthError{Msg: "account " + account + " failed its connection test: the server rejected the credentials", Hint: hint}
	}
	r.warn(fmt.Sprintf("account %s failed its connection test (%v); run: chainmail accounts test %s", account, err, account))
	return nil
}

// finish records the service state at the end.
func (r *applyRun) finish(ctx context.Context) {
	st, err := r.a.Service.State(ctx)
	if err != nil {
		r.warn("could not read the state of " + r.l.ServiceLabel + ": " + err.Error())
		return
	}
	r.res.ServiceRunning = st.Running
}

// warn adds a warning to the result.
func (r *applyRun) warn(msg string) { r.res.Warnings = append(r.res.Warnings, msg) }
