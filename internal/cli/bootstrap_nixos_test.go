package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/api"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/setup"
	"github.com/yaklabco/chainmail/internal/setup/setuptest"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// The fake NixOS host: the --nix-dir alice passes, the unit and daemon
// binary the module put in the store, and the ids the module gave the
// daemon user, its groups and the store.
const (
	nixTestDir       = "/home/alice/src/nixos/chainmail-nixos"
	nixStoreFragment = "/nix/store/x-unit/chainmail.service"
	nixStoreBinary   = "/nix/store/x-chainmail/bin/chainmail"
	nixDaemonUID     = 991
	nixDaemonGID     = 992
	nixClientsGID    = 993
	nixbldGID        = 30000
)

// The accounts the NixOS tests set up: alice's, which bootstrap runs
// for, and bob's, which a module config may already hold.
var (
	nixAlice = setup.AccountSpec{Name: "alice", Provider: "gmail", Address: "alice@gmail.com", Auth: provider.AuthPassword}
	nixBob   = setup.AccountSpec{Name: "bob", Provider: "gmail", Address: "bob@gmail.com", Auth: provider.AuthPassword}
)

// newNixBootRig is a NixOS host. With config nil it has no chainmail;
// otherwise the chainmail module is installed and its config.toml holds
// config, with chainmail.service loaded from the store and running.
func newNixBootRig(t *testing.T, config []byte) *bootRig {
	t.Helper()
	r := newBootRig(t, "linux")
	r.l = setup.LayoutFor(setup.NixOS)
	r.units = &setuptest.Units{}
	r.creds = &setuptest.Creds{}
	r.host.Files.Journal = &setuptest.Journal{}
	alice, _ := r.host.Accounts.User("alice")
	r.host.Files.File("/etc/NIXOS", 0, 0, 0o644, nil).
		Dir("/nix", 0, 0, 0o755).
		Dir(setup.NixStore, 0, nixbldGID, fs.ModeSticky|0o775).
		Dir("/home/alice/src", alice.UID, alice.GID, 0o755).
		Dir("/home/alice/src/nixos", alice.UID, alice.GID, 0o755)
	if config == nil {
		return r
	}
	r.host.Files.Dir("/nix/store/x-unit", 0, 0, 0o555).
		File(nixStoreFragment, 0, 0, 0o444, []byte("[Unit]\n")).
		Dir("/nix/store/x-chainmail", 0, 0, 0o555).
		Dir("/nix/store/x-chainmail/bin", 0, 0, 0o555).
		File(nixStoreBinary, 0, 0, 0o555, []byte("chainmail from the store")).
		Dir(r.l.ConfigDir, 0, 0, 0o755).
		File(r.l.Config, 0, 0, 0o444, config).
		Dir(r.l.StateDir, nixDaemonUID, nixDaemonGID, 0o700).
		File(r.l.Vault, nixDaemonUID, nixDaemonGID, 0o600, []byte("vault"))
	r.host.Accounts.AddUser(system.User{
		Name: r.l.DaemonUser, UID: nixDaemonUID, GID: nixDaemonGID,
		Home: "/var/empty", Shell: "/run/current-system/sw/bin/nologin",
	})
	r.host.Accounts.AddGroup(system.GroupInfo{Name: r.l.DaemonUser, GID: nixDaemonGID})
	r.host.Accounts.AddGroup(system.GroupInfo{Name: r.l.ClientGroup, GID: nixClientsGID, Members: []string{"alice"}})
	r.units.Info = system.UnitInfo{Loaded: true, Fragment: nixStoreFragment, ExecPath: nixStoreBinary, Active: true}
	r.svc.SetState(system.ServiceState{Loaded: true, Running: true})
	return r
}

// nixModuleConfig is the config.toml the module renders for spec.
func nixModuleConfig(spec setup.AccountSpec) []byte {
	return setup.NewConfig(setup.LayoutFor(setup.NixOS), spec, nil)
}

// nixPath is file name's path in the test's Nix directory.
func nixPath(name string) string { return nixTestDir + "/" + name }

