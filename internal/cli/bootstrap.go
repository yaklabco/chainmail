package cli

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
	"maps"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	chainmailv1 "github.com/yaklabco/chainmail/gen/chainmail/v1"
	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/auth"
	"github.com/yaklabco/chainmail/internal/client"
	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/setup"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// Limits and fixed texts of chainmail bootstrap.
const (
	// maxApplyPayload caps the JSON bootstrap apply reads from stdin.
	maxApplyPayload = 1 << 20
	// maxApplyResult caps the JSON bootstrap reads back from bootstrap apply.
	maxApplyResult = 1 << 20
	// healthTimeout bounds asking a running daemon about the account.
	healthTimeout = 2 * time.Second
	// askTries is how many answers a yes or no question reads.
	askTries = 3
	// maxDaemonStderr caps the stderr of a daemon-user command quoted in
	// an error.
	maxDaemonStderr = 500
	// maxVersionText caps the version another daemon reports, quoted in a
	// refusal.
	maxVersionText = 64

	// nixosMarker exists on every NixOS system.
	nixosMarker = "/etc/NIXOS"

	// sudoDisclosure ends every summary.
	sudoDisclosure = "An agent that can run sudo without a password can get around all of this. " +
		"Run agents from an account that cannot."

	showNeedsTerminal = "--key-backup show needs a terminal; captured output would hold the key"
	applyHint         = "run chainmail bootstrap as yourself; it runs bootstrap apply through sudo"
)

// bootstrapFlags are chainmail bootstrap's flags.
type bootstrapFlags struct {
	name, provider             string
	oauthClientID, oauthClient string
	fromPass                   string
	keyBackup                  string
	keyBackupGiven             bool
	dryRun, yes                bool
	nixDir                     string
}

// applyPayload is the one JSON object bootstrap sends bootstrap apply on
// stdin. The secret is only ever here, never in argv or the environment.
type applyPayload struct {
	Plan    setup.Plan           `json:"plan"`
	Request setup.Request        `json:"request"`
	Secret  []byte               `json:"secret,omitempty"`
	Health  *setup.AccountHealth `json:"health,omitempty"`
}

// newBootstrapCmd builds chainmail bootstrap and its hidden apply step.
func newBootstrapCmd(ctx context.Context, env Env, g *globals) *cobra.Command {
	var f bootstrapFlags
	cmd := &cobra.Command{
		Use:   "bootstrap <email>",
		Short: "Set up a production daemon and one account, with one sudo call",
		Long: "Set up chainmail on this machine: a daemon that runs as its own user, with its config, vault key " +
			"and service, and one account whose credentials are stored and tested.\n\n" +
			"Run it as the user who will use chainmail, never as root. It prints its plan, asks for the " +
			"account's app password or OAuth client secret and for confirmation, then asks for sudo once. " +
			"A rerun adds another account or repairs the install, and changes only what differs.\n\n" +
			"On NixOS it writes Nix files for your configuration to import instead, with the vault key sealed " +
			"by systemd-creds, and never edits the configuration or rebuilds. Run it again after the rebuild " +
			"to store the credentials and test the account.",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f.keyBackupGiven = cmd.Flags().Changed("key-backup")
			return runBootstrap(ctx, env, g, f, args[0])
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.name, "name", "", "account name (default: the address's local part)")
	fl.StringVar(&f.provider, "provider", "",
		"provider preset, one of "+strings.Join(setup.BootstrapProviders(), ", ")+" (default: detected from the address)")
	fl.StringVar(&f.oauthClientID, "oauth-client-id", "", "OAuth client ID, needed by google-workspace")
	fl.StringVar(&f.oauthClient, "oauth-client", "", "name of the OAuth client table (default google)")
	fl.StringVar(&f.fromPass, "from-pass", "", "read the password or client secret from this pass entry (first line)")
	fl.StringVar(&f.keyBackup, "key-backup", string(setup.BackupNone),
		"vault key backup when a new key is made: none, or show to print it once on the terminal")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the plan and change nothing")
	fl.BoolVar(&f.yes, "yes", false, "skip the confirmation and the questions that have defaults")
	fl.StringVar(&f.nixDir, "nix-dir", "", "directory for the Nix files bootstrap writes on NixOS (default ./chainmail-nixos)")
	cmd.AddCommand(skipSettings(newBootstrapApplyCmd(ctx, env)))
	return cmd
}

// runBootstrap is the unprivileged phase: detect, inspect, plan, ask,
// then run bootstrap apply once through sudo and report.
func runBootstrap(ctx context.Context, env Env, g *globals, f bootstrapFlags, address string) error {
	uid := env.getuid()
	if uid == 0 {
		return &setup.Refusal{
			Msg:  "run bootstrap as the user who will use chainmail; it asks for sudo itself",
			Hint: "run chainmail bootstrap without sudo",
		}
	}
	l, err := bootstrapLayout(env)
	if err != nil {
		return err
	}
	if l.Platform == setup.NixOS {
		return runBootstrapNixOS(ctx, env, g, f, l, address)
	}
	files := env.setupFiles()
	exe, req, err := startBootstrap(ctx, env, files, f, address)
	if err != nil {
		return err
	}
	invoking, err := env.username()
	if err != nil {
		return err
	}
	cur, err := setup.Inspect(ctx, l, files, env.setupAccounts(), invoking)
	if err != nil {
		return err
	}
	if err := checkInvokingUID(uid, invoking, cur.InvokingUser); err != nil {
		return err
	}
	if err := refuseOtherProduction(ctx, env, g, l); err != nil {
		return err
	}
	cur.Health = askHealth(ctx, env, l, address, f.name)
	plan, err := setup.Build(l, cur, req)
	if err != nil {
		return err
	}

	if f.dryRun {
		return printDryRun(env, g, l, plan)
	}
	replan := func(req setup.Request) (setup.Plan, error) { return setup.Build(l, cur, req) }
	plan, secret, err := askPlan(ctx, env, f, address, setup.ActCreateKey, replan, &req, plan)
	if err != nil {
		return err
	}
	defer clear(secret)
	res, err := sudoApply(ctx, env, exe, applyPayload{Plan: plan, Request: req, Secret: secret, Health: cur.Health})
	if err != nil {
		return err
	}
	return reportBootstrap(env, g, l, res)
}

