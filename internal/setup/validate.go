package setup

import (
	"bytes"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/yaklabco/chainmail/internal/config"
	"github.com/yaklabco/chainmail/internal/provider"
)

// validateHint is the hint on every plan Validate refuses.
const validateHint = "run chainmail bootstrap again; bootstrap apply only takes plans bootstrap made"

// userNameRE matches the user names a plan may name: portable POSIX names.
var userNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// Validate is the apply-side check: platform, version, every Entry known
// to l, user and group names equal to l's, account fields valid, overrides
// only when e2eEnabled. It returns a *Refusal.
//
// Everything is checked against l and this binary's own rendering, never
// against the plan: the service text must equal RenderService(l), and
// the config text must parse, hold exactly the plan's account and keep
// the layout's socket and client group. The actions must come in the
// order Build writes them, each at most once (ensure-dir at most once per
// directory, in layout order), with test-account present unless the plan
// leaves an OAuth login to the user, and ServiceChanged true exactly when
// the plan changes the binary, key, config or service definition and
// activates the service.
//
// Validate is a structural gate only. It sees the plan and nothing else,
// so it cannot see the trusted current bytes on disk: it cannot tell a
// config change bootstrap planned from one someone wrote into the plan by
// hand, as long as both parse and hold the account, and it cannot verify
// that InvokingUser is the user who really ran sudo. Apply binds the
// plan's content: under the lock it inspects the installation again,
// resolves the invoking user from SUDO_UID, rebuilds the plan with Build
// and refuses unless Same reports the two plans equal.
func Validate(l Layout, p Plan) error {
	if p.Version != planVersion {
		return refuse(validateHint, "plan version %d is not %d", p.Version, planVersion)
	}
	if p.Platform != l.Platform {
		return refuse(validateHint, "plan is for %q, this host is %q", p.Platform, l.Platform)
	}
	if !userNameRE.MatchString(p.InvokingUser) || p.InvokingUser == "root" || p.InvokingUser == l.DaemonUser {
		return refuse(validateHint, "plan names invoking user %q", p.InvokingUser)
	}
	if err := validateAccount(p); err != nil {
		return err
	}
	if _, err := ParseBackup(string(p.Backup)); err != nil {
		return err
	}
	for _, a := range p.Actions {
		if err := validateAction(l, p, a); err != nil {
			return err
		}
	}
	counts, err := validateOrder(l, p.Actions)
	if err != nil {
		return err
	}
	return validateSteps(p, counts)
}

// actionOrder is the order Build writes action kinds in.
var actionOrder = []ActionKind{
	ActCreateGroup, ActCreateUser, ActAddMember, ActEnsureDir,
	ActInstallBin, ActCreateKey, ActWriteConfig, ActWriteService,
	ActActivate, ActStoreSecret, ActAuthLogin, ActTestAccount,
}

// validateOrder refuses actions out of Build's order, a kind other than
// ensure-dir more than once, and an ensure-dir repeated or out of layout
// order. It returns how many actions of each kind the plan has. Every
// action already passed validateAction, so its kind and entry are known.
func validateOrder(l Layout, actions []Action) (map[ActionKind]int, error) {
	var dirs []string
	for _, e := range l.Entries() {
		if e.Kind == KindDir {
			dirs = append(dirs, e.Name)
		}
	}
	counts := map[ActionKind]int{}
	lastKind, lastDir := -1, -1
	for _, a := range actions {
		kind := slices.Index(actionOrder, a.Kind)
		if kind < lastKind {
			return nil, refuse(validateHint, "plan step %s comes after %s", a.Kind, actionOrder[lastKind])
		}
		lastKind = kind
		counts[a.Kind]++
		if a.Kind == ActEnsureDir {
			dir := slices.Index(dirs, a.Entry)
			if dir <= lastDir {
				return nil, refuse(validateHint, "plan step ensure-dir %s is repeated or out of layout order", a.Entry)
			}
			lastDir = dir
			continue
		}
		if counts[a.Kind] > 1 {
			return nil, refuse(validateHint, "plan has more than one %s step", a.Kind)
		}
	}
	return counts, nil
}

// validateSteps checks the plan's fields against the actions it has:
// secret, login and test steps, next steps, ServiceChanged and Backup.
func validateSteps(p Plan, counts map[ActionKind]int) error {
	stores, logins, tests := counts[ActStoreSecret], counts[ActAuthLogin], counts[ActTestAccount]
	if p.NeedSecret != (stores == 1) {
		return refuse(validateHint, "plan has %d store-secret steps with NeedSecret %v", stores, p.NeedSecret)
	}
	if logins == 1 && !p.NeedSecret {
		return refuse(validateHint, "plan logs in to account %q without storing its secret", p.Account.Name)
	}
	var wantSteps []string
	if p.Account.Auth == provider.AuthOAuth2 && p.NeedSecret && logins == 0 {
		wantSteps = []string{"chainmail auth login " + p.Account.Name}
	}
	if !slices.Equal(p.NextSteps, wantSteps) {
		return refuse(validateHint, "plan has next steps %q", p.NextSteps)
	}
	// The account is tested unless the user still has to log in.
	if wantTests := 1 - len(wantSteps); tests != wantTests {
		return refuse(validateHint, "plan has %d test-account steps, want %d", tests, wantTests)
	}
	changes := counts[ActInstallBin] + counts[ActCreateKey] + counts[ActWriteConfig] + counts[ActWriteService]
	if p.ServiceChanged != (changes > 0) || p.ServiceChanged != (counts[ActActivate] == 1) {
		return refuse(validateHint, "plan has ServiceChanged %v with %d changes and %d activate-service steps",
			p.ServiceChanged, changes, counts[ActActivate])
	}
	if p.Backup == BackupShow && counts[ActCreateKey] == 0 {
		return refuse(validateHint, "plan shows a vault key it does not create")
	}
	return nil
}

