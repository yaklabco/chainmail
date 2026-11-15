package setup

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// nixStorePrefix starts every path in the Nix store.
const nixStorePrefix = NixStore + "/"

// The lines of the printed steps that name the chainmail flake.
const (
	flakeInputLine = `inputs.chainmail.url = "github:yaklabco/chainmail";`
	flakeModule    = "inputs.chainmail.nixosModules.default"
	rebuildStep    = "Rebuild the system the way you normally do, for example\n  sudo nixos-rebuild switch"
)

// notByModuleHint is the hint when chainmail on NixOS was set up some
// other way than by the module.
const notByModuleHint = "remove that setup, then set chainmail up with the NixOS module: " + deployPage + "#nixos"

// NixCurrent is a NixOS host as found.
type NixCurrent struct {
	InvokingUser system.User
	Unit         system.UnitInfo
	DaemonUser   *system.User   // nil when missing
	Config       []byte         // nil when missing
	VaultExists  bool           // read like Inspect: a vault it may not look at counts as missing
	Health       *AccountHealth // nil when the daemon could not be asked

	trust *trustSnapshot // the trust chain of DaemonBinary and the lock directory
	// binary is the metadata of Unit.ExecPath, read without following a
	// final symlink. BuildNixOS refuses a daemon binary that is not a
	// regular file, because the trust check passes a missing path.
	binary system.FileInfo
}

// ModuleInstalled reports whether the module installed chainmail: the
// unit is loaded and its fragment resolves into the store.
func (c NixCurrent) ModuleInstalled() bool {
	return c.Unit.Loaded && inNixStore(c.Unit.Fragment)
}

// InspectNixOS reads a NixOS host. It never writes. Health is left nil.
//
// Like Inspect, it runs as the invoking user as well as root, so a path it
// may not look at counts as missing. The config is read through symlinks:
// on NixOS it is a root-owned copy or a link into the store.
func InspectNixOS(ctx context.Context, l Layout, f system.Files, a system.Accounts, u system.Units, invoking string) (NixCurrent, error) {
	var cur NixCurrent
	user, err := lookupUser(ctx, a, invoking)
	if err != nil {
		return NixCurrent{}, err
	}
	if user == nil {
		return NixCurrent{}, fmt.Errorf("inspect: invoking user %q does not exist", invoking)
	}
	cur.InvokingUser = *user

	if cur.Unit, err = u.Show(ctx, l.ServiceLabel); err != nil {
		return NixCurrent{}, fmt.Errorf("inspect %s: %w", l.ServiceLabel, err)
	}
	if cur.DaemonUser, err = lookupUser(ctx, a, l.DaemonUser); err != nil {
		return NixCurrent{}, err
	}
	if cur.Config, err = readFollowing(f, l.Config); err != nil {
		return NixCurrent{}, err
	}
	vault, err := peek(f, l.Vault)
	if err != nil {
		return NixCurrent{}, err
	}
	cur.VaultExists = vault.Exists

	paths := []string{filepath.Dir(l.Lock)}
	if exec := cur.Unit.ExecPath; filepath.IsAbs(exec) {
		paths = append(paths, exec)
		if cur.binary, err = peek(f, exec); err != nil {
			return NixCurrent{}, err
		}
	}
	if cur.trust, err = recordTrustPaths(f, paths); err != nil {
		return NixCurrent{}, err
	}
	return cur, nil
}

// readFollowing reads path like readRegular, but follows symlinks, a
// final one included. It returns nil when the path, or a link's target,
// is missing or is not a regular file.
func readFollowing(f system.Files, path string) ([]byte, error) {
	at := path
	for range maxSymlinkHops {
		info, err := peek(f, at)
		if err != nil || !info.Exists {
			return nil, err
		}
		if info.Mode&fs.ModeSymlink == 0 {
			data, _, err := readRegular(f, at)
			return data, err
		}
		dest := info.LinkDest
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(at), dest)
		}
		at = dest
	}
	return nil, fmt.Errorf("inspect %s: too many symlinks", path)
}