// startBootstrap checks the flags, detects the provider and returns the
// running binary and the request every platform plans from.
func startBootstrap(ctx context.Context, env Env, files system.Files, f bootstrapFlags, address string) (string, setup.Request, error) {
	backup, err := setup.ParseBackup(f.keyBackup)
	if err != nil {
		return "", setup.Request{}, err
	}
	if backup == setup.BackupShow && !env.canShowKey() {
		return "", setup.Request{}, &setup.Refusal{Msg: showNeedsTerminal, Hint: "run bootstrap in a terminal, or pass --key-backup none"}
	}
	if err := checkPassPath(f.fromPass); err != nil {
		return "", setup.Request{}, err
	}

	det, err := setup.Detect(ctx, env.resolver(), address, f.provider)
	if err != nil {
		return "", setup.Request{}, err
	}
	if det.ViaMX && env.ReadLine != nil && !f.yes {
		if err := confirmMX(env, address, det.Provider); err != nil {
			return "", setup.Request{}, err
		}
	}

	exe, err := env.executable()
	if err != nil {
		return "", setup.Request{}, err
	}
	req, err := bootstrapRequest(env, files, exe, address, f, det, backup)
	if err != nil {
		return "", setup.Request{}, err
	}
	return exe, req, nil
}

// checkInvokingUID refuses to go on when the user the host reports for
// invoking has another uid than the process.
func checkInvokingUID(uid int, invoking string, u system.User) error {
	if int64(u.UID) != int64(uid) {
		return fmt.Errorf("bootstrap runs as uid %d, but user %s has uid %d", uid, invoking, u.UID)
	}
	return nil
}

// askPlan prints the plan, then asks for what it needs: the secret, the
// key backup choice when the plan has an action of kind makeKey, and the
// confirmation. It returns the plan to apply, planned again through
// replan when the backup choice changed it, and the secret, which the
// caller clears.
func askPlan(ctx context.Context, env Env, f bootstrapFlags, address string, makeKey setup.ActionKind,
	replan func(setup.Request) (setup.Plan, error), req *setup.Request, plan setup.Plan,
) (setup.Plan, []byte, error) {
	if err := writeString(env.Stderr, "chainmail bootstrap will:\n"); err != nil {
		return setup.Plan{}, nil, err
	}
	if err := plan.Render(env.Stderr); err != nil {
		return setup.Plan{}, nil, err
	}
	if err := writeString(env.Stderr, "\n"); err != nil {
		return setup.Plan{}, nil, err
	}
	if env.ReadLine == nil && !f.yes {
		return setup.Plan{}, nil, &setup.Refusal{
			Msg:  "bootstrap needs a terminal to confirm the plan",
			Hint: "run it in a terminal, or pass --yes to go ahead without confirming",
		}
	}

	secret, err := bootstrapSecret(ctx, env, plan, address, f.fromPass)
	if err != nil {
		return setup.Plan{}, nil, err
	}
	if plan, err = confirmPlan(env, f, makeKey, replan, req, plan); err != nil {
		clear(secret)
		return setup.Plan{}, nil, err
	}
	return plan, secret, nil
}

// confirmPlan asks the key backup choice when the plan makes a key (an
// action of kind makeKey) and no flag answered it, planning again with the
// answer through replan, then asks the user to confirm. --yes skips both.
func confirmPlan(env Env, f bootstrapFlags, makeKey setup.ActionKind, replan func(setup.Request) (setup.Plan, error),
	req *setup.Request, plan setup.Plan,
) (setup.Plan, error) {
	if f.yes {
		return plan, nil
	}
	makesKey := slices.ContainsFunc(plan.Actions, func(a setup.Action) bool { return a.Kind == makeKey })
	if makesKey && !f.keyBackupGiven && env.ReadLine != nil {
		var err error
		if plan, err = askBackup(env, replan, req, plan); err != nil {
			return setup.Plan{}, err
		}
	}
	ok, err := askYesNo(env, "Go ahead? [y/N] ", false)
	if err != nil {
		return setup.Plan{}, err
	}
	if !ok {
		return setup.Plan{}, &setup.Refusal{
			Msg:  "bootstrap stopped before changing anything",
			Hint: "run it again and answer y, or pass --yes",
		}
	}
	return plan, nil
}