// wantNixFile fails unless the Nix directory holds name with data, mode
// 0644, owned by alice.
func wantNixFile(t *testing.T, r *bootRig, name string, data []byte) {
	t.Helper()
	got, info, ok := r.host.Files.Content(nixPath(name))
	if !ok {
		t.Errorf("%s was not written", name)
		return
	}
	if !bytes.Equal(got, data) {
		t.Errorf("%s holds %q, want %q", name, got, data)
	}
	if info.Mode != 0o644 || info.UID != setuptest.AliceLinuxUID || info.GID != setuptest.AliceLinuxUID {
		t.Errorf("%s has mode %v owner %d:%d, want 0644 owned by alice", name, info.Mode, info.UID, info.GID)
	}
}

// wantNoNixFiles fails when the Nix directory holds any file bootstrap
// writes.
func wantNoNixFiles(t *testing.T, r *bootRig) {
	t.Helper()
	for _, name := range []string{setup.CredFile, setup.ServiceNixFile, setup.AccountNixFile("alice")} {
		if _, _, ok := r.host.Files.Content(nixPath(name)); ok {
			t.Errorf("%s exists", name)
		}
	}
}

// wantFileStatus fails unless the summary lists file name as status.
func wantFileStatus(t *testing.T, summary, name, status string) {
	t.Helper()
	if !regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + ` +` + status + `$`).MatchString(summary) {
		t.Errorf("summary does not list %s as %s:\n%s", name, status, summary)
	}
}

// probeRE matches the writability probe bootstrap makes in the Nix
// directory before bootstrap apply.
var probeRE = regexp.MustCompile(`/\.chainmail-probe-[0-9a-f]+`)

// nixFileWrites is the paths written through Files, without the
// writability probe. It fails unless every probe made was removed again.
func nixFileWrites(t *testing.T, r *bootRig) []string {
	t.Helper()
	var out []string
	probes := map[string]int{}
	for _, w := range r.host.Files.Writes() {
		if probeRE.MatchString(w) {
			probes[w]++
			continue
		}
		out = append(out, w)
	}
	for p, n := range probes {
		if _, _, ok := r.host.Files.Content(p); ok || n != 2 {
			t.Errorf("probe %s changed %d times and is still there: %v; want made and removed", p, n, ok)
		}
	}
	return out
}

// unsealedKey is the vault key the fake Creds sealed into cred.
func unsealedKey(t *testing.T, cred []byte) string {
	t.Helper()
	enc, ok := bytes.CutPrefix(cred, []byte("sealed:"))
	if !ok {
		t.Fatalf("the credential (%d bytes) was not sealed by the fake", len(cred))
	}
	key, err := base64.StdEncoding.DecodeString(string(enc))
	if err != nil {
		t.Fatalf("decode the sealed credential: %v", err)
	}
	for line := range strings.Lines(string(key)) {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "AGE-SECRET-KEY-") {
			return s
		}
	}
	t.Fatal("the sealed key holds no AGE-SECRET-KEY line")
	return ""
}

