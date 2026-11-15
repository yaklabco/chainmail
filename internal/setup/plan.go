package setup

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strings"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
	"github.com/yaklabco/chainmail/internal/setup/system"
)

// planVersion is the Plan format this binary writes and accepts.
const planVersion = 1

// defaultOAuthClient names the OAuth client table when --oauth-client is
// not given.
const defaultOAuthClient = "google"

// deployPage documents production installs, including the NixOS module.
const deployPage = "https://chainmail.sh/docs/operations/deploy"

// nologinShells are the shells a daemon user may have: none of them lets
// anyone log in.
var nologinShells = []string{
	"/usr/sbin/nologin", "/sbin/nologin", "/usr/bin/nologin",
	"/usr/bin/false", "/bin/false",
}

// Request is what the user asked for, as the CLI collected it.
type Request struct {
	Address       string
	Name          string // --name, "" for the default
	Detection     Detection
	OAuthClientID string // --oauth-client-id
	OAuthClient   string // --oauth-client, "" means "google"
	ExecSHA256    string // SHA-256 of the running binary
	Backup        Backup
	Terminal      bool                     // bootstrap has a terminal, so OAuth login can run
	IMAP, SMTP    *config.EndpointOverride // e2e only
	NixDir        string                   // absolute --nix-dir; NixOS only
	NixCred       []byte                   // the credential already in NixDir, nil when none
}

// ActionKind is one kind of step in a plan.
type ActionKind string

// The steps a plan can hold, in the order a plan lists them.
const (
	ActCreateGroup  ActionKind = "create-group"
	ActCreateUser   ActionKind = "create-user"
	ActAddMember    ActionKind = "add-member"
	ActEnsureDir    ActionKind = "ensure-dir"
	ActInstallBin   ActionKind = "install-binary"
	ActCreateKey    ActionKind = "create-key"
	ActWriteConfig  ActionKind = "write-config"
	ActWriteService ActionKind = "write-service"
	ActActivate     ActionKind = "activate-service"
	ActStoreSecret  ActionKind = "store-secret"
	ActAuthLogin    ActionKind = "auth-login"
	ActTestAccount  ActionKind = "test-account"

	// NixOS only. seal-key and check-cred come first, then write-nix.
	ActSealKey   ActionKind = "seal-key"   // root: make the key and seal it
	ActCheckCred ActionKind = "check-cred" // root: the existing credential decrypts here
	ActWriteNix  ActionKind = "write-nix"  // invoking user: write a Nix file
)

// rootActions are the actions bootstrap apply runs as root.
var rootActions = []ActionKind{ActSealKey, ActCheckCred, ActStoreSecret, ActAuthLogin, ActTestAccount}

// NixState is where a NixOS host stands.
type NixState string

// The states of a NixOS host.
const (
	NixFresh  NixState = "fresh"  // no module: seal a key, write the modules
	NixAdd    NixState = "add"    // module, no such account: write its module
	NixFinish NixState = "finish" // account configured: store, log in, test
)

// Action is one step of a plan.
type Action struct {
	Kind  ActionKind
	Entry string // layout entry name for dir and file actions, the file's base name for write-nix, else ""
	Name  string // group, user or account name, else ""
	Data  []byte // file contents for write-config, write-service and write-nix
	Note  string // one line for the printed plan
}

// Plan is everything bootstrap will change, built by Build and checked by
// Validate.
type Plan struct {
	Version        int // 1
	Platform       Platform
	InvokingUser   string
	Account        AccountSpec
	OAuthClient    *OAuthClientSpec
	SecretName     string // accounts/<name> or oauth_clients/<client>
	NeedSecret     bool
	Backup         Backup
	ServiceChanged bool // config, binary or service definition changes
	Actions        []Action
	// NextSteps are commands the user runs after bootstrap because the
	// plan could not do them, such as an OAuth login without a terminal.
	NextSteps []string

	// NixOS only; zero on other platforms.
	NixState     NixState
	NixDir       string // absolute --nix-dir
	DaemonBinary string // Finish: the store binary from the unit's ExecStart
}

// NeedsRoot reports whether p holds an action bootstrap apply runs.
func (p Plan) NeedsRoot() bool {
	return slices.ContainsFunc(p.Actions, func(a Action) bool { return slices.Contains(rootActions, a.Kind) })
}