// bootstrapRequest collects the request from the flags, the detection
// and the running binary exe.
func bootstrapRequest(env Env, files system.Files, exe, address string, f bootstrapFlags, det setup.Detection, backup setup.Backup) (setup.Request, error) {
	sum, err := fileSHA256(files, exe)
	if err != nil {
		return setup.Request{}, err
	}
	imap, smtp, err := setup.E2EOverrides(env.Getenv)
	if err != nil {
		return setup.Request{}, usageError{err: err, hint: "set CHAINMAIL_E2E_IMAP and CHAINMAIL_E2E_SMTP to host:port"}
	}
	return setup.Request{
		Address:       address,
		Name:          f.name,
		Detection:     det,
		OAuthClientID: f.oauthClientID,
		OAuthClient:   f.oauthClient,
		ExecSHA256:    sum,
		Backup:        backup,
		Terminal:      env.ReadLine != nil,
		IMAP:          imap,
		SMTP:          smtp,
	}, nil
}

// fileSHA256 is the hex SHA-256 of the file at path.
func fileSHA256(files system.Files, path string) (string, error) {
	data, err := files.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the running binary: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// bootstrapLayout is the layout for the platform bootstrap runs on: NixOS
// when a Linux host has /etc/NIXOS.
func bootstrapLayout(env Env) (setup.Layout, error) {
	switch p := setup.Platform(env.goos()); p {
	case setup.Linux:
		nixos, err := isNixOS(env.setupFiles())
		if err != nil {
			return setup.Layout{}, err
		}
		if nixos {
			return setup.LayoutFor(setup.NixOS), nil
		}
		return setup.LayoutFor(p), nil
	case setup.Darwin:
		return setup.LayoutFor(p), nil
	default:
		return setup.Layout{}, &setup.Refusal{
			Msg:  fmt.Sprintf("bootstrap does not support %s", p),
			Hint: "bootstrap supports Linux with systemd, NixOS and macOS",
		}
	}
}

// isNixOS reports whether the NixOS marker exists. A marker it may not
// look at counts as missing.
func isNixOS(files system.Files) (bool, error) {
	info, err := files.Lstat(nixosMarker)
	if err != nil && !errors.Is(err, fs.ErrPermission) {
		return false, fmt.Errorf("inspect %s: %w", nixosMarker, err)
	}
	return info.Exists, nil
}

// checkPassPath refuses a pass entry that pass would read as an option
// or that leaves the store.
func checkPassPath(entry string) error {
	if strings.HasPrefix(entry, "-") || slices.Contains(strings.Split(entry, "/"), "..") {
		return &exitError{reason: api.ReasonInvalidArgument, msg: fmt.Sprintf("invalid pass entry %q", entry)}
	}
	return nil
}

// providerHint lists the presets --provider accepts.
func providerHint() string {
	return "pass --provider <preset>, one of: " + strings.Join(setup.BootstrapProviders(), ", ")
}

// confirmMX asks the user to confirm a provider the MX lookup found.
func confirmMX(env Env, address, prov string) error {
	_, domain, err := setup.ParseAddress(address)
	if err != nil {
		return err
	}
	ok, err := askYesNo(env, fmt.Sprintf("Use provider %s for %s? [Y/n] ", prov, domain), true)
	if err != nil {
		return err
	}
	if !ok {
		return &setup.Refusal{Msg: fmt.Sprintf("provider %s for %s was not confirmed", prov, domain), Hint: providerHint()}
	}
	return nil
}

// askYesNo asks prompt on stderr and reads the answer from the terminal.
// An empty answer is def; the end of input is no. After askTries answers
// that are neither yes nor no, the answer is no.
func askYesNo(env Env, prompt string, def bool) (bool, error) {
	for range askTries {
		if err := writeString(env.Stderr, prompt); err != nil {
			return false, err
		}
		line, err := env.ReadLine()
		ended := errors.Is(err, io.EOF)
		if err != nil && !ended {
			return false, fmt.Errorf("read the answer: %w", err)
		}
		switch answer := strings.ToLower(strings.TrimSpace(line)); {
		case answer == "y" || answer == "yes":
			return true, nil
		case answer == "n" || answer == "no", ended:
			// Nothing more will come, so asking again cannot help.
			return false, nil
		case answer == "":
			return def, nil
		}
	}
	return false, nil
}

// askHealth asks the daemon at the planned socket about the account. Any
// failure, such as a user not yet in the client group, leaves it unknown.
func askHealth(ctx context.Context, env Env, l setup.Layout, address, name string) *setup.AccountHealth {
	if name == "" {
		var err error
		if name, err = setup.AccountName(address); err != nil {
			return nil // Build refuses the address with the reason
		}
	}
	if !config.ValidName(name) {
		return nil
	}
	ask := env.AccountHealth
	if ask == nil {
		ask = daemonAccountHealth
	}
	h, err := ask(ctx, l.Socket, name)
	if err != nil {
		return nil
	}
	return h
}

// daemonAccountHealth asks the daemon at socket about account, giving up
// after healthTimeout.
func daemonAccountHealth(ctx context.Context, socket, account string) (*setup.AccountHealth, error) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	c := client.New(socket, "bootstrap")
	defer c.Close()
	list, err := c.Account.ListAccounts(ctx, connect.NewRequest(&chainmailv1.ListAccountsRequest{}))
	if err != nil {
		return nil, err
	}
	status, err := c.Admin.GetStatus(ctx, connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if err != nil {
		return nil, err
	}
	return accountHealthFrom(list.Msg, status.Msg, account), nil
}

// socketSelection is a socket the user's commands are pointed at, and
// what points them there.
type socketSelection struct {
	socket, source string
}

// selectedSockets lists the sockets this user's chainmail commands are
// pointed at in place of the layout's, each with what selects it. The
// first is the socket a client dials, resolved as every other command
// resolves it: --socket, then CHAINMAIL_SOCKET, then cli.toml, then the
// built-in default, which on macOS is not the layout's. CHAINMAIL_SOCKET
// and cli.toml follow when a setting before them hides them, since the
// user's next command without that setting would dial them.
func selectedSockets(env Env, g *globals, l setup.Layout) []socketSelection {
	var out []socketSelection
	add := func(socket, source string) {
		if socket == "" || sameSocket(env, socket, l.Socket) || slices.ContainsFunc(out, func(s socketSelection) bool { return s.socket == socket }) {
			return
		}
		out = append(out, socketSelection{socket: socket, source: source})
	}
	envSocket := env.Getenv(socketBinding.envVar)
	var fileSocket, path string
	if path = cliConfigPath(env); path != "" {
		// resolve already read it; a file that broke since is reported by
		// the next command.
		if file, err := readCLIConfig(path); err == nil {
			fileSocket, _ = file[socketBinding.key].(string)
		}
	}
	source := "the built-in default"
	switch {
	case g.socketFlag != "":
		source = "--" + socketBinding.flag
	case envSocket != "":
		source = socketBinding.envVar
	case fileSocket != "":
		source = path
	}
	add(g.socketPath(), source)
	add(envSocket, socketBinding.envVar)
	add(fileSocket, path)
	return out
}

// sameSocket reports whether a and b name the same socket, such as
// /var/run/chainmail/chainmail.sock and /run/chainmail/chainmail.sock
// where /var/run links to /run. The paths are read as written, never
// cleaned first: the kernel follows a symlink before the ".." after it,
// so /run/redirect/../chainmail is not /run/chainmail when
// /run/redirect is a symlink. When both files exist it compares them.
// Otherwise it compares their directories with every symlink resolved,
// and the names in them. Only when neither directory resolves does it
// compare the cleaned paths. When just one resolves they are different
// sockets, so the other is still asked about.
func sameSocket(env Env, a, b string) bool {
	stat := env.stat()
	if ai, err := stat(a); err == nil {
		if bi, err := stat(b); err == nil {
			return os.SameFile(ai, bi)
		}
	}
	ra, aErr := resolveSocketDir(env, a)
	rb, bErr := resolveSocketDir(env, b)
	switch {
	case aErr == nil && bErr == nil:
		return ra == rb
	case aErr != nil && bErr != nil:
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return false
}

// resolveSocketDir is socket with every symlink in its directory
// resolved. filepath.Split keeps the directory as written, where
// filepath.Dir would clean it.
func resolveSocketDir(env Env, socket string) (string, error) {
	dir, file := filepath.Split(socket)
	if dir == "" {
		dir = "."
	}
	resolved, err := env.evalSymlinks()(dir)
	if err != nil {
		return "", err
	}
	// resolved holds no symlink, so a final "." or ".." in file cleans
	// away as the kernel would resolve it.
	return filepath.Join(resolved, file), nil
}

// refuseOtherProduction asks the daemon at each socket the user selects in
// place of the layout's for its status. One that answers and is not in
// dev mode is another production installation, and bootstrap refuses to
// set up a second. A dev daemon, or a socket nothing answers on, is no
// reason to stop; one --socket, CHAINMAIL_SOCKET or cli.toml selects
// gets the warning clientSocketWarnings gives.
func refuseOtherProduction(ctx context.Context, env Env, g *globals, l setup.Layout) error {
	ask := env.DaemonStatus
	if ask == nil {
		ask = daemonStatus
	}
	for _, sel := range selectedSockets(env, g, l) {
		st, err := ask(ctx, sel.socket)
		if err != nil || st.Dev {
			continue
		}
		return &setup.Refusal{
			Msg: fmt.Sprintf("a production chainmail daemon (version %s) already answers at %s, which %s selects; "+
				"bootstrap would set up a second one at %s",
				clip(termText(st.Version), maxVersionText), sel.socket, sel.source, l.Socket),
			Hint: "use that installation, or stop and remove it before running bootstrap",
		}
	}
	return nil
}

// daemonStatus asks the daemon at socket for its dev mode and version,
// giving up after healthTimeout.
func daemonStatus(ctx context.Context, socket string) (DaemonInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	c := client.New(socket, "bootstrap")
	defer c.Close()
	status, err := c.Admin.GetStatus(ctx, connect.NewRequest(&chainmailv1.GetStatusRequest{}))
	if err != nil {
		return DaemonInfo{}, err
	}
	return DaemonInfo{Dev: status.Msg.GetDevMode(), Version: status.Msg.GetVersion()}, nil
}

// accountHealthFrom reads account's health from accounts list and admin
// status: whether it exists, whether its credentials are stored (the
// password, or the OAuth refresh token) and whether it can log in.
func accountHealthFrom(list *chainmailv1.ListAccountsResponse, status *chainmailv1.GetStatusResponse, account string) *setup.AccountHealth {
	h := &setup.AccountHealth{}
	for _, a := range list.GetAccounts() {
		if a.GetName() != account {
			continue
		}
		secret := "accounts/" + account
		if a.GetAuthMethod() == string(provider.AuthOAuth2) {
			secret = auth.RefreshTokenSecret(account)
		}
		h.Exists = true
		h.SecretStored = slices.Contains(status.GetSecretNames(), secret)
		h.AuthOK = a.GetAuthState() == chainmailv1.AuthState_AUTH_STATE_OK
	}
	return h
}

// bootstrapSecret reads the secret the plan stores, or nil when it
// stores none. It says first where an app password is made.
func bootstrapSecret(ctx context.Context, env Env, plan setup.Plan, address, fromPass string) ([]byte, error) {
	if !plan.NeedSecret {
		return nil, nil
	}
	if url := setup.AppPasswordURL(plan.Account.Provider); url != "" && fromPass == "" {
		if err := writeString(env.Stderr, fmt.Sprintf("Make an app password for %s at %s\n", address, url)); err != nil {
			return nil, err
		}
	}
	secret, err := readSecret(ctx, env, plan.SecretName, fromPass)
	if err != nil {
		return nil, err
	}
	// SASL PLAIN separates its fields with NUL, so a password holding one
	// could never log in.
	if strings.HasPrefix(plan.SecretName, "accounts/") && bytes.IndexByte(secret, 0) >= 0 {
		clear(secret)
		return nil, &exitError{reason: api.ReasonInvalidArgument, msg: "the password contains a NUL byte, which no login can send"}
	}
	return secret, nil
}

// askBackup asks the key backup choice and plans again with it through
// replan.
func askBackup(env Env, replan func(setup.Request) (setup.Plan, error), req *setup.Request, plan setup.Plan) (setup.Plan, error) {
	choice, err := setup.AskBackup(&lineReader{read: env.ReadLine}, env.Stderr)
	if err != nil {
		return setup.Plan{}, err
	}
	if choice == setup.BackupShow && !env.canShowKey() {
		return setup.Plan{}, &setup.Refusal{Msg: showNeedsTerminal, Hint: "run bootstrap in a terminal, or choose 1"}
	}
	if choice == req.Backup {
		return plan, nil
	}
	req.Backup = choice
	return replan(*req)
}

// lineReader turns ReadLine into an io.Reader that yields one line, with
// its newline, per call of read.
type lineReader struct {
	read func() (string, error)
	buf  []byte
}

func (r *lineReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		line, err := r.read()
		if err != nil {
			return 0, err
		}
		r.buf = append([]byte(line), '\n')
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// sudoApply runs bootstrap apply through sudo with in on stdin and reads
// its result. bootstrap apply reports its own failure on the shared
// stderr, so a failed run only passes its exit code on.
func sudoApply(ctx context.Context, env Env, exe string, in applyPayload) (setup.Result, error) {
	payload, err := json.Marshal(in)
	if err != nil {
		return setup.Result{}, fmt.Errorf("encode the plan: %w", err)
	}
	defer clear(payload)
	if env.Sudo == nil {
		return setup.Result{}, errors.New("running sudo is not available")
	}
	var out bytes.Buffer
	if err := env.Sudo(ctx, bytes.NewReader(payload), &out, exe, "bootstrap", "apply"); err != nil {
		var exit interface{ ExitCode() int }
		if errors.As(err, &exit) && exit.ExitCode() > 0 {
			return setup.Result{}, reportedExit{code: exit.ExitCode()}
		}
		return setup.Result{}, fmt.Errorf("run bootstrap apply through sudo: %w", err)
	}
	if out.Len() > maxApplyResult {
		return setup.Result{}, errors.New("bootstrap apply wrote more than 1 MiB")
	}
	var res setup.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		return setup.Result{}, fmt.Errorf("read the result of bootstrap apply: %w", err)
	}
	return res, nil
}

// reportBootstrap sets up how this user's commands find the daemon, then
// prints the summary or the result JSON.
func reportBootstrap(env Env, g *globals, l setup.Layout, res setup.Result) error {
	warnings, err := socketWarnings(env, g, l)
	if err != nil {
		return err
	}
	res.Warnings = append(res.Warnings, warnings...)
	return writeBootstrapResult(env, g, res, bootstrapSummary(l, res))
}

// socketWarnings sets up how this user's commands find the daemon and
// prints a warning for each setting that points elsewhere, returning them.
func socketWarnings(env Env, g *globals, l setup.Layout) ([]string, error) {
	warnings := clientSocketWarnings(env, g, l, true)
	for _, w := range warnings {
		if err := writeString(env.Stderr, "warning: "+w+"\n"); err != nil {
			return nil, err
		}
	}
	return warnings, nil
}

// writeBootstrapResult prints summary, or res as JSON. The summary is for
// a person at a terminal. Redirected stdout always gets the JSON result,
// whatever output setting is saved.
func writeBootstrapResult(env Env, g *globals, res any, summary string) error {
	if !g.table() || !env.StdoutTerminal {
		if err := writeJSON(env.Stdout, res); err != nil {
			return err
		}
		return writeString(env.Stderr, sudoDisclosure+"\n")
	}
	return writeString(env.Stdout, summary)
}

// bootstrapSummary is the summary a person reads at the end.
func bootstrapSummary(l setup.Layout, res setup.Result) string {
	var b strings.Builder
	b.WriteString("chainmail is set up.\n\n")
	tw := newTable(&b)
	state := "not running"
	if res.ServiceRunning {
		state = "running"
	}
	key := "kept only on this machine"
	if res.KeyShown {
		key = "shown once on the terminal"
	}
	_, _ = fmt.Fprintf(tw, "account\t%s\n", res.Account)
	_, _ = fmt.Fprintf(tw, "provider\t%s\n", res.Provider)
	_, _ = fmt.Fprintf(tw, "service\t%s, %s\n", l.ServiceLabel, state)
	_, _ = fmt.Fprintf(tw, "test\t%s\n", res.Test)
	_, _ = fmt.Fprintf(tw, "vault key\t%s\n", key)
	_ = tw.Flush() // a strings.Builder never fails
	b.WriteString("\nPaths:\n")
	tw = newTable(&b)
	for _, name := range slices.Sorted(maps.Keys(res.Paths)) {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", name, res.Paths[name])
	}
	_ = tw.Flush() // as above
	if len(res.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range res.Warnings {
			b.WriteString("  - " + termText(w) + "\n")
		}
	}
	b.WriteString("\nNext steps:\n")
	for i, step := range res.NextSteps {
		_, _ = fmt.Fprintf(&b, "  %d. %s\n", i+1, termText(step))
	}
	b.WriteString("\n" + sudoDisclosure + "\n")
	return b.String()
}

// dryRunOutput is what --dry-run prints as JSON.
type dryRunOutput struct {
	DryRun     bool           `json:"dry_run"`
	Account    string         `json:"account"`
	Provider   string         `json:"provider"`
	SecretName string         `json:"secret_name"`
	NeedSecret bool           `json:"need_secret"`
	Actions    []dryRunAction `json:"actions"`
	NextSteps  []string       `json:"next_steps,omitempty"`
	Warnings   []string       `json:"warnings,omitempty"`
}

// dryRunAction is one planned step.
type dryRunAction struct {
	Kind string `json:"kind"`
	Note string `json:"note"`
}

// printDryRun prints the plan on stdout and changes nothing.
func printDryRun(env Env, g *globals, l setup.Layout, plan setup.Plan) error {
	warnings := clientSocketWarnings(env, g, l, false)
	for _, w := range warnings {
		if err := writeString(env.Stderr, "warning: "+w+"\n"); err != nil {
			return err
		}
	}
	// As with the summary, redirected stdout always gets JSON.
	if g.table() && env.StdoutTerminal {
		if err := writeString(env.Stdout, "Dry run: bootstrap would make these changes, and made none.\n"); err != nil {
			return err
		}
		return plan.Render(env.Stdout)
	}
	out := dryRunOutput{
		DryRun: true, Account: plan.Account.Name, Provider: plan.Account.Provider,
		SecretName: plan.SecretName, NeedSecret: plan.NeedSecret,
		Actions: []dryRunAction{}, NextSteps: plan.NextSteps, Warnings: warnings,
	}
	for _, a := range plan.Actions {
		out.Actions = append(out.Actions, dryRunAction{Kind: string(a.Kind), Note: a.Note})
	}
	return writeJSON(env.Stdout, out)
}

// clientSocketWarnings checks how this user's chainmail commands find the
// daemon, and returns a warning, with the exact line to change, for each
// setting that points somewhere else: cli.toml, CHAINMAIL_SOCKET and
// this run's --socket. When write is set and the layout's socket is not
// the default (macOS), it first writes a cli.toml naming the socket if
// there is none.
func clientSocketWarnings(env Env, g *globals, l setup.Layout, write bool) []string {
	var warnings []string
	line := "socket = " + strconv.Quote(l.Socket)
	path := cliConfigPath(env)
	needsSetting := l.Socket != config.DefaultSocket
	created := false
	if needsSetting && write {
		if path == "" {
			warnings = append(warnings, "no cli.toml could be found because HOME is not set; add this line to it: "+line)
		} else {
			var err error
			if created, err = createCLIConfig(path, line); err != nil {
				warnings = append(warnings, fmt.Sprintf("could not write %s (%v); add this line to it: %s", path, err, line))
			}
		}
	}
	if path != "" && !created {
		file, err := readCLIConfig(path)
		socket, set := file["socket"].(string)
		_, statErr := os.Lstat(path)
		switch {
		case err != nil:
			// resolve already read it; a file that broke since is reported
			// by the next command.
		case set && !sameSocket(env, socket, l.Socket):
			warnings = append(warnings, fmt.Sprintf(
				"%s sets socket = %q, so chainmail commands still reach that daemon, not the one bootstrap set up at %s; change that line to: %s",
				path, socket, l.Socket, line))
		case !set && needsSetting && statErr == nil:
			warnings = append(warnings, fmt.Sprintf(
				"%s has no socket line, so chainmail commands look for %s, not the daemon bootstrap set up at %s; add this line to it: %s",
				path, config.DefaultSocket, l.Socket, line))
		}
	}
	if s := env.Getenv(socketBinding.envVar); s != "" && !sameSocket(env, s, l.Socket) {
		warnings = append(warnings, fmt.Sprintf(
			"%s is %s, so chainmail commands still reach that daemon, not the one bootstrap set up at %s; change the line that sets it to: %s=%s",
			socketBinding.envVar, s, l.Socket, socketBinding.envVar, l.Socket))
	}
	if s := g.socketFlag; s != "" && !sameSocket(env, s, l.Socket) {
		flag := "--" + socketBinding.flag
		warnings = append(warnings, fmt.Sprintf(
			"%s is %s, so chainmail commands given it still reach that daemon, not the one bootstrap set up at %s; leave %s out, or give it: %s %s",
			flag, s, l.Socket, flag, flag, l.Socket))
	}
	return warnings
}

// createCLIConfig writes a cli.toml holding line at path when there is
// none, and reports whether it did. An existing file is left alone.
func createCLIConfig(path, line string) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close() // the write error is the one to report
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return true, nil
}

// newBootstrapApplyCmd builds the hidden step bootstrap runs through sudo.
func newBootstrapApplyCmd(ctx context.Context, env Env) *cobra.Command {
	return &cobra.Command{
		Use:    "apply",
		Short:  "Carry out a bootstrap plan as root; chainmail bootstrap runs it through sudo",
		Hidden: true,
		Args:   exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			return runBootstrapApply(ctx, env)
		},
	}
}