func TestBootstrapNixOSFresh(t *testing.T) {
	t.Parallel()
	r := newNixBootRig(t, nil)
	r.terminal = true
	r.lines = []string{"1", "y"} // the backup choice, then the confirmation
	got := r.run("alice@gmail.com", "--nix-dir", nixTestDir)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(r.sudoArgs) != 1 {
		t.Errorf("sudo calls = %d, want 1", len(r.sudoArgs))
	}
	if r.passwordReads != 0 {
		t.Errorf("password reads = %d, want none before the module is installed", r.passwordReads)
	}
	if calls := r.creds.Calls(); !slices.Equal(calls, []string{"encrypt vault-key"}) {
		t.Errorf("Creds calls = %q, want one encrypt", calls)
	}

	// The directory was made, then the credential written first.
	if _, info, _ := r.host.Files.Content(nixTestDir); !info.Mode.IsDir() || info.Mode.Perm() != 0o755 || info.UID != setuptest.AliceLinuxUID {
		t.Errorf("Nix directory: mode %v owner %d, want a 0755 directory owned by alice", info.Mode, info.UID)
	}
	cred, _, _ := r.host.Files.Content(nixPath(setup.CredFile))
	if !bytes.HasPrefix(cred, []byte("sealed:")) {
		t.Errorf("the credential (%d bytes) is not what the fake sealed", len(cred))
	}
	wantNixFile(t, r, setup.CredFile, cred)
	wantNixFile(t, r, setup.ServiceNixFile, setup.RenderServiceNix("alice"))
	wantNixFile(t, r, setup.AccountNixFile("alice"), setup.RenderAccountNix(nixAlice, nil))
	// The directory is made and proved writable before bootstrap apply
	// takes its lock; the credential is written first after it.
	var changes []string
	for _, e := range r.host.Files.Journal.Entries() {
		if strings.Contains(e, nixTestDir) || strings.Contains(e, "lock ") {
			changes = append(changes, probeRE.ReplaceAllString(e, "/.chainmail-probe-*"))
		}
	}
	want := []string{
		"mkdir " + nixTestDir,
		"create " + nixPath(".chainmail-probe-*"),
		"remove " + nixPath(".chainmail-probe-*"),
		"lock /run/chainmail-bootstrap.lock",
		"unlock /run/chainmail-bootstrap.lock",
		"create " + nixPath(setup.CredFile),
		"write " + nixPath(setup.ServiceNixFile),
		"write " + nixPath(setup.AccountNixFile("alice")),
	}
	if !slices.Equal(changes, want) {
		t.Errorf("changes in the Nix directory:\n got %q\nwant %q", changes, want)
	}

	for _, name := range []string{setup.CredFile, setup.ServiceNixFile, setup.AccountNixFile("alice")} {
		wantFileStatus(t, got.stdout, name, "written")
	}
	for _, line := range []string{
		"\n  1. Move " + nixTestDir + " into your NixOS configuration",
		"\n       inputs.chainmail.url = \"github:yaklabco/chainmail\";\n",
		"\n       modules = [ inputs.chainmail.nixosModules.default\n",
		"\n                   ./chainmail-nixos/chainmail.nix\n",
		"\n                   ./chainmail-nixos/chainmail-alice.nix ];\n",
		"\n  2. Rebuild the system the way you normally do, for example\n       sudo nixos-rebuild switch\n",
		"\n  3. Run bootstrap again to store the password and test the login:\n       chainmail bootstrap alice@gmail.com\n",
		sudoDisclosure,
	} {
		if !strings.Contains(got.stdout, line) {
			t.Errorf("summary lacks %q:\n%s", line, got.stdout)
		}
	}
	if strings.Contains(got.stdout, "chainmail.service") || strings.Contains(got.stdout, "\n  4. ") {
		t.Errorf("Fresh summary has a service row or a fourth step:\n%s", got.stdout)
	}

	t.Run("a failed write keeps the credential", func(t *testing.T) {
		t.Parallel()
		r := newNixBootRig(t, nil)
		r.terminal = true
		r.lines = []string{"y"}
		r.host.Files.FailWrite(nixPath(setup.ServiceNixFile), errors.New("disk full"))
		got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--key-backup", "none")
		if got.code == 0 {
			t.Fatalf("exit 0 although %s could not be written; stdout %s", setup.ServiceNixFile, got.stdout)
		}
		if !strings.Contains(got.stderr, "disk full") {
			t.Errorf("stderr lacks the write error:\n%s", got.stderr)
		}
		cred, _, ok := r.host.Files.Content(nixPath(setup.CredFile))
		if !ok || !bytes.HasPrefix(cred, []byte("sealed:")) {
			t.Errorf("the credential is missing after a later write failed")
		}
	})
}