// Build plans everything from cur and req, or returns a *Refusal. cur
// must come from Inspect, which records the trust chain Build checks.
//
// Build checks the invoking user, the existing daemon user and group and
// the existing vault key first, because the trust check trusts the daemon
// user's uid. Then it checks the trust chain and every other refusal,
// and plans actions only for what is missing or different. Directories
// are always ensured, which fixes their owners and modes. The binary, the
// config and the service definition are rewritten when their bytes differ
// or when their owner, group or mode is not the layout's, so a rewrite
// with unchanged bytes repairs their metadata. It never plans
// a repair to an existing user or group: an account of the wrong shape is
// refused.
//
// A NixOS host is planned by BuildNixOS. Build returns a plain error for
// one, whichever layout it is given, so a caller that forgets to dispatch
// never plans an Ubuntu install there.
func Build(l Layout, cur Current, req Request) (Plan, error) {
	if l.Platform == NixOS || cur.NixOS {
		return Plan{}, errors.New("plan: a NixOS host is planned by BuildNixOS, not Build")
	}
	if cur.Platform != l.Platform {
		return Plan{}, fmt.Errorf("plan: installation inspected for %s, layout is for %s", cur.Platform, l.Platform)
	}
	if err := checkInvoking(l, cur.InvokingUser); err != nil {
		return Plan{}, err
	}
	if err := checkDaemonUser(l, cur); err != nil {
		return Plan{}, err
	}
	if err := checkKey(l, cur); err != nil {
		return Plan{}, err
	}
	if err := checkTrust(l, cur); err != nil {
		return Plan{}, err
	}
	if err := checkHost(l, cur); err != nil {
		return Plan{}, err
	}
	backup, err := checkRequest(req)
	if err != nil {
		return Plan{}, err
	}
	if backup == BackupShow && !cur.KeyExists && !req.Terminal {
		return Plan{}, errBackupNeedsTerminal()
	}
	acct, err := planAccount(l, cur, req)
	if err != nil {
		return Plan{}, err
	}

	p := Plan{
		Version:      planVersion,
		Platform:     l.Platform,
		InvokingUser: cur.InvokingUser.Name,
		Account:      acct.spec,
		OAuthClient:  acct.client,
		SecretName:   secretName(acct.spec),
		Backup:       BackupNone,
	}
	p.NeedSecret = acct.isNew || !healthy(cur.Health)

	p.Actions = planAccounts(l, cur)
	for _, e := range l.Entries() {
		if e.Kind == KindDir {
			p.add(Action{Kind: ActEnsureDir, Entry: e.Name, Note: entryNote(l, e, "ensure directory")})
		}
	}
	binary, _ := l.Entry("binary")
	switch {
	case cur.BinarySHA256 == "":
		p.add(Action{Kind: ActInstallBin, Entry: binary.Name, Note: "install the service binary " + l.Binary})
	case cur.BinarySHA256 != req.ExecSHA256:
		p.add(Action{Kind: ActInstallBin, Entry: binary.Name, Note: "update the service binary " + l.Binary})
	case !cur.metadataOK(binary):
		p.add(Action{Kind: ActInstallBin, Entry: binary.Name, Note: repairNote(l, binary)})
	}
	if !cur.KeyExists {
		e, _ := l.Entry("vault-key")
		p.add(Action{Kind: ActCreateKey, Entry: e.Name, Note: entryNote(l, e, "create the vault key")})
		p.Backup = backup
	}
	config, _ := l.Entry("config")
	switch {
	case acct.config != nil && cur.Config == nil:
		p.add(Action{Kind: ActWriteConfig, Entry: config.Name, Data: acct.config, Note: "write " + l.Config + " with account " + acct.spec.Name})
	case acct.config != nil:
		p.add(Action{Kind: ActWriteConfig, Entry: config.Name, Data: acct.config, Note: "add account " + acct.spec.Name + " to " + l.Config})
	case cur.Config != nil && !cur.metadataOK(config):
		if err := checkConfigRepair(l, cur, p, config); err != nil {
			return Plan{}, err
		}
		p.add(Action{Kind: ActWriteConfig, Entry: config.Name, Data: cur.Config, Note: repairNote(l, config)})
	}
	service, _ := l.Entry("service")
	switch rendered := RenderService(l); {
	case !bytes.Equal(cur.Service, rendered):
		p.add(Action{Kind: ActWriteService, Entry: service.Name, Data: rendered, Note: "write the service definition " + l.Service})
	case !cur.metadataOK(service):
		p.add(Action{Kind: ActWriteService, Entry: service.Name, Data: rendered, Note: repairNote(l, service)})
	}
	// Every file change restarts the daemon: a new binary, key, config or
	// service definition, and a rewrite that only repairs metadata.
	p.ServiceChanged = slices.ContainsFunc(p.Actions, func(a Action) bool {
		return slices.Contains([]ActionKind{ActInstallBin, ActCreateKey, ActWriteConfig, ActWriteService}, a.Kind)
	})
	if p.ServiceChanged {
		p.add(Action{Kind: ActActivate, Note: "start or restart " + l.ServiceLabel})
	}
	planSecret(&p, req.Terminal)
	return p, nil
}