// runBootstrapApply is the privileged phase. It trusts nothing in the
// payload: Apply validates the plan, rebuilds it under the lock and
// refuses any difference.
func runBootstrapApply(ctx context.Context, env Env) error {
	if env.getuid() != 0 {
		return &setup.Refusal{Msg: "bootstrap apply runs only as root, through sudo", Hint: applyHint}
	}
	rawUID := env.Getenv("SUDO_UID")
	sudoUID, err := strconv.ParseUint(rawUID, 10, 32)
	if err != nil || sudoUID == 0 {
		return &setup.Refusal{Msg: fmt.Sprintf("SUDO_UID %q does not name a user other than root", rawUID), Hint: applyHint}
	}
	sudoUser := env.Getenv("SUDO_USER")
	if sudoUser == "" || sudoUser == "root" {
		return &setup.Refusal{Msg: fmt.Sprintf("SUDO_USER %q does not name a user other than root", sudoUser), Hint: applyHint}
	}
	in, err := readApplyPayload(env.Stdin)
	if err != nil {
		return err
	}
	defer clear(in.Secret)

	l, err := bootstrapLayout(env)
	if err != nil {
		return err
	}
	exe, err := env.executable()
	if err != nil {
		return err
	}
	files, accounts := env.setupFiles(), env.setupAccounts()
	service := env.SetupService
	if service == nil {
		service = system.NewService(env.goos(), l.ServiceLabel, l.Service, l.LogFile, system.ExecRunner)
	}
	asDaemon := env.AsDaemon
	if asDaemon == nil {
		asDaemon = daemonRunner(l, accounts)
	}
	var units system.Units
	var creds system.Creds
	if l.Platform == setup.NixOS {
		// The plan is not validated yet, but Apply validates it, and
		// rebuilds it under the lock, before anything runs as the daemon.
		asDaemon = nixDaemonRunner(env, l, accounts, in.Plan.DaemonBinary)
		units, creds = env.setupUnits(), env.setupCreds()
	}
	openTerminal := env.OpenTTY
	if openTerminal == nil {
		openTerminal = openTTY
	}
	a := &setup.Applier{
		Layout:     l,
		Files:      files,
		Accounts:   accounts,
		Service:    service,
		AsDaemon:   asDaemon,
		Executable: exe,
		OpenTTY:    openTerminal,
		Now:        env.now,
		Sleep:      func(d time.Duration) { <-env.after(d) },
		Stderr:     env.Stderr,
		Units:      units,
		Creds:      creds,
	}
	res, err := a.Apply(ctx, setup.Input{
		Plan:     in.Plan,
		Request:  in.Request,
		Secret:   in.Secret,
		Health:   in.Health,
		SudoUID:  uint32(sudoUID),
		SudoUser: sudoUser,
	})
	if err != nil {
		return err
	}
	return writeJSON(env.Stdout, res)
}

