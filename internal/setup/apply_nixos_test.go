package setup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/yaklabco/chainmail/internal/setup/setuptest"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// nixLock is the lock bootstrap apply takes on NixOS.
const nixLock = "/run/chainmail-bootstrap.lock"

// nixSocketArgs are the flags every daemon call on NixOS ends with.
const nixSocketArgs = "--socket /run/chainmail/chainmail.sock"

// nixRig is a rig on a fake NixOS host, with systemd's view of the unit
// and a fake systemd-creds.
type nixRig struct {
	*rig
	nh    nixHost
	creds *setuptest.Creds
}

// newNixRig wires nh. The user's binary is newBinary at userExe.
func newNixRig(nh nixHost) *nixRig {
	r := newRig(NixOS, host{files: nh.files, accounts: nh.accounts})
	nh.units.Journal = r.journal
	creds := &setuptest.Creds{Journal: r.journal}
	r.applier.Units = nh.units
	r.applier.Creds = creds
	return &nixRig{rig: r, nh: nh, creds: creds}
}

// nixApplyRequest is nixRequest running newBinary.
func nixApplyRequest() Request {
	req := nixRequest()
	req.ExecSHA256 = sha256Hex(newBinary)
	return req
}

// input plans req on the rig's host as alice would, with health, and
// returns the Input bootstrap apply gets.
func (r *nixRig) input(t *testing.T, req Request, health *AccountHealth, secret []byte) Input {
	t.Helper()
	cur := r.nh.inspect(t)
	cur.Health = health
	alice, _ := r.nh.accounts.User("alice")
	return Input{
		Plan:     mustBuildNixOS(t, cur, req),
		Request:  req,
		Secret:   secret,
		Health:   health,
		SudoUID:  alice.UID,
		SudoUser: "alice",
	}
}

// applyLogged runs Apply and returns the journal entries it added.
func (r *nixRig) applyLogged(in Input) (Result, []string, error) {
	before := len(r.journal.Entries())
	res, err := r.apply(in)
	return res, r.journal.Entries()[before:], err
}

// wantNoServiceChange fails unless the service was never activated or
// restarted.
func (r *nixRig) wantNoServiceChange(t *testing.T) {
	t.Helper()
	for _, c := range r.svc.Calls() {
		if strings.HasPrefix(c, "activate") || c == "restart" {
			t.Errorf("service call %q on NixOS; bootstrap never manages chainmail.service there", c)
		}
	}
}

// wantNoWrites fails when anything was created or written through Files.
func (r *nixRig) wantNoWrites(t *testing.T) {
	t.Helper()
	if w := r.nh.files.Writes(); len(w) > 0 {
		t.Errorf("Files writes = %q, want none on NixOS", w)
	}
}

// unsealed is the plaintext the fake Creds sealed into cred.
func unsealed(t *testing.T, cred []byte) []byte {
	t.Helper()
	enc, ok := bytes.CutPrefix(cred, []byte("sealed:"))
	if !ok {
		t.Fatalf("credential %q was not sealed by the fake", cred)
	}
	key, err := base64.StdEncoding.DecodeString(string(enc))
	if err != nil {
		t.Fatalf("decode the sealed credential: %v", err)
	}
	return key
}

// secretLine is the AGE-SECRET-KEY line of an age-keygen key file.
func secretLine(t *testing.T, key []byte) string {
	t.Helper()
	for line := range strings.Lines(string(key)) {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "AGE-SECRET-KEY-") {
			return s
		}
	}
	t.Fatal("the sealed key holds no AGE-SECRET-KEY line")
	return ""
}