// errBackupNeedsTerminal refuses --key-backup show without a terminal.
func errBackupNeedsTerminal() error {
	return refuse("run bootstrap from a terminal, or pass --key-backup none",
		"--key-backup show needs a terminal; captured output would hold the key")
}

// add appends a to the plan's actions.
func (p *Plan) add(a Action) { p.Actions = append(p.Actions, a) }

// planSecret adds the secret, login and test steps.
func planSecret(p *Plan, terminal bool) {
	name := p.Account.Name
	oauth := p.Account.Auth == provider.AuthOAuth2
	if p.NeedSecret {
		p.add(Action{Kind: ActStoreSecret, Name: name, Note: "store the secret " + p.SecretName})
		if oauth {
			if !terminal {
				// Without a terminal nobody can finish the browser flow,
				// and the account cannot be tested before it.
				p.NextSteps = append(p.NextSteps, "chainmail auth login "+name)
				return
			}
			p.add(Action{Kind: ActAuthLogin, Name: name, Note: "log in to account " + name + " through the browser"})
		}
	}
	p.add(Action{Kind: ActTestAccount, Name: name, Note: "test account " + name})
}

// healthy reports whether the daemon said the account has working stored
// credentials.
func healthy(h *AccountHealth) bool {
	return h != nil && h.Exists && h.SecretStored && h.AuthOK
}

// secretName is the vault secret bootstrap stores for a.
func secretName(a AccountSpec) string {
	if a.Auth == provider.AuthOAuth2 {
		return "oauth_clients/" + a.OAuthClient
	}
	return "accounts/" + a.Name
}

// planAccounts plans the group, the daemon user and the invoking user's
// membership. An existing daemon user passed checkDaemonUser, so it is
// already in the group.
func planAccounts(l Layout, cur Current) []Action {
	var actions []Action
	if cur.ClientGroup == nil {
		actions = append(actions, Action{Kind: ActCreateGroup, Name: l.ClientGroup, Note: "create group " + l.ClientGroup})
	}
	if cur.DaemonUser == nil {
		actions = append(actions, Action{
			Kind: ActCreateUser, Name: l.DaemonUser,
			Note: "create user " + l.DaemonUser + " to run the daemon",
		})
	}
	if !isMember(cur.InvokingUser, cur.ClientGroup, l.ClientGroup) {
		actions = append(actions, Action{
			Kind: ActAddMember, Name: cur.InvokingUser.Name,
			Note: "add " + cur.InvokingUser.Name + " to group " + l.ClientGroup + " so it can reach the daemon",
		})
	}
	return actions
}

// isMember reports whether u belongs to the group called name, described
// by g (nil when it does not exist): as a supplementary group, as its
// primary group, or as a listed member.
func isMember(u system.User, g *system.GroupInfo, name string) bool {
	if g == nil {
		return false
	}
	return slices.Contains(u.Groups, name) || u.GID == g.GID || slices.Contains(g.Members, u.Name)
}

// entryNote describes e with its owner and mode.
func entryNote(l Layout, e Entry, what string) string {
	return fmt.Sprintf("%s %s (%s:%s, %04o)", what, e.Path, ownerName(l, e.Owner), groupName(l, e.Group), e.Mode.Perm())
}

// repairNote describes rewriting e, whose bytes are unchanged, to give it
// the layout's owner and mode.
func repairNote(l Layout, e Entry) string {
	return entryNote(l, e, "set the owner and mode of")
}

