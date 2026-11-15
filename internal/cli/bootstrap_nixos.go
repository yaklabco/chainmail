package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yaklabco/chainmail/internal/setup"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// The Nix directory and the files bootstrap writes into it on NixOS.
const (
	// defaultNixDir is the --nix-dir when none is given, relative to the
	// working directory.
	defaultNixDir = "chainmail-nixos"
	// nixDirMode is the mode of a Nix directory bootstrap makes.
	nixDirMode = 0o755
	// nixFileMode is the mode of every file bootstrap writes there. They
	// are safe to read and commit: the credential is encrypted and the
	// Nix holds no secrets.
	nixFileMode = 0o644
)

// The status of a file in the Nix directory after bootstrap.
const (
	nixWritten   = "written"
	nixUnchanged = "unchanged"
)

// daemonRun runs a chainmail command as the daemon user, as
// setup.Applier.AsDaemon does.
type daemonRun = func(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error

// nixFile is a file bootstrap wrote into the Nix directory, or found
// there with the bytes it would write.
type nixFile struct {
	Path   string `json:"path"`
	Status string `json:"status"` // nixWritten or nixUnchanged
}

// nixBootstrapOutput is what bootstrap reports on NixOS: the result of
// bootstrap apply, or of the run without it, and the Nix files. Result.Cred
// is always cleared: the credential goes into the Nix directory only.
type nixBootstrapOutput struct {
	setup.Result
	NixDir string    `json:"nix_dir,omitempty"`
	Files  []nixFile `json:"files,omitempty"`
}

// runBootstrapNixOS is runBootstrap on NixOS. It plans with BuildNixOS,
// runs bootstrap apply through sudo only when the plan holds root work
// (sealing or checking the vault key, or storing, logging in and testing
// an account the configuration has), then writes the Nix files as the
// invoking user: the sealed credential first, then each module whose
// bytes differ. It never edits the NixOS configuration, rebuilds, or
// touches chainmail.service.
func runBootstrapNixOS(ctx context.Context, env Env, g *globals, f bootstrapFlags, l setup.Layout, address string) error {
	uid := env.getuid()
	files := env.setupFiles()
	exe, req, err := startBootstrap(ctx, env, files, f, address)
	if err != nil {
		return err
	}
	if req.NixDir, err = resolveNixDir(f.nixDir); err != nil {
		return err
	}
	if req.NixCred, err = readNixCred(files, req.NixDir); err != nil {
		return err
	}
	invoking, err := env.username()
	if err != nil {
		return err
	}
	cur, err := setup.InspectNixOS(ctx, l, files, env.setupAccounts(), env.setupUnits(), invoking)
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
	plan, err := setup.BuildNixOS(l, cur, req)
	if err != nil {
		return err
	}
	if err := checkNixDir(files, plan); err != nil {
		return err
	}

	if f.dryRun {
		return printDryRun(env, g, l, plan)
	}
	replan := func(req setup.Request) (setup.Plan, error) { return setup.BuildNixOS(l, cur, req) }
	plan, secret, err := askPlan(ctx, env, f, address, setup.ActSealKey, replan, &req, plan)
	if err != nil {
		return err
	}
	defer clear(secret)
	res := localNixResult(l, plan)
	if plan.NeedsRoot() {
		if res, err = sudoApply(ctx, env, exe, applyPayload{Plan: plan, Request: req, Secret: secret, Health: cur.Health}); err != nil {
			return err
		}
	}
	written, err := writeNixFiles(files, cur.InvokingUser, plan, res.Cred)
	if err != nil {
		return err
	}
	return reportNixBootstrap(env, g, l, plan, res, written)
}

// resolveNixDir is --nix-dir as an absolute path, ./chainmail-nixos when
// it is not given.
func resolveNixDir(dir string) (string, error) {
	if dir == "" {
		dir = defaultNixDir
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve --nix-dir %q: %w", dir, err)
	}
	return abs, nil
}

// readNixCred reads the vault key credential in dir, or returns nil when
// there is none: dir is no directory, or holds no regular file by that
// name. checkNixDir refuses a credential path that is something else when
// the plan would write it. An empty file is still a credential, which
// bootstrap apply checks and never replaces.
func readNixCred(files system.Files, dir string) ([]byte, error) {
	info, err := files.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.Exists || !info.Mode.IsDir() {
		return nil, nil
	}
	path := filepath.Join(dir, setup.CredFile)
	if info, err = files.Lstat(path); err != nil {
		return nil, err
	}
	if !info.Exists || !info.Mode.IsRegular() {
		return nil, nil
	}
	cred, err := files.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the vault key credential: %w", err)
	}
	if cred == nil {
		cred = []byte{}
	}
	return cred, nil
}

// nixWrites are the plan's write-nix actions.
func nixWrites(p setup.Plan) []setup.Action {
	return slices.DeleteFunc(slices.Clone(p.Actions), func(a setup.Action) bool { return a.Kind != setup.ActWriteNix })
}