func TestBootstrapNixOSFreshRerun(t *testing.T) {
	t.Parallel()
	r := newNixBootRig(t, nil)
	cred := []byte("sealed on this host before")
	r.creds.Accept(cred)
	r.host.Files.Dir(nixTestDir, setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755).
		File(nixPath(setup.CredFile), setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o644, cred).
		File(nixPath(setup.ServiceNixFile), setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o644, setup.RenderServiceNix("alice")).
		File(nixPath(setup.AccountNixFile("alice")), setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o644, setup.RenderAccountNix(nixAlice, nil))
	r.terminal = true
	r.lines = []string{"y"}
	got := r.run("alice@gmail.com", "--nix-dir", nixTestDir)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(r.sudoArgs) != 1 {
		t.Errorf("sudo calls = %d, want 1, for check-cred", len(r.sudoArgs))
	}
	if calls := r.creds.Calls(); !slices.Equal(calls, []string{"decrypt vault-key"}) {
		t.Errorf("Creds calls = %q, want one decrypt", calls)
	}
	if strings.Contains(got.stderr, "Vault key backup") {
		t.Error("a rerun with a credential asked about the key backup")
	}
	if w := nixFileWrites(t, r); len(w) > 0 {
		t.Errorf("files written: %q", w)
	}
	for _, name := range []string{setup.CredFile, setup.ServiceNixFile, setup.AccountNixFile("alice")} {
		wantFileStatus(t, got.stdout, name, "unchanged")
	}

	// A module that differs is written again; the credential never is.
	r.host.Files.File(nixPath(setup.AccountNixFile("alice")), setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o644, []byte("# edited\n"))
	r.lines = []string{"y"}
	got = r.run("alice@gmail.com", "--nix-dir", nixTestDir)
	if got.code != 0 {
		t.Fatalf("rerun with an edited module: exit %d: %s", got.code, got.stderr)
	}
	if w := nixFileWrites(t, r); !slices.Equal(w, []string{nixPath(setup.AccountNixFile("alice"))}) {
		t.Errorf("files written = %q, want only the account module", w)
	}
	wantNixFile(t, r, setup.AccountNixFile("alice"), setup.RenderAccountNix(nixAlice, nil))
	wantFileStatus(t, got.stdout, setup.AccountNixFile("alice"), "written")
	wantFileStatus(t, got.stdout, setup.CredFile, "unchanged")
}

func TestBootstrapNixOSSealFails(t *testing.T) {
	t.Parallel()
	r := newNixBootRig(t, nil)
	r.host.Files.Dir(nixTestDir, setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755)
	r.creds.EncryptErr = errors.New("no usable key")
	r.terminal = true
	r.lines = []string{"y"}
	got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--key-backup", "none")
	wantError(t, got, api.ExitUsage, "could not seal the vault key", "no usable key")
	if w := nixFileWrites(t, r); len(w) > 0 {
		t.Errorf("files written: %q", w)
	}
	wantNoNixFiles(t, r)
}

func TestBootstrapNixOSFreshBadCred(t *testing.T) {
	t.Parallel()
	r := newNixBootRig(t, nil)
	cred := []byte("sealed on another host")
	r.host.Files.Dir(nixTestDir, setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755).
		File(nixPath(setup.CredFile), setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o600, cred)
	r.terminal = true
	r.lines = []string{"y"}
	got := r.run("alice@gmail.com", "--nix-dir", nixTestDir)
	wantError(t, got, api.ExitUsage, "does not decrypt on this host", "move it aside")
	if w := nixFileWrites(t, r); len(w) > 0 {
		t.Errorf("files written: %q", w)
	}
	after, info, _ := r.host.Files.Content(nixPath(setup.CredFile))
	if !bytes.Equal(after, cred) || info.Mode != 0o600 {
		t.Errorf("the credential changed: %d bytes, mode %v", len(after), info.Mode)
	}

	t.Run("systemd-creds cannot run", func(t *testing.T) {
		t.Parallel()
		r := newNixBootRig(t, nil)
		r.host.Files.Dir(nixTestDir, setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755).
			File(nixPath(setup.CredFile), setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o644, cred)
		r.creds.DecryptErr = errors.New("systemd-creds: not found")
		r.terminal = true
		r.lines = []string{"y"}
		got := r.run("alice@gmail.com", "--nix-dir", nixTestDir)
		wantError(t, got, api.ExitUsage, "systemd-creds could not check the credential", "systemd-creds: not found")
		if w := nixFileWrites(t, r); len(w) > 0 {
			t.Errorf("files written: %q", w)
		}
	})
}

func TestBootstrapNixOSAdd(t *testing.T) {
	t.Parallel()
	r := newNixBootRig(t, nixModuleConfig(nixBob))
	r.host.Files.Dir(nixTestDir, setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755)
	r.terminal = true
	r.lines = []string{"y"}
	got := r.run("alice@gmail.com", "--nix-dir", nixTestDir)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(r.sudoArgs) > 0 {
		t.Errorf("sudo was called %d times; Add only writes a file as alice", len(r.sudoArgs))
	}
	if r.passwordReads != 0 {
		t.Errorf("password reads = %d, want none", r.passwordReads)
	}
	if w := nixFileWrites(t, r); !slices.Equal(w, []string{nixPath(setup.AccountNixFile("alice"))}) {
		t.Errorf("files written = %q, want only the account module", w)
	}
	wantNixFile(t, r, setup.AccountNixFile("alice"), setup.RenderAccountNix(nixAlice, nil))
	wantFileStatus(t, got.stdout, setup.AccountNixFile("alice"), "written")
	for _, want := range []string{
		"\n  1. Import ./chainmail-nixos/chainmail-alice.nix from your NixOS configuration",
		"\n       sudo nixos-rebuild switch\n",
		"\n       chainmail bootstrap alice@gmail.com\n",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, got.stdout)
		}
	}
	if strings.Contains(got.stdout, setup.CredFile) || strings.Contains(got.stdout, "chainmail.service") {
		t.Errorf("Add summary names the credential or the service:\n%s", got.stdout)
	}
}