// metadataOK reports whether the file entry e, as Inspect found it, has
// the layout's owner, group and mode exactly, special bits included. A
// file that is missing or not a regular file counts as fine here: Build
// writes it anyway, because it has no bytes to compare.
func (cur Current) metadataOK(e Entry) bool {
	info := cur.files[e.Name]
	if !info.Exists || !info.Mode.IsRegular() {
		return true
	}
	uid, gid, ok := cur.entryIDs(e)
	return ok && info.UID == uid && info.GID == gid && info.Mode == e.Mode
}

// entryIDs resolves e's owner and group to the ids Inspect found, and
// false when the user or group does not exist yet.
func (cur Current) entryIDs(e Entry) (uid, gid uint32, ok bool) {
	if e.Owner == OwnerDaemon {
		if cur.DaemonUser == nil {
			return 0, 0, false
		}
		uid = cur.DaemonUser.UID
	}
	var g *system.GroupInfo
	switch e.Group {
	case GroupDaemon:
		g = cur.daemonGroup
	case GroupClients:
		g = cur.ClientGroup
	default:
		return uid, 0, true
	}
	if g == nil {
		return 0, 0, false
	}
	return uid, g.GID, true
}

// checkConfigRepair refuses a metadata-only rewrite of the existing config
// that Validate would refuse, such as one whose account has endpoint
// overrides a plan may not carry. The user fixes the owner and mode by
// hand instead, and the config is left as it is.
func checkConfigRepair(l Layout, cur Current, p Plan, e Entry) error {
	info := cur.files[e.Name]
	owner := ownerName(l, e.Owner) + ":" + groupName(l, e.Group)
	mode := fmt.Sprintf("%04o", e.Mode.Perm())
	return validateConfig(l, p, cur.Config, func(why string) error {
		return refuse(fmt.Sprintf("fix it with: sudo chown %s %s && sudo chmod %s %s, then run bootstrap again", owner, l.Config, mode, l.Config),
			"the config %s has owner uid %d gid %d and mode %s, not %s %s, and bootstrap cannot rewrite it through a plan: %s",
			l.Config, info.UID, info.GID, octalMode(info.Mode), owner, mode, why)
	})
}

// ownerName is the user name o resolves to on l's platform.
func ownerName(l Layout, o Owner) string {
	if o == OwnerDaemon {
		return l.DaemonUser
	}
	return "root"
}

// groupName is the group name g resolves to on l's platform.
func groupName(l Layout, g Group) string {
	switch g {
	case GroupDaemon:
		return daemonGroup(l)
	case GroupClients:
		return l.ClientGroup
	default:
		return l.RootGroup
	}
}

// checkTrust runs the trust check on every trust path against the chain
// Inspect recorded. The daemon user's uid is trusted once it exists, and
// only after checkDaemonIdentity has passed it: a uid shared with root or
// with the invoking user is refused, never trusted.
func checkTrust(l Layout, cur Current) error {
	if cur.trust == nil {
		return errors.New("plan: the installation was not inspected")
	}
	if err := checkDaemonIdentity(cur.DaemonUser, cur.InvokingUser); err != nil {
		return err
	}
	if info, err := cur.trust.Lstat(l.Config); err == nil && info.Mode&fs.ModeSymlink != 0 {
		return refuse("replace the link with a regular file owned by root, or manage chainmail by hand",
			"the config %s is a symlink, as one home-manager manages would be; bootstrap does not write through links", l.Config)
	}
	trusted := []uint32{0}
	if cur.DaemonUser != nil {
		trusted = append(trusted, cur.DaemonUser.UID)
	}
	for _, path := range trustPaths(l) {
		if err := CheckTrust(cur.trust, path, trusted); err != nil {
			return err
		}
	}
	return nil
}

// checkInvoking refuses an invoking user bootstrap must not run as.
func checkInvoking(l Layout, inv system.User) error {
	switch {
	case inv.Name == "":
		return errors.New("plan: no invoking user")
	case inv.UID == 0 || inv.Name == "root":
		return refuse("run bootstrap as the user who will use chainmail; it asks for sudo itself",
			"bootstrap must not run as root")
	case inv.Name == l.DaemonUser:
		return refuse("run bootstrap as the user who will use chainmail",
			"bootstrap must not run as the daemon user %s", l.DaemonUser)
	}
	return nil
}