// BuildNixOS plans bootstrap on a NixOS host, or returns a *Refusal. cur
// must come from InspectNixOS, which records the trust chain BuildNixOS
// checks.
//
// It checks the invoking user, the Nix directory and the request, then
// decides the state. Without the module it plans Fresh: seal a new key,
// or check the one already in the Nix directory, and write both modules.
// It refuses a host with a vault, the daemon user, a config or a unit the
// module did not make, because a new key could never open that vault and
// the module would fight that setup. With the module it plans Add, which
// writes the account's module, or Finish, which stores the secret, logs in
// and tests an account the config already has. It never plans a change
// to the system configuration or to the service.
func BuildNixOS(l Layout, cur NixCurrent, req Request) (Plan, error) {
	if l.Platform != NixOS {
		return Plan{}, fmt.Errorf("plan: BuildNixOS needs the NixOS layout, not %s", l.Platform)
	}
	if err := checkInvoking(l, cur.InvokingUser); err != nil {
		return Plan{}, err
	}
	if !cleanAbs(req.NixDir) {
		return Plan{}, fmt.Errorf("plan: --nix-dir %q is not a clean absolute path", req.NixDir)
	}
	backup, err := checkRequest(req)
	if err != nil {
		return Plan{}, err
	}
	if cur.trust == nil {
		return Plan{}, errors.New("plan: the host was not inspected")
	}
	name, err := requestName(req)
	if err != nil {
		return Plan{}, err
	}
	state, cfg, err := nixState(l, cur, name)
	if err != nil {
		return Plan{}, err
	}
	spec, client, _, err := resolveAccount(cfg, name, req)
	if err != nil {
		return Plan{}, err
	}

	p := Plan{
		Version:      planVersion,
		Platform:     NixOS,
		InvokingUser: cur.InvokingUser.Name,
		Account:      spec,
		OAuthClient:  client,
		SecretName:   secretName(spec),
		Backup:       BackupNone,
		NixState:     state,
		NixDir:       req.NixDir,
	}
	switch state {
	case NixFresh:
		err = planNixFresh(l, cur, req, backup, &p)
	case NixAdd:
		planNixAdd(cfg, &p)
	case NixFinish:
		err = planNixFinish(l, cur, req.Terminal, &p)
	}
	if err != nil {
		return Plan{}, err
	}
	return p, nil
}

// nixState decides where the host stands, refusing chainmail set up on it
// without the module, and returns the parsed config (nil when there is
// none).
func nixState(l Layout, cur NixCurrent, name string) (NixState, *config.Config, error) {
	if !cur.ModuleInstalled() {
		if cur.VaultExists {
			return "", nil, refuse("restore the chainmail module with its credential, or move "+l.Vault+" aside, then run bootstrap again",
				"the vault %s exists but there is no chainmail module to open it; a new key could never open that vault", l.Vault)
		}
		var found string
		switch {
		case cur.Unit.Loaded:
			found = l.ServiceLabel + " is loaded from " + cmp.Or(cur.Unit.Fragment, "outside the Nix store")
		case cur.DaemonUser != nil:
			found = "the user " + l.DaemonUser + " exists"
		case cur.Config != nil:
			found = l.Config + " exists"
		default:
			return NixFresh, nil, nil
		}
		return "", nil, refuse(notByModuleHint, "chainmail is set up on this NixOS host, but not by the NixOS module: %s", found)
	}
	var cfg *config.Config
	if cur.Config != nil {
		var err error
		if cfg, err = config.Parse(cur.Config); err != nil {
			return "", nil, refuse("fix the config in your NixOS configuration so it parses, rebuild, then run bootstrap again",
				"the config %s does not parse: %v", l.Config, err)
		}
	}
	if cfg != nil {
		if _, ok := cfg.Accounts[name]; ok {
			return NixFinish, cfg, nil
		}
	}
	return NixAdd, cfg, nil
}

// planNixFresh plans the key and both modules on a host without the
// module.
func planNixFresh(l Layout, cur NixCurrent, req Request, backup Backup, p *Plan) error {
	if err := checkLockTrust(l, cur); err != nil {
		return err
	}
	cred := filepath.Join(p.NixDir, CredFile)
	if req.NixCred != nil {
		// An existing credential is never replaced: apply checks it
		// decrypts here, and refuses when it does not.
		p.add(Action{Kind: ActCheckCred, Note: "check that " + cred + " decrypts on this host"})
	} else {
		if backup == BackupShow && !req.Terminal {
			return errBackupNeedsTerminal()
		}
		p.add(Action{Kind: ActSealKey, Note: "seal a new vault key into " + cred})
		p.Backup = backup
	}
	p.add(serviceNixAction(*p))
	p.add(accountNixAction(*p, p.OAuthClient))
	p.NextSteps = freshNixSteps(*p)
	return nil
}

// planNixAdd plans the module of an account the config does not have yet.
// An OAuth client the config already has is left out of it.
func planNixAdd(cfg *config.Config, p *Plan) {
	client := p.OAuthClient
	if client != nil && cfg != nil {
		if _, ok := cfg.OAuthClients[client.Name]; ok {
			client = nil
		}
	}
	p.add(accountNixAction(*p, client))
	p.NextSteps = addNixSteps(*p)
}