func TestBootstrapNixOSFinish(t *testing.T) {
	t.Parallel()
	r := newNixBootRig(t, nixModuleConfig(nixAlice))
	r.terminal = true
	r.lines = []string{"y"}
	got := r.run("alice@gmail.com", "--nix-dir", nixTestDir)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if len(r.sudoArgs) != 1 {
		t.Errorf("sudo calls = %d, want 1", len(r.sudoArgs))
	}
	if r.passwordReads != 1 {
		t.Errorf("password reads = %d, want 1", r.passwordReads)
	}
	if !slices.Equal(r.runnerBinaries, []string{nixStoreBinary}) {
		t.Errorf("daemon runners built for %q, want only the store binary %s", r.runnerBinaries, nixStoreBinary)
	}
	v, ok := r.daemon.Secret("accounts/alice")
	checkSecret(t, "accounts/alice", v, ok, bootSecret)
	if w := r.host.Files.Writes(); len(w) > 0 {
		t.Errorf("files written: %q", w)
	}
	for _, want := range []string{
		"chainmail is set up.",
		"alice", "gmail", "chainmail.service, running",
		"\n  1. log out and back in, or run: newgrp chainmail-clients\n",
		"chainmail accounts list",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("summary lacks %q:\n%s", want, got.stdout)
		}
	}
	if !regexp.MustCompile(`(?m)^test +ok$`).MatchString(got.stdout) {
		t.Errorf("summary lacks the test result:\n%s", got.stdout)
	}
}

func TestBootstrapNixOSNeverManagesService(t *testing.T) {
	t.Parallel()
	for name, config := range map[string][]byte{
		"fresh":  nil,
		"add":    nixModuleConfig(nixBob),
		"finish": nixModuleConfig(nixAlice),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newNixBootRig(t, config)
			r.terminal = true
			r.lines = []string{"y"}
			got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--key-backup", "none")
			if got.code != 0 {
				t.Fatalf("exit %d: %s", got.code, got.stderr)
			}
			for _, c := range r.svc.Calls() {
				if strings.HasPrefix(c, "activate") || c == "restart" {
					t.Errorf("service call %q; bootstrap never manages chainmail.service on NixOS", c)
				}
			}
			var commands [][]string
			commands = append(commands, r.sudoArgs...)
			for _, c := range r.daemon.Calls() {
				commands = append(commands, c.Args)
			}
			for _, args := range commands {
				line := strings.Join(args, " ")
				if strings.Contains(line, "nixos-rebuild") || regexp.MustCompile(`systemctl (start|restart|enable)`).MatchString(line) {
					t.Errorf("ran %q", line)
				}
			}
		})
	}
}

func TestBootstrapNixOSDryRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		config []byte
		want   string
	}{
		{"fresh", nil, "seal a new vault key into " + nixPath(setup.CredFile)},
		{"add", nixModuleConfig(nixBob), "write " + nixPath(setup.AccountNixFile("alice")) + " for account alice"},
		{"finish", nixModuleConfig(nixAlice), "test account alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newNixBootRig(t, tc.config)
			r.terminal = true
			got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--dry-run")
			if got.code != 0 {
				t.Fatalf("exit %d: %s", got.code, got.stderr)
			}
			if !strings.Contains(got.stdout, tc.want) {
				t.Errorf("plan lacks %q:\n%s", tc.want, got.stdout)
			}
			if len(r.sudoArgs) > 0 {
				t.Error("dry run called sudo")
			}
			if r.passwordReads > 0 {
				t.Error("dry run asked for the password")
			}
			r.noChanges()
		})
	}
}

