package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
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
	var changes []string
	for _, e := range r.host.Files.Journal.Entries() {
		if strings.Contains(e, nixTestDir) {
			changes = append(changes, e)
		}
	}
	want := []string{
		"mkdir " + nixTestDir,
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
	if w := r.host.Files.Writes(); len(w) > 0 {
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
	if w := r.host.Files.Writes(); !slices.Equal(w, []string{nixPath(setup.AccountNixFile("alice"))}) {
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
	if w := r.host.Files.Writes(); len(w) > 0 {
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
	if w := r.host.Files.Writes(); len(w) > 0 {
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
		if w := r.host.Files.Writes(); len(w) > 0 {
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
	if w := r.host.Files.Writes(); !slices.Equal(w, []string{nixPath(setup.AccountNixFile("alice"))}) {
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