// readApplyPayload reads the one JSON object bootstrap sends, at most
// maxApplyPayload bytes, refusing anything else.
func readApplyPayload(r io.Reader) (applyPayload, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxApplyPayload+1))
	defer clear(data)
	if err != nil {
		return applyPayload{}, fmt.Errorf("read the plan from stdin: %w", err)
	}
	hint := "run chainmail bootstrap; bootstrap apply only takes the plan it sends"
	if len(data) > maxApplyPayload {
		return applyPayload{}, &setup.Refusal{Msg: "the plan on stdin is larger than 1 MiB", Hint: hint}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var p applyPayload
	if err := dec.Decode(&p); err != nil {
		clear(p.Secret)
		return applyPayload{}, &setup.Refusal{Msg: "the plan on stdin is not valid: " + err.Error(), Hint: hint}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		clear(p.Secret)
		return applyPayload{}, &setup.Refusal{Msg: "the plan on stdin holds more than one JSON value", Hint: hint}
	}
	return p, nil
}

// daemonRunner runs the installed binary as the daemon user, as
// daemonRunnerFor does.
func daemonRunner(l setup.Layout, accounts system.Accounts) func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	return daemonRunnerFor(l, accounts, l.Binary)
}

// daemonRunnerFor runs binary as the daemon user, with the client group as
// its only group. It looks the user up at each call: a fresh install
// creates it part way through Apply. The command's stderr goes into the
// error, except when stdout is the terminal, as for auth login, which
// prints the consent URL on stderr.
func daemonRunnerFor(l setup.Layout, accounts system.Accounts, binary string) func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	return func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
		u, ok, err := accounts.LookupUser(ctx, l.DaemonUser)
		if err != nil {
			return fmt.Errorf("look up %s: %w", l.DaemonUser, err)
		}
		if !ok {
			return fmt.Errorf("user %s does not exist", l.DaemonUser)
		}
		grp, ok, err := accounts.LookupGroup(ctx, l.ClientGroup)
		if err != nil {
			return fmt.Errorf("look up group %s: %w", l.ClientGroup, err)
		}
		if !ok {
			return fmt.Errorf("group %s does not exist", l.ClientGroup)
		}
		var errBuf bytes.Buffer
		var stderr io.Writer = &errBuf
		if f, ok := stdout.(*os.File); ok {
			stderr = f
		}
		if err := system.RunAs(ctx, u.UID, grp.GID, stdin, stdout, stderr, binary, args...); err != nil {
			if msg := strings.Join(strings.Fields(errBuf.String()), " "); msg != "" {
				return fmt.Errorf("%w: %s", err, clip(msg, maxDaemonStderr))
			}
			return err
		}
		return nil
	}
}