// validateAccount checks the account, its OAuth client, its secret name
// and its overrides.
func validateAccount(p Plan) error {
	a := p.Account
	if !config.ValidName(a.Name) {
		return refuse(validateHint, "plan names account %q", a.Name)
	}
	if _, _, err := ParseAddress(a.Address); err != nil {
		return err
	}
	if err := checkProvider(a.Provider, a.Auth); err != nil {
		return err
	}
	if err := checkOverrides(a.IMAP, a.SMTP); err != nil {
		return err
	}
	switch a.Auth {
	case provider.AuthPassword:
		if a.OAuthClient != "" || p.OAuthClient != nil {
			return refuse(validateHint, "plan gives password account %q an OAuth client", a.Name)
		}
	case provider.AuthOAuth2:
		c := p.OAuthClient
		if !config.ValidName(a.OAuthClient) || c == nil || c.Name != a.OAuthClient || c.ClientID == "" {
			return refuse(validateHint, "plan gives OAuth account %q no valid client", a.Name)
		}
	}
	if p.SecretName != secretName(a) {
		return refuse(validateHint, "plan stores secret %q for account %q", p.SecretName, a.Name)
	}
	return nil
}

// validateAction checks one action against l and the plan's account.
func validateAction(l Layout, p Plan, a Action) error {
	bad := func(why string) error {
		return refuse(validateHint, "plan step %s %s%s: %s", a.Kind, a.Entry, a.Name, why)
	}
	if a.Note == "" || strings.ContainsFunc(a.Note, unicode.IsControl) {
		return bad("has an empty note or one with control characters")
	}
	if a.Data != nil && a.Kind != ActWriteConfig && a.Kind != ActWriteService {
		return bad("carries file contents")
	}
	switch a.Kind {
	case ActCreateGroup:
		return wantTarget(a, "", l.ClientGroup, bad)
	case ActCreateUser:
		return wantTarget(a, "", l.DaemonUser, bad)
	case ActAddMember:
		// An existing daemon user is never repaired, and a new one is
		// created in the group, so only the invoking user is added.
		return wantTarget(a, "", p.InvokingUser, bad)
	case ActEnsureDir:
		e, ok := l.Entry(a.Entry)
		if !ok || e.Kind != KindDir || a.Name != "" {
			return bad("is not a directory of the layout")
		}
	case ActInstallBin:
		return wantTarget(a, "binary", "", bad)
	case ActCreateKey:
		return wantTarget(a, "vault-key", "", bad)
	case ActWriteConfig:
		if err := wantTarget(a, "config", "", bad); err != nil {
			return err
		}
		return validateConfig(l, p, a.Data, bad)
	case ActWriteService:
		if err := wantTarget(a, "service", "", bad); err != nil {
			return err
		}
		if !bytes.Equal(a.Data, RenderService(l)) {
			return bad("service definition differs from the one this binary writes")
		}
	case ActActivate:
		return wantTarget(a, "", "", bad)
	case ActStoreSecret, ActTestAccount:
		return wantTarget(a, "", p.Account.Name, bad)
	case ActAuthLogin:
		if p.Account.Auth != provider.AuthOAuth2 {
			return bad("logs in to an account that does not use OAuth")
		}
		return wantTarget(a, "", p.Account.Name, bad)
	default:
		return bad("unknown step")
	}
	return nil
}

// wantTarget refuses an action whose Entry or Name is not the one given.
func wantTarget(a Action, entry, name string, bad func(string) error) error {
	if a.Entry != entry || a.Name != name {
		return bad("names the wrong target")
	}
	return nil
}

// validateConfig checks that data parses, keeps the layout's socket and
// client group, and holds the plan's account, and its OAuth client,
// exactly.
func validateConfig(l Layout, p Plan, data []byte, bad func(string) error) error {
	cfg, err := config.Parse(data)
	if err != nil {
		return bad("config does not parse")
	}
	if cfg.Daemon.Socket != l.Socket || cfg.Daemon.ClientGroup != l.ClientGroup {
		return bad("config moves the daemon socket or its client group")
	}
	want := p.Account
	got, ok := cfg.Accounts[want.Name]
	if !ok {
		return bad("config does not hold the account")
	}
	if got.Provider != want.Provider || got.Address != want.Address ||
		provider.AuthMethod(got.Auth) != want.Auth || got.OAuthClient != want.OAuthClient ||
		!sameEndpoint(got.IMAP, withSecurity(want.IMAP)) || !sameEndpoint(got.SMTP, withSecurity(want.SMTP)) {
		return bad("config account differs from the plan's")
	}
	if c := p.OAuthClient; c != nil && cfg.OAuthClients[c.Name].ClientID != c.ClientID {
		return bad("config OAuth client differs from the plan's")
	}
	return nil
}

// withSecurity returns ep with config.Parse's default security applied.
func withSecurity(ep *config.EndpointOverride) *config.EndpointOverride {
	if ep == nil || ep.Security != "" {
		return ep
	}
	c := *ep
	c.Security = provider.SecurityTLS.String()
	return &c
}