func TestApplyNixOSFreshSeals(t *testing.T) {
	t.Parallel()
	r := newNixRig(freshNixHost())
	in := r.input(t, nixApplyRequest(), nil, nil)

	res, entries, err := r.applyLogged(in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{
		"lock " + nixLock,
		"units show chainmail.service",
		"creds encrypt vault-key",
		"unlock " + nixLock,
	}
	if !slices.Equal(entries, want) {
		t.Errorf("journal:\n got %q\nwant %q", entries, want)
	}
	r.wantNoWrites(t)
	r.wantNoServiceChange(t)
	if calls := r.daemonCalls(); len(calls) > 0 {
		t.Errorf("daemon calls = %q, want none in Fresh", calls)
	}
	if calls := r.creds.Calls(); !slices.Equal(calls, []string{"encrypt vault-key"}) {
		t.Errorf("Creds calls = %q, want one encrypt of vault-key", calls)
	}

	key := unsealed(t, res.Cred)
	if _, err := age.ParseX25519Identity(secretLine(t, key)); err != nil {
		t.Errorf("the sealed key is not an age identity: %v", err)
	}
	if res.NixState != NixFresh {
		t.Errorf("NixState = %q, want fresh", res.NixState)
	}
	if res.KeyShown || r.ttyOpens != 0 {
		t.Errorf("KeyShown %v, terminal opened %d times; the key backup was none", res.KeyShown, r.ttyOpens)
	}
	// write-nix is the CLI's: apply neither writes nor announces it.
	if got, want := r.stderr.String(), "[1/1] seal a new vault key into /home/alice/chainmail-nixos/chainmail-vault-key.cred\n"; got != want {
		t.Errorf("progress = %q, want %q", got, want)
	}

	t.Run("untrusted /run takes no lock", func(t *testing.T) {
		t.Parallel()
		r := newNixRig(freshNixHost())
		in := r.input(t, nixApplyRequest(), nil, nil)
		r.nh.files.Dir("/run", fakeUserUID, fakeUserUID, 0o755)
		_, entries, err := r.applyLogged(in)
		wantRefusal(t, err, "/run")
		if countPrefix(entries, "lock ") > 0 {
			t.Errorf("journal = %q, want no lock under an untrusted /run", entries)
		}
		if calls := r.creds.Calls(); len(calls) > 0 {
			t.Errorf("Creds calls = %q, want none", calls)
		}
	})
}

func TestApplyNixOSKeyShow(t *testing.T) {
	t.Parallel()
	r := newNixRig(freshNixHost())
	req := nixApplyRequest()
	req.Backup = BackupShow
	in := r.input(t, req, nil, nil)

	res, err := r.apply(in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	key := unsealed(t, res.Cred)
	secret := secretLine(t, key)
	if !res.KeyShown || r.ttyOpens != 1 {
		t.Errorf("KeyShown %v, terminal opened %d times, want shown once", res.KeyShown, r.ttyOpens)
	}
	if !strings.Contains(r.tty.Output(), string(key)) {
		t.Error("the terminal did not get the key")
	}
	out, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal the result: %v", err)
	}
	places := map[string]string{
		"stderr":       r.stderr.String(),
		"result JSON":  string(out),
		"journal":      strings.Join(r.journal.Entries(), "\n"),
		"Creds calls":  strings.Join(r.creds.Calls(), "\n"),
		"daemon calls": strings.Join(r.daemonCalls(), "\n"),
		"Units calls":  strings.Join(r.nh.units.Calls(), "\n"),
		"Files writes": strings.Join(r.nh.files.Writes(), "\n"),
		"warnings":     strings.Join(res.Warnings, "\n"),
	}
	for _, name := range slices.Sorted(maps.Keys(places)) {
		if strings.Contains(places[name], secret) {
			t.Errorf("the vault key appears in the %s", name)
		}
	}

	t.Run("backup none opens no terminal", func(t *testing.T) {
		t.Parallel()
		r := newNixRig(freshNixHost())
		res, err := r.apply(r.input(t, nixApplyRequest(), nil, nil))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if r.ttyOpens != 0 || res.KeyShown {
			t.Errorf("terminal opened %d times, KeyShown %v; want neither", r.ttyOpens, res.KeyShown)
		}
	})
}

func TestApplyNixOSSealFails(t *testing.T) {
	t.Parallel()
	r := newNixRig(freshNixHost())
	r.creds.EncryptErr = errors.New("exec: no such file or directory")
	req := nixApplyRequest()
	req.Backup = BackupShow
	in := r.input(t, req, nil, nil)

	res, err := r.apply(in)
	if err == nil {
		t.Fatal("Apply succeeded with sealing failing")
	}
	if !strings.Contains(err.Error(), "systemd-creds") {
		t.Errorf("error = %q, want it to name systemd-creds", err)
	}
	if len(res.Cred) > 0 {
		t.Errorf("Result.Cred = %q, want empty", res.Cred)
	}
	if r.ttyOpens != 0 || r.tty.Output() != "" {
		t.Errorf("terminal opened %d times with %q; an unsealed key is never shown", r.ttyOpens, r.tty.Output())
	}
	r.wantNoWrites(t)
	if r.nh.files.Locked(nixLock) {
		t.Error("the lock is still held")
	}
}

func TestApplyNixOSCheckCred(t *testing.T) {
	t.Parallel()
	existing := []byte("sealed on this host before")
	credReq := func() Request {
		req := nixApplyRequest()
		req.NixCred = existing
		return req
	}

	t.Run("decrypts", func(t *testing.T) {
		t.Parallel()
		r := newNixRig(freshNixHost())
		r.creds.Accept(existing)
		res, err := r.apply(r.input(t, credReq(), nil, nil))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if res.Cred != nil {
			t.Errorf("Result.Cred = %q, want none: the existing credential stays", res.Cred)
		}
		if calls := r.creds.Calls(); !slices.Equal(calls, []string{"decrypt vault-key"}) {
			t.Errorf("Creds calls = %q, want one decrypt of vault-key", calls)
		}
		if res.NixState != NixFresh || res.KeyShown {
			t.Errorf("NixState %q KeyShown %v", res.NixState, res.KeyShown)
		}
		r.wantNoWrites(t)
	})

	t.Run("does not decrypt", func(t *testing.T) {
		t.Parallel()
		r := newNixRig(freshNixHost())
		_, err := r.apply(r.input(t, credReq(), nil, nil))
		ref := asRefusal(t, err)
		if want := "the credential in /home/alice/chainmail-nixos does not decrypt on this host"; ref.Msg != want {
			t.Errorf("refusal = %q, want %q", ref.Msg, want)
		}
		if want := "move it aside and run bootstrap again to seal a new key"; ref.Hint != want {
			t.Errorf("hint = %q, want %q", ref.Hint, want)
		}
		if calls := r.creds.Calls(); !slices.Equal(calls, []string{"decrypt vault-key"}) {
			t.Errorf("Creds calls = %q, want only the decrypt", calls)
		}
	})

	t.Run("systemd-creds cannot run", func(t *testing.T) {
		t.Parallel()
		r := newNixRig(freshNixHost())
		r.creds.Accept(existing)
		r.creds.DecryptErr = fmt.Errorf("systemd-creds decrypt --name=vault-key - -: %w", exec.ErrNotFound)
		_, err := r.apply(r.input(t, credReq(), nil, nil))
		ref := asRefusal(t, err)
		const prefix = "systemd-creds could not check the credential in /home/alice/chainmail-nixos: "
		if !strings.HasPrefix(ref.Msg, prefix) || !strings.Contains(ref.Msg, exec.ErrNotFound.Error()) {
			t.Errorf("refusal = %q, want %q and the cause", ref.Msg, prefix)
		}
		if want := "make sure systemd-creds works on this host (it ships with systemd), then run bootstrap again"; ref.Hint != want {
			t.Errorf("hint = %q, want %q", ref.Hint, want)
		}
		if strings.Contains(ref.Msg+ref.Hint, "move it aside") {
			t.Error("the refusal tells the user to move a credential that may be fine")
		}
		if calls := r.creds.Calls(); !slices.Equal(calls, []string{"decrypt vault-key"}) {
			t.Errorf("Creds calls = %q, want only the decrypt, never an encrypt", calls)
		}
	})

	t.Run("too large", func(t *testing.T) {
		t.Parallel()
		r := newNixRig(freshNixHost())
		req := nixApplyRequest()
		req.NixCred = bytes.Repeat([]byte("x"), 64<<10+1)
		r.creds.Accept(req.NixCred)
		_, entries, err := r.applyLogged(r.input(t, req, nil, nil))
		wantRefusal(t, err, "credential")
		if calls := r.creds.Calls(); len(calls) > 0 {
			t.Errorf("Creds calls = %q, want none for an oversized credential", calls)
		}
		if countPrefix(entries, "lock ") > 0 {
			t.Errorf("journal = %q, want the size refused before the lock", entries)
		}
	})

	t.Run("64 KiB passes", func(t *testing.T) {
		t.Parallel()
		r := newNixRig(freshNixHost())
		req := nixApplyRequest()
		req.NixCred = bytes.Repeat([]byte("x"), 64<<10)
		r.creds.Accept(req.NixCred)
		if _, err := r.apply(r.input(t, req, nil, nil)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	})
}

// finishRig is a NixOS host whose module config has alice's Gmail
// account, with chainmail.service running.
func finishRig() *nixRig {
	r := newNixRig(moduleNixHost(moduleConfig(aliceGmail, nil)))
	r.svc.SetState(system.ServiceState{Loaded: true, Running: true})
	return r
}

func TestApplyNixOSFinish(t *testing.T) {
	t.Parallel()
	r := finishRig()
	in := r.input(t, nixApplyRequest(), nil, testSecret)

	res, err := r.apply(in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{
		"secret set accounts/alice --output json " + nixSocketArgs,
		"accounts test alice --output json " + nixSocketArgs,
	}
	if got := r.daemonCalls(); !slices.Equal(got, want) {
		t.Errorf("daemon calls:\n got %q\nwant %q", got, want)
	}
	if calls := r.daemon.Calls(); len(calls) > 0 && !bytes.Equal(calls[0].Stdin, testSecret) {
		t.Errorf("secret set read %q from stdin, want the secret", calls[0].Stdin)
	}
	if res.Test != testOK || !res.ServiceRunning || res.NixState != NixFinish {
		t.Errorf("Test %q ServiceRunning %v NixState %q", res.Test, res.ServiceRunning, res.NixState)
	}
	if res.Cred != nil {
		t.Errorf("Result.Cred = %q, want none in Finish", res.Cred)
	}
	if calls := r.creds.Calls(); len(calls) > 0 {
		t.Errorf("Creds calls = %q, want none in Finish", calls)
	}
	r.wantNoServiceChange(t)
	r.wantNoWrites(t)
	if countPrefix(r.journal.Entries(), "lock "+nixLock) != 1 || r.nh.files.Locked(nixLock) {
		t.Errorf("journal = %q, want one lock of %s", r.journal.Entries(), nixLock)
	}

	t.Run("healthy account only tests", func(t *testing.T) {
		t.Parallel()
		r := finishRig()
		res, err := r.apply(r.input(t, nixApplyRequest(), okHealth, nil))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		want := []string{"accounts test alice --output json " + nixSocketArgs}
		if got := r.daemonCalls(); !slices.Equal(got, want) {
			t.Errorf("daemon calls:\n got %q\nwant %q", got, want)
		}
		if res.Test != testOK {
			t.Errorf("Test = %q, want ok", res.Test)
		}
		r.wantNoServiceChange(t)
	})
}

func TestApplyNixOSFinishNotRunning(t *testing.T) {
	t.Parallel()
	r := finishRig()
	in := r.input(t, nixApplyRequest(), nil, testSecret)
	// The service stopped after bootstrap planned, while systemd still
	// reported the unit active to the re-inspection.
	r.svc.SetState(system.ServiceState{Loaded: true})

	_, err := r.apply(in)
	ref := asRefusal(t, err)
	if !strings.Contains(ref.Msg, "chainmail.service is not active") {
		t.Errorf("refusal = %q, want it to say chainmail.service is not active", ref.Msg)
	}
	if want := "check it with: journalctl -u chainmail"; ref.Hint != want {
		t.Errorf("hint = %q, want %q", ref.Hint, want)
	}
	if calls := r.daemonCalls(); len(calls) > 0 {
		t.Errorf("daemon calls = %q, want none", calls)
	}
	r.wantNoServiceChange(t)
}

func TestApplyNixOSRecheck(t *testing.T) {
	t.Parallel()
	r := finishRig()
	in := r.input(t, nixApplyRequest(), nil, testSecret)
	// A rebuild between planning and apply moved the unit to a new store
	// path.
	const rebuilt = "/nix/store/y-chainmail/bin/chainmail"
	r.nh.files.Dir("/nix/store/y-chainmail", 0, 0, 0o555).
		Dir("/nix/store/y-chainmail/bin", 0, 0, 0o555).
		File(rebuilt, 0, 0, 0o555, []byte("chainmail after the rebuild"))
	r.nh.units.Info.ExecPath = rebuilt

	cur := r.nh.inspect(t)
	if Same(mustBuildNixOS(t, cur, in.Request), in.Plan) {
		t.Fatal("Same = true after the daemon binary changed")
	}
	_, err := r.apply(in)
	wantRefusal(t, err, "the installation changed while bootstrap was running; run it again")
	if calls := r.daemonCalls(); len(calls) > 0 {
		t.Errorf("daemon calls = %q, want none", calls)
	}
}

func TestApplyNixOSValidates(t *testing.T) {
	t.Parallel()
	r := newNixRig(freshNixHost())
	in := r.input(t, nixApplyRequest(), nil, nil)
	in.Plan.Actions = append(in.Plan.Actions, Action{
		Kind: ActWriteService, Entry: "service", Data: []byte("[Unit]\n"), Note: "write the service",
	})

	_, entries, err := r.applyLogged(in)
	wantRefusal(t, err, "write-service")
	if countPrefix(entries, "lock ") > 0 {
		t.Errorf("journal = %q, want the plan refused before the lock", entries)
	}
	if calls := r.creds.Calls(); len(calls) > 0 {
		t.Errorf("Creds calls = %q, want none", calls)
	}
}

func TestApplyNixOSResult(t *testing.T) {
	t.Parallel()
	l := LayoutFor(NixOS)
	wantPaths := map[string]string{
		"config":    l.Config,
		"state-dir": l.StateDir,
		"vault":     l.Vault,
		"socket":    l.Socket,
		"service":   l.Service,
	}

	t.Run("fresh", func(t *testing.T) {
		t.Parallel()
		r := newNixRig(freshNixHost())
		in := r.input(t, nixApplyRequest(), nil, nil)
		res, err := r.apply(in)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if !maps.Equal(res.Paths, wantPaths) {
			t.Errorf("Paths = %v, want %v", res.Paths, wantPaths)
		}
		// Before the rebuild there is no group to join and no daemon to
		// list accounts from: the steps are the plan's.
		if !slices.Equal(res.NextSteps, in.Plan.NextSteps) {
			t.Errorf("NextSteps:\n got %q\nwant %q", res.NextSteps, in.Plan.NextSteps)
		}
		if res.Account != "alice" || res.Provider != "gmail" {
			t.Errorf("Account %q Provider %q", res.Account, res.Provider)
		}
		out, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal the result: %v", err)
		}
		if !bytes.Contains(out, []byte(`"nix_state":"fresh"`)) || !bytes.Contains(out, []byte(`"cred":`)) {
			t.Errorf("result JSON = %s, want nix_state and cred", out)
		}
	})

	t.Run("finish", func(t *testing.T) {
		t.Parallel()
		r := finishRig()
		res, err := r.apply(r.input(t, nixApplyRequest(), nil, testSecret))
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if !maps.Equal(res.Paths, wantPaths) {
			t.Errorf("Paths = %v, want %v", res.Paths, wantPaths)
		}
		if len(res.NextSteps) == 0 || res.NextSteps[0] != "log out and back in, or run: newgrp chainmail-clients" {
			t.Errorf("NextSteps = %q, want the newgrp step first", res.NextSteps)
		}
	})
}