// The real implementations of the bootstrap fields of Env.

// goos is the operating system bootstrap sets up.
func (e Env) goos() string {
	if e.GOOS == "" {
		return runtime.GOOS
	}
	return e.GOOS
}

// getuid is e.Getuid(), or the process's real uid.
func (e Env) getuid() int {
	if e.Getuid == nil {
		return os.Getuid()
	}
	return e.Getuid()
}

// username is the name of the user running chainmail.
func (e Env) username() (string, error) {
	get := e.Username
	if get == nil {
		get = currentUsername
	}
	name, err := get()
	if err != nil {
		return "", fmt.Errorf("look up the current user: %w", err)
	}
	return name, nil
}

// executable is the running binary with its symlinks resolved.
func (e Env) executable() (string, error) {
	get := e.Executable
	if get == nil {
		get = resolvedExecutable
	}
	return get()
}

// resolver looks up MX records.
func (e Env) resolver() setup.Resolver {
	if e.Resolver == nil {
		return net.DefaultResolver
	}
	return e.Resolver
}

// setupFiles is the file view bootstrap reads and bootstrap apply writes.
func (e Env) setupFiles() system.Files {
	if e.SetupFiles == nil {
		return system.OSFiles{}
	}
	return e.SetupFiles
}

// setupAccounts is the user and group database.
func (e Env) setupAccounts() system.Accounts {
	if e.SetupAccounts == nil {
		return system.NewAccounts(e.goos(), system.ExecRunner)
	}
	return e.SetupAccounts
}