// planNixFinish plans the secret, login and test of an account the config
// has, run as the daemon user with the store binary the unit runs.
func planNixFinish(l Layout, cur NixCurrent, terminal bool, p *Plan) error {
	if !cur.Unit.Active {
		return refuse("check it with: journalctl -u chainmail",
			"%s is not active; bootstrap never starts it on NixOS", l.ServiceLabel)
	}
	if cur.DaemonUser == nil {
		return refuse(notByModuleHint, "the user %s does not exist, although %s is loaded from the Nix store", l.DaemonUser, l.ServiceLabel)
	}
	if err := checkDaemonIdentity(cur.DaemonUser, cur.InvokingUser); err != nil {
		return err
	}
	bin := cur.Unit.ExecPath
	if !inNixStore(bin) {
		return refuse("load chainmail through the NixOS module: "+deployPage+"#nixos",
			"the daemon binary %q of %s is not in the Nix store", bin, l.ServiceLabel)
	}
	// The trust check passes a missing path, which a build user could
	// create in the store, so the binary must exist first.
	if !cur.binary.Exists || !cur.binary.Mode.IsRegular() {
		return refuse("check "+l.ServiceLabel+" with: systemctl status chainmail",
			"the daemon binary %s of %s is missing or is not a regular file", bin, l.ServiceLabel)
	}
	if err := CheckTrust(cur.trust, bin, []uint32{0}); err != nil {
		return err
	}
	if err := checkLockTrust(l, cur); err != nil {
		return err
	}
	p.DaemonBinary = bin
	p.NeedSecret = !healthy(cur.Health)
	planSecret(p, terminal)
	return nil
}

// checkLockTrust checks the trust chain of the directory bootstrap apply
// takes its lock in. Only root may own it.
func checkLockTrust(l Layout, cur NixCurrent) error {
	return CheckTrust(cur.trust, filepath.Dir(l.Lock), []uint32{0})
}

// serviceNixAction writes chainmail.nix for the plan's invoking user.
func serviceNixAction(p Plan) Action {
	return Action{
		Kind: ActWriteNix, Entry: ServiceNixFile, Data: RenderServiceNix(p.InvokingUser),
		Note: "write " + filepath.Join(p.NixDir, ServiceNixFile),
	}
}

// accountNixAction writes the plan's account module, with client c.
func accountNixAction(p Plan, c *OAuthClientSpec) Action {
	file := AccountNixFile(p.Account.Name)
	return Action{
		Kind: ActWriteNix, Entry: file, Data: RenderAccountNix(p.Account, c),
		Note: "write " + filepath.Join(p.NixDir, file) + " for account " + p.Account.Name,
	}
}

// freshNixSteps are the steps after Fresh: move the directory into the
// configuration and import both modules, rebuild, run bootstrap again.
// Each step's lines after the first are indented by two spaces.
func freshNixSteps(p Plan) []string {
	dir := "./" + filepath.Base(p.NixDir) + "/"
	return []string{
		"Move " + p.NixDir + " into your NixOS configuration (for example /etc/nixos, or your flake's repository), " +
			"then add the chainmail flake input and import the files:\n" +
			"  " + flakeInputLine + "\n" +
			"  modules = [ " + flakeModule + "\n" +
			"              " + dir + ServiceNixFile + "\n" +
			"              " + dir + AccountNixFile(p.Account.Name) + " ];",
		rebuildStep,
		rerunStep(p),
	}
}

// addNixSteps are the steps after Add: import the account's module,
// rebuild, run bootstrap again.
func addNixSteps(p Plan) []string {
	return []string{
		"Import ./" + filepath.Base(p.NixDir) + "/" + AccountNixFile(p.Account.Name) +
			" from your NixOS configuration, next to the chainmail module.",
		rebuildStep,
		rerunStep(p),
	}
}

// rerunStep runs bootstrap again for the plan's account, naming it when
// the address alone would give another name.
func rerunStep(p Plan) string {
	cmd := "chainmail bootstrap " + p.Account.Address
	if derived, err := AccountName(p.Account.Address); err != nil || derived != p.Account.Name {
		cmd += " --name " + p.Account.Name
	}
	return "Run bootstrap again to store the password and test the login:\n  " + cmd
}

// cleanAbs reports whether path is absolute, already clean and free of
// control characters.
func cleanAbs(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsFunc(path, unicode.IsControl)
}

// inNixStore reports whether path is a clean absolute path below the
// store directory.
func inNixStore(path string) bool {
	return cleanAbs(path) && strings.HasPrefix(path, nixStorePrefix) && len(path) > len(nixStorePrefix)
}

// validateNixOS is Validate for a NixOS plan, after the checks every plan
// gets. Fresh must seal a key or check the existing one, then write
// chainmail.nix and the account's module; Add writes only the account's
// module; Finish stores, logs in and tests as Build's plans do, with a
// daemon binary in the store. Every write-nix's bytes are rendered again
// and compared, and the next steps are recomputed.
func validateNixOS(l Layout, p Plan) error {
	if !cleanAbs(p.NixDir) {
		return refuse(validateHint, "plan names the Nix directory %q", p.NixDir)
	}
	if p.ServiceChanged {
		return refuse(validateHint, "plan changes the service on NixOS")
	}
	for _, a := range p.Actions {
		if err := validateNixAction(l, p, a); err != nil {
			return err
		}
	}
	switch p.NixState {
	case NixFresh, NixAdd:
		return validateNixFiles(p)
	case NixFinish:
		return validateNixFinish(l, p)
	default:
		return refuse(validateHint, "plan has NixOS state %q", p.NixState)
	}
}