// accountHint is the hint on every refusal of an existing user or group.
const accountHint = "remove or fix the existing account by hand, then run bootstrap again"

// darwinDaemonHome is the macOS daemon user's home directory.
const darwinDaemonHome = "/var/empty"

// checkDaemonIdentity refuses an existing daemon user u (nil when
// missing) whose uid is root's or a login user's. Only the invoking user
// inv is visible as a login user. The trust check trusts the daemon
// user's uid, so this runs before it.
func checkDaemonIdentity(u *system.User, inv system.User) error {
	if u == nil {
		return nil
	}
	if u.UID < 1 {
		return refuse(accountHint, "the existing user %s has uid %d; the daemon user must not be root", u.Name, u.UID)
	}
	if u.UID == inv.UID {
		return refuse(accountHint, "the existing user %s has uid %d, the same as %s; the daemon user needs a uid of its own",
			u.Name, u.UID, inv.Name)
	}
	return nil
}

// checkDaemonUser refuses an existing daemon user or group that is not
// the shape bootstrap creates. Linux: primary group chainmail, member of
// chainmail-clients, home the state directory, a shell nobody can log in
// with. macOS: primary group chainmail-clients, home /var/empty, shell
// /usr/bin/false. A group with gid 0 is refused on either.
func checkDaemonUser(l Layout, cur Current) error {
	if err := checkDaemonIdentity(cur.DaemonUser, cur.InvokingUser); err != nil {
		return err
	}
	for _, g := range []*system.GroupInfo{cur.ClientGroup, cur.daemonGroup} {
		if g != nil && g.GID == 0 {
			return refuse(accountHint, "the existing group %s has gid 0", g.Name)
		}
	}
	u := cur.DaemonUser
	if u == nil {
		return nil
	}
	bad := func(field, got, want string) error {
		return refuse(accountHint, "the existing user %s has %s %s; the daemon user needs %s", u.Name, field, got, want)
	}
	clients := cur.ClientGroup
	if clients == nil {
		return refuse(accountHint, "the existing user %s needs the group %s, which does not exist", u.Name, l.ClientGroup)
	}
	primary, home, shells := cur.daemonGroup, l.StateDir, nologinShells
	if l.Platform == Darwin {
		home, shells = darwinDaemonHome, []string{"/usr/bin/false"}
	}
	switch {
	case primary == nil:
		return refuse(accountHint, "the existing user %s needs the group %s as its primary group, which does not exist",
			u.Name, daemonGroup(l))
	case u.GID != primary.GID:
		return bad("primary group", fmt.Sprintf("gid %d", u.GID), fmt.Sprintf("the group %s (gid %d)", primary.Name, primary.GID))
	case !isMember(*u, clients, l.ClientGroup):
		return refuse(accountHint, "the existing user %s is not a member of the group %s", u.Name, l.ClientGroup)
	case u.Home != home:
		return bad("home", fmt.Sprintf("%q", u.Home), fmt.Sprintf("%q", home))
	case !slices.Contains(shells, u.Shell):
		return bad("login shell", fmt.Sprintf("%q", u.Shell), "one of "+strings.Join(shells, ", ")+", so nobody can log in as it")
	}
	return nil
}

// keyMode is the only mode an existing vault key may have.
const keyMode fs.FileMode = 0o400

// checkKey refuses an existing vault key that is not a regular file owned
// by the daemon user and group with mode 0400. It runs after
// checkDaemonUser, so the daemon user's gid is the daemon group's.
func checkKey(l Layout, cur Current) error {
	if !cur.KeyExists {
		return nil
	}
	k := cur.key
	owner := l.DaemonUser + ":" + daemonGroup(l)
	fix := fmt.Sprintf("sudo chown %s %s && sudo chmod 0400 %s", owner, l.VaultKey, l.VaultKey)
	exposed := "Anyone who could read the key may already have read it. If it may have been exposed, move " +
		l.VaultKey + " and " + l.Vault + " aside, run bootstrap again to make a new key, and enter every account's password again."
	hint := "fix it with: " + fix + ". " + exposed
	want := fmt.Sprintf("it must be a regular file owned by %s with mode 0400", owner)
	u := cur.DaemonUser
	switch {
	case k.Mode&fs.ModeSymlink != 0:
		return refuse("replace the link with the key file itself, then "+hint,
			"the vault key %s is a symlink; %s", l.VaultKey, want)
	case !k.Mode.IsRegular():
		return refuse("put the key file itself at "+l.VaultKey+", then "+hint,
			"the vault key %s is not a regular file; %s", l.VaultKey, want)
	case u == nil || k.UID != u.UID || k.GID != u.GID:
		return refuse(hint, "the vault key %s is owned by uid %d gid %d; %s", l.VaultKey, k.UID, k.GID, want)
	case k.Mode != keyMode:
		return refuse(hint, "the vault key %s has mode %s; %s", l.VaultKey, octalMode(k.Mode), want)
	}
	return nil
}

