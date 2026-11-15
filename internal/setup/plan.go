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
)

// Action is one step of a plan.
type Action struct {
	Kind  ActionKind
	Entry string // layout entry name for dir and file actions, else ""
	Name  string // group, user or account name, else ""
	Data  []byte // file contents for write-config and write-service
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
}

// Build plans everything from cur and req, or returns a *Refusal. cur
// must come from Inspect, which records the trust chain Build checks.
//
// Build checks the trust chain first, then every other refusal, and
// plans actions only for what is missing or different. Directories are
// always ensured, which fixes their owners and modes.
func Build(l Layout, cur Current, req Request) (Plan, error) {
	if cur.Platform != l.Platform {
		return Plan{}, fmt.Errorf("plan: installation inspected for %s, layout is for %s", cur.Platform, l.Platform)
	}
	if cur.NixOS {
		return Plan{}, refuse("use the NixOS module instead: "+deployPage+"#nixos",
			"bootstrap does not set up chainmail on NixOS; the system configuration owns users, files and services there")
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
	if cur.BinarySHA256 != req.ExecSHA256 {
		verb := "install"
		if cur.BinarySHA256 != "" {
			verb = "update"
		}
		p.add(Action{Kind: ActInstallBin, Entry: "binary", Note: verb + " the service binary " + l.Binary})
		p.ServiceChanged = true
	}
	if !cur.KeyExists {
		e, _ := l.Entry("vault-key")
		p.add(Action{Kind: ActCreateKey, Entry: e.Name, Note: entryNote(l, e, "create the vault key")})
		p.Backup = backup
		p.ServiceChanged = true
	}
	if acct.config != nil {
		note := "write " + l.Config + " with account " + acct.spec.Name
		if cur.Config != nil {
			note = "add account " + acct.spec.Name + " to " + l.Config
		}
		p.add(Action{Kind: ActWriteConfig, Entry: "config", Data: acct.config, Note: note})
		p.ServiceChanged = true
	}
	if service := RenderService(l); !bytes.Equal(cur.Service, service) {
		p.add(Action{Kind: ActWriteService, Entry: "service", Data: service, Note: "write the service definition " + l.Service})
		p.ServiceChanged = true
	}
	if p.ServiceChanged {
		p.add(Action{Kind: ActActivate, Note: "start or restart " + l.ServiceLabel})
	}
	planSecret(&p, req.Terminal)
	return p, nil
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

// planAccounts plans the group, the daemon user and the memberships.
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
	} else if !isMember(*cur.DaemonUser, cur.ClientGroup, l.ClientGroup) {
		actions = append(actions, Action{
			Kind: ActAddMember, Name: l.DaemonUser,
			Note: "add " + l.DaemonUser + " to group " + l.ClientGroup,
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
// Inspect recorded. The daemon user's uid is trusted once it exists.
func checkTrust(l Layout, cur Current) error {
	if cur.trust == nil {
		return errors.New("plan: the installation was not inspected")
	}
	if info, err := cur.trust.Lstat(l.Config); err == nil && info.Mode&fs.ModeSymlink != 0 {
		return refuse("replace the link with a regular file owned by root, or manage chainmail by hand",
			"the config %s is a symlink, as one home-manager manages would be; bootstrap does not write through links", l.Config)
	}
	trusted := []uint32{0}
	if cur.DaemonUser != nil && cur.DaemonUser.UID != 0 {
		trusted = append(trusted, cur.DaemonUser.UID)
	}
	for _, path := range trustPaths(l) {
		if err := CheckTrust(cur.trust, path, trusted); err != nil {
			return err
		}
	}
	return nil
}

// checkHost refuses an invoking user, daemon user, group, service file or
// key that bootstrap cannot work with.
func checkHost(l Layout, cur Current) error {
	inv := cur.InvokingUser
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

	shapeHint := "remove or fix the existing account by hand, then run bootstrap again"
	if u := cur.DaemonUser; u != nil {
		if u.UID == 0 {
			return refuse(shapeHint, "the existing user %s has uid 0", u.Name)
		}
		if !slices.Contains(nologinShells, u.Shell) {
			return refuse(shapeHint, "the existing user %s has the login shell %q; the daemon user must not be able to log in", u.Name, u.Shell)
		}
	}
	if g := cur.ClientGroup; g != nil && g.GID == 0 {
		return refuse(shapeHint, "the existing group %s has gid 0", g.Name)
	}

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
	name := req.Name
	if name == "" {
		var err error
		if name, err = AccountName(req.Address); err != nil {
			return plannedAccount{}, err
		}
	}
	if !config.ValidName(name) {
		return plannedAccount{}, refuse("pass --name <account> with lowercase letters, digits and dashes",
			"invalid account name %q", name)
	}

	var cfg *config.Config
	if cur.Config != nil {
		var err error
		if cfg, err = config.Parse(cur.Config); err != nil {
			return plannedAccount{}, refuse("fix the config by hand so it parses, then run bootstrap again",
				"the existing config %s does not parse: %v", l.Config, err)
		}
	}

	prov, auth := req.Detection.Provider, req.Detection.Auth
	pa := plannedAccount{spec: AccountSpec{
		Name: name, Provider: prov, Address: req.Address, Auth: auth,
		IMAP: req.IMAP, SMTP: req.SMTP,
	}}
	existing, found := config.Account{}, false
	if cfg != nil {
		existing, found = cfg.Accounts[name]
	}
	pa.isNew = !found
	if found {
		if err := matchExisting(name, existing, req); err != nil {
			return plannedAccount{}, err
		}
		pa.spec.Address = existing.Address
		if auth == provider.AuthOAuth2 {
			pa.spec.OAuthClient = existing.OAuthClient
		}
	} else if auth == provider.AuthOAuth2 {
		pa.spec.OAuthClient = cmp.Or(req.OAuthClient, defaultOAuthClient)
	}

	if auth == provider.AuthOAuth2 {
		client, err := planClient(cfg, pa.spec.OAuthClient, req.OAuthClientID, prov)
		if err != nil {
			return plannedAccount{}, err
		}
		pa.client = client
	}

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
		slices.Equal(a.NextSteps, b.NextSteps)
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
