package setup

import (
	"context"
	"fmt"
	"path/filepath"
)

// maxNixCred bounds the credential bootstrap apply hands to systemd-creds.
// A sealed vault key is a few hundred bytes.
const maxNixCred = 64 << 10

// Hints for the NixOS key steps.
const (
	badCredHint   = "move it aside and run bootstrap again to seal a new key"
	sealFailHint  = "check that systemd-creds is installed and can encrypt on this host, then run bootstrap again"
	notActiveHint = "check it with: journalctl -u chainmail"
)

// errNotActive is the refusal for a chainmail.service that is not running
// when bootstrap needs the daemon on NixOS, where it never starts it.
func errNotActive(l Layout) error {
	return refuse(notActiveHint, "%s is not active; bootstrap never starts it on NixOS", l.ServiceLabel)
}

// applyNixOS is Apply for a NixOS plan, after the checks every plan gets.
//
// It writes nothing to the file system but the lock file, which lives in
// /run once the trust check has passed that directory and its ancestors.
// Under the lock it inspects the host again, rebuilds the plan with
// BuildNixOS and refuses unless it is the same. Then it seals a new key
// or checks the existing credential, or, in Finish, stores the secret,
// logs in and tests through the daemon the module runs. It leaves the
// write-nix steps to the CLI and never starts, stops or restarts the
// service.
func (a *Applier) applyNixOS(ctx context.Context, in Input) (Result, error) {
	l := a.Layout
	if len(in.Request.NixCred) > maxNixCred {
		return Result{}, refuse(badCredHint, "the credential in %s is %d bytes, more than a vault key credential can be",
			in.Plan.NixDir, len(in.Request.NixCred))
	}
	if err := CheckTrust(filesView{a.Files}, filepath.Dir(l.Lock), rootOnly); err != nil {
		return Result{}, err
	}
	unlock, err := a.Files.Lock(l.Lock)
	if err != nil {
		return Result{}, fmt.Errorf("lock %s: %w", l.Lock, err)
	}
	defer func() { _ = unlock() }() // the process exits soon after; the kernel drops the lock anyway

	cur, err := InspectNixOS(ctx, l, a.Files, a.Accounts, a.Units, in.SudoUser)
	if err != nil {
		return Result{}, err
	}
	cur.Health = in.Health
	rebuilt, err := BuildNixOS(l, cur, in.Request)
	if err != nil {
		return Result{}, err
	}
	if !Same(rebuilt, in.Plan) {
		return Result{}, refuse(changedHint, "the installation changed while bootstrap was running; run it again")
	}

	r := &applyRun{a: a, l: l, in: in, plan: in.Plan, res: newResult(l, in.Plan)}
	if r.usesDaemon() {
		if r.pre, err = a.Service.State(ctx); err != nil {
			return Result{}, fmt.Errorf("read the state of %s: %w", l.ServiceLabel, err)
		}
	}
	return r.run(ctx)
}

// usesDaemon reports whether the run talks to the daemon: always, except
// for a NixOS plan before the module is installed, which only makes or
// checks the key and leaves the Nix files to the CLI.
func (r *applyRun) usesDaemon() bool {
	return r.l.Platform != NixOS || r.plan.NixState == NixFinish
}

// sealKey makes a new vault key, seals it with systemd-creds, and shows
// it when the user asked for that. The key exists only in memory and on
// the pipe to systemd-creds, and is cleared before sealKey returns. A key
// that could not be sealed is never shown.
func (r *applyRun) sealKey(ctx context.Context) error {
	key, err := GenerateKey(r.a.Now())
	if err != nil {
		return err
	}
	defer clear(key)
	cred, err := r.a.Creds.Encrypt(ctx, CredName, key)
	if err != nil {
		return refuse(sealFailHint, "could not seal the vault key with systemd-creds: %v", err)
	}
	if len(cred) == 0 {
		return refuse(sealFailHint, "systemd-creds sealed the vault key into an empty credential")
	}
	if r.plan.Backup == BackupShow {
		r.showKey(key)
	}
	r.res.Cred = cred
	return nil
}

// checkCred refuses unless the credential already in the Nix directory
// decrypts on this host. Its plaintext is never kept.
func (r *applyRun) checkCred(ctx context.Context) error {
	if err := r.a.Creds.CheckDecrypt(ctx, CredName, r.in.Request.NixCred); err != nil {
		return refuse(badCredHint, "the credential in %s does not decrypt on this host", r.plan.NixDir)
	}
	return nil
}