// validateNixAction checks one action of a NixOS plan.
func validateNixAction(l Layout, p Plan, a Action) error {
	bad := func(why string) error {
		return refuse(validateHint, "plan step %s %s%s: %s", a.Kind, a.Entry, a.Name, why)
	}
	switch a.Kind {
	case ActSealKey, ActCheckCred:
		if badNote(a.Note) {
			return bad("has an empty note or one with control characters")
		}
		if a.Data != nil {
			return bad("carries file contents")
		}
		return wantTarget(a, "", "", bad)
	case ActWriteNix:
		if badNote(a.Note) {
			return bad("has an empty note or one with control characters")
		}
		if a.Name != "" {
			return bad("names the wrong target")
		}
		switch a.Entry {
		case ServiceNixFile:
			if !bytes.Equal(a.Data, RenderServiceNix(p.InvokingUser)) {
				return bad("differs from the module this binary writes")
			}
		case AccountNixFile(p.Account.Name):
			if !accountNixMatches(p, a.Data) {
				return bad("differs from the module this binary writes")
			}
		default:
			return bad("is not a file bootstrap writes")
		}
		return nil
	case ActStoreSecret, ActAuthLogin, ActTestAccount:
		return validateAction(l, p, a)
	default:
		return bad("is not a step bootstrap plans on NixOS")
	}
}

// accountNixMatches reports whether data is the account module this
// binary renders for the plan. In Add an OAuth client the config already
// has is left out of the file, and the plan cannot say which, so either
// form passes there.
func accountNixMatches(p Plan, data []byte) bool {
	if bytes.Equal(data, RenderAccountNix(p.Account, p.OAuthClient)) {
		return true
	}
	return p.NixState == NixAdd && p.OAuthClient != nil && bytes.Equal(data, RenderAccountNix(p.Account, nil))
}

// validateNixFiles checks a Fresh or Add plan: its steps, in order, and
// its fields.
func validateNixFiles(p Plan) error {
	if p.DaemonBinary != "" {
		return refuse(validateHint, "plan for a %s host names a daemon binary", p.NixState)
	}
	if p.NeedSecret {
		return refuse(validateHint, "plan for a %s host stores a secret", p.NixState)
	}
	want := []string{nixStep(ActWriteNix, AccountNixFile(p.Account.Name))}
	steps := addNixSteps(p)
	sealed := false
	if p.NixState == NixFresh {
		key := ActSealKey
		if len(p.Actions) > 0 && p.Actions[0].Kind == ActCheckCred {
			key = ActCheckCred
		}
		sealed = key == ActSealKey
		want = append([]string{nixStep(key, ""), nixStep(ActWriteNix, ServiceNixFile)}, want...)
		steps = freshNixSteps(p)
	}
	got := make([]string, 0, len(p.Actions))
	for _, a := range p.Actions {
		got = append(got, nixStep(a.Kind, a.Entry))
	}
	if !slices.Equal(got, want) {
		return refuse(validateHint, "plan for a %s host has steps %q, want %q", p.NixState, got, want)
	}
	if p.Backup == BackupShow && !sealed {
		return refuse(validateHint, "plan shows a vault key it does not seal")
	}
	if !slices.Equal(p.NextSteps, steps) {
		return refuse(validateHint, "plan has next steps %q", p.NextSteps)
	}
	return nil
}

// nixStep names an action by its kind and file, such as
// "write-nix chainmail.nix".
func nixStep(kind ActionKind, entry string) string {
	if entry == "" {
		return string(kind)
	}
	return string(kind) + " " + entry
}

// validateNixFinish checks a Finish plan: a daemon binary in the store,
// and the secret, login and test steps under the rules Build's plans
// follow.
func validateNixFinish(l Layout, p Plan) error {
	if !inNixStore(p.DaemonBinary) {
		return refuse(validateHint, "plan runs the daemon binary %q, which is not in the Nix store", p.DaemonBinary)
	}
	for _, a := range p.Actions {
		if !slices.Contains([]ActionKind{ActStoreSecret, ActAuthLogin, ActTestAccount}, a.Kind) {
			return refuse(validateHint, "plan step %s does not belong in a finish plan", a.Kind)
		}
	}
	counts, err := validateOrder(l, p.Actions)
	if err != nil {
		return err
	}
	return validateSteps(p, counts)
}