func TestBootstrapNixOSNixDirDefault(t *testing.T) {
	t.Parallel()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	r := newNixBootRig(t, nil)
	r.host.Files.Dir(cwd, setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755)
	r.terminal = true
	got := r.run("alice@gmail.com", "--dry-run")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if want := "seal a new vault key into " + cwd + "/chainmail-nixos/" + setup.CredFile; !strings.Contains(got.stdout, want) {
		t.Errorf("plan lacks %q:\n%s", want, got.stdout)
	}
}

func TestBootstrapNixOSRefusesFileInNixDir(t *testing.T) {
	t.Parallel()
	alice := uint32(setuptest.AliceLinuxUID)
	for _, tc := range []struct {
		name  string
		setup func(f *setuptest.Files)
		want  string
	}{
		{"regular file", func(f *setuptest.Files) {
			f.File(nixTestDir, alice, alice, 0o644, []byte("notes"))
		}, "not a directory"},
		{"symlinked directory", func(f *setuptest.Files) {
			f.Symlink(nixTestDir, alice, "/tmp/elsewhere")
		}, "not a directory"},
		{"missing parent", func(f *setuptest.Files) {
			f.Remove("/home/alice/src/nixos")
		}, "does not exist"},
		{"symlink at a module", func(f *setuptest.Files) {
			f.Dir(nixTestDir, alice, alice, 0o755).Symlink(nixPath(setup.ServiceNixFile), alice, "/etc/passwd")
		}, "symlink"},
		{"directory at the credential", func(f *setuptest.Files) {
			f.Dir(nixTestDir, alice, alice, 0o755).Dir(nixPath(setup.CredFile), alice, alice, 0o755)
		}, "not a regular file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newNixBootRig(t, nil)
			tc.setup(r.host.Files)
			r.terminal = true
			r.lines = []string{"y"}
			got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--key-backup", "none")
			wantError(t, got, api.ExitUsage, tc.want)
			if len(r.sudoArgs) > 0 {
				t.Error("sudo was called")
			}
			if w := r.host.Files.Writes(); len(w) > 0 {
				t.Errorf("files written: %q", w)
			}
		})
	}
}

func TestBootstrapNixOSKeyBackupShow(t *testing.T) {
	t.Parallel()
	r := newNixBootRig(t, nil)
	r.terminal = true
	r.lines = []string{"y"}
	got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--key-backup", "show")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	cred, _, _ := r.host.Files.Content(nixPath(setup.CredFile))
	secret := unsealedKey(t, cred)
	if n := strings.Count(r.tty.Output(), secret); n != 1 {
		t.Errorf("the terminal got the key %d times, want once", n)
	}
	for name, s := range map[string]string{"stdout": got.stdout, "stderr": got.stderr} {
		if strings.Contains(s, secret) {
			t.Errorf("the key is on %s", name)
		}
	}
	if !regexp.MustCompile(`(?m)^vault key +shown once on the terminal$`).MatchString(got.stdout) {
		t.Errorf("summary does not say the key was shown:\n%s", got.stdout)
	}
}