// octalMode prints m's permission, setuid, setgid and sticky bits in
// octal, such as 0644 or 4400.
func octalMode(m fs.FileMode) string {
	bits := uint32(m.Perm())
	for flag, bit := range map[fs.FileMode]uint32{fs.ModeSetuid: 0o4000, fs.ModeSetgid: 0o2000, fs.ModeSticky: 0o1000} {
		if m&flag != 0 {
			bits |= bit
		}
	}
	return fmt.Sprintf("%04o", bits)
}

// checkHost refuses a service file or vault bootstrap cannot work with.
func checkHost(l Layout, cur Current) error {
	if cur.Service != nil && !cur.ServiceMarked {
		return refuse("move it aside to let bootstrap write its own, or keep managing the service by hand",
			"the service definition %s has no bootstrap marker, so someone else wrote it; bootstrap will not replace it", l.Service)
	}
	if !cur.KeyExists && cur.VaultExists {
		return refuse("restore the old key to "+l.VaultKey+", or move "+l.Vault+" aside, then run bootstrap again",
			"the vault %s exists but the vault key %s does not; a new key could never open that vault", l.Vault, l.VaultKey)
	}
	return nil
}

// checkRequest refuses a request with a bad binary hash, backup choice,
// overrides, address or provider, and returns the backup choice.
func checkRequest(req Request) (Backup, error) {
	if !validSHA256(req.ExecSHA256) {
		return "", refuse("run bootstrap from an installed chainmail binary",
			"the SHA-256 of the running binary is missing or malformed")
	}
	backup, err := ParseBackup(string(req.Backup))
	if err != nil {
		return "", err
	}
	if err := checkOverrides(req.IMAP, req.SMTP); err != nil {
		return "", err
	}
	if _, _, err := ParseAddress(req.Address); err != nil {
		return "", err
	}
	if err := checkProvider(req.Detection.Provider, req.Detection.Auth); err != nil {
		return "", err
	}
	return backup, nil
}