// hasAction reports whether p holds an action of kind.
func hasAction(p setup.Plan, kind setup.ActionKind) bool {
	return slices.ContainsFunc(p.Actions, func(a setup.Action) bool { return a.Kind == kind })
}

// checkNixDir refuses, before anything runs, a Nix directory the plan's
// files cannot be written into: it must be a directory, or missing with
// its parent there, and no file the plan writes there, the credential it
// seals included, may be a symlink or anything but a regular file. A plan
// that writes no Nix file passes.
func checkNixDir(files system.Files, p setup.Plan) error {
	writes := nixWrites(p)
	if len(writes) == 0 {
		return nil
	}
	info, err := files.Lstat(p.NixDir)
	if err != nil {
		return err
	}
	if !info.Exists {
		parent := filepath.Dir(p.NixDir)
		pinfo, err := files.Lstat(parent)
		if err != nil {
			return err
		}
		if !pinfo.Exists {
			return &setup.Refusal{
				Msg:  fmt.Sprintf("the directory %s, which would hold --nix-dir %s, does not exist", parent, p.NixDir),
				Hint: "create it, or pass a --nix-dir in a directory that exists",
			}
		}
		return nil
	}
	if !info.Mode.IsDir() {
		return &setup.Refusal{
			Msg:  fmt.Sprintf("--nix-dir %s is not a directory", p.NixDir),
			Hint: "pass a --nix-dir that is a directory, or one that does not exist yet",
		}
	}
	var names []string
	if hasAction(p, setup.ActSealKey) {
		names = append(names, setup.CredFile)
	}
	for _, a := range writes {
		names = append(names, a.Entry)
	}
	for _, name := range names {
		if _, err := checkNixTarget(files, filepath.Join(p.NixDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// checkNixTarget refuses a path bootstrap would write that is a symlink
// or not a regular file. A missing path passes. It returns what Lstat
// found.
func checkNixTarget(files system.Files, path string) (system.FileInfo, error) {
	info, err := files.Lstat(path)
	if err != nil {
		return system.FileInfo{}, err
	}
	switch {
	case !info.Exists, info.Mode.IsRegular():
		return info, nil
	case info.Mode&fs.ModeSymlink != 0:
		return system.FileInfo{}, &setup.Refusal{
			Msg:  fmt.Sprintf("%s is a symlink; bootstrap never writes through one", path),
			Hint: "remove it, then run bootstrap again",
		}
	default:
		return system.FileInfo{}, &setup.Refusal{
			Msg:  fmt.Sprintf("%s is not a regular file", path),
			Hint: "move it aside, then run bootstrap again",
		}
	}
}

// localNixResult is the result of a plan bootstrap carries out without
// bootstrap apply: one that only writes Nix files. Its paths are the ones
// bootstrap apply reports on NixOS, where the module puts each.
func localNixResult(l setup.Layout, p setup.Plan) setup.Result {
	return setup.Result{
		Account:  p.Account.Name,
		Provider: p.Account.Provider,
		Paths: map[string]string{
			"config": l.Config, "state-dir": l.StateDir, "vault": l.Vault, "socket": l.Socket, "service": l.Service,
		},
		Test:      "skipped",
		NextSteps: slices.Clone(p.NextSteps),
		NixState:  p.NixState,
	}
}

// writeNixFiles writes the plan's Nix files into its Nix directory as
// owner, making the directory first when it is missing. The sealed
// credential cred comes first, created exclusively so an existing one is
// never replaced; then each module, replaced whole, and only when its
// bytes differ. It never writes through a symlink.
func writeNixFiles(files system.Files, owner system.User, p setup.Plan, cred []byte) ([]nixFile, error) {
	writes := nixWrites(p)
	if len(writes) == 0 {
		return nil, nil
	}
	info, err := files.Lstat(p.NixDir)
	if err != nil {
		return nil, err
	}
	switch {
	case !info.Exists:
		if err := files.EnsureDir(p.NixDir, owner.UID, owner.GID, nixDirMode); err != nil {
			return nil, fmt.Errorf("make the Nix directory: %w", err)
		}
	case !info.Mode.IsDir():
		return nil, &setup.Refusal{
			Msg:  fmt.Sprintf("--nix-dir %s is no longer a directory", p.NixDir),
			Hint: "run bootstrap again",
		}
	}

	var out []nixFile
	credPath := filepath.Join(p.NixDir, setup.CredFile)
	switch {
	case hasAction(p, setup.ActSealKey):
		if len(cred) == 0 {
			return nil, errors.New("bootstrap apply sealed a vault key but returned no credential")
		}
		if err := files.CreateExclusive(credPath, cred, owner.UID, owner.GID, nixFileMode); err != nil {
			return nil, fmt.Errorf("write the sealed vault key: %w", err)
		}
		out = append(out, nixFile{Path: credPath, Status: nixWritten})
	case hasAction(p, setup.ActCheckCred):
		out = append(out, nixFile{Path: credPath, Status: nixUnchanged})
	}
	for _, a := range writes {
		path := filepath.Join(p.NixDir, a.Entry)
		status, err := writeNixFile(files, owner, path, a.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, nixFile{Path: path, Status: status})
	}
	return out, nil
}

// writeNixFile writes data to path as owner unless path already holds
// it, and says which it did. A symlink or anything but a regular file at
// path is refused.
func writeNixFile(files system.Files, owner system.User, path string, data []byte) (string, error) {
	info, err := checkNixTarget(files, path)
	if err != nil {
		return "", err
	}
	if info.Exists {
		old, err := files.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", path, err)
		}
		if bytes.Equal(old, data) {
			return nixUnchanged, nil
		}
	}
	if err := files.WriteFile(path, data, owner.UID, owner.GID, nixFileMode); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return nixWritten, nil
}

// nixDaemonRunner runs the store binary a Finish plan names as the daemon
// user, through Env.DaemonRunnerFor when it is set. A plan that names none
// runs nothing as the daemon user, so its runner only fails.
func nixDaemonRunner(env Env, l setup.Layout, accounts system.Accounts, binary string) daemonRun {
	if binary == "" {
		return func(context.Context, io.Reader, io.Writer, ...string) error {
			return errors.New("this plan runs nothing as the daemon user")
		}
	}
	if env.DaemonRunnerFor != nil {
		return env.DaemonRunnerFor(binary)
	}
	return daemonRunnerFor(l, accounts, binary)
}

// reportNixBootstrap sets up how this user's commands find the daemon,
// then prints the NixOS summary or the result JSON, never the
// credential.
func reportNixBootstrap(env Env, g *globals, l setup.Layout, p setup.Plan, res setup.Result, written []nixFile) error {
	warnings, err := socketWarnings(env, g, l)
	if err != nil {
		return err
	}
	res.Warnings = append(res.Warnings, warnings...)
	res.Cred = nil
	out := nixBootstrapOutput{Result: res, Files: written}
	if len(written) > 0 {
		out.NixDir = p.NixDir
	}
	return writeBootstrapResult(env, g, out, nixBootstrapSummary(l, out))
}

// nixBootstrapSummary is the summary a person reads at the end on NixOS.
// Fresh and Add list the Nix files and the steps to import them; Finish
// reports the account like other hosts. The next steps keep their lines.
func nixBootstrapSummary(l setup.Layout, out nixBootstrapOutput) string {
	var b strings.Builder
	if out.NixState == setup.NixFinish {
		b.WriteString("chainmail is set up.\n\n")
	} else {
		b.WriteString("The Nix files for chainmail are ready in " + termText(out.NixDir) + ".\n\n")
	}
	tw := newTable(&b)
	_, _ = fmt.Fprintf(tw, "account\t%s\n", out.Account)
	_, _ = fmt.Fprintf(tw, "provider\t%s\n", out.Provider)
	switch out.NixState {
	case setup.NixFinish:
		state := "not running"
		if out.ServiceRunning {
			state = "running"
		}
		_, _ = fmt.Fprintf(tw, "service\t%s, %s\n", l.ServiceLabel, state)
		_, _ = fmt.Fprintf(tw, "test\t%s\n", out.Test)
	case setup.NixFresh:
		key := "sealed to this machine"
		if out.KeyShown {
			key = "shown once on the terminal"
		}
		_, _ = fmt.Fprintf(tw, "vault key\t%s\n", key)
	}
	_ = tw.Flush() // a strings.Builder never fails
	if len(out.Files) > 0 {
		b.WriteString("\nFiles:\n")
		tw = newTable(&b)
		for _, f := range out.Files {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\n", termText(filepath.Base(f.Path)), f.Status)
		}
		_ = tw.Flush() // as above
	}
	if len(out.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range out.Warnings {
			b.WriteString("  - " + termText(w) + "\n")
		}
	}
	b.WriteString("\nNext steps:\n")
	writeSteps(&b, out.NextSteps)
	b.WriteString("\n" + sudoDisclosure + "\n")
	return b.String()
}

// writeSteps writes numbered steps that may span lines. Each line is
// cleaned on its own, keeping its indent, and the lines after a step's
// first are indented to line up under its text.
func writeSteps(b *strings.Builder, steps []string) {
	for i, step := range steps {
		prefix := fmt.Sprintf("  %d. ", i+1)
		pad := strings.Repeat(" ", len(prefix))
		for j, line := range strings.Split(step, "\n") {
			indent := len(line) - len(strings.TrimLeft(line, " "))
			lead := pad
			if j == 0 {
				lead, indent = prefix, 0
			}
			b.WriteString(lead + strings.Repeat(" ", indent) + termText(line) + "\n")
		}
	}
}