func TestBootstrapNixOSJSON(t *testing.T) {
	t.Parallel()
	r := newNixBootRig(t, nil)
	r.terminal = true
	r.lines = []string{"1", "y"}
	got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--output", "json")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	dec := json.NewDecoder(strings.NewReader(got.stdout))
	var res map[string]json.RawMessage
	if err := dec.Decode(&res); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, got.stdout)
	}
	if dec.More() {
		t.Errorf("stdout holds more than one JSON value:\n%s", got.stdout)
	}
	var out struct {
		NixState  string   `json:"nix_state"`
		NixDir    string   `json:"nix_dir"`
		NextSteps []string `json:"next_steps"`
		Files     []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(got.stdout), &out); err != nil {
		t.Fatalf("decode the result: %v", err)
	}
	if out.NixState != "fresh" || out.NixDir != nixTestDir || len(out.NextSteps) != 3 {
		t.Errorf("nix_state %q, nix_dir %q, %d next steps; want fresh, %s, 3", out.NixState, out.NixDir, len(out.NextSteps), nixTestDir)
	}
	var files []string
	for _, f := range out.Files {
		files = append(files, f.Path+" "+f.Status)
	}
	want := []string{
		nixPath(setup.CredFile) + " written",
		nixPath(setup.ServiceNixFile) + " written",
		nixPath(setup.AccountNixFile("alice")) + " written",
	}
	if !slices.Equal(files, want) {
		t.Errorf("files = %q, want %q", files, want)
	}
	if _, ok := res["cred"]; ok {
		t.Error("the result JSON holds the credential")
	}
	cred, _, _ := r.host.Files.Content(nixPath(setup.CredFile))
	b64, _ := json.Marshal(cred)
	if len(cred) == 0 || strings.Contains(got.stdout, string(cred)) || strings.Contains(got.stdout, strings.Trim(string(b64), `"`)) {
		t.Error("stdout holds the credential bytes")
	}

	// Add runs without bootstrap apply, and reports the same paths.
	t.Run("add", func(t *testing.T) {
		t.Parallel()
		r := newNixBootRig(t, nixModuleConfig(nixBob))
		r.terminal = true
		r.lines = []string{"y"}
		got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--output", "json")
		if got.code != 0 {
			t.Fatalf("exit %d: %s", got.code, got.stderr)
		}
		var res struct {
			setup.Result
			Files []map[string]string `json:"files"`
		}
		if err := json.Unmarshal([]byte(got.stdout), &res); err != nil {
			t.Fatalf("decode the result: %v\n%s", err, got.stdout)
		}
		if res.NixState != setup.NixAdd || res.Account != "alice" || res.Test != "skipped" || len(res.NextSteps) != 3 || len(res.Files) != 1 {
			t.Errorf("result = %+v", res)
		}
		l := setup.LayoutFor(setup.NixOS)
		want := map[string]string{"config": l.Config, "state-dir": l.StateDir, "vault": l.Vault, "socket": l.Socket, "service": l.Service}
		if !maps.Equal(res.Paths, want) {
			t.Errorf("paths = %q, want %q", res.Paths, want)
		}
	})
}

func TestBootstrapNixOSUnwritableNixDir(t *testing.T) {
	t.Parallel()
	alice := uint32(setuptest.AliceLinuxUID)
	sealed := []byte("sealed on this host before")
	for _, tc := range []struct {
		name   string
		config []byte
		setup  func(r *bootRig)
	}{
		{"cannot make the directory", nil, func(r *bootRig) {
			r.host.Files.FailWrite(nixTestDir, fs.ErrPermission)
		}},
		{"cannot write in the directory", nil, func(r *bootRig) {
			r.host.Files.Dir(nixTestDir, 0, 0, 0o755).FailWriteIn(nixTestDir, fs.ErrPermission)
		}},
		{"check-cred", nil, func(r *bootRig) {
			r.creds.Accept(sealed)
			r.host.Files.Dir(nixTestDir, 0, 0, 0o755).
				File(nixPath(setup.CredFile), alice, alice, 0o644, sealed).
				FailWriteIn(nixTestDir, fs.ErrPermission)
		}},
		{"add", nixModuleConfig(nixBob), func(r *bootRig) {
			r.host.Files.Dir(nixTestDir, 0, 0, 0o755).FailWriteIn(nixTestDir, fs.ErrPermission)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newNixBootRig(t, tc.config)
			tc.setup(r)
			r.terminal = true
			r.lines = []string{"y"}
			got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--key-backup", "show")
			wantError(t, got, api.ExitUsage, nixTestDir, "choose a --nix-dir you can write to")
			if len(r.sudoArgs) > 0 {
				t.Errorf("sudo was called %d times", len(r.sudoArgs))
			}
			if r.ttyOpens > 0 {
				t.Errorf("the terminal was opened %d times", r.ttyOpens)
			}
			if calls := r.creds.Calls(); len(calls) > 0 {
				t.Errorf("Creds calls = %q, want none", calls)
			}
			for _, name := range []string{setup.ServiceNixFile, setup.AccountNixFile("alice")} {
				if _, _, ok := r.host.Files.Content(nixPath(name)); ok {
					t.Errorf("%s was written", name)
				}
			}
		})
	}
}

// recoveryBlock is the credential bootstrap printed on stderr after it
// could not write it: the save command line, and the base64 lines
// between it and the closing EOF.
func recoveryBlock(t *testing.T, stderr string) (cmd string, lines []string) {
	t.Helper()
	all := strings.Split(stderr, "\n")
	start := slices.IndexFunc(all, func(l string) bool { return strings.HasPrefix(l, "  base64 -d > ") })
	if start < 0 {
		t.Fatalf("stderr has no recovery block:\n%s", stderr)
	}
	end := slices.Index(all[start:], "EOF")
	if end < 0 {
		t.Fatalf("the recovery block has no EOF line:\n%s", stderr)
	}
	return all[start], all[start+1 : start+end]
}