// validSHA256 reports whether s is a lowercase hex SHA-256.
func validSHA256(s string) bool {
	if len(s) != hex.EncodedLen(32) || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// checkProvider refuses a preset bootstrap does not set up and an auth
// method the preset does not take.
func checkProvider(name string, auth provider.AuthMethod) error {
	if _, err := detectOverride(name); err != nil {
		return err
	}
	if p, _ := provider.Lookup(name); !p.SupportsAuth(auth) {
		return refuse(providerHint(), "provider %s does not take auth %q", name, auth)
	}
	return nil
}

// checkOverrides refuses endpoint overrides outside an e2e build, and
// malformed ones inside it.
func checkOverrides(eps ...*config.EndpointOverride) error {
	for _, ep := range eps {
		if ep == nil {
			continue
		}
		if !E2EEnabled() {
			return refuse("endpoint overrides exist only for chainmail's end-to-end tests",
				"endpoint overrides are only accepted by an e2e build")
		}
		if ep.Host == "" || ep.Port < 1 || ep.Port > 65535 ||
			(ep.Security != "" && ep.Security != provider.SecurityTLS.String()) {
			return refuse("set CHAINMAIL_E2E_IMAP and CHAINMAIL_E2E_SMTP to host:port",
				"endpoint override %s:%d with security %q is not valid", ep.Host, ep.Port, ep.Security)
		}
	}
	return nil
}

// plannedAccount is the account a plan sets up.
type plannedAccount struct {
	spec   AccountSpec
	client *OAuthClientSpec
	isNew  bool   // the config does not have the account yet
	config []byte // the config to write, nil for no change
}

// planAccount works out the account from the request and the existing
// config, refusing conflicts, and renders the config to write.
func planAccount(l Layout, cur Current, req Request) (plannedAccount, error) {
	name, err := requestName(req)
	if err != nil {
		return plannedAccount{}, err
	}

	var cfg *config.Config
	if cur.Config != nil {
		if cfg, err = config.Parse(cur.Config); err != nil {
			return plannedAccount{}, refuse("fix the config by hand so it parses, then run bootstrap again",
				"the existing config %s does not parse: %v", l.Config, err)
		}
		if err := checkDaemonTable(l, cfg.Daemon); err != nil {
			return plannedAccount{}, err
		}
		if err := checkCacheDir(l, cfg.CacheDir()); err != nil {
			return plannedAccount{}, err
		}
	}

	spec, client, found, err := resolveAccount(cfg, name, req)
	if err != nil {
		return plannedAccount{}, err
	}
	pa := plannedAccount{spec: spec, client: client, isNew: !found}

	switch {
	case cur.Config == nil:
		pa.config = NewConfig(l, pa.spec, pa.client)
	case !found:
		data, err := AppendAccount(cur.Config, pa.spec, pa.client)
		if err != nil {
			return plannedAccount{}, err
		}
		pa.config = data
	}
	return pa, nil
}

// requestName is the account name req asks for: --name, or the name
// derived from the address.
func requestName(req Request) (string, error) {
	name := req.Name
	if name == "" {
		var err error
		if name, err = AccountName(req.Address); err != nil {
			return "", err
		}
	}
	if !config.ValidName(name) {
		return "", refuse("pass --name <account> with lowercase letters, digits and dashes",
			"invalid account name %q", name)
	}
	return name, nil
}

// resolveAccount works out the account called name that req sets up, and
// its OAuth client, from the existing config cfg (nil when there is none).
// It refuses a request that conflicts with the account or client already
// in cfg. found reports whether cfg has the account.
func resolveAccount(cfg *config.Config, name string, req Request) (spec AccountSpec, client *OAuthClientSpec, found bool, err error) {
	prov, auth := req.Detection.Provider, req.Detection.Auth
	spec = AccountSpec{
		Name: name, Provider: prov, Address: req.Address, Auth: auth,
		IMAP: req.IMAP, SMTP: req.SMTP,
	}
	var existing config.Account
	if cfg != nil {
		existing, found = cfg.Accounts[name]
	}
	if found {
		if err := matchExisting(name, existing, req); err != nil {
			return AccountSpec{}, nil, false, err
		}
		spec.Address = existing.Address
		if auth == provider.AuthOAuth2 {
			spec.OAuthClient = existing.OAuthClient
		}
	} else if auth == provider.AuthOAuth2 {
		spec.OAuthClient = cmp.Or(req.OAuthClient, defaultOAuthClient)
	}

	if auth == provider.AuthOAuth2 {
		if client, err = planClient(cfg, spec.OAuthClient, req.OAuthClientID, prov); err != nil {
			return AccountSpec{}, nil, false, err
		}
	}
	return spec, client, found, nil
}

// checkDaemonTable refuses a parsed [daemon] table that moves the socket
// away from the layout's or hands it to another group, so an existing
// config cannot point the daemon somewhere bootstrap does not check.
// config.Parse fills in the defaults, so an unset socket is
// config.DefaultSocket, which is the layout's on Linux only.
func checkDaemonTable(l Layout, d config.Daemon) error {
	if d.Socket != l.Socket {
		return refuse(fmt.Sprintf("set socket = %q in the [daemon] table of %s, or remove it on Linux", l.Socket, l.Config),
			"the existing config %s puts daemon.socket at %q; bootstrap's daemon listens on %s", l.Config, d.Socket, l.Socket)
	}
	if d.ClientGroup != l.ClientGroup {
		return refuse("remove client_group from the [daemon] table of "+l.Config+", or set it to "+l.ClientGroup,
			"the existing config %s sets daemon.client_group to %q; bootstrap's daemon serves the group %s", l.Config, d.ClientGroup, l.ClientGroup)
	}
	return nil
}

// checkCacheDir refuses an existing config whose [cache] dir is set to
// anything but the layout's state directory. The daemon keeps cache.db,
// whose contents it trusts, in that directory, and bootstrap checks the
// trust chain of the state directory only. The comparison is exact, so
// another spelling of the same directory is refused too.
func checkCacheDir(l Layout, dir string) error {
	if dir == "" || dir == l.StateDir {
		return nil
	}
	return refuse("remove dir from the [cache] table of "+l.Config+", or set it to "+l.StateDir,
		"the existing config %s sets cache.dir to %q; bootstrap's daemon keeps its cache in %s", l.Config, dir, l.StateDir)
}

// matchExisting refuses a request that conflicts with the account of the
// same name already in the config.
func matchExisting(name string, existing config.Account, req Request) error {
	hint := "choose another account name with --name"
	if !strings.EqualFold(existing.Address, req.Address) {
		return refuse(hint, "account %q already exists with another address, %s", name, existing.Address)
	}
	if existing.Provider != req.Detection.Provider {
		return refuse(hint+", or pass --provider "+existing.Provider,
			"account %q uses provider %s, not %s", name, existing.Provider, req.Detection.Provider)
	}
	if provider.AuthMethod(existing.Auth) != req.Detection.Auth {
		return refuse(hint+", or change the account's auth by hand",
			"account %q uses auth %s, not %s", name, existing.Auth, req.Detection.Auth)
	}
	return nil
}

// planClient returns the OAuth client an oauth2 account uses: the table
// already in cfg (nil when there is no config), or a new one with id.
func planClient(cfg *config.Config, name, id, prov string) (*OAuthClientSpec, error) {
	if !config.ValidName(name) {
		return nil, refuse("pass --oauth-client <name> with lowercase letters, digits and dashes",
			"invalid OAuth client name %q", name)
	}
	if strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return nil, refuse("pass the client ID from the Google Cloud console", "the OAuth client ID holds control characters")
	}
	if cfg != nil {
		if have, ok := cfg.OAuthClients[name]; ok {
			if id != "" && id != have.ClientID {
				return nil, refuse("use the client ID already in the config, or name another client with --oauth-client",
					"OAuth client %q already exists in the config with another client ID", name)
			}
			return &OAuthClientSpec{Name: name, ClientID: have.ClientID}, nil
		}
	}
	if id == "" {
		return nil, refuse("pass --oauth-client-id <id> from the Google Cloud console",
			"provider %s needs an OAuth client ID; pass --oauth-client-id", prov)
	}
	return &OAuthClientSpec{Name: name, ClientID: id}, nil
}