// setupUnits reads systemd's unit facts on NixOS.
func (e Env) setupUnits() system.Units {
	if e.SetupUnits == nil {
		return system.NewUnits(system.ExecRunner)
	}
	return e.SetupUnits
}

// setupCreds runs systemd-creds on NixOS.
func (e Env) setupCreds() system.Creds {
	if e.SetupCreds == nil {
		return system.NewCreds(system.ExecPipeRunner)
	}
	return e.SetupCreds
}

// canShowKey reports whether stdin and stdout are terminals, so the key
// shown on the terminal is not also captured.
func (e Env) canShowKey() bool { return e.StdoutTerminal && e.ReadLine != nil }

// currentUsername is the name of the process's user.
func currentUsername() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// resolvedExecutable is os.Executable with every symlink resolved, so
// bootstrap apply can read it without following a link.
func resolvedExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find the running binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("find the running binary: %w", err)
	}
	return resolved, nil
}

// sudoDirs are where bootstrap looks for sudo, in order. PATH is never
// searched: a directory in it that the user or an agent can write would
// get the plan, and the secret in it, from a fake sudo. A copy in
// /usr/local/bin counts only when root owns it and its directories and
// only root can write them, which rules out a Homebrew prefix.
var sudoDirs = []string{"/usr/bin", "/bin", "/usr/local/bin", "/run/wrappers/bin"}

// realSudo runs the trusted sudo with the given stdin and stdout. Its
// stderr, and so bootstrap apply's progress and errors, is chainmail's own.
func realSudo(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	find := func() (string, error) { return system.LookupTrusted("sudo", sudoDirs) }
	return runSudo(ctx, find, stdin, stdout, args...)
}

// runSudo runs the sudo find returns with args.
func runSudo(ctx context.Context, find func() (string, error), stdin io.Reader, stdout io.Writer, args ...string) error {
	path, err := find()
	if err != nil {
		return fmt.Errorf("find sudo: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Args[0] = "sudo"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, os.Stderr
	return cmd.Run()
}

// openTTY opens the controlling terminal.
func openTTY() (io.ReadWriteCloser, error) {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open the terminal: %w", err)
	}
	return f, nil
}

// readTerminalLine reads one line from r a byte at a time, so nothing
// after the newline is consumed and a password prompt reading the same
// terminal later sees all its input. The line has no newline. A last line
// without one is returned as a line; after it comes io.EOF.
func readTerminalLine(r io.Reader) (string, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			line = append(line, b[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return string(line), nil
			}
			return string(line), err
		}
	}
}