func TestBootstrapNixOSCredWriteFails(t *testing.T) {
	t.Parallel()
	r := newNixBootRig(t, nil)
	r.host.Files.Dir(nixTestDir, setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755).
		FailWrite(nixPath(setup.CredFile), errors.New("disk full"))
	r.terminal = true
	r.lines = []string{"y"}
	got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--key-backup", "none")
	if got.code == 0 {
		t.Fatalf("exit 0 although the credential could not be written")
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want nothing", got.stdout)
	}
	if len(r.sudoArgs) != 1 {
		t.Errorf("sudo calls = %d, want 1", len(r.sudoArgs))
	}
	for _, want := range []string{
		"bootstrap could not write " + nixPath(setup.CredFile) + ": ",
		"disk full",
		"The sealed vault key credential is below. It is encrypted for this host only, so it is safe to save. Save it with:\n",
	} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, got.stderr)
		}
	}
	cmd, lines := recoveryBlock(t, got.stderr)
	if want := "  base64 -d > " + nixPath(setup.CredFile) + " <<'EOF'"; cmd != want {
		t.Errorf("save command = %q, want %q", cmd, want)
	}
	for i, l := range lines {
		if len(l) > 76 || (i < len(lines)-1 && len(l) != 76) {
			t.Errorf("base64 line %d is %d columns, want 76 (shorter only on the last)", i+1, len(l))
		}
	}
	cred, err := base64.StdEncoding.DecodeString(strings.Join(lines, ""))
	if err != nil {
		t.Fatalf("the recovery block is not base64: %v", err)
	}
	if !bytes.HasPrefix(cred, []byte("sealed:")) || r.creds.CheckDecrypt(t.Context(), setup.CredName, cred) != nil {
		t.Errorf("the recovery block (%d bytes) is not the credential bootstrap apply sealed", len(cred))
	}
	if w := nixFileWrites(t, r); len(w) > 0 {
		t.Errorf("files written after the credential failed: %q", w)
	}

	// The printed command saves the credential as it is.
	if bash, err := exec.LookPath("bash"); err == nil {
		out := filepath.Join(t.TempDir(), "saved.cred")
		script := strings.Replace(cmd, nixPath(setup.CredFile), out, 1) + "\n" + strings.Join(lines, "\n") + "\nEOF\n"
		if res, err := exec.CommandContext(t.Context(), bash, "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("running the save command: %v: %s", err, res)
		}
		if saved, err := os.ReadFile(out); err != nil || !bytes.Equal(saved, cred) {
			t.Errorf("the save command wrote %d bytes (%v), want the %d credential bytes", len(saved), err, len(cred))
		}
	}

	t.Run("never in the JSON result", func(t *testing.T) {
		t.Parallel()
		r := newNixBootRig(t, nil)
		r.host.Files.Dir(nixTestDir, setuptest.AliceLinuxUID, setuptest.AliceLinuxUID, 0o755).
			FailWrite(nixPath(setup.CredFile), errors.New("disk full"))
		r.terminal = true
		r.lines = []string{"y"}
		got := r.run("alice@gmail.com", "--nix-dir", nixTestDir, "--key-backup", "none", "--output", "json")
		if got.code == 0 || got.stdout != "" {
			t.Errorf("exit %d, stdout %q; want a failure and nothing on stdout", got.code, got.stdout)
		}
		if _, lines := recoveryBlock(t, got.stderr); len(lines) == 0 {
			t.Error("the recovery block is empty")
		}
	})
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"/home/alice/chainmail-nixos/chainmail-vault-key.cred": "/home/alice/chainmail-nixos/chainmail-vault-key.cred",
		"/home/alice/my config/x.cred":                         "'/home/alice/my config/x.cred'",
		"/home/alice/it's/x.cred":                              `'/home/alice/it'\''s/x.cred'`,
		"/tmp/$(rm -rf ~)/x":                                   "'/tmp/$(rm -rf ~)/x'",
		"":                                                     "''",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