// Same reports whether two plans would make the same changes: every
// field and every action, with its data and note, is equal.
func Same(a, b Plan) bool {
	return a.Version == b.Version &&
		a.Platform == b.Platform &&
		a.InvokingUser == b.InvokingUser &&
		sameAccount(a.Account, b.Account) &&
		sameClient(a.OAuthClient, b.OAuthClient) &&
		a.SecretName == b.SecretName &&
		a.NeedSecret == b.NeedSecret &&
		a.Backup == b.Backup &&
		a.ServiceChanged == b.ServiceChanged &&
		slices.EqualFunc(a.Actions, b.Actions, sameAction) &&
		slices.Equal(a.NextSteps, b.NextSteps) &&
		a.NixState == b.NixState &&
		a.NixDir == b.NixDir &&
		a.DaemonBinary == b.DaemonBinary
}

func sameAction(a, b Action) bool {
	return a.Kind == b.Kind && a.Entry == b.Entry && a.Name == b.Name &&
		a.Note == b.Note && bytes.Equal(a.Data, b.Data)
}

func sameAccount(a, b AccountSpec) bool {
	return a.Name == b.Name && a.Provider == b.Provider && a.Address == b.Address &&
		a.Auth == b.Auth && a.OAuthClient == b.OAuthClient &&
		sameEndpoint(a.IMAP, b.IMAP) && sameEndpoint(a.SMTP, b.SMTP)
}

func sameClient(a, b *OAuthClientSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameEndpoint(a, b *config.EndpointOverride) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Render prints the plan as numbered lines for the terminal, one per
// action. It prints notes only, never file contents.
func (p Plan) Render(w io.Writer) error {
	width := len(fmt.Sprint(len(p.Actions)))
	for i, a := range p.Actions {
		if _, err := fmt.Fprintf(w, "%*d. %s\n", width, i+1, a.Note); err != nil {
			return fmt.Errorf("print plan: %w", err)
		}
	}
	return nil
}
